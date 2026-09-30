package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestRecordPricesUnderTheActiveCardAndBuildsTheCharge(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	e := event("evt-1")
	e.Subjects = domain.Subjects{"user": "u1"}
	e.Attributes = map[string]string{"trace_id": "t9", "operation_type": "query"}
	rc, err := r.m.Record(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	// 1,200 in × 150000/1e6 + 800 out × 600000/1e6 = 180 + 480 = 660, exactly.
	c := r.bal.debited[0]
	if c.Amount != 660 || c.RateCardVersion != "v1" || c.CostMicros != 120 || c.RateKey != "gpt" || c.Resource != "llm.chat" {
		t.Fatalf("%+v", c)
	}
	if c.Reason != "query" || c.Ref["trace_id"] != "t9" || c.Ref["operation_type"] != "" {
		t.Fatalf("operation_type comes from the attribute and is not duplicated into ref: %+v", c)
	}
	if !c.OccurredAt.Equal(e.OccurredAt) || c.Subjects["user"] != "u1" || len(c.Quantities) != 2 {
		t.Fatalf("invariant 8: the debit carries the usage record: %+v", c)
	}
	if !rc.Applied || rc.Deferred || rc.RateCardVersion != "v1" || rc.Delta != -660 {
		t.Fatalf("%+v", rc)
	}
	if len(r.journal.entries) != 0 {
		t.Fatal("outbox settlement writes no journal row on the happy path")
	}
	// The reason defaults to the resource.
	e2 := event("evt-2")
	_, _ = r.m.Record(context.Background(), e2)
	if r.bal.debited[1].Reason != "llm.chat" {
		t.Fatalf("%q", r.bal.debited[1].Reason)
	}
}

func TestRecordRefusesStaleAndFutureEvents(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	window := r.cfg.UsageIdemWindow - r.cfg.IdemSafetyMargin // 72h - 1h
	at := func(d time.Duration) domain.Event { e := event("k"); e.OccurredAt = epoch.Add(d); return e }

	if _, err := r.m.Record(context.Background(), at(-window)); err != nil {
		t.Fatalf("exactly at the boundary is still inside: %v", err)
	}
	_, err := r.m.Record(context.Background(), at(-window-time.Second))
	if !errors.Is(err, domain.ErrStaleEvent) {
		t.Fatalf("an event older than the window must be refused, not risk a second charge: %v", err)
	}
	if r.met.count(ports.MetricStaleEvent) != 1 {
		t.Fatal("stale events are counted")
	}
	if _, err := r.m.Record(context.Background(), at(r.cfg.MaxClockSkew+time.Second)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a timestamp from the future: %v", err)
	}
	if _, err := r.m.Record(context.Background(), at(r.cfg.MaxClockSkew)); err != nil {
		t.Fatalf("within the allowed skew: %v", err)
	}
	if len(r.bal.debited) != 2 {
		t.Fatalf("a refused event must never reach the hot tier: %d debits", len(r.bal.debited))
	}
}

func TestRecordFailsClosedWhenItCannotPrice(t *testing.T) {
	t.Parallel()
	bad := event("k")
	bad.RateKey = "unknown-model"
	unit := event("k")
	unit.Quantities = []domain.Quantity{{Unit: "no-such-unit", Amount: 1}}

	cases := map[string]struct {
		opt  rigOpt
		ev   domain.Event
		want error
		why  string
	}{
		"unknown rate key":            {rigOpt{}, bad, domain.ErrUnpriceable, "unpriceable"},
		"unknown unit":                {rigOpt{}, unit, domain.ErrUnpriceable, "unpriceable"},
		"no active card":              {rigOpt{cards: map[domain.Realm][]domain.RateCard{}}, event("k"), domain.ErrUnpriceable, "no_card"},
		"unregistered pricer":         {rigOpt{cards: cardNaming(t, "compiled")}, event("k"), domain.ErrUnpriceable, "no_pricer"},
		"a pricer that overshoots":    {rigOpt{cards: cardNaming(t, "greedy"), pricers: registryWith("greedy", greedy{})}, event("k"), domain.ErrAmountOutOfRange, "out_of_range"},
		"a pricer that goes negative": {rigOpt{cards: cardNaming(t, "refunder"), pricers: registryWith("refunder", negative{})}, event("k"), domain.ErrAmountOutOfRange, "out_of_range"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, c.opt)
			_, err := r.m.Record(context.Background(), c.ev)
			if !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
			if len(r.bal.debited) != 0 || len(r.journal.entries) != 0 {
				t.Fatal("pricing at zero, or guessing, would under-bill silently: nothing may be debited or journaled")
			}
			if r.met.count("metering_price_error_total{"+c.why+"}") != 1 {
				t.Fatalf("a fail-closed pricing refusal is observable: %v", r.met.counts)
			}
		})
	}
}

func TestRecordDefersToTheJournalDuringAnOutage(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{domain.ErrHotStoreUnavailable, domain.ErrShardFrozen, domain.ErrColdShard, domain.ErrBackpressure} {
		r := newRig(t, rigOpt{})
		r.bal.debit = func(domain.Charge) (domain.Receipt, error) { return domain.Receipt{}, cause }
		e := event("evt-1")
		rc, err := r.m.Record(context.Background(), e)
		if err != nil {
			t.Fatalf("%v: invariant 16 — an outage must not drop or fail usage: %v", cause, err)
		}
		if !rc.Applied || !rc.Deferred || rc.Seq != 0 || rc.RateCardVersion != "v1" {
			t.Fatalf("%v: %+v", cause, rc)
		}
		if len(r.journal.entries) != 1 {
			t.Fatalf("%v: the charge must be in the journal", cause)
		}
		j := r.journal.entries[0]
		if j.Charge.Amount != 660 || j.Fingerprint != j.Charge.Fingerprint() || j.Charge.IdemKey != "evt-1" || j.Charge.RateCardVersion != "v1" {
			t.Fatalf("%v: priced ONCE, at Record, and replayed unchanged: %+v", cause, j)
		}
		if r.met.count(ports.MetricRecordDeferred) != 1 {
			t.Fatalf("%v: deferral is observable", cause)
		}
	}
}

func TestRecordDoesNotClaimAcceptanceWhenBothStoresFail(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.debit = func(domain.Charge) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrHotStoreUnavailable }
	r.journal.putErr = errors.New("postgres down")
	rc, err := r.m.Record(context.Background(), event("evt-1"))
	if err == nil || rc.Deferred || rc.Applied {
		t.Fatalf("if neither store took it the host must see an error and retry: %+v %v", rc, err)
	}
	if !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("the cause stays reachable: %v", err)
	}
}

func TestJournalSettlementWritesTheJournalFirst(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{cfg: metering.Config{Settlement: metering.SettlementJournal}})
	rc, err := r.m.Record(context.Background(), event("evt-1"))
	if err != nil || !rc.Applied || rc.Deferred {
		t.Fatalf("%+v %v", rc, err)
	}
	if len(r.order) != 2 || r.order[0] != "journal" || r.order[1] != "debit" {
		t.Fatalf("Postgres is the intent's first durable home, then the hot function: %v", r.order)
	}
	// And if the hot tier then fails, the entry is already there: it is not written twice.
	r2 := newRig(t, rigOpt{cfg: metering.Config{Settlement: metering.SettlementJournal}})
	r2.bal.debit = func(domain.Charge) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrShardFrozen }
	rc, err = r2.m.Record(context.Background(), event("evt-1"))
	if err != nil || !rc.Deferred || len(r2.journal.entries) != 1 {
		t.Fatalf("%+v %v %d", rc, err, len(r2.journal.entries))
	}
	// A journal that cannot take the row fails the call: under journal settlement nothing is accepted without it.
	r3 := newRig(t, rigOpt{cfg: metering.Config{Settlement: metering.SettlementJournal}})
	r3.journal.putErr = errors.New("postgres down")
	if _, err := r3.m.Record(context.Background(), event("evt-1")); err == nil || len(r3.bal.debited) != 0 {
		t.Fatalf("no journal row, no hot write: %v (%d debits)", err, len(r3.bal.debited))
	}
}

func TestRecordSurfacesIdempotencyConflictsLoudly(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.debit = func(domain.Charge) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrIdemConflict }
	if _, err := r.m.Record(context.Background(), event("k")); !errors.Is(err, domain.ErrIdemConflict) {
		t.Fatal(err)
	}
	if r.met.count("metering_idem_conflict_total{usage}") != 1 || len(r.journal.entries) != 0 {
		t.Fatal("a conflict is counted and must not be journaled around")
	}
	j := newRig(t, rigOpt{cfg: metering.Config{Settlement: metering.SettlementJournal}})
	j.journal.putErr = domain.ErrIdemConflict
	if _, err := j.m.Record(context.Background(), event("k")); !errors.Is(err, domain.ErrIdemConflict) || j.met.count("metering_idem_conflict_total{usage}") != 1 {
		t.Fatalf("%v", err)
	}
}

func TestRecordRehydratesAColdScopeAndRetries(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	calls := 0
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		calls++
		if calls == 1 {
			return domain.Receipt{}, domain.ErrColdScope
		}
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Seq: 1}, nil
	}
	rc, err := r.m.Record(context.Background(), event("k"))
	if err != nil || !rc.Applied || calls != 2 || len(r.bal.rehydOn) != 1 {
		t.Fatalf("%+v %v calls=%d rehydrates=%d", rc, err, calls, len(r.bal.rehydOn))
	}
	if r.met.count("metering_rehydrate_total{new_scope}") != 1 {
		t.Fatal("a scope with no watermark is a new_scope rehydrate")
	}
}

func TestEventAttributesNeverChangeTheRequestIdentity(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	a, b := event("k"), event("k")
	a.Attributes = map[string]string{"trace_id": "a"}
	b.Attributes = map[string]string{"trace_id": "b-a-retry"}
	_, _ = r.m.Record(context.Background(), a)
	_, _ = r.m.Record(context.Background(), b)
	if r.bal.debited[0].Fingerprint() != r.bal.debited[1].Fingerprint() {
		t.Fatal("a retry with a new trace id is the SAME request; a conflict here would drop charges")
	}
}

func TestRecordRefusesAMalformedEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	bad := event("k")
	bad.IdemKey = ""
	if _, err := r.m.Record(context.Background(), bad); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	bad = event("k")
	bad.OccurredAt = time.Time{}
	if _, err := r.m.Record(context.Background(), bad); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
	if len(r.bal.debited) != 0 {
		t.Fatal("nothing reaches the hot tier")
	}
}

func cardNaming(t *testing.T, pricer string) map[domain.Realm][]domain.RateCard {
	t.Helper()
	c, err := domain.NewRateCard("t1", "v9", pricer, []domain.RateEntry{
		{RateKey: "gpt", Unit: "in", CreditsPerBlock: 1, BlockSize: 1}, {RateKey: "gpt", Unit: "out", CreditsPerBlock: 1, BlockSize: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return map[domain.Realm][]domain.RateCard{"t1": {c}}
}

func registryWith(name string, p ports.Pricer) ports.PricerRegistry {
	return ports.PricerRegistry{"table": nilPricer{}, name: p}
}

type nilPricer struct{}

func (nilPricer) Price(context.Context, domain.RateCard, domain.Event) (domain.Price, error) {
	return domain.Price{}, nil
}

// greedy is a compiled pricer that ignores MaxOperationAmount; the Meter must not trust it to.
type greedy struct{}

func (greedy) Price(context.Context, domain.RateCard, domain.Event) (domain.Price, error) {
	return domain.Price{Credits: domain.MaxOperationAmount + 1, RateCardVersion: "v9"}, nil
}

type negative struct{}

func (negative) Price(context.Context, domain.RateCard, domain.Event) (domain.Price, error) {
	return domain.Price{Credits: -5}, nil
}

// fixedPrice is a compiled pricer that answers one price whatever the event.
type fixedPrice domain.Price

func (f fixedPrice) Price(context.Context, domain.RateCard, domain.Event) (domain.Price, error) {
	return domain.Price(f), nil
}

// The Meter does not trust a compiled pricer with the range check the table pricer does for itself,
// so the bounds it enforces are its own: a free unit is a price, the ceiling itself is allowed, and
// one step past either end is refused.
func TestRecordHoldsACompiledPricerToTheRangeExactly(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		credits domain.Credits
		refused bool
	}{
		"free (a trial, a zero-rated unit)": {0, false},
		"one credit":                        {1, false},
		"exactly the ceiling":               {domain.MaxOperationAmount, false},
		"one past the ceiling":              {domain.MaxOperationAmount + 1, true},
		"one below zero":                    {-1, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOpt{cards: cardNaming(t, "compiled"),
				pricers: registryWith("compiled", fixedPrice{Credits: c.credits, RateCardVersion: "v9"})})
			_, err := r.m.Record(context.Background(), event("k"))
			if c.refused {
				if !errors.Is(err, domain.ErrAmountOutOfRange) || len(r.bal.debited) != 0 {
					t.Fatalf("%d credits must be refused and never debited: %v, %d debits", c.credits, err, len(r.bal.debited))
				}
				return
			}
			if err != nil {
				t.Fatalf("%d credits is inside the range: %v", c.credits, err)
			}
			if len(r.bal.debited) != 1 || r.bal.debited[0].Amount != c.credits {
				t.Fatalf("debits: %+v", r.bal.debited)
			}
		})
	}
}

// Invariant 5: a ledger row names the card that priced it, whether the pricer said so or not.
func TestRecordNamesTheCardOnTheRowWhenThePricerDoesNot(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ named, want string }{
		"a pricer that names no version gets the card's": {"", "v9"},
		"a pricer that names its own version keeps it":   {"pricer-7", "pricer-7"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, rigOpt{cards: cardNaming(t, "compiled"),
				pricers: registryWith("compiled", fixedPrice{Credits: 5, RateCardVersion: c.named})})
			rc, err := r.m.Record(context.Background(), event("k"))
			if err != nil {
				t.Fatal(err)
			}
			if rc.RateCardVersion != c.want || r.bal.debited[0].RateCardVersion != c.want {
				t.Fatalf("want card %q on the receipt and the row, got %q and %q", c.want, rc.RateCardVersion, r.bal.debited[0].RateCardVersion)
			}
		})
	}
}

// An operator debugging a refused event reads the message: it must state the bound actually enforced
// (the window less its safety margin), not some other number.
func TestRecordStaleRefusalNamesTheBoundItEnforces(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	window := r.cfg.UsageIdemWindow - r.cfg.IdemSafetyMargin
	e := event("k")
	e.OccurredAt = epoch.Add(-window - time.Hour)
	_, err := r.m.Record(context.Background(), e)
	if !errors.Is(err, domain.ErrStaleEvent) {
		t.Fatalf("want a stale refusal, got %v", err)
	}
	for _, want := range []string{"occurred " + (window + time.Hour).String() + " ago", "beyond " + window.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal should say %q: %v", want, err)
		}
	}
}
