package app

import (
	"context"
	"errors"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// JournalReplayer applies spend-journal entries through the hot function once the shard is live
// (FR-017e, D41). It is the `billing.journal.tick` handler: every replay is idempotent on the same
// key, so a duplicated tick, or an entry the hot tier already has, is a REPLAY and changes nothing.
// The LedgerWriter marks a row settled when it books the resulting intent — not this.
type JournalReplayer struct {
	cfg metering.Config
	d   Deps
	met ports.Metrics
	reh *rehydrator
}

// NewJournalReplayer builds the replayer over the same dependencies as the Meter.
func NewJournalReplayer(cfg metering.Config, d Deps) (*JournalReplayer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.WithDefaults()
	if err := d.validate(cfg); err != nil {
		return nil, err
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &JournalReplayer{cfg: cfg, d: d, met: met, reh: newRehydrator(cfg, d, met)}, nil
}

// ReplayResult counts what one tick did.
type ReplayResult struct {
	Claimed  int // entries taken
	Applied  int // applied to the hot tier now
	Replayed int // the hot tier already had them (a REPLAY)
	Retried  int // the hot tier could not take them yet; rescheduled with backoff
	Failed   int // refused for a reason that will not fix itself (an idempotency conflict, a bad amount)
}

// Replay processes up to limit due entries.
func (r *JournalReplayer) Replay(ctx context.Context, limit int) (ReplayResult, error) {
	var res ReplayResult
	now := r.d.Clock.Now()
	due, err := r.d.Journal.Due(ctx, now, limit)
	if err != nil {
		return res, err
	}
	res.Claimed = len(due)
	realms := map[domain.Realm]bool{}
	for _, e := range due {
		realms[e.Charge.Scope.Realm.Or()] = true
		var rc domain.Receipt
		err := r.reh.heal(ctx, e.Charge.Scope, func() (e2 error) { rc, e2 = r.d.Balance.Debit(ctx, e.Charge); return })
		switch {
		case err == nil && rc.Applied:
			res.Applied++
		case err == nil:
			res.Replayed++
		case domain.IsHotUnavailable(err):
			res.Retried++
			r.reschedule(ctx, e, now, err)
		default:
			// The entry stays in the journal — never dropped — but a conflict or a bad amount will
			// not fix itself, so it backs off and is counted for a human.
			res.Failed++
			if errors.Is(err, domain.ErrIdemConflict) {
				r.met.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": string(e.Charge.Scope.Realm), "op": "usage"})
			}
			r.reschedule(ctx, e, now, err)
		}
	}
	for realm := range realms {
		if n, err := r.d.Journal.Pending(ctx, realm); err == nil {
			r.met.Gauge(ports.MetricJournalPending, n, ports.Labels{"realm": string(realm)})
		}
	}
	return res, nil
}

// reschedule backs an entry off exponentially (1s, 2s, 4s … capped at 5 min).
func (r *JournalReplayer) reschedule(ctx context.Context, e ports.JournalEntry, now time.Time, cause error) {
	attempts := e.Attempts + 1
	delay := time.Second << min(attempts, 9)
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	_ = r.d.Journal.Retry(ctx, e.ID, attempts, now.Add(delay), cause.Error())
}
