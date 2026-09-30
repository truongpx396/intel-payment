package domain_test

import (
	"errors"
	"math"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Money must never wrap. Every comparison in the overflow guard is pinned from both sides: the
// largest sum that still fits is accepted, and the smallest one that does not is refused.
const (
	maxC = domain.Credits(math.MaxInt64)
	minC = domain.Credits(math.MinInt64)
)

func TestCreditsAddRefusesOverflowAtTheExactBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		a, b    domain.Credits
		want    domain.Credits
		allowed bool
	}{
		{"zero", 0, 0, 0, true},
		{"largest positive sum that fits", maxC - 1, 1, maxC, true},
		{"one past the top", maxC, 1, 0, false},
		{"top plus nothing", maxC, 0, maxC, true},
		{"the same, operands swapped", 1, maxC - 1, maxC, true},
		{"one past the top, swapped", 1, maxC, 0, false},
		{"largest negative sum that fits", minC + 1, -1, minC, true},
		{"one past the bottom", minC, -1, 0, false},
		{"bottom plus nothing", minC, 0, minC, true},
		{"the same, operands swapped", -1, minC + 1, minC, true},
		{"one past the bottom, swapped", -1, minC, 0, false},
		{"extremes cancel", maxC, minC, -1, true},
		{"extremes cancel, swapped", minC, maxC, -1, true},
	}
	for _, c := range cases {
		got, err := c.a.Add(c.b)
		switch {
		case c.allowed && (err != nil || got != c.want):
			t.Errorf("%s: %d + %d = %d, %v; want %d", c.name, c.a, c.b, got, err, c.want)
		case !c.allowed && !errors.Is(err, domain.ErrAmountOutOfRange):
			t.Errorf("%s: %d + %d = %d, %v; want ErrAmountOutOfRange — money wrapped", c.name, c.a, c.b, got, err)
		case !c.allowed && got != 0:
			t.Errorf("%s: a refused sum must return zero, got %d", c.name, got)
		}
	}
}

func TestSumCreditsTotalsPoolsAndRefusesOverflow(t *testing.T) {
	t.Parallel()
	if got, err := domain.SumCredits(); err != nil || got != 0 {
		t.Fatalf("an empty sum is zero: %d %v", got, err)
	}
	if got, err := domain.SumCredits(5, -2, 10); err != nil || got != 13 {
		t.Fatalf("got %d %v, want 13", got, err)
	}
	if _, err := domain.SumCredits(maxC, 1); !errors.Is(err, domain.ErrAmountOutOfRange) {
		t.Fatalf("a pool total that overflows must be refused, got %v", err)
	}
}

func TestMoneyAddRefusesOverflowAtTheExactBoundary(t *testing.T) {
	t.Parallel()
	usd := func(n int64) domain.Money { return domain.Money{MinorUnits: n, Currency: "USD"} }
	cases := []struct {
		name    string
		a, b    int64
		want    int64
		allowed bool
	}{
		{"zero", 0, 0, 0, true},
		{"largest positive sum that fits", math.MaxInt64 - 1, 1, math.MaxInt64, true},
		{"one past the top", math.MaxInt64, 1, 0, false},
		{"top plus nothing", math.MaxInt64, 0, math.MaxInt64, true},
		{"largest negative sum that fits", math.MinInt64 + 1, -1, math.MinInt64, true},
		{"one past the bottom", math.MinInt64, -1, 0, false},
		{"bottom plus nothing", math.MinInt64, 0, math.MinInt64, true},
		{"refund of a payment", 1999, -1999, 0, true},
	}
	for _, c := range cases {
		got, err := usd(c.a).Add(usd(c.b))
		switch {
		case c.allowed && (err != nil || got != usd(c.want)):
			t.Errorf("%s: %d + %d = %+v, %v; want %d", c.name, c.a, c.b, got, err, c.want)
		case !c.allowed && !errors.Is(err, domain.ErrAmountOutOfRange):
			t.Errorf("%s: %d + %d = %+v, %v; want ErrAmountOutOfRange — money wrapped", c.name, c.a, c.b, got, err)
		}
	}
}

func TestMoneyRefusesMixedCurrenciesAndMalformedCodes(t *testing.T) {
	t.Parallel()
	if _, err := (domain.Money{MinorUnits: 1, Currency: "USD"}).Add(domain.Money{MinorUnits: 1, Currency: "EUR"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("there is no FX: adding EUR to USD must be refused, got %v", err)
	}
	for _, code := range []string{"", "usd", "US", "USDD", "U$D", "US1"} {
		if err := (domain.Money{Currency: code}).Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("currency %q is not an ISO-4217 shape and must be refused, got %v", code, err)
		}
	}
	if err := (domain.Money{Currency: "VND"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
