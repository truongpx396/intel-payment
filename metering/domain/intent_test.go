package domain_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

func TestDrawsRoundTrip(t *testing.T) {
	t.Parallel()
	in := []domain.PoolDelta{{Pool: "general", Delta: -11000}, {Pool: "promo", Delta: -500}}
	s := domain.FormatDraws(in)
	if s != "general:-11000;promo:-500" {
		t.Fatal(s)
	}
	out, err := domain.ParseDraws(s)
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("%v %v", out, err)
	}
	if d, err := domain.ParseDraws(""); err != nil || d != nil {
		t.Fatalf("a zero-amount usage has no draws: %v %v", d, err)
	}
	for _, bad := range []string{"general", "general:", ":5", "general:x", "general:1;;promo:2"} {
		if _, err := domain.ParseDraws(bad); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", bad, err)
		}
	}
}

func TestIntentRoundTripsThroughStreamFields(t *testing.T) {
	t.Parallel()
	c := charge()
	in := domain.Intent{
		Op: domain.OpUsage, Scope: c.Scope, Gen: 3, Seq: 42, IdemKey: c.IdemKey, Fingerprint: c.Fingerprint(),
		Draws:    []domain.PoolDelta{{Pool: "promo", Delta: -60}, {Pool: "general", Delta: -40}},
		Writeoff: 0, LowWater: "general:1000", Payload: domain.UsagePayload(c),
	}
	f, err := in.Fields()
	if err != nil {
		t.Fatal(err)
	}
	out, err := domain.IntentFromFields(f)
	if err != nil {
		t.Fatal(err)
	}
	if out.Seq != 42 || out.Gen != 3 || out.Op != domain.OpUsage || out.Scope != c.Scope || out.LowWater != "general:1000" {
		t.Fatalf("%+v", out)
	}
	if !reflect.DeepEqual(out.Draws, in.Draws) || !reflect.DeepEqual(out.Payload, in.Payload) {
		t.Fatalf("draws or payload changed in transit:\n%+v\n%+v", out, in)
	}
	if total, _ := out.Total(); total != -100 {
		t.Fatalf("total %d", total)
	}
	when, err := out.Payload.Occurred()
	if err != nil || !when.Equal(t0) {
		t.Fatalf("occurred_at lost precision: %v %v", when, err)
	}
	if q := out.Payload.QuantitiesOf(); !reflect.DeepEqual(q, c.Quantities) {
		t.Fatalf("%v", q)
	}
}

func TestIntentWithWriteoff(t *testing.T) {
	t.Parallel()
	in := domain.Intent{Op: domain.OpGrant, Scope: domain.Scope{Kind: "a", ID: "b"}, Gen: 1, Seq: 1, IdemKey: "k", Fingerprint: "f",
		Draws: []domain.PoolDelta{{Pool: "general", Delta: -10}}, Writeoff: 90}
	f, _ := in.Fields()
	out, err := domain.IntentFromFields(f)
	if err != nil || out.Writeoff != 90 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestIntentFromFieldsRefusesMalformedEntries(t *testing.T) {
	t.Parallel()
	good := func() map[string]string {
		in := domain.Intent{Op: domain.OpUsage, Scope: domain.Scope{Realm: "t1", Kind: "a", ID: "b"}, Gen: 1, Seq: 1, IdemKey: "k", Fingerprint: "f"}
		f, _ := in.Fields()
		return f
	}
	if _, err := domain.IntentFromFields(good()); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(map[string]string){
		"version":   func(f map[string]string) { f["v"] = "2" },
		"op":        func(f map[string]string) { f["op"] = "mint" },
		"gen":       func(f map[string]string) { f["gen"] = "x" },
		"seq zero":  func(f map[string]string) { f["seq"] = "0" },
		"no idem":   func(f map[string]string) { f["idem"] = "" },
		"no fp":     func(f map[string]string) { f["fp"] = "" },
		"draws":     func(f map[string]string) { f["draws"] = "general" },
		"wo":        func(f map[string]string) { f["wo"] = "x" },
		"payload":   func(f map[string]string) { f["p"] = "{not json" },
		"bad scope": func(f map[string]string) { f["kind"] = "A:B" },
	}
	for name, mutate := range bad {
		f := good()
		mutate(f)
		if _, err := domain.IntentFromFields(f); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid (the writer parks it), got %v", name, err)
		}
	}
}

func TestChargeGrantTransferValidation(t *testing.T) {
	t.Parallel()
	s := domain.Scope{Realm: "t1", Kind: "org", ID: "o1"}
	w := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	must := func(name string, err error, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: want %v, got %v", name, want, err)
		}
	}
	must("charge without key", domain.Charge{Scope: s, Amount: 1}.Validate(), domain.ErrInvalid)
	must("negative charge", domain.Charge{Scope: s, Amount: -1, IdemKey: "k"}.Validate(), domain.ErrInvalid)
	must("negative quantity", domain.Charge{Scope: s, IdemKey: "k", Quantities: []domain.Quantity{{Unit: "u", Amount: -1}}}.Validate(), domain.ErrInvalid)
	if err := (domain.Charge{Scope: s, Amount: 0, IdemKey: "k"}).Validate(); err != nil {
		t.Errorf("a zero-cost charge is legal (a free event still logs): %v", err)
	}

	must("grant zero", domain.Grant{Scope: s, IdemKey: "k", Reason: "r"}.Validate(), domain.ErrInvalid)
	must("grant without reason", domain.Grant{Scope: s, IdemKey: "k", Amount: 1}.Validate(), domain.ErrInvalid)
	must("bad pool", domain.Grant{Scope: s, IdemKey: "k", Amount: 1, Reason: "r", Pool: "Bad Pool"}.Validate(), domain.ErrInvalid)
	exp := t0
	must("expiring refund", domain.Grant{Scope: s, IdemKey: "k", Amount: -1, Reason: "refund", ExpiresAt: &exp}.Validate(), domain.ErrInvalid)

	must("transfer across realms", domain.Transfer{From: s, To: domain.Scope{Realm: "t2", Kind: "workspace", ID: "w"}, Amount: 1, IdemKey: "k"}.Validate(), domain.ErrCrossRealm)
	must("transfer to self", domain.Transfer{From: s, To: s, Amount: 1, IdemKey: "k"}.Validate(), domain.ErrInvalid)
	must("transfer zero", domain.Transfer{From: s, To: w, IdemKey: "k"}.Validate(), domain.ErrInvalid)
	must("negative cap", domain.Transfer{From: s, To: w, Amount: 1, IdemKey: "k", MaxDestBalance: -1}.Validate(), domain.ErrInvalid)
	if err := (domain.Transfer{From: s, To: s, FromPool: "promo", Amount: 1, IdemKey: "k"}).Validate(); err != nil {
		t.Errorf("moving credits between two pools of one scope is a valid transfer: %v", err)
	}
	if err := (domain.Transfer{From: s, To: w, Amount: 1, IdemKey: "k"}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestIsHotUnavailable(t *testing.T) {
	t.Parallel()
	for _, e := range []error{domain.ErrHotStoreUnavailable, domain.ErrColdShard, domain.ErrShardFrozen, domain.ErrBackpressure} {
		if !domain.IsHotUnavailable(e) {
			t.Errorf("%v: usage must be journaled, not lost", e)
		}
	}
	if domain.IsHotUnavailable(domain.ErrColdScope) || domain.IsHotUnavailable(domain.ErrIdemConflict) {
		t.Error("a cold scope is repaired in place; a conflict is the caller's bug")
	}
}

func TestEventValidate(t *testing.T) {
	t.Parallel()
	ok := domain.Event{Scope: domain.Scope{Kind: "a", ID: "b"}, RateKey: "k", IdemKey: "e", OccurredAt: t0}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]domain.Event{
		"no idem key":     {Scope: ok.Scope, RateKey: "k", OccurredAt: t0},
		"no occurred_at":  {Scope: ok.Scope, RateKey: "k", IdemKey: "e"},
		"no rate key":     {Scope: ok.Scope, IdemKey: "e", OccurredAt: t0},
		"no scope":        {RateKey: "k", IdemKey: "e", OccurredAt: t0},
		"empty subject":   {Scope: ok.Scope, RateKey: "k", IdemKey: "e", OccurredAt: t0, Subjects: domain.Subjects{"user": ""}},
		"negative amount": {Scope: ok.Scope, RateKey: "k", IdemKey: "e", OccurredAt: t0, Quantities: []domain.Quantity{{Unit: "u", Amount: -1}}},
		"unit-less":       {Scope: ok.Scope, RateKey: "k", IdemKey: "e", OccurredAt: t0, Quantities: []domain.Quantity{{Amount: 1}}},
	} {
		if err := e.Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestRateCardValidation(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewRateCard("t1", "", "table", nil, nil); err == nil {
		t.Error("a card needs a version")
	}
	dup := []domain.RateEntry{{RateKey: "k", Unit: "u", BlockSize: 1}, {RateKey: "k", Unit: "u", BlockSize: 2}}
	if _, err := domain.NewRateCard("t1", "v", "table", dup, nil); err == nil {
		t.Error("two prices for one (key, unit) is an ambiguity, not a choice")
	}
	for name, e := range map[string]domain.RateEntry{
		"zero block":    {RateKey: "k", Unit: "u", BlockSize: 0},
		"negative rate": {RateKey: "k", Unit: "u", BlockSize: 1, CreditsPerBlock: -1},
		"no key":        {Unit: "u", BlockSize: 1},
		"no unit":       {RateKey: "k", BlockSize: 1},
		"negative cost": {RateKey: "k", Unit: "u", BlockSize: 1, CostMicrosPerBlock: -1},
	} {
		if _, err := domain.NewRateCard("t1", "v", "", []domain.RateEntry{e}, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	c, err := domain.NewRateCard("", "v", "", []domain.RateEntry{{RateKey: "k", Unit: "u", CreditsPerBlock: 1, BlockSize: 1}}, nil)
	if err != nil || c.Realm != domain.DefaultRealm || c.Pricer != "table" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
}
