package domain_test

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 123, time.UTC)

func charge() domain.Charge {
	return domain.Charge{
		Scope: domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}, Amount: 100, IdemKey: "chg-1",
		Reason: "query", Resource: "llm.chat", RateKey: "gpt", OccurredAt: t0,
		Quantities: []domain.Quantity{{Unit: "in", Amount: 10}, {Unit: "out", Amount: 5}},
		Subjects:   domain.Subjects{"user": "u1", "job": "j1"},
		Ref:        map[string]string{"trace_id": "a"},
	}
}

func TestChargeFingerprintIsStableAndExcludesAuditOnlyData(t *testing.T) {
	t.Parallel()
	a := charge()
	b := charge()
	b.Ref = map[string]string{"trace_id": "a-different-retry"}                           // a retry legitimately gets a new trace id
	b.Quantities = []domain.Quantity{{Unit: "out", Amount: 5}, {Unit: "in", Amount: 10}} // order is not identity
	b.CostMicros, b.RateCardVersion = 999, "v2"                                          // never a request identity
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("audit-only fields and quantity order must not change the fingerprint")
	}
	if len(a.Fingerprint()) != 64 {
		t.Fatal("expected sha256 hex")
	}
}

func TestChargeFingerprintCoversWhatIdentifiesTheRequest(t *testing.T) {
	t.Parallel()
	base := charge().Fingerprint()
	mut := map[string]func(*domain.Charge){
		"amount":      func(c *domain.Charge) { c.Amount = 700 },
		"scope":       func(c *domain.Charge) { c.Scope.ID = "w2" },
		"realm":       func(c *domain.Charge) { c.Scope.Realm = "t2" },
		"reason":      func(c *domain.Charge) { c.Reason = "ingest" },
		"resource":    func(c *domain.Charge) { c.Resource = "storage" },
		"rate key":    func(c *domain.Charge) { c.RateKey = "other" },
		"quantity":    func(c *domain.Charge) { c.Quantities[0].Amount = 11 },
		"unit":        func(c *domain.Charge) { c.Quantities[0].Unit = "cached" },
		"subject":     func(c *domain.Charge) { c.Subjects = domain.Subjects{"user": "u2", "job": "j1"} },
		"occurred_at": func(c *domain.Charge) { c.OccurredAt = t0.Add(time.Nanosecond) },
	}
	for name, f := range mut {
		c := charge()
		c.Quantities = append([]domain.Quantity(nil), c.Quantities...)
		f(&c)
		if c.Fingerprint() == base {
			t.Errorf("changing the %s did not change the fingerprint: a reused key would replay silently", name)
		}
	}
}

// Length-prefixing: no choice of field values can make two different operations encode alike.
func TestFingerprintIsUnambiguous(t *testing.T) {
	t.Parallel()
	a := charge()
	a.Reason, a.Resource = "ab", "c"
	b := charge()
	b.Reason, b.Resource = "a", "bc"
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("(ab, c) and (a, bc) collided")
	}
}

func TestGrantAndTransferFingerprints(t *testing.T) {
	t.Parallel()
	s := domain.Scope{Realm: "t1", Kind: "org", ID: "o1"}
	g := domain.Grant{Scope: s, Amount: 100, IdemKey: "k", Reason: "purchase"}
	if g.Fingerprint() != (domain.Grant{Scope: s, Pool: domain.GeneralPool, Amount: 100, IdemKey: "other-key", Reason: "purchase"}).Fingerprint() {
		t.Fatal("an empty pool is the general pool, and the key is not part of the fingerprint")
	}
	if g.Fingerprint() == (domain.Grant{Scope: s, Amount: 101, Reason: "purchase"}).Fingerprint() {
		t.Fatal("amount must matter")
	}
	exp := t0
	if g.Fingerprint() == (domain.Grant{Scope: s, Amount: 100, Reason: "purchase", ExpiresAt: &exp}).Fingerprint() {
		t.Fatal("expiry must matter")
	}
	if g.Fingerprint() == (domain.Charge{Scope: s, Amount: 100}).Fingerprint() {
		t.Fatal("a grant and a charge are different operations")
	}

	tr := domain.Transfer{From: s, To: domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}, Amount: 5, IdemKey: "t"}
	other := tr
	other.MaxDestBalance = 10
	if tr.Fingerprint() == other.Fingerprint() {
		t.Fatal("the destination cap is part of the request")
	}
	if domain.CorrectionFingerprint(domain.OpCorrection, s, "", 5, "k") == domain.CorrectionFingerprint(domain.OpExpiry, s, "", 5, "k") {
		t.Fatal("op must matter")
	}
}
