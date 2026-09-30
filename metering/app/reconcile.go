package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

const reconcilePage = 500

// Reconcile compares each recently booked scope's hot account with booked + open suspense AT THE
// SAME SEQUENCE NUMBER (FR-009, D39). Intents still in flight make the hot tier AHEAD of the books —
// that is lag, reported as `deferred`, never drift. A hot sequence BEHIND the watermark is a
// regression: the shard freezes and is rebuilt. Only a disagreement at equal sequence numbers is
// drift, a defect by construction; it is healed on the HOT side by compare-and-set and pages beyond
// ReconcileTolerance. Reconcile NEVER books a ledger row — the books change only through intents and
// audited adjustments.
func (w *Writer) Reconcile(ctx context.Context, shard domain.Shard, kind domain.ReconcileKind) (domain.ReconcileRun, error) {
	started := w.d.Clock.Now()
	run := domain.ReconcileRun{Shard: shard, Kind: kind}
	var since time.Time // the zero time: every scope
	switch kind {
	case domain.ReconcileFull:
	case domain.ReconcileIncremental:
		last, err := w.d.Books.LastReconcile(ctx, shard, kind)
		if err != nil {
			return run, err
		}
		since = last // never reconciled: zero, i.e. everything once
	default:
		return run, fmt.Errorf("%w: reconcile kind %q", domain.ErrInvalid, kind)
	}

	var after *domain.Scope
	regressed := false
	for {
		page, err := w.d.Books.ScopesSince(ctx, shard, since, after, reconcilePage)
		if err != nil {
			return run, err
		}
		if len(page) == 0 {
			break
		}
		for _, ss := range page {
			fs, status, err := w.checkScope(ctx, ss.Scope)
			if err != nil {
				return run, err
			}
			run.Checked++
			switch status {
			case "in_sync":
				run.InSync++
			case "cold":
				run.Cold++
			case "deferred":
				run.Deferred++
			case "regression":
				run.Regressions++
				regressed = true
			case "drift":
				run.Drifted++
			}
			for _, f := range fs {
				run.Findings = append(run.Findings, f)
				if f.Outcome == domain.OutcomeDrift {
					d := f.Drift
					if d < 0 {
						d = -d
					}
					run.AbsDrift += d // Σ |drift|: a +100 and a -100 never cancel
				}
			}
		}
		last := page[len(page)-1].Scope
		after = &last
		if len(page) < reconcilePage {
			break
		}
	}

	label := ports.Labels{"shard": fmt.Sprint(int(shard))}
	w.met.Gauge(ports.MetricDriftAbsCredits, int64(run.AbsDrift), label)
	w.met.Count(ports.MetricReconcileDeferred, int64(run.Deferred), label)
	for _, f := range run.Findings {
		w.met.Count(ports.MetricDriftFindings, 1, ports.Labels{"realm": string(f.Scope.Realm.Or()), "outcome": f.Outcome})
	}
	if err := w.d.Books.SaveReconcile(ctx, ports.ReconcileRecord{Run: run, StartedAt: started}); err != nil {
		return run, err
	}
	if regressed {
		// The hot tier lost history. Freeze, drain, bump, rebuild.
		if err := w.Recover(ctx, shard, "regression found by reconcile"); err != nil {
			return run, err
		}
	}
	return run, nil
}

// checkScope reads the hot account and the books for one scope and classifies them. The two reads
// happen at different instants, so a sequence skew is re-read a couple of times before it is
// believed: a charge landing between the reads makes the hot tier look ahead (lag) or the books look
// ahead of a stale hot read, neither of which is a defect.
func (w *Writer) checkScope(ctx context.Context, scope domain.Scope) ([]domain.ReconcileFinding, string, error) {
	const attempts = 3
	var acct ports.HotAccount
	var st ports.BookedState
	for attempt := 1; ; attempt++ {
		var err error
		acct, err = w.d.Balance.Snapshot(ctx, scope)
		switch {
		case errors.Is(err, domain.ErrShardFrozen), errors.Is(err, domain.ErrColdShard):
			return nil, "cold", nil // being rebuilt: nothing to compare yet
		case err != nil:
			return nil, "", err
		}
		if acct.Cold {
			return nil, "cold", nil // it will rebuild from the books on first use
		}
		if st, err = w.d.Books.Booked(ctx, scope); err != nil {
			return nil, "", err
		}
		applied := st.Watermark.AppliedSeq
		switch {
		case acct.Seq == applied:
			return w.comparePools(ctx, scope, acct, st)
		case attempt < attempts:
			continue
		case acct.Seq > applied:
			return nil, "deferred", nil // intents in flight: the outbox-age alert's job, not drift
		default:
			return []domain.ReconcileFinding{{
				Scope: scope, Pool: domain.GeneralPool, Outcome: domain.OutcomeRegression,
				HotSeq: acct.Seq, AppliedSeq: applied, Alarmed: true,
			}}, "regression", nil
		}
	}
}

// comparePools compares hot with booked + open suspense per pool at equal sequence numbers.
func (w *Writer) comparePools(ctx context.Context, scope domain.Scope, acct ports.HotAccount, st ports.BookedState) ([]domain.ReconcileFinding, string, error) {
	pools := map[domain.Pool]bool{}
	for p := range acct.Pools {
		pools[p] = true
	}
	for p := range st.Booked {
		pools[p] = true
	}
	for p := range st.Suspense {
		pools[p] = true
	}
	names := make([]domain.Pool, 0, len(pools))
	for p := range pools {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	var findings []domain.ReconcileFinding
	targets := map[domain.Pool]domain.Credits{}
	for _, p := range names {
		expected := st.Booked[p] + st.Suspense[p]
		hot := acct.Pools[p]
		if drift := hot - expected; drift != 0 {
			findings = append(findings, domain.ReconcileFinding{
				Scope: scope, Pool: p, Outcome: domain.OutcomeDrift, HotSeq: acct.Seq, AppliedSeq: st.Watermark.AppliedSeq,
				Hot: hot, Expected: expected, Drift: drift,
			})
			targets[p] = expected
		}
	}
	if len(findings) == 0 {
		return nil, "in_sync", nil
	}
	// Heal the HOT side, as a compare-and-set on seq: a charge that landed since this was measured is
	// never overwritten (the next reconcile sees the new state). The books are never touched.
	healed, err := w.d.Balance.Heal(ctx, scope, acct.Seq, targets)
	if err != nil && !errors.Is(err, domain.ErrShardFrozen) {
		return nil, "", err
	}
	for i := range findings {
		abs := findings[i].Drift
		if abs < 0 {
			abs = -abs
		}
		findings[i].Healed = healed
		findings[i].Alarmed = abs > w.cfg.ReconcileTolerance
	}
	return findings, "drift", nil
}
