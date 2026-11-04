package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

var scope = md.Scope{Realm: "t1", Kind: "org", ID: "o1"}

func TestSourcesAreTheThreeLevelsAndTheirNamesAreTheWire(t *testing.T) {
	t.Parallel()
	if domain.FromDefault != "default" || domain.FromSubscription != "subscription" || domain.FromOverride != "override" {
		t.Error("the source names are what Resolve reports and support reads")
	}
	for _, s := range []domain.Source{domain.FromDefault, domain.FromSubscription, domain.FromOverride} {
		if !s.Valid() {
			t.Errorf("%q is a source", s)
		}
	}
	for _, s := range []domain.Source{"", "plan", "Override"} {
		if s.Valid() {
			t.Errorf("%q is not a source", s)
		}
	}
}

// The precedence is one order, stated once: override > subscription > default.
func TestPrecedenceIsOneStrictTotalOrder(t *testing.T) {
	t.Parallel()
	o, s, d := domain.FromOverride, domain.FromSubscription, domain.FromDefault
	for _, c := range []struct {
		hi, lo domain.Source
	}{{o, s}, {o, d}, {s, d}} {
		if !c.hi.Beats(c.lo) {
			t.Errorf("%s must beat %s", c.hi, c.lo)
		}
		if c.lo.Beats(c.hi) {
			t.Errorf("%s must not beat %s", c.lo, c.hi)
		}
	}
	for _, x := range []domain.Source{o, s, d} {
		if x.Beats(x) {
			t.Errorf("%s beats itself: two grants at one level are merged, not ranked", x)
		}
	}
	for _, bogus := range []domain.Source{"", "plan"} {
		if bogus.Beats(d) || d.Beats(bogus) {
			t.Errorf("an unknown source %q took part in the order", bogus)
		}
	}
}

func goodGrant() domain.Grant {
	return domain.Grant{Scope: scope, Key: "sso", Value: domain.FlagValue(true), Source: domain.FromSubscription}
}

func TestGrantValidate(t *testing.T) {
	t.Parallel()
	if err := goodGrant().Validate(); err != nil {
		t.Fatalf("a plain grant: %v", err)
	}
	override := goodGrant()
	override.Source, override.Reason = domain.FromOverride, "2026-Q3 enterprise pilot, approved by sales"
	if err := override.Validate(); err != nil {
		t.Errorf("an override with a reason: %v", err)
	}
	mut := func(f func(*domain.Grant)) domain.Grant { g := goodGrant(); f(&g); return g }
	for name, g := range map[string]domain.Grant{
		"a bad scope":       mut(func(g *domain.Grant) { g.Scope.Kind = "" }),
		"a bad key":         mut(func(g *domain.Grant) { g.Key = "No Good" }),
		"an unset value":    mut(func(g *domain.Grant) { g.Value = domain.Value{} }),
		"no source":         mut(func(g *domain.Grant) { g.Source = "" }),
		"an unknown source": mut(func(g *domain.Grant) { g.Source = "plan" }),
	} {
		if err := g.Validate(); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// Invariant 5: an override says who approved it and why. An unexplained override is indistinguishable from a bug.
func TestAnOverrideWithoutAReasonIsRefused(t *testing.T) {
	t.Parallel()
	for name, reason := range map[string]string{"empty": "", "spaces": "   ", "a tab and a newline": "\t\n"} {
		g := goodGrant()
		g.Source, g.Reason = domain.FromOverride, reason
		err := g.Validate()
		if !errors.Is(err, domain.ErrReasonRequired) {
			t.Errorf("%s: want ErrReasonRequired, got %v", name, err)
		}
		if !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: a missing reason is the caller's mistake (400), got %v", name, err)
		}
	}
	// The reason is required of an override only: a subscription's explanation is its plan.
	g := goodGrant()
	g.Reason = ""
	if err := g.Validate(); err != nil {
		t.Errorf("a subscription grant needs no reason: %v", err)
	}
}

func TestGrantActiveAtItsExpiry(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	g := goodGrant()
	if !g.ActiveAt(end.Add(1000 * time.Hour)) {
		t.Error("a grant with no expiry lasts as long as its source")
	}
	g.ExpiresAt = &end
	for name, c := range map[string]struct {
		now  time.Time
		want bool
	}{
		"a nanosecond before": {end.Add(-time.Nanosecond), true},
		"exactly at":          {end, false}, // the instant it ends is already over: a comp that "ends at noon" ends at noon
		"a nanosecond after":  {end.Add(time.Nanosecond), false},
	} {
		if got := g.ActiveAt(c.now); got != c.want {
			t.Errorf("%s: active = %v, want %v", name, got, c.want)
		}
	}
}
