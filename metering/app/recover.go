package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// recentWindow and recentLimit bound the eager rehydrate after a recovery: the scopes booked in the
// last hour, up to this many, so the first requests after a recovery do not all read Postgres at once.
const (
	recentWindow  = time.Hour
	recentLimit   = 5000
	recoverPasses = 200
)

// Recover runs the one procedure for every kind of hot-tier loss — a total loss, a rollback, a
// failover to a lagging replica, a reshard (hot-path-consistency.md §5):
//
//	freeze → drain → bump the generation → unfreeze
//
// Freeze makes every hot function refuse (Admit applies AdmitFailPolicy; Record defers to the
// journal), so nothing is lost while it lasts. Drain books everything the hot tier still holds —
// including intents issued with regressed sequence numbers, which are booked on their own
// idempotency rows. Bumping the generation then makes every account in the shard stale at once,
// without a scan, and each rebuilds from booked + open suspense on first use. Rehydrate is only safe
// AFTER the drain, because an undrained intent is in neither `booked` nor the rebuilt balance.
//
// The caller owns the shard. Recover is idempotent: a crashed recovery is resumed by running it again.
func (w *Writer) Recover(ctx context.Context, shard domain.Shard, reason string) error {
	start := w.d.Clock.Now()
	label := ports.Labels{"shard": fmt.Sprint(int(shard))}
	rec, err := w.d.Books.EnsureShard(ctx, shard)
	if err != nil {
		return err
	}

	// 1. Freeze — in the books first (authoritative), then the hot mirror.
	if err := w.d.Books.SetFrozen(ctx, shard, true, reason); err != nil {
		return err
	}
	if err := w.d.Balance.FreezeShard(ctx, shard, reason); err != nil {
		if !errors.Is(err, domain.ErrColdShard) {
			return err
		}
	}
	// A shard whose meta is gone (a fresh or emptied Redis) has nothing to freeze on the hot side:
	// open it at the current generation so the bump below has something to raise.
	if st, serr := w.d.Balance.ShardState(ctx, shard); serr == nil && !st.Open {
		if err := w.d.Balance.OpenShard(ctx, shard, rec.Gen); err != nil {
			return err
		}
		if err := w.d.Balance.FreezeShard(ctx, shard, reason); err != nil {
			return err
		}
	}
	w.met.Gauge(ports.MetricShardFrozen, 0, label)

	// 2. Drain to zero, claiming everything pending (we own the shard, so nothing is in flight).
	if err := w.drainToZero(ctx, shard); err != nil {
		return fmt.Errorf("recover shard %d: drain: %w", shard, err)
	}

	// 3. Bump: Postgres first (authoritative), then the hot mirror; remember the node we now trust.
	gen, err := w.d.Books.BumpGen(ctx, shard)
	if err != nil {
		return err
	}
	if err := w.d.Balance.BumpShard(ctx, shard, gen); err != nil {
		return err
	}
	if id, err := w.d.Balance.NodeID(ctx, shard); err == nil {
		if err := w.d.Books.SetNodeReplID(ctx, shard, id); err != nil {
			return err
		}
	}

	// 4. Unfreeze, and warm the scopes that were just active.
	if err := w.d.Books.SetFrozen(ctx, shard, false, ""); err != nil {
		return err
	}
	if err := w.d.Balance.UnfreezeShard(ctx, shard); err != nil {
		return err
	}
	w.met.Gauge(ports.MetricShardFrozen, int64(w.d.Clock.Now().Sub(start).Seconds()), label)
	w.rehydrateRecent(ctx, shard)
	return nil
}

// drainToZero books everything the shard's stream holds. A delivery stuck behind a sequence gap
// that will never fill (its predecessor is gone with the lost suffix) is parked — never dropped,
// and its hot movement kept as poison suspense — once a frozen shard has shown no other progress.
func (w *Writer) drainToZero(ctx context.Context, shard domain.Shard) error {
	stalled := 0
	for pass := 0; pass < recoverPasses; pass++ {
		n, err := w.drain(ctx, shard, false, 0)
		if err != nil {
			return err
		}
		st, err := w.d.Stream.Stats(ctx, shard)
		if err != nil {
			return err
		}
		if st.Pending == 0 && st.Undelivered == 0 {
			return nil
		}
		if n > 0 {
			stalled = 0
			continue
		}
		if stalled++; stalled >= 3 {
			return w.parkStuck(ctx, shard)
		}
	}
	return fmt.Errorf("the stream did not drain in %d passes", recoverPasses)
}

// parkStuck parks every delivery still pending after a frozen drain made no progress: they sit
// behind a gap that can no longer fill.
func (w *Writer) parkStuck(ctx context.Context, shard domain.Shard) error {
	ds, err := w.d.Stream.Read(ctx, shard, w.cfg.DrainBatch, 0)
	if err != nil {
		return err
	}
	var ack []string
	for _, d := range ds {
		if d.ParseErr != nil {
			if err := w.parkMalformed(ctx, d); err != nil {
				return err
			}
		} else if err := w.parkPoison(ctx, shard, d, errors.New("stuck behind a sequence gap that cannot fill")); err != nil {
			return err
		}
		ack = append(ack, d.ID)
	}
	if err := w.d.Stream.Ack(ctx, shard, ack...); err != nil {
		return err
	}
	w.met.Count("metering_recover_parked_total", int64(len(ack)), ports.Labels{"shard": fmt.Sprint(int(shard))})
	return nil
}

// rehydrateRecent eagerly rebuilds the shard's recently active accounts. A failure is not a failure
// of the recovery: a scope not warmed here is rebuilt lazily on its first use.
func (w *Writer) rehydrateRecent(ctx context.Context, shard domain.Shard) {
	since := w.d.Clock.Now().Add(-recentWindow)
	var after *domain.Scope
	for done := 0; done < recentLimit; {
		page, err := w.d.Books.ScopesSince(ctx, shard, since, after, 200)
		if err != nil || len(page) == 0 {
			return
		}
		for _, s := range page {
			_ = w.reh.Rehydrate(ctx, s.Scope)
			done++
		}
		last := page[len(page)-1].Scope
		after = &last
	}
}
