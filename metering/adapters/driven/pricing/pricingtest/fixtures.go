// Package pricingtest holds the three rate cards, in disjoint unit spaces, that the pricer
// conformance suite runs over (SC-004): tokens, seat-days and byte-days. It is test support, and
// lives beside the pricers so every pricer's test can share it.
package pricingtest

import (
	"testing"

	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// Case is a card plus the fixture that describes it.
type Case struct {
	Name string
	Card domain.RateCard
	Fx   contracts.PricerFixture
}

func card(t *testing.T, version, pricer string, entries ...domain.RateEntry) domain.RateCard {
	t.Helper()
	c, err := domain.NewRateCard("t1", version, pricer, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Tokens: gpt-4o-mini-like rates at 1 credit = 1 µ$. Input is $0.15 per million tokens
// ({150000, 1000000}) — a sub-credit price an integer "per unit" column cannot express — and output
// is $0.60 per million.
func Tokens(t *testing.T, pricer string) Case {
	t.Helper()
	return Case{
		Name: "tokens",
		Card: card(t, "tokens-v1", pricer,
			domain.RateEntry{RateKey: "gpt-mini", Unit: "llm_input_token", CreditsPerBlock: 150000, BlockSize: 1000000, CostMicrosPerBlock: 120000},
			domain.RateEntry{RateKey: "gpt-mini", Unit: "llm_output_token", CreditsPerBlock: 600000, BlockSize: 1000000, CostMicrosPerBlock: 480000},
			domain.RateEntry{RateKey: "gpt-mini", Unit: "llm_cached_token", CreditsPerBlock: 75000, BlockSize: 1000000},
		),
		Fx: contracts.PricerFixture{RateKey: "gpt-mini", UnitA: "llm_input_token", UnitB: "llm_output_token", Num: 150000, Den: 1000000},
	}
}

// Seats: flat per seat-day. It has ONE unit, so the fixture uses it as both A and B: a per-seat
// price is the same arithmetic whichever term it is.
func Seats(t *testing.T, pricer string) Case {
	t.Helper()
	return Case{
		Name: "seats",
		Card: card(t, "seats-v1", pricer,
			domain.RateEntry{RateKey: "pro", Unit: "seat_day", CreditsPerBlock: 33333, BlockSize: 100000},
		),
		Fx: contracts.PricerFixture{RateKey: "pro", UnitA: "seat_day", UnitB: "seat_day", Num: 33333, Den: 100000},
	}
}

// Bytes: storage per byte-day and egress per byte.
func Bytes(t *testing.T, pricer string) Case {
	t.Helper()
	return Case{
		Name: "bytes",
		Card: card(t, "bytes-v1", pricer,
			domain.RateEntry{RateKey: "standard", Unit: "storage_byte_day", CreditsPerBlock: 21, BlockSize: 1000000000},
			domain.RateEntry{RateKey: "standard", Unit: "egress_byte", CreditsPerBlock: 90, BlockSize: 100000},
		),
		Fx: contracts.PricerFixture{RateKey: "standard", UnitA: "storage_byte_day", UnitB: "egress_byte", Num: 21, Den: 1000000000},
	}
}
