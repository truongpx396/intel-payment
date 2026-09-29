package app

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Expirer pre-applies credit expiry on the hot tier (`billing.expiry.tick`, FR-016). Under
// CreditExpiry=lots_fifo each grant is a lot; a lot past its expiry still holding a remainder is
// removed from the hot balance at the remainder the books hold AT THE WATERMARK — an upper bound,
// because usage still in flight may have consumed some of the lot since. The writer books the
// EXACT remainder when it reaches the intent and trues up the difference through suspense.
//
// The hot function caps the deduction at the pool's available (non-negative) balance, so an expiry
// can never drive a pool below zero, and it is idempotent on the lot's id.
type Expirer struct {
	cfg metering.Config
	d   Deps
	met ports.Metrics
	reh *rehydrator
}

// NewExpirer builds the expiry tick handler.
func NewExpirer(cfg metering.Config, d Deps) (*Expirer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.WithDefaults()
	if d.Balance == nil || d.Books == nil || d.Clock == nil {
		return nil, fmt.Errorf("metering expirer: Balance, Books and Clock are required")
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &Expirer{cfg: cfg, d: d, met: met, reh: newRehydrator(cfg, d, met)}, nil
}

// Tick expires up to limit due lots and returns how many it issued. With CreditExpiry=off it does
// nothing: a system that silently destroys something a customer paid for must have been asked to.
func (e *Expirer) Tick(ctx context.Context, limit int) (int, error) {
	if e.cfg.CreditExpiry != metering.ExpiryLotsFIFO {
		return 0, nil
	}
	lots, err := e.d.Books.DueLots(ctx, e.d.Clock.Now(), limit)
	if err != nil {
		return 0, err
	}
	issued := 0
	var firstErr error
	for _, l := range lots {
		d := ports.HotDelta{
			Op: domain.OpExpiry, Scope: l.Scope, Pool: l.Pool, Delta: -l.Remaining, IdemKey: l.ID, CapAtAvailable: true,
			Payload: domain.Payload{Reason: "expiry", LotID: l.ID},
		}
		err := e.reh.heal(ctx, l.Scope, func() error { _, err := e.d.Balance.ApplyDelta(ctx, d); return err })
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("expire lot %s: %w", l.ID, err)
			}
			continue
		}
		issued++
	}
	return issued, firstErr
}
