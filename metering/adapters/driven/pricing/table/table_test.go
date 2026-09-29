package table_test

import (
	"context"
	"errors"
	"go.uber.org/goleak"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/llmtoken"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/pricingtest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/seat"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/storagebyte"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// SC-004: the SAME suite, unchanged, over the table pricer in three disjoint unit spaces …
func TestTablePricer_Tokens(t *testing.T) {
	t.Parallel()
	c := pricingtest.Tokens(t, "table")
	contracts.PricerContract(t, table.New(0), c.Card, c.Fx)
}

func TestTablePricer_Seats(t *testing.T) {
	t.Parallel()
	c := pricingtest.Seats(t, "table")
	contracts.PricerContract(t, table.New(0), c.Card, c.Fx)
}

func TestTablePricer_Bytes(t *testing.T) {
	t.Parallel()
	c := pricingtest.Bytes(t, "table")
	contracts.PricerContract(t, table.New(0), c.Card, c.Fx)
}

// … and over a compiled pricer resolved through the registry, which is what proves "swap one card
// or one Pricer, no money-path change" is a test result rather than an assertion.
func registry() ports.PricerRegistry {
	return ports.PricerRegistry{
		"table":          table.New(0),
		llmtoken.Name:    llmtoken.New(0),
		seat.Name:        seat.New(0),
		storagebyte.Name: storagebyte.New(0),
	}
}

func TestRegistry_LLMToken(t *testing.T) {
	t.Parallel()
	c := pricingtest.Tokens(t, llmtoken.Name)
	contracts.PricerContract(t, registry()[c.Card.Pricer], c.Card, c.Fx)
}

func TestRegistry_Seat(t *testing.T) {
	t.Parallel()
	c := pricingtest.Seats(t, seat.Name)
	contracts.PricerContract(t, registry()[c.Card.Pricer], c.Card, c.Fx)
}

func TestRegistry_StorageByte(t *testing.T) {
	t.Parallel()
	c := pricingtest.Bytes(t, storagebyte.Name)
	contracts.PricerContract(t, registry()[c.Card.Pricer], c.Card, c.Fx)
}

// Worked examples from the contract, at 1 credit = 1 µ$.
func TestTablePricer_WorkedExamples(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	card, err := domain.NewRateCard("t1", "worked", "table", []domain.RateEntry{
		{RateKey: "gpt-4o", Unit: "llm_input_token", CreditsPerBlock: 2500000, BlockSize: 1000000, CostMicrosPerBlock: 2000000},
		{RateKey: "gpt-4o", Unit: "llm_output_token", CreditsPerBlock: 10000000, BlockSize: 1000000},
		{RateKey: "gpt-4o-mini", Unit: "llm_input_token", CreditsPerBlock: 150000, BlockSize: 1000000},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := table.New(0)

	// 1,200 input + 800 output at $2.50 / $10 per million = 3,000 + 8,000 = 11,000 credits, exactly.
	got, err := p.Price(ctx, card, domain.Event{RateKey: "gpt-4o", Quantities: []domain.Quantity{
		{Unit: "llm_input_token", Amount: 1200}, {Unit: "llm_output_token", Amount: 800}}})
	if err != nil || got.Credits != 11000 {
		t.Fatalf("got %+v, %v; want 11000 credits", got, err)
	}
	if got.CostMicros != 2400 { // 1200 × 2,000,000 / 1,000,000, rounded up once
		t.Fatalf("cost micros = %d, want 2400", got.CostMicros)
	}

	// 100 gpt-4o-mini input tokens = 15 credits — its exact list cost, where per-token rounding
	// (the design this replaces) billed a whole 1,000-µ$ credit per event.
	got, err = p.Price(ctx, card, domain.Event{RateKey: "gpt-4o-mini", Quantities: []domain.Quantity{{Unit: "llm_input_token", Amount: 100}}})
	if err != nil || got.Credits != 15 {
		t.Fatalf("got %+v, %v; want 15 credits", got, err)
	}
}

func TestTablePricer_MaxAmountIsConfigurable(t *testing.T) {
	t.Parallel()
	card, _ := domain.NewRateCard("t1", "v", "table", []domain.RateEntry{{RateKey: "k", Unit: "u", CreditsPerBlock: 1, BlockSize: 1}}, nil)
	ev := domain.Event{RateKey: "k", Quantities: []domain.Quantity{{Unit: "u", Amount: 1001}}}
	if _, err := table.New(1000).Price(context.Background(), card, ev); !errors.Is(err, domain.ErrAmountOutOfRange) {
		t.Fatalf("1001 credits must exceed a 1000 limit, got %v", err)
	}
	if got, err := table.New(1001).Price(context.Background(), card, ev); err != nil || got.Credits != 1001 {
		t.Fatalf("1001 credits must fit a 1001 limit, got %+v %v", got, err)
	}
}

// A hand-built card (no index) must price identically to an indexed one.
func TestTablePricer_HandBuiltCardMatchesIndexed(t *testing.T) {
	t.Parallel()
	entries := []domain.RateEntry{{RateKey: "k", Unit: "u", CreditsPerBlock: 7, BlockSize: 3}}
	indexed, _ := domain.NewRateCard("t1", "v", "table", entries, nil)
	hand := domain.RateCard{Realm: "t1", Version: "v", Pricer: "table", Entries: entries}
	ev := domain.Event{RateKey: "k", Quantities: []domain.Quantity{{Unit: "u", Amount: 10}}}
	a, errA := table.New(0).Price(context.Background(), indexed, ev)
	b, errB := table.New(0).Price(context.Background(), hand, ev)
	if errA != nil || errB != nil || a != b || a.Credits != 24 { // ceil(70/3) = 24
		t.Fatalf("indexed %+v %v vs hand-built %+v %v", a, errA, b, errB)
	}
}

func TestReferencePricersRefuseForeignUnits(t *testing.T) {
	t.Parallel()
	card := pricingtest.Tokens(t, llmtoken.Name).Card
	ev := domain.Event{RateKey: "gpt-mini", Quantities: []domain.Quantity{{Unit: "seat_day", Amount: 1}}}
	if _, err := llmtoken.New(0).Price(context.Background(), card, ev); !errors.Is(err, domain.ErrUnpriceable) {
		t.Fatalf("an LLM pricer must not price a seat unit, got %v", err)
	}
}

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
