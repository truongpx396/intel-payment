package app

import (
	"context"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The writer's idempotent sweeps. Each finishes work an effect could not (the hot tier was down, a
// destination shard was frozen) and none can do harm twice: every hot mutation is keyed, and every
// booking is guarded. They are the `billing.transfer.tick` and correction re-issue handlers.

// RedriveTransfers re-issues the credit leg of every transfer in transit for longer than olderThan
// (`TransferRedrive`, 30 s by default). Once phase 1 succeeded both sides apply — this is what makes
// that true across a crash between the two — and it exports the age of the oldest one, which pages
// beyond TransferTransitAlarm.
func (w *Writer) RedriveTransfers(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	now := w.d.Clock.Now()
	stuck, err := w.d.Books.InTransit(ctx, now.Add(-olderThan), limit)
	if err != nil {
		return 0, err
	}
	redriven := 0
	var firstErr error
	for _, t := range stuck {
		if err := w.creditDestination(t.From, t.To, t.FromPool, t.ToPool, t.Amount, t.IdemKey).run(ctx); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("redrive transfer %s: %w", t.IdemKey, err)
			}
			continue
		}
		redriven++
	}
	realms := map[domain.Realm]bool{}
	for _, t := range stuck {
		realms[t.Realm] = true
	}
	for realm := range realms {
		if oldest, err := w.d.Books.OldestInTransit(ctx, realm); err == nil && !oldest.IsZero() {
			w.met.Gauge(ports.MetricTransferAge, int64(now.Sub(oldest).Seconds()), ports.Labels{"realm": string(realm)})
		}
	}
	return redriven, firstErr
}

// ReissueCorrections re-sends the hot-side reversal of open suspense entries whose first attempt did
// not land (the hot tier was frozen, or Redis was down). It only touches entries younger than the
// hot idempotency TTL: past that a repeat could apply twice, and an entry still open then is a
// defect for a human, not something to retry blindly.
func (w *Writer) ReissueCorrections(ctx context.Context, minAge time.Duration, limit int) (int, error) {
	now := w.d.Clock.Now()
	open, err := w.d.Books.OpenSuspenseOlderThan(ctx, now.Add(-minAge), limit)
	if err != nil {
		return 0, err
	}
	issued := 0
	var firstErr error
	for _, s := range open {
		if now.Sub(s.CreatedAt) >= w.cfg.HotIdemTTL {
			w.met.Count("metering_correction_stale_total", 1, ports.Labels{"realm": string(s.Scope.Realm)})
			continue
		}
		if s.Delta == 0 {
			continue
		}
		if err := w.issueCorrection(ctx, s.Scope, s.Pool, -s.Delta, s.ID, s.Reason); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("reissue correction %s: %w", s.ID, err)
			}
			continue
		}
		issued++
	}
	return issued, firstErr
}

// ExportGauges publishes the deployment-wide gauges: open suspense by realm and reason.
func (w *Writer) ExportGauges(ctx context.Context) error {
	counts, err := w.d.Books.SuspenseCounts(ctx)
	if err != nil {
		return err
	}
	for _, c := range counts {
		w.met.Gauge(ports.MetricSuspenseOpen, c.Count, ports.Labels{"realm": string(c.Realm), "reason": c.Reason})
	}
	return nil
}
