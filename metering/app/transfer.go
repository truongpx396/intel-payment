package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Transfer is phase 1 of a transfer (FR-017b, D33): an atomic check-and-debit of the source's HOT
// pool, refusing rather than overdrawing. The writer completes the destination's side — books
// allocation_out, credits the destination, books allocation_in — so once this returns the outcome
// is both sides, never one. PRIVILEGED.
func (m *meter) Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	if err := t.Validate(); err != nil {
		return domain.TransferReceipt{}, err
	}
	t.From, t.To = t.From.Normalized(), t.To.Normalized()
	var rc domain.TransferReceipt
	err := m.reh.heal(ctx, t.From, func() (e error) { rc, e = m.d.Balance.Transfer(ctx, t); return })
	switch {
	case errors.Is(err, domain.ErrIdemConflict):
		m.met.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": string(t.From.Realm), "op": "transfer"})
		return domain.TransferReceipt{}, err
	case err != nil:
		return domain.TransferReceipt{}, err
	}
	if rc.Status == "" {
		// An idempotent replay: Redis knows the request was accepted, not whether the writer has
		// since settled it. The books do — or, if the writer has not reached it, it is in transit.
		rc.Status = domain.TransferInTransit
		rec, found, err := m.d.Books.Transfer(ctx, t.From.Realm, t.IdemKey)
		if err != nil {
			return domain.TransferReceipt{}, fmt.Errorf("transfer status: %w", err)
		}
		if found {
			rc.Status = rec.Status
		}
	}
	return rc, nil
}
