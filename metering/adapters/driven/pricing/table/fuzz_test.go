package table_test

import (
	"context"
	"errors"
	"math"
	"math/bits"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// ceilMulDiv is ceil(q·c / b) in 128-bit integer arithmetic. It is the reference the pricer is held
// to, and it shares no code with it: the pricer computes with math/big rationals, this with
// math/bits — so a mistake in one is not a mistake in the other.
func ceilMulDiv(q, c, b uint64) (uint64, bool) {
	hi, lo := bits.Mul64(q, c)
	if hi >= b { // the quotient would not fit in 64 bits
		return 0, false
	}
	quo, rem := bits.Div64(hi, lo, b)
	if rem != 0 {
		if quo == math.MaxUint64 {
			return 0, false
		}
		quo++
	}
	return quo, true
}

func singleEntryCard(tb testing.TB, credits, block, cost int64) domain.RateCard {
	tb.Helper()
	card, err := domain.NewRateCard("t1", "v1", "table", []domain.RateEntry{
		{RateKey: "k", Unit: "u", CreditsPerBlock: credits, BlockSize: block, CostMicrosPerBlock: cost},
	}, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return card
}

// One quantity, one rate: the price is EXACTLY ceil(quantity × credits / block), or the pricer
// refuses because the result is out of range. No input may produce anything in between — not a
// rounded-down price, not a wrapped one.
func FuzzPriceIsTheExactCeilingOfTheRational(f *testing.F) {
	f.Add(int64(100), int64(150_000), int64(1_000_000), int64(0))             // $0.15 / 1M tokens: a sub-credit price
	f.Add(int64(1), int64(1), int64(1_000_000), int64(0))                     // the smallest charge rounds UP to one credit
	f.Add(int64(1_000_000), int64(1), int64(1_000_000), int64(0))             // exactly one credit, no rounding
	f.Add(int64(1_000_001), int64(1), int64(1_000_000), int64(0))             // one past: two credits
	f.Add(int64(0), int64(5), int64(3), int64(0))                             // nothing used costs nothing
	f.Add(int64(math.MaxInt64), int64(math.MaxInt64), int64(1), int64(0))     // overflows the 128-bit intermediate's quotient
	f.Add(int64(1<<50), int64(1), int64(1), int64(0))                         // exactly MaxOperationAmount
	f.Add(int64(1<<50+1), int64(1), int64(1), int64(0))                       // one past it
	f.Add(int64(10), int64(7), int64(3), int64(math.MaxInt64))                // upstream cost beyond int64
	f.Add(int64(2_400), int64(2_500_000), int64(1_000_000), int64(2_000_000)) // the contract's worked example
	f.Fuzz(func(t *testing.T, qty, credits, block, cost int64) {
		if qty < 0 || credits < 0 || block <= 0 || cost < 0 {
			t.Skip("the card and event validators refuse these before the pricer sees them")
		}
		card := singleEntryCard(t, credits, block, cost)
		got, err := table.New(0).Price(context.Background(), card,
			domain.Event{RateKey: "k", Quantities: []domain.Quantity{{Unit: "u", Amount: qty}}})

		wantCredits, okC := ceilMulDiv(uint64(qty), uint64(credits), uint64(block))
		wantCost, okM := ceilMulDiv(uint64(qty), uint64(cost), uint64(block))
		inRange := okC && okM && wantCredits <= uint64(domain.MaxOperationAmount) && wantCost <= math.MaxInt64
		if !inRange {
			if !errors.Is(err, domain.ErrAmountOutOfRange) {
				t.Fatalf("%d × %d / %d is out of range but the pricer answered %+v, %v", qty, credits, block, got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("%d × %d / %d is %d credits, in range, but the pricer refused: %v", qty, credits, block, wantCredits, err)
		}
		if uint64(got.Credits) != wantCredits || uint64(got.CostMicros) != wantCost {
			t.Fatalf("%d × %d / %d: got %d credits / %d µ$, want %d / %d", qty, credits, block, got.Credits, got.CostMicros, wantCredits, wantCost)
		}
		if got.RateCardVersion != "v1" {
			t.Fatalf("the price must carry the card version it was made under, got %q", got.RateCardVersion)
		}
	})
}

// Rounding happens ONCE per event, on the total. So an event with two quantities costs no more than
// its two halves priced separately, and at most one credit less; and using more never costs less.
func FuzzPriceRoundsOnceAndNeverDecreases(f *testing.F) {
	f.Add(int64(1), int64(1), int64(1), int64(3), int64(1), int64(3)) // 1/3 + 1/3: one credit together, two apart
	f.Add(int64(1_200), int64(800), int64(2_500_000), int64(1_000_000), int64(10_000_000), int64(1_000_000))
	f.Add(int64(0), int64(0), int64(5), int64(7), int64(5), int64(7))
	f.Add(int64(7), int64(9), int64(2), int64(3), int64(2), int64(3))
	f.Fuzz(func(t *testing.T, q1, q2, c1, b1, c2, b2 int64) {
		const qCap, cCap = int64(1) << 30, int64(1) << 20
		if q1 < 0 || q2 < 0 || q1 > qCap || q2 > qCap || c1 < 0 || c2 < 0 || c1 > cCap || c2 > cCap || b1 < 1 || b2 < 1 || b1 > cCap || b2 > cCap {
			t.Skip()
		}
		card, err := domain.NewRateCard("t1", "v1", "table", []domain.RateEntry{
			{RateKey: "k", Unit: "a", CreditsPerBlock: c1, BlockSize: b1},
			{RateKey: "k", Unit: "b", CreditsPerBlock: c2, BlockSize: b2},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := table.New(0)
		price := func(qs ...domain.Quantity) (int64, bool) {
			got, err := p.Price(context.Background(), card, domain.Event{RateKey: "k", Quantities: qs})
			if errors.Is(err, domain.ErrAmountOutOfRange) {
				return 0, false
			}
			if err != nil {
				t.Fatalf("pricing %+v: %v", qs, err)
			}
			return int64(got.Credits), true
		}
		qa, qb := domain.Quantity{Unit: "a", Amount: q1}, domain.Quantity{Unit: "b", Amount: q2}
		both, ok1 := price(qa, qb)
		onlyA, ok2 := price(qa)
		onlyB, ok3 := price(qb)
		if !ok1 || !ok2 || !ok3 {
			t.Skip("out of range is covered by the exactness target")
		}
		if both > onlyA+onlyB {
			t.Fatalf("one rounding cost MORE than two: %d > %d + %d — the event was rounded per quantity", both, onlyA, onlyB)
		}
		if both < onlyA+onlyB-1 {
			t.Fatalf("the total %d is more than one credit below its parts %d + %d: rounding dropped a whole credit", both, onlyA, onlyB)
		}
		if again, _ := price(qa, qb); again != both {
			t.Fatalf("pricing is not deterministic: %d then %d", both, again)
		}
		if more, ok := price(domain.Quantity{Unit: "a", Amount: q1 + q2}); ok && more < onlyA {
			t.Fatalf("using more (%d) cost less (%d) than using %d (%d)", q1+q2, more, q1, onlyA)
		}
		if zero, _ := price(domain.Quantity{Unit: "a", Amount: 0}); zero != 0 {
			t.Fatalf("a zero quantity must cost zero, got %d", zero)
		}
	})
}
