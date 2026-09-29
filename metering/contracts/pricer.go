package contracts

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// PricerFixture describes a card for the suite: one rate key, one unit priced BELOW one credit per
// unit (Num/Den credits per unit, Num < Den), and a second unit priced at least 1/1000 credit per
// unit, so a huge quantity of it exceeds MaxOperationAmount.
type PricerFixture struct {
	RateKey      string
	UnitA, UnitB domain.Unit
	Num, Den     int64
}

// PricerContract runs against ANY Pricer and a card for it — pure, no infrastructure. It is the
// specification of FR-001 and invariant 5: the same suite passes for the table pricer over three
// disjoint unit spaces and for a compiled pricer resolved through the registry (SC-004).
func PricerContract(t *testing.T, p ports.Pricer, card domain.RateCard, fx PricerFixture) {
	t.Helper()
	ctx := context.Background()
	base := domain.Event{RateKey: fx.RateKey, IdemKey: "e1",
		Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: 1000}, {Unit: fx.UnitB, Amount: 500}}}

	t.Run("deterministic", func(t *testing.T) {
		t.Parallel()
		a, err := p.Price(ctx, card, base)
		mustNoErr(t, err)
		b, _ := p.Price(ctx, card, base)
		if a != b {
			t.Fatalf("non-deterministic: %+v != %+v", a, b)
		}
	})
	t.Run("attributes and subjects never affect price", func(t *testing.T) {
		t.Parallel()
		w := base
		w.Attributes = map[string]string{"trace_id": "t9"}
		w.Subjects = domain.Subjects{"user": "u9"}
		a, _ := p.Price(ctx, card, base)
		b, _ := p.Price(ctx, card, w)
		if a != b {
			t.Fatalf("attributes/subjects changed price: %+v != %+v", a, b)
		}
	})
	t.Run("empty event under a known rate key is free", func(t *testing.T) {
		t.Parallel()
		z := base
		z.Quantities = nil
		got, err := p.Price(ctx, card, z)
		mustNoErr(t, err)
		if got.Credits != 0 {
			t.Fatalf("empty event not free: %+v", got)
		}
	})
	t.Run("fails closed on an unknown rate key or unit", func(t *testing.T) {
		t.Parallel()
		u := base
		u.RateKey = "no-such-key"
		if _, err := p.Price(ctx, card, u); !errors.Is(err, domain.ErrUnpriceable) {
			t.Fatalf("unknown key must fail closed, got %v", err)
		}
		v := base
		v.Quantities = []domain.Quantity{{Unit: "no-such-unit", Amount: 1}}
		if _, err := p.Price(ctx, card, v); !errors.Is(err, domain.ErrUnpriceable) {
			t.Fatalf("unknown unit must fail closed, got %v", err)
		}
	})
	t.Run("a negative quantity fails closed, never a credit", func(t *testing.T) {
		t.Parallel()
		n := base
		n.Quantities = []domain.Quantity{{Unit: fx.UnitA, Amount: -5}}
		if _, err := p.Price(ctx, card, n); !errors.Is(err, domain.ErrUnpriceable) {
			t.Fatalf("a negative quantity must not price to a refund, got %v", err)
		}
	})
	t.Run("a sub-credit unit price is exact, not rounded per unit", func(t *testing.T) {
		t.Parallel()
		// fx.UnitA costs fx.Num/fx.Den credits per unit with fx.Num < fx.Den (e.g. 0.15).
		n := fx.Den * 7 // a quantity at which the exact price is a whole number
		got, err := p.Price(ctx, card, domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: n}}})
		mustNoErr(t, err)
		if int64(got.Credits) != fx.Num*7 {
			t.Fatalf("inexact: %d credits for %d units, want %d", got.Credits, n, fx.Num*7)
		}
	})
	t.Run("rounds up once per event, never per unit or per term", func(t *testing.T) {
		t.Parallel()
		one := domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: 1}, {Unit: fx.UnitA, Amount: 1}}}
		got, _ := p.Price(ctx, card, one)
		if want := ceilDiv(2*fx.Num, fx.Den); int64(got.Credits) != want {
			t.Fatalf("rounded per term: %d want %d", got.Credits, want)
		}
	})
	t.Run("never rounds down: a positive usage never prices to zero", func(t *testing.T) {
		t.Parallel()
		tiny := domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: 1}}}
		got, err := p.Price(ctx, card, tiny)
		mustNoErr(t, err)
		if fx.Num > 0 && got.Credits < 1 {
			t.Fatalf("one unit of a priced resource rounded down to %d — that is an under-bill", got.Credits)
		}
	})
	t.Run("monotonic in quantity", func(t *testing.T) {
		t.Parallel()
		more := base
		more.Quantities = []domain.Quantity{{Unit: fx.UnitA, Amount: 2000}, {Unit: fx.UnitB, Amount: 500}}
		a, _ := p.Price(ctx, card, base)
		b, _ := p.Price(ctx, card, more)
		if b.Credits < a.Credits {
			t.Fatalf("more usage cost less: %d < %d", b.Credits, a.Credits)
		}
	})
	t.Run("reports the card version", func(t *testing.T) {
		t.Parallel()
		got, _ := p.Price(ctx, card, base)
		if got.RateCardVersion != card.Version {
			t.Fatal("the card version must travel onto the ledger row")
		}
	})
	t.Run("refuses an amount above MaxOperationAmount", func(t *testing.T) {
		t.Parallel()
		huge := domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitB, Amount: math.MaxInt64 / 2}}}
		if _, err := p.Price(ctx, card, huge); !errors.Is(err, domain.ErrAmountOutOfRange) {
			t.Fatalf("overflow must fail closed, got %v", err)
		}
	})
	t.Run("is safe for concurrent use and gives one answer", func(t *testing.T) {
		t.Parallel()
		want, err := p.Price(ctx, card, base)
		mustNoErr(t, err)
		var wg sync.WaitGroup
		errs := make(chan string, 32)
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got, err := p.Price(ctx, card, base); err != nil || got != want {
					errs <- "concurrent Price disagreed"
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
	})
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
