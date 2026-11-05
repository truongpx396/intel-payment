package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"

	"github.com/truongpx396/intel-payment/cmd/internal/events"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
)

// tickWork is the work behind each scheduled tick (bus-subjects.md § Scheduled ticks). Every handler is an idempotent claim, run by
// app.Ticks: the claim keeps two runs of one tick from working at once, and the work is the same code the worker's own timers run, which
// is idempotent by construction — so a run killed midway is finished by the next, and a tick with nothing due writes nothing.
//
// Not served: `subdrift`. The sweep that asks each provider for the current state of every locally active subscription — the backstop
// for a missed webhook — is designed and not built, so a process does not claim to serve it; a tick published for it is not consumed.
type tickWork struct {
	s        settings
	log      *slog.Logger
	clock    ports.Clock
	r        *runner
	writer   *app.Writer
	replayer *app.JournalReplayer
	expirer  *app.Expirer
	books    *pgadapter.Books
	maint    *pgadapter.Maintenance
	usage    ports.UsageStore
	stream   ports.IntentStream
	bus      ports.Bus
	pay      *payments.Loops
	out      *events.Worker
	met      ports.Metrics
}

// How far ahead partitions are made, in months of ledger and days of usage guard and usage events.
const (
	partitionMonthsAhead = 3
	partitionDaysAhead   = 4
)

func (w *tickWork) handlers() []app.TickHandler {
	shardKey := func(p []byte) (string, error) {
		sp, err := app.ParseShard(p, w.s.Engine.Shards)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d|%s", *sp.Shard, sp.Kind), nil
	}
	return []app.TickHandler{
		{Name: "reconcile", Key: shardKey, Run: func(ctx context.Context, p []byte) (int, error) {
			sp, err := app.ParseShard(p, w.s.Engine.Shards)
			if err != nil {
				return 0, err
			}
			sh, kind := domain.Shard(*sp.Shard), domain.ReconcileKind(sp.Kind)
			return w.onShard(ctx, sh, func() (int, error) {
				due, err := w.writer.ReconcileDue(ctx, sh, kind)
				if err != nil || !due {
					return 0, err
				}
				run, err := w.writer.Reconcile(ctx, sh, kind)
				return run.Checked, err
			})
		}},
		{Name: "audit", Key: shardKey, Run: func(ctx context.Context, p []byte) (int, error) {
			sp, err := app.ParseShard(p, w.s.Engine.Shards)
			if err != nil {
				return 0, err
			}
			sh := domain.Shard(*sp.Shard)
			return w.onShard(ctx, sh, func() (int, error) {
				rep, err := w.writer.Audit(ctx, sh)
				return rep.Checked, err
			})
		}},
		{Name: "transfer", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			a, err := w.writer.RedriveTransfers(ctx, w.s.TransferRedrive, 200)
			if err != nil {
				return a, err
			}
			b, err := w.writer.ReissueCorrections(ctx, w.s.TransferRedrive, 200)
			return a + b, err
		}},
		{Name: "journal", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			total := 0
			for range 20 {
				res, err := w.replayer.Replay(ctx, 200)
				total += res.Claimed
				if err != nil || res.Claimed == 0 {
					return total, err
				}
			}
			return total, nil
		}},
		// The processor does not count what it leased, so a webhook tick that completes is reported as having run.
		{Name: "webhook", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) { return 1, w.pay.Webhook(ctx) }},
		{Name: "outbound", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			res, err := w.out.Dispatcher().Tick(ctx)
			return res.Delivered + res.Retried + res.Dead, err
		}},
		{Name: "expiry", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) { return w.expirer.Tick(ctx, 100) }},
		{Name: "dunning", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) { return w.pay.Dunning(ctx) }},
		{Name: "partitions", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			created, err := w.maint.EnsurePartitions(ctx, partitionMonthsAhead, partitionDaysAhead)
			if err != nil {
				return created, err
			}
			// A row in a DEFAULT partition means a partition came too late: it pages, and must be fixed before a partition can be made for that range.
			rows, err := w.books.DefaultPartitionRows(ctx)
			if err != nil {
				return created, err
			}
			for table, n := range rows {
				w.met.Gauge(ports.MetricDefaultPartition, n, ports.Labels{"table": table})
			}
			return created, nil
		}},
		{Name: "idem.purge", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			return w.maint.PurgeIdemPartitions(ctx, w.clock.Now().Add(-w.s.Engine.UsageIdemWindow))
		}},
		{Name: "events.purge", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			inbox, err := w.pay.PurgeInbox(ctx)
			if err != nil {
				return inbox, err
			}
			out, err := w.out.Purge(ctx)
			return inbox + out, err
		}},
		// The days still open to a late row (a usage row is booked within the dedup window of happening) are recomputed from the ledger and
		// replaced; a payload {"days": N} reaches further back, for a backfill. A window nothing changed in writes nothing.
		{Name: "usage.rollup", Key: app.NoPayload, Run: func(ctx context.Context, p []byte) (int, error) {
			now := w.clock.Now()
			back := w.s.Engine.UsageIdemWindow + 24*time.Hour
			if days, ok, err := app.ParseRollupDays(p); err != nil {
				return 0, err
			} else if ok {
				back = time.Duration(days) * 24 * time.Hour
			}
			return w.usage.Rollup(ctx, now.Add(-back), now, time.UTC)
		}},
		// The bus does not count what it trims, so a trim that completes is reported as having run.
		{Name: "trim", Key: app.NoPayload, Run: func(ctx context.Context, _ []byte) (int, error) {
			keep := ports.RetentionPolicy{MinAge: w.s.BusRetention}
			for _, subject := range []string{app.TickSubject(w.s.Engine.SubjectPrefix, "trim"), w.s.Engine.SubjectPrefix + ".trim"} { // ticks, then events
				if err := w.bus.Trim(ctx, subject, keep); err != nil {
					return 0, fmt.Errorf("trim the bus: %w", err)
				}
			}
			for i := range w.s.Engine.Shards {
				if _, err := w.stream.Trim(ctx, domain.Shard(i), keep); err != nil {
					return 0, fmt.Errorf("trim shard %d's outbox: %w", i, err)
				}
			}
			return 1, nil
		}},
	}
}

// onShard runs per-shard work under the shard's lease: this process's own when it owns the shard, otherwise a lease taken for the
// duration. A shard some other worker owns is that worker's to reconcile and audit, and the tick is skipped here: two processes working
// one shard's books at once is what the lease exists to prevent, and a reconcile that finds a regression freezes and rebuilds the shard.
func (w *tickWork) onShard(ctx context.Context, sh domain.Shard, run func() (int, error)) (int, error) {
	if w.r.owns(sh) {
		return run()
	}
	lease, ok, err := w.books.Acquire(ctx, sh)
	if err != nil {
		return 0, fmt.Errorf("take shard %d: %w", sh, err)
	}
	if !ok {
		return 0, fmt.Errorf("shard %d is owned by another worker, whose own loops work it: %w", sh, app.ErrTickSkipped)
	}
	defer func() {
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			w.log.Warn("releasing a shard taken for a tick", "shard", int(sh), "error", err.Error())
		}
	}()
	return run()
}
