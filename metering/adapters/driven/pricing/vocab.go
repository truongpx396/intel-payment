// Package pricing holds what the reference pricers share: they validate a unit vocabulary, then
// delegate the arithmetic to the table pricer.
package pricing

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Vocabulary wraps a pricer so an event carrying a unit outside the set fails CLOSED with
// ErrUnpriceable before any arithmetic. It is how a reference pricer says "these are my units"
// without re-implementing the math: a card that prices a unit the vocabulary does not know is a
// misconfiguration, and an event that carries one is a producer bug — neither should charge.
type Vocabulary struct {
	Name  string
	Units map[domain.Unit]bool
	Next  ports.Pricer
}

// NewVocabulary builds a Vocabulary over next.
func NewVocabulary(name string, next ports.Pricer, units ...domain.Unit) Vocabulary {
	set := make(map[domain.Unit]bool, len(units))
	for _, u := range units {
		set[u] = true
	}
	return Vocabulary{Name: name, Units: set, Next: next}
}

// Price implements ports.Pricer.
func (v Vocabulary) Price(ctx context.Context, card domain.RateCard, e domain.Event) (domain.Price, error) {
	for _, q := range e.Quantities {
		if !v.Units[q.Unit] {
			return domain.Price{}, fmt.Errorf("%w: %s pricer does not meter unit %q", domain.ErrUnpriceable, v.Name, q.Unit)
		}
	}
	return v.Next.Price(ctx, card, e)
}
