package domain_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
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

// The fingerprint is STORED — in the hot guard and in the durable one — and compared on every retry.
// If its encoding ever changed, every retry in flight across a deploy would be an ErrIdemConflict.
// These tests therefore pin the encoding itself, built by hand from the documented form
// (length-prefixed fields, in this order), not merely its properties.
func sha(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func TestChargeFingerprintIsTheDocumentedEncoding(t *testing.T) {
	t.Parallel()
	canonical := "5:usage" + "15:t1/workspace:w1" + "3:100" + "5:query" + "8:llm.chat" + "3:gpt" +
		"1:2" + "2:in" + "2:10" + "3:out" + "1:5" + // quantities, sorted by unit
		"1:2" + "3:job" + "2:j1" + "4:user" + "2:u1" + // subjects, sorted by kind
		"30:2026-09-29T12:00:00.000000123Z"
	if got, want := charge().Fingerprint(), sha(canonical); got != want {
		t.Fatalf("the canonical form changed:\n got %s\nwant %s", got, want)
	}

	// One unit named twice: sorted by unit, then amount — so both listings encode identically.
	c := charge()
	c.Subjects = nil
	dup := "5:usage" + "15:t1/workspace:w1" + "3:100" + "5:query" + "8:llm.chat" + "3:gpt" +
		"1:2" + "1:u" + "1:1" + "1:u" + "1:2" + "1:0" + "30:2026-09-29T12:00:00.000000123Z"
	for _, qs := range [][]domain.Quantity{{{Unit: "u", Amount: 1}, {Unit: "u", Amount: 2}}, {{Unit: "u", Amount: 2}, {Unit: "u", Amount: 1}}} {
		c.Quantities = qs
		if got, want := c.Fingerprint(), sha(dup); got != want {
			t.Fatalf("quantities %v:\n got %s\nwant %s", qs, got, want)
		}
	}
}

func TestGrantAndTransferFingerprintsAreTheDocumentedEncoding(t *testing.T) {
	t.Parallel()
	org := domain.Scope{Realm: "t1", Kind: "org", ID: "o1"}
	ws := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}

	g := domain.Grant{Scope: org, Amount: 100, Reason: "purchase"}
	if got, want := g.Fingerprint(), sha("5:grant"+"9:t1/org:o1"+"7:general"+"3:100"+"8:purchase"+"0:"); got != want {
		t.Errorf("grant:\n got %s\nwant %s", got, want)
	}
	exp := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	g.Pool, g.ExpiresAt = "promo", &exp
	if got, want := g.Fingerprint(), sha("5:grant"+"9:t1/org:o1"+"5:promo"+"3:100"+"8:purchase"+"20:2027-01-02T03:04:05Z"); got != want {
		t.Errorf("expiring grant:\n got %s\nwant %s", got, want)
	}

	tr := domain.Transfer{From: org, To: ws, FromPool: "promo", Amount: 5, Reason: "allocate", MaxDestBalance: 50}
	if got, want := tr.Fingerprint(), sha("8:transfer"+"9:t1/org:o1"+"5:promo"+"15:t1/workspace:w1"+"7:general"+"1:5"+"8:allocate"+"2:50"); got != want {
		t.Errorf("transfer:\n got %s\nwant %s", got, want)
	}

	if got, want := domain.CorrectionFingerprint(domain.OpCorrection, org, "", -7, "fix-1"), sha(lp(string(domain.OpCorrection))+"9:t1/org:o1"+"7:general"+"2:-7"+"5:fix-1"); got != want {
		t.Errorf("correction:\n got %s\nwant %s", got, want)
	}
}

// lp length-prefixes one field, as the canonical form does.
func lp(s string) string { return strconv.Itoa(len(s)) + ":" + s }
