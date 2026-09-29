// Package table is the data-driven default Pricer, behind every deployment's PricerRegistry.
//
// It prices Σ quantity × CreditsPerBlock / BlockSize over an event's quantities, EXACTLY, and rounds
// UP once. That covers every per-unit linear price — tokens, calls, GB-days, seconds, requests —
// without code: a non-Go host prices anything linear by publishing a rate card.
package table

import (
	"context"
	"fmt"
	"math/big"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Pricer is pure: it reads only the card, the rate key and the quantities. It holds no state, so
// one value is safe to share across goroutines.
type Pricer struct {
	// MaxAmount is MaxOperationAmount; 0 means the default, 2^50.
	MaxAmount int64
}

// New returns a Pricer bounded by limit (0 = the default 2^50).
func New(limit domain.Credits) Pricer { return Pricer{MaxAmount: int64(limit)} }

// Price implements ports.Pricer.
func (p Pricer) Price(_ context.Context, card domain.RateCard, e domain.Event) (domain.Price, error) {
	rates := card.Rates(e.RateKey)
	if len(rates) == 0 { // an unknown rate key fails closed even for an empty event
		return domain.Price{}, fmt.Errorf("%w: no rate key %q in card %q", domain.ErrUnpriceable, e.RateKey, card.Version)
	}
	credits, cost := new(big.Rat), new(big.Rat)
	qty, num, blk := new(big.Int), new(big.Int), new(big.Int)
	term := new(big.Rat)
	for _, q := range e.Quantities {
		if q.Amount < 0 {
			return domain.Price{}, fmt.Errorf("%w: negative quantity for %q", domain.ErrUnpriceable, q.Unit)
		}
		r, ok := rates[q.Unit]
		if !ok {
			return domain.Price{}, fmt.Errorf("%w: no rate for %q/%q in card %q", domain.ErrUnpriceable, e.RateKey, q.Unit, card.Version)
		}
		qty.SetInt64(q.Amount)
		blk.SetInt64(r.BlockSize)
		credits.Add(credits, term.SetFrac(num.Mul(qty, big.NewInt(r.CreditsPerBlock)), blk))
		cost.Add(cost, term.SetFrac(num.Mul(qty, big.NewInt(r.CostMicrosPerBlock)), blk))
	}
	c, m := ceil(credits), ceil(cost)
	limit := p.MaxAmount
	if limit == 0 {
		limit = int64(domain.MaxOperationAmount)
	}
	if !c.IsInt64() || c.Int64() > limit || !m.IsInt64() {
		return domain.Price{}, fmt.Errorf("%w: %s credits", domain.ErrAmountOutOfRange, c)
	}
	return domain.Price{
		Credits:         domain.Credits(c.Int64()),
		CostMicros:      m.Int64(),
		RateCardVersion: card.Version, // travels onto the ledger row — invariant 5
	}, nil
}

// ceil rounds a non-negative rational UP: rounding never under-bills, and with a fine accounting
// unit it never over-bills by more than one unit per event.
func ceil(r *big.Rat) *big.Int {
	n := new(big.Int).Add(r.Num(), new(big.Int).Sub(r.Denom(), big.NewInt(1)))
	return n.Quo(n, r.Denom())
}
