package domain_test

import (
	"errors"
	"math"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R100 (entitlement invariant 6): -1 means UNLIMITED. It is handled explicitly — never compared as a number, never
// coerced to a large one, never confused with "unset" or with zero. These tests pin the four things that must stay
// distinct: unset, a ceiling of zero, a ceiling of n, and no ceiling at all.

func TestUnsetUnlimitedAndZeroAreThreeDifferentThings(t *testing.T) {
	t.Parallel()
	unset, zero, unlimited := domain.Value{}, domain.QuotaValue(0), domain.UnlimitedQuota()

	if unset.IsSet() {
		t.Error("the zero Value must read as unset")
	}
	if !zero.IsSet() || !unlimited.IsSet() {
		t.Error("a ceiling of zero and an unlimited ceiling are both SET")
	}
	if unset.IsUnlimited() || zero.IsUnlimited() {
		t.Error("only -1 on a quota is unlimited: unset and zero are not")
	}
	if !unlimited.IsUnlimited() {
		t.Error("UnlimitedQuota must be unlimited")
	}
	if unlimited != domain.QuotaValue(domain.Unlimited) {
		t.Error("UnlimitedQuota and QuotaValue(Unlimited) must be the same value")
	}
	if unset == zero || unset == unlimited || zero == unlimited {
		t.Errorf("unset, zero and unlimited collapsed into one another: %+v %+v %+v", unset, zero, unlimited)
	}
	if domain.Unlimited != -1 {
		t.Errorf("unlimited is -1 on the wire and in the books, got %d", domain.Unlimited)
	}
}

func TestOnlyAQuotaCanBeUnlimited(t *testing.T) {
	t.Parallel()
	// An Int of -1 on a flag or an enum is a malformed value, not an unlimited one.
	if (domain.Value{Kind: domain.Flag, Int: -1}).IsUnlimited() {
		t.Error("a flag is never unlimited")
	}
	if (domain.Value{Kind: domain.Enum, Text: "gold", Int: -1}).IsUnlimited() {
		t.Error("an enum is never unlimited")
	}
}

func TestValueValidate(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]domain.Value{
		"a flag, on":            domain.FlagValue(true),
		"a flag, off":           domain.FlagValue(false),
		"a ceiling of zero":     domain.QuotaValue(0),
		"a ceiling of 25":       domain.QuotaValue(25),
		"the largest ceiling":   domain.QuotaValue(math.MaxInt64),
		"unlimited":             domain.UnlimitedQuota(),
		"an enum member":        domain.EnumValue("gold"),
		"an enum with a space ": domain.EnumValue("on call"),
	} {
		if err := v.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, v := range map[string]domain.Value{
		"unset: not a grant":                    {},
		"an unknown kind":                       {Kind: "range", Int: 3},
		"a quota below -1":                      domain.QuotaValue(-2),
		"a quota at the smallest int64":         domain.QuotaValue(math.MinInt64),
		"a flag carrying a number":              {Kind: domain.Flag, Bool: true, Int: 3},
		"a flag carrying text":                  {Kind: domain.Flag, Text: "x"},
		"a quota carrying a bool":               {Kind: domain.Quota, Int: 3, Bool: true},
		"a quota carrying text":                 {Kind: domain.Quota, Int: 3, Text: "x"},
		"an enum carrying a number":             {Kind: domain.Enum, Text: "gold", Int: 1},
		"an enum carrying a bool":               {Kind: domain.Enum, Text: "gold", Bool: true},
		"an enum with no member":                domain.EnumValue(""),
		"an enum whose member is only a spaces": domain.EnumValue("  "),
	} {
		err := v.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, md.ErrInvalid) || !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want the engine's ErrInvalid (400), got %v", name, err)
		}
	}
}

func TestValueString(t *testing.T) {
	t.Parallel()
	for want, v := range map[string]domain.Value{
		"true":      domain.FlagValue(true),
		"false":     domain.FlagValue(false),
		"25":        domain.QuotaValue(25),
		"0":         domain.QuotaValue(0),
		"unlimited": domain.UnlimitedQuota(),
		"gold":      domain.EnumValue("gold"),
		"unset":     {},
	} {
		if got := v.String(); got != want {
			t.Errorf("%+v rendered as %q, want %q", v, got, want)
		}
	}
	// What a person reads must not let -1 pass for a number.
	if domain.UnlimitedQuota().String() == "-1" {
		t.Error("unlimited must read as unlimited, not as -1")
	}
}
