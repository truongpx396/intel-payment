package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// runner owns shards and runs what the writer does for them. A shard is owned through its Postgres session
// advisory lock; one goroutine per owned shard drains it, so a slow shard holds up no other.
type runner struct {
	s        settings
	log      *slog.Logger
	books    *pgadapter.Books
	writer   *app.Writer
	replayer *app.JournalReplayer
	expirer  *app.Expirer

	owned map[domain.Shard]*ownership // touched only by the ownership loop
	wg    sync.WaitGroup              // every goroutine it starts
}

type ownership struct {
	lease  ports.Lease
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *runner) run(ctx context.Context) error {
	r.owned = map[domain.Shard]*ownership{}
	r.start(ctx, "journal replay", r.s.JournalReplayInterval, r.replay)
	r.start(ctx, "transfer re-drive", r.s.TransferRedrive/2, r.redrive)
	r.start(ctx, "gauges", 15*time.Second, func(ctx context.Context) error { return r.writer.ExportGauges(ctx) })
	r.start(ctx, "credit expiry", time.Minute, func(ctx context.Context) error { _, err := r.expirer.Tick(ctx, 100); return err })

	r.manageOwnership(ctx) // returns when ctx is done
	r.log.Info("stopping", "owned", len(r.owned), "timeout", r.s.ShutdownTimeout.String())

	for _, o := range r.owned {
		o.cancel()
	}
	finished := make(chan struct{})
	go func() { r.wg.Wait(); close(finished) }()
	select {
	case <-finished:
		return nil
	case <-time.After(r.s.ShutdownTimeout):
		return fmt.Errorf("work was still in hand after %v and was abandoned: raise PAYMENT_SHUTDOWN_TIMEOUT if batches outlast it", r.s.ShutdownTimeout)
	}
}

// manageOwnership takes every shard it can have, drops those whose lock has gone, and tries again.
func (r *runner) manageOwnership(ctx context.Context) {
	t := time.NewTicker(r.s.OwnershipRetry)
	defer t.Stop()
	for {
		r.reap(ctx)
		r.acquire(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reap lets go of shards whose session is gone. The lock went with it, so another worker may already be writing;
// this one must stop.
func (r *runner) reap(ctx context.Context) {
	for sh, o := range r.owned {
		select {
		case <-o.done: // its loop ended on its own
		default:
			if o.lease.Alive(ctx) {
				continue
			}
			r.log.Warn("shard lock lost", "shard", int(sh))
			o.cancel()
			<-o.done
		}
		delete(r.owned, sh)
	}
}

func (r *runner) acquire(ctx context.Context) {
	failed := 0
	var first error
	for i := range r.s.Engine.Shards {
		sh := domain.Shard(i)
		if ctx.Err() != nil {
			return
		}
		if _, mine := r.owned[sh]; mine {
			continue
		}
		lease, ok, err := r.books.Acquire(ctx, sh)
		if err != nil {
			failed++
			if first == nil {
				first = err
			}
			break // the pool or the server is the problem; the rest would fail the same way
		}
		if !ok {
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		o := &ownership{lease: lease, cancel: cancel, done: make(chan struct{})}
		r.owned[sh] = o
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer close(o.done)
			r.shardLoop(sctx, sh, lease)
		}()
	}
	if failed > 0 {
		r.log.Error("could not take shards", "owned", len(r.owned), "of", r.s.Engine.Shards, "error", first.Error())
	}
}

// shardLoop drains one shard until told to stop, then lets it go. The batch in hand when it is told to stop is
// finished first: it is a transaction that either commits whole or not at all, and abandoning it only delays the
// acknowledgement.
func (r *runner) shardLoop(ctx context.Context, sh domain.Shard, lease ports.Lease) {
	r.log.Info("shard acquired", "shard", int(sh))
	defer func() {
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			r.log.Warn("releasing a shard", "shard", int(sh), "error", err.Error())
		}
		r.log.Info("shard released", "shard", int(sh))
	}()

	shards := time.Duration(r.s.Engine.Shards)
	spread := func(every time.Duration) time.Time { // shards do not all reconcile at once
		return time.Now().Add(every * time.Duration(int64(sh)+1) / shards)
	}
	nextIncremental, nextFull := spread(r.s.Engine.ReconcileInterval), spread(r.s.Engine.ReconcileFullSweepInterval)
	var lastErr time.Time
	for ctx.Err() == nil {
		n, err := r.drain(ctx, sh)
		if err != nil && time.Since(lastErr) > 10*time.Second {
			lastErr = time.Now()
			r.log.Error("draining a shard", "shard", int(sh), "error", err.Error())
		}
		now := time.Now()
		if now.After(nextIncremental) {
			nextIncremental = now.Add(r.s.Engine.ReconcileInterval)
			r.sweep(ctx, sh, "reconcile", func(c context.Context) error {
				_, err := r.writer.Reconcile(c, sh, domain.ReconcileIncremental)
				return err
			})
		}
		if now.After(nextFull) {
			nextFull = now.Add(r.s.Engine.ReconcileFullSweepInterval)
			r.sweep(ctx, sh, "full reconcile", func(c context.Context) error { _, err := r.writer.Reconcile(c, sh, domain.ReconcileFull); return err })
			r.sweep(ctx, sh, "audit", func(c context.Context) error { _, err := r.writer.Audit(c, sh); return err })
		}
		if n == 0 || err != nil {
			select {
			case <-ctx.Done():
			case <-time.After(r.s.DrainInterval):
			}
		}
	}
}

// drain books one batch. It does not take the loop's context: a stop must not tear a batch in half.
func (r *runner) drain(ctx context.Context, sh domain.Shard) (int, error) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.s.ShutdownTimeout)
	defer cancel()
	return r.writer.Drain(c, sh)
}

func (r *runner) sweep(ctx context.Context, sh domain.Shard, what string, f func(context.Context) error) {
	if ctx.Err() != nil {
		return
	}
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.s.ShutdownTimeout)
	defer cancel()
	if err := f(c); err != nil {
		r.log.Error(what+" failed", "shard", int(sh), "error", err.Error())
	}
}

// start runs f every interval until ctx is done. A failure is logged and tried again next time: every sweep is
// idempotent, and a worker that stopped on one would leave the rest of its work undone.
func (r *runner) start(ctx context.Context, what string, every time.Duration, f func(context.Context) error) {
	if every <= 0 {
		every = time.Second
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.s.ShutdownTimeout)
			if err := f(c); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error(what+" failed", "error", err.Error())
			}
			cancel()
		}
	}()
}

// replay applies journalled usage to the hot tier, again while there is more due.
func (r *runner) replay(ctx context.Context) error {
	for range 20 {
		res, err := r.replayer.Replay(ctx, 200)
		if err != nil {
			return err
		}
		if res.Claimed == 0 {
			return nil
		}
	}
	return nil
}

func (r *runner) redrive(ctx context.Context) error {
	if _, err := r.writer.RedriveTransfers(ctx, r.s.TransferRedrive, 200); err != nil {
		return err
	}
	_, err := r.writer.ReissueCorrections(ctx, r.s.TransferRedrive, 200)
	return err
}
