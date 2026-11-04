package domain_test

import (
	"errors"
	"math"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R100: two grants for one key at one level are not hypothetical — a migration between plans looks like this for as long as
// it runs. A flag ORs, a quota takes the MAXIMUM, an enum takes the highest-ranked member of the host-declared order. The
// order is the key's (entitlement_keys.enum_order), not the alphabet's.

var (
	flagKey  = domain.KeyDecl{Key: "sso", Kind: domain.Flag}
	quotaKey = domain.KeyDecl{Key: "seats", Kind: domain.Quota}
	tierKey  = domain.KeyDecl{Key: "support_tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "silver", "gold"}}
)

func TestMergeFlagsOr(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ a, b, want bool }{{false, false, false}, {true, false, true}, {false, true, true}, {true, true, true}} {
		got, err := flagKey.Merge(domain.FlagValue(c.a), domain.FlagValue(c.b))
		if err != nil || got != domain.FlagValue(c.want) {
			t.Errorf("%v OR %v = %+v, %v; want %v", c.a, c.b, got, err, c.want)
		}
	}
}

func TestMergeQuotaTakesTheMaximumAndUnlimitedBeatsEveryNumber(t *testing.T) {
	t.Parallel()
	top := int64(math.MaxInt64)
	for _, c := range []struct{ a, b, want int64 }{
		{5, 25, 25}, {25, 5, 25}, {25, 25, 25}, {0, 0, 0}, {0, 7, 7},
		{-1, 25, -1}, // a numeric comparison would answer 25: -1 is the LARGEST ceiling, not the smallest
		{25, -1, -1},
		{-1, -1, -1},
		{0, -1, -1},
		{top, -1, -1}, // unlimited is not "a big number" that top can exceed
		{-1, top, -1},
		{top, top - 1, top},
	} {
		got, err := quotaKey.Merge(domain.QuotaValue(c.a), domain.QuotaValue(c.b))
		if err != nil || got != domain.QuotaValue(c.want) {
			t.Errorf("max(%d, %d) = %+v, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
}

// The declared order decides, not the alphabet: "gold" < "silver" alphabetically and still outranks it.
func TestMergeEnumFollowsTheDeclaredOrderNotTheAlphabet(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ a, b, want string }{
		{"gold", "silver", "gold"},
		{"silver", "gold", "gold"},
		{"bronze", "silver", "silver"},
		{"bronze", "gold", "gold"},
		{"silver", "silver", "silver"},
	} {
		got, err := tierKey.Merge(domain.EnumValue(c.a), domain.EnumValue(c.b))
		if err != nil || got != domain.EnumValue(c.want) {
			t.Errorf("highest(%s, %s) = %+v, %v; want %s", c.a, c.b, got, err, c.want)
		}
	}
	// The same members under another order give another winner: the order is data on the key.
	reversed := domain.KeyDecl{Key: "support_tier", Kind: domain.Enum, EnumOrder: []string{"gold", "silver", "bronze"}}
	if got, _ := reversed.Merge(domain.EnumValue("gold"), domain.EnumValue("bronze")); got != domain.EnumValue("bronze") {
		t.Errorf("a key that ranks bronze highest picked %+v", got)
	}
}

func TestMergeRefusesWhatItCannotRank(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]func() (domain.Value, error){
		"an enum member the key never declared": func() (domain.Value, error) {
			return tierKey.Merge(domain.EnumValue("gold"), domain.EnumValue("platinum"))
		},
		"a flag against a quota": func() (domain.Value, error) {
			return quotaKey.Merge(domain.FlagValue(true), domain.QuotaValue(3))
		},
		"a quota under a flag key": func() (domain.Value, error) {
			return flagKey.Merge(domain.QuotaValue(3), domain.QuotaValue(4))
		},
		"a quota below -1": func() (domain.Value, error) {
			return quotaKey.Merge(domain.QuotaValue(-2), domain.QuotaValue(4))
		},
		"an enum key with no declared order": func() (domain.Value, error) {
			return domain.KeyDecl{Key: "region", Kind: domain.Enum}.Merge(domain.EnumValue("eu"), domain.EnumValue("us"))
		},
	} {
		if got, err := f(); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %+v, %v", name, got, err)
		}
	}
}

// Unset merges to the other side. It does not stand for zero, and it does not stand for unlimited.
func TestMergeWithUnsetKeepsTheOtherSide(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		decl domain.KeyDecl
		v    domain.Value
	}{
		"flag":      {flagKey, domain.FlagValue(false)},
		"quota 0":   {quotaKey, domain.QuotaValue(0)},
		"unlimited": {quotaKey, domain.UnlimitedQuota()},
		"enum":      {tierKey, domain.EnumValue("bronze")},
	} {
		for _, side := range []string{"left", "right"} {
			a, b := domain.Value{}, c.v
			if side == "right" {
				a, b = b, a
			}
			if got, err := c.decl.Merge(a, b); err != nil || got != c.v {
				t.Errorf("%s, unset on the %s: got %+v, %v; want %+v", name, side, got, err, c.v)
			}
		}
	}
	if got, err := quotaKey.Merge(domain.Value{}, domain.Value{}); err != nil || got.IsSet() {
		t.Errorf("two unset values merged into %+v, %v; want unset", got, err)
	}
}

// Merge is a join: commutative, associative and idempotent, so the order two migrating grants arrive in decides nothing.
// Exhaustive over a set that holds every boundary, per kind.
func TestMergeIsCommutativeAssociativeAndIdempotent(t *testing.T) {
	t.Parallel()
	sets := map[string]struct {
		decl   domain.KeyDecl
		values []domain.Value
	}{
		"flag":  {flagKey, []domain.Value{{}, domain.FlagValue(false), domain.FlagValue(true)}},
		"quota": {quotaKey, []domain.Value{{}, domain.QuotaValue(0), domain.QuotaValue(1), domain.QuotaValue(25), domain.QuotaValue(math.MaxInt64), domain.UnlimitedQuota()}},
		"enum":  {tierKey, []domain.Value{{}, domain.EnumValue("bronze"), domain.EnumValue("silver"), domain.EnumValue("gold")}},
	}
	for name, s := range sets {
		m := func(a, b domain.Value) domain.Value {
			t.Helper()
			v, err := s.decl.Merge(a, b)
			if err != nil {
				t.Fatalf("%s: merge(%+v, %+v): %v", name, a, b, err)
			}
			return v
		}
		for _, a := range s.values {
			if m(a, a) != a {
				t.Errorf("%s: merge(%+v, itself) = %+v", name, a, m(a, a))
			}
			for _, b := range s.values {
				if m(a, b) != m(b, a) {
					t.Errorf("%s: merge(%+v, %+v) = %+v but reversed = %+v", name, a, b, m(a, b), m(b, a))
				}
				for _, c := range s.values {
					if m(m(a, b), c) != m(a, m(b, c)) {
						t.Errorf("%s: (%+v ∨ %+v) ∨ %+v differs from %+v ∨ (… ∨ …)", name, a, b, c, a)
					}
				}
			}
		}
	}
}
