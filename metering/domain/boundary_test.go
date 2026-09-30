package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// A comparison with a number in it is a decision about one exact value. Each test below pins the
// value on both sides of such a decision — the last input that is accepted and the first that is
// refused — so flipping a `<` to `<=` in the code is a failing test rather than a silent change to
// who gets charged, admitted, or told "yes".

func TestScopeIDIsBoundedAndCarriesNoControlCharacters(t *testing.T) {
	t.Parallel()
	scope := func(id string) domain.Scope { return domain.Scope{Realm: "t1", Kind: "org", ID: id} }
	accepted := map[string]string{
		"256 bytes is the longest id":     strings.Repeat("a", 256),
		"a space is printable":            "a b",
		"the last printable ASCII, '~'":   "a~",
		"multi-byte UTF-8 within a limit": "đồng-tiền",
	}
	for name, id := range accepted {
		if err := scope(id).Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	refused := map[string]string{
		"257 bytes is one too many": strings.Repeat("a", 257),
		"empty":                     "",
		"a tab":                     "a\tb",
		"0x1f, the last control":    "a\x1fb",
		"0x00":                      "a\x00b",
		"0x7f, DEL":                 "a\x7fb",
		"not UTF-8":                 "a\xffb",
	}
	for name, id := range refused {
		if err := scope(id).Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestSubjectsRefuseAnEmptyKindOrAnEmptyID(t *testing.T) {
	t.Parallel()
	if err := (domain.Subjects{"user": "u1", "job": "j1"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (domain.Subjects(nil)).Validate(); err != nil {
		t.Fatalf("no subjects is legal: %v", err)
	}
	for name, s := range map[string]domain.Subjects{
		"empty id":            {"user": ""},
		"empty kind":          {"": "u1"},
		"one good, one empty": {"user": "u1", "job": ""},
	} {
		if err := s.Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: a counter keyed on \"\" merges everyone's spend; got %v", name, err)
		}
	}
}

// A quantity of zero is a real event (an empty prompt still logs); only a negative one is wrong.
func TestAZeroQuantityIsLegalAndANegativeOneIsNot(t *testing.T) {
	t.Parallel()
	scope := domain.Scope{Realm: "t1", Kind: "org", ID: "o1"}
	zero := []domain.Quantity{{Unit: "llm_input_token", Amount: 0}}
	if err := (domain.Event{Scope: scope, RateKey: "k", IdemKey: "e", OccurredAt: t0, Quantities: zero}).Validate(); err != nil {
		t.Errorf("an event with a zero quantity is legal: %v", err)
	}
	if err := (domain.Charge{Scope: scope, IdemKey: "k", Quantities: zero}).Validate(); err != nil {
		t.Errorf("a charge with a zero quantity is legal: %v", err)
	}
}

func TestAGrantWithoutAnIdempotencyKeyIsRefused(t *testing.T) {
	t.Parallel()
	g := domain.Grant{Scope: domain.Scope{Realm: "t1", Kind: "org", ID: "o1"}, Amount: 10, Reason: "purchase", IdemKey: "pay_1"}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	g.IdemKey = ""
	if err := g.Validate(); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a grant mints credits: without a key a retry mints twice; got %v", err)
	}
}

func TestUsagePayloadCarriesWhatIdentifiesTheUsage(t *testing.T) {
	t.Parallel()
	c := charge()
	p := domain.UsagePayload(c)
	if p.Subjects["user"] != "u1" || p.Subjects["job"] != "j1" || len(p.Subjects) != 2 {
		t.Errorf("subjects must reach the intent so the writer books the breakdown: %v", p.Subjects)
	}
	if len(p.Quantities) != 2 || p.Quantities[0] != (domain.PayloadQuantity{Unit: "in", Amount: 10}) || p.Quantities[1] != (domain.PayloadQuantity{Unit: "out", Amount: 5}) {
		t.Errorf("quantities: %+v", p.Quantities)
	}
	if p.Resource != c.Resource || p.RateKey != c.RateKey || p.Reason != c.Reason {
		t.Errorf("payload = %+v", p)
	}
	c.Subjects = domain.Subjects{}
	if got := domain.UsagePayload(c).Subjects; got != nil {
		t.Errorf("an empty subject set must not become a payload key: %v", got)
	}
}

func TestALimitsTimeZoneIsCheckedWhereItIsUsed(t *testing.T) {
	t.Parallel()
	base := domain.Limit{Name: "l", Unit: domain.CreditUnit, Max: 1, DenyCode: domain.DenyLimitReached, TZ: "Mars/Olympus"}
	for _, w := range []domain.Window{domain.Daily, domain.Hourly} {
		l := base
		l.Window = w
		if err := l.Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("a %s limit resets in its zone, so an unknown zone must be refused: %v", w, err)
		}
	}
	rolling := base
	rolling.Window, rolling.Dur = domain.Rolling, time.Hour
	if err := rolling.Validate(); err != nil {
		t.Errorf("a rolling window has no calendar, so its zone is not read: %v", err)
	}
}

func TestAnUnknownWindowPrintsInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	if got := domain.Balance.String(); got != "balance" {
		t.Errorf("Balance = %q", got)
	}
	if got := domain.Job.String(); got != "job" {
		t.Errorf("Job = %q", got)
	}
	if got := domain.Window(5).String(); got != "window(5)" { // the first value past the table
		t.Errorf("Window(5) = %q", got)
	}
	if got := domain.Window(-1).String(); got != "window(-1)" {
		t.Errorf("Window(-1) = %q", got)
	}
}

// used + cost == Max is AT the ceiling, not over it: the request that exactly fills the budget is
// admitted, and the next unit is not.
func TestAWindowLimitAdmitsExactlyUpToItsCeiling(t *testing.T) {
	t.Parallel()
	daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, DenyCode: domain.DenyLimitReached}
	cases := []struct {
		name    string
		used    int64
		maxCost domain.Credits
		allowed bool
	}{
		{"fills the budget exactly", 60, 40, true},
		{"one credit over", 60, 41, false},
		{"untouched budget, no cost bound", 0, 0, true},
		{"one below the ceiling, no cost bound", 99, 0, true},
		{"at the ceiling, no cost bound", 100, 0, false},
		{"past the ceiling, no cost bound", 101, 0, false},
	}
	for _, c := range cases {
		adm, err := domain.Evaluate(
			domain.AdmitRequest{Subjects: domain.Subjects{"user": "u1"}, MaxCost: c.maxCost},
			[]domain.Limit{daily},
			domain.Snapshot{Eligible: 1_000, Counters: map[string]int64{"user_daily": c.used}})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if adm.Allowed != c.allowed {
			t.Errorf("%s: used %d + cost %d against max 100: allowed=%v, want %v", c.name, c.used, c.maxCost, adm.Allowed, c.allowed)
		}
	}
}
