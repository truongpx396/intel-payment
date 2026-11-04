package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R100: a capability is named by the host and declared per realm. A key is part of a cache entry, a URL and a log line,
// so it is a short lower-case word; a declaration says what kind of value it holds and, for an enum, its ranking.

func TestKeyValidate(t *testing.T) {
	t.Parallel()
	for _, k := range []domain.Key{"sso", "seats", "daily_user_credits", "a", "x9", domain.Key(strings.Repeat("a", 64))} {
		if err := k.Validate(); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []domain.Key{"", "SSO", "9lives", "_x", "has space", "dash-ed", "dot.ted", "slash/ed", "ünï", domain.Key(strings.Repeat("a", 65))} {
		if err := k.Validate(); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%q: want ErrInvalid, got %v", k, err)
		}
	}
}

func TestKindValid(t *testing.T) {
	t.Parallel()
	for _, k := range []domain.Kind{domain.Flag, domain.Quota, domain.Enum} {
		if !k.Valid() {
			t.Errorf("%q is a kind", k)
		}
	}
	for _, k := range []domain.Kind{"", "FLAG", "range", "int"} {
		if k.Valid() {
			t.Errorf("%q is not a kind", k)
		}
	}
	if domain.Flag != "flag" || domain.Quota != "quota" || domain.Enum != "enum" {
		t.Error("the kinds are the strings the schema's CHECK constraints name")
	}
}

func TestKeyDeclValidate(t *testing.T) {
	t.Parallel()
	good := map[string]domain.KeyDecl{
		"a flag":                {Key: "sso", Kind: domain.Flag},
		"a quota":               {Key: "seats", Kind: domain.Quota},
		"an enum":               {Key: "tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "gold"}},
		"an enum of one member": {Key: "tier", Kind: domain.Enum, EnumOrder: []string{"only"}},
	}
	for name, d := range good {
		if err := d.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]domain.KeyDecl{
		"a bad key":                   {Key: "No Good", Kind: domain.Flag},
		"an unknown kind":             {Key: "sso", Kind: "range"},
		"an enum with no order":       {Key: "tier", Kind: domain.Enum},
		"an enum with an empty order": {Key: "tier", Kind: domain.Enum, EnumOrder: []string{}},
		"an enum with a duplicate":    {Key: "tier", Kind: domain.Enum, EnumOrder: []string{"gold", "silver", "gold"}},
		"an enum with a blank member": {Key: "tier", Kind: domain.Enum, EnumOrder: []string{"gold", " "}},
		"a flag with an order":        {Key: "sso", Kind: domain.Flag, EnumOrder: []string{"on"}},
		"a quota with an order":       {Key: "seats", Kind: domain.Quota, EnumOrder: []string{"1"}},
	}
	for name, d := range bad {
		if err := d.Validate(); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// A value is only meaningful against its declaration: the kind must match and an enum member must be ranked.
func TestKeyDeclCheckAValue(t *testing.T) {
	t.Parallel()
	if err := tierKey.Check(domain.EnumValue("silver")); err != nil {
		t.Errorf("a declared member: %v", err)
	}
	for name, v := range map[string]domain.Value{
		"an undeclared member": domain.EnumValue("platinum"),
		"the wrong kind":       domain.QuotaValue(3),
		"unset":                {},
	} {
		if err := tierKey.Check(v); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if err := quotaKey.Check(domain.UnlimitedQuota()); err != nil {
		t.Errorf("unlimited is a legal quota: %v", err)
	}
	if err := quotaKey.Check(domain.QuotaValue(-3)); !errors.Is(err, md.ErrInvalid) {
		t.Errorf("a quota below -1: want ErrInvalid, got %v", err)
	}
}

func TestKeyDeclRank(t *testing.T) {
	t.Parallel()
	for member, want := range map[string]int{"bronze": 0, "silver": 1, "gold": 2} {
		if got, ok := tierKey.Rank(member); !ok || got != want {
			t.Errorf("rank(%s) = %d, %v; want %d", member, got, ok, want)
		}
	}
	// Not ranked is not "rank -1": an absent member must be told apart from every position.
	if got, ok := tierKey.Rank("platinum"); ok {
		t.Errorf("rank(platinum) = %d, true; an undeclared member has no rank", got)
	}
}
