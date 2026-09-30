package domain_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

func TestScopeTagIncludesTheRealm(t *testing.T) {
	t.Parallel()
	if got := (domain.Scope{Kind: "workspace", ID: "w1"}).Tag(); got != "default/workspace:w1" {
		t.Fatalf("empty realm must default: %q", got)
	}
	a := domain.Scope{Realm: "prod-a", Kind: "org", ID: "1"}
	b := domain.Scope{Realm: "prod-b", Kind: "org", ID: "1"}
	if a.Tag() == b.Tag() {
		t.Fatal("two realms must never share a tag (invariant 11)")
	}
}

// The shard is PLACEMENT and must agree with every other implementation of fnv1a64 — a process that
// hashes differently keeps a second set of books. The expected values were computed independently.
func TestScopeShardMatchesFNV1a64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		scope domain.Scope
		n     int
		want  domain.Shard
	}{
		{domain.Scope{Kind: "workspace", ID: "w1"}, 256, 70},
		{domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}, 256, 98},
		{domain.Scope{Realm: "prod-a", Kind: "org", ID: "1"}, 256, 250},
		{domain.Scope{Realm: "prod-b", Kind: "org", ID: "1"}, 256, 171},
		{domain.Scope{Kind: "workspace", ID: "w1"}, 16384, 10054},
		{domain.Scope{Kind: "workspace", ID: "w1"}, 1, 0},
	}
	for _, c := range cases {
		if got := c.scope.Shard(c.n); got != c.want {
			t.Errorf("%s over %d shards = %d, want %d", c.scope.Tag(), c.n, got, c.want)
		}
	}
}

func TestScopeShardRefusesNoShards(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Shard(0) must not silently divide by zero")
		}
	}()
	domain.Scope{Kind: "a", ID: "b"}.Shard(0)
}

func TestScopeValidate(t *testing.T) {
	t.Parallel()
	ok := domain.Scope{Realm: "t1", Kind: "workspace", ID: "018f-uuid"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]domain.Scope{
		"kind with colon": {Kind: "a:b", ID: "c"},
		"kind with slash": {Kind: "a/b", ID: "c"},
		"uppercase kind":  {Kind: "Workspace", ID: "c"},
		"empty kind":      {Kind: "", ID: "c"},
		"empty id":        {Kind: "a", ID: ""},
		"newline in id":   {Kind: "a", ID: "x\ny"},
		"huge id":         {Kind: "a", ID: strings.Repeat("x", 257)},
		"bad realm":       {Realm: "Prod A", Kind: "a", ID: "b"},
		"invalid utf8 id": {Kind: "a", ID: "\xff\xfe"},
	}
	for name, s := range bad {
		if err := s.Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// A kind cannot contain ':', so these two scopes — which would otherwise share the tag
// "r/a:b:c" and therefore one balance — cannot both exist.
func TestScopeTagsCannotCollide(t *testing.T) {
	t.Parallel()
	one := domain.Scope{Realm: "r", Kind: "a:b", ID: "c"}
	two := domain.Scope{Realm: "r", Kind: "a", ID: "b:c"}
	if one.Tag() != two.Tag() {
		t.Fatal("test premise: these do collide as raw tags")
	}
	if one.Validate() == nil {
		t.Fatal("the ambiguous scope must be refused at Validate")
	}
	if err := two.Validate(); err != nil {
		t.Fatalf("the well-formed one is fine: %v", err)
	}
}

func TestPools(t *testing.T) {
	t.Parallel()
	defs := []domain.PoolDef{
		{Name: "promo", Priority: 10},
		{Name: "gpu", Priority: 20, AppliesTo: []string{"gpu.job"}},
		{Name: "early", Priority: 10}, // ties break by name
	}
	got := domain.EligiblePools(defs, "llm.chat")
	want := []domain.Pool{"early", "promo", "general"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if g := domain.EligiblePools(defs, "gpu.job"); g[len(g)-1] != domain.GeneralPool || len(g) != 4 {
		t.Fatalf("a GPU job may draw the GPU pool, general last: %v", g)
	}
	if g := domain.EligiblePools(nil, "x"); len(g) != 1 || g[0] != domain.GeneralPool {
		t.Fatalf("a realm with no pools has exactly general: %v", g)
	}
	// Even if a store returned a row named general, it is not drawn twice or early.
	if g := domain.EligiblePools([]domain.PoolDef{{Name: "general", Priority: -1}}, "x"); len(g) != 1 {
		t.Fatalf("general is implicit: %v", g)
	}
}

func TestMoneyAndCreditsAreDistinctAndChecked(t *testing.T) {
	t.Parallel()
	if _, err := (domain.Credits(1 << 62)).Add(1 << 62); !errors.Is(err, domain.ErrAmountOutOfRange) {
		t.Fatal("credit overflow must be refused, not wrapped")
	}
	if sum, err := domain.SumCredits(5, -2, 10); err != nil || sum != 13 {
		t.Fatalf("sum = %d, %v", sum, err)
	}
	usd := domain.Money{MinorUnits: 100, Currency: "USD"}
	if _, err := usd.Add(domain.Money{MinorUnits: 1, Currency: "EUR"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("no FX: mixed currencies must be refused")
	}
	if got, err := usd.Add(domain.Money{MinorUnits: 250, Currency: "USD"}); err != nil || got.MinorUnits != 350 {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := (domain.Money{MinorUnits: 1 << 62, Currency: "USD"}).Add(domain.Money{MinorUnits: 1 << 62, Currency: "USD"}); err == nil {
		t.Fatal("money overflow must be refused")
	}
	if err := (domain.Money{Currency: "usd"}).Validate(); err == nil {
		t.Fatal("currency must be an upper-case ISO-4217 code")
	}
}

func TestSubjectTokenCannotSplitOrBecomeAWildcard(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"plain", "a.b", "a*b", "a>b", "a b", "a\tb", "100%", "%2E", "x.y.*.>"} {
		s := domain.Scope{Realm: "prod", Kind: "workspace", ID: id}
		tok := s.SubjectToken()
		if strings.ContainsAny(tok, ".*> \t\n\r") {
			t.Errorf("%q → %q still holds a reserved character", id, tok)
		}
		back, err := url.PathUnescape(tok)
		if err != nil || back != s.Tag() {
			t.Errorf("%q → %q does not decode back to its tag: %q %v", id, tok, back, err)
		}
	}
	// Distinct scopes never share a token, or a subject could be consumed as another scope's.
	a := domain.Scope{Realm: "prod", Kind: "workspace", ID: "a.b"}.SubjectToken()
	b := domain.Scope{Realm: "prod", Kind: "workspace", ID: "a%2Eb"}.SubjectToken()
	if a == b {
		t.Fatalf("%q and %q collide", a, b)
	}
}
