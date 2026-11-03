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

// R102: what may be written into the overrides table is an override that says who wrote it and why. One rule, so the stores and the
// use case that fronts them cannot disagree about it.
func TestAnOverrideIsWrittenByAnActorWithAReason(t *testing.T) {
	t.Parallel()
	good := goodGrant()
	good.Source, good.Reason = domain.FromOverride, "2026-Q3 enterprise pilot, approved by sales"
	if err := good.ValidateOverride("alice"); err != nil {
		t.Fatalf("an explained override by a named actor: %v", err)
	}
	mut := func(f func(*domain.Grant)) domain.Grant { g := good; f(&g); return g }
	for name, c := range map[string]struct {
		g     domain.Grant
		actor string
		want  error
	}{
		"no reason":               {mut(func(g *domain.Grant) { g.Reason = "" }), "alice", domain.ErrReasonRequired},
		"a reason of only spaces": {mut(func(g *domain.Grant) { g.Reason = " \t" }), "alice", domain.ErrReasonRequired},
		"no actor":                {good, "", md.ErrInvalid},
		"an actor of only spaces": {good, "  ", md.ErrInvalid},
		"a subscription grant":    {mut(func(g *domain.Grant) { g.Source = domain.FromSubscription }), "alice", md.ErrInvalid},
		"a default grant":         {mut(func(g *domain.Grant) { g.Source = domain.FromDefault }), "alice", md.ErrInvalid},
		"an unset value":          {mut(func(g *domain.Grant) { g.Value = domain.Value{} }), "alice", md.ErrInvalid},
		"a scope with no kind":    {mut(func(g *domain.Grant) { g.Scope.Kind = "" }), "alice", md.ErrInvalid},
	} {
		if err := c.g.ValidateOverride(c.actor); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", name, c.want, err)
		}
	}
}
