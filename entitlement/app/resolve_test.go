package app

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R102: precedence is override > subscription > default, in exactly one order. The eight combinations of the three being
// present are tested on the pure resolution, with the LOWER level carrying the LARGER value everywhere: if precedence were
// "the biggest wins", or the last one read wins, one of these would pick the wrong level.

var (
	t0    = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	scope = md.Scope{Realm: "t1", Kind: "org", ID: "o1"}
)

func ptr[T any](v T) *T { return &v }

// An override that says less than the plan is the point of an override: "no despite the plan".
type level struct {
	present bool
	quota   int64
	flag    bool
	member  string
}

func inputsFor(over, sub, def level, subStatus domain.SubStatus, grace *time.Time) inputs {
	in := inputs{
		scope: scope, now: t0,
		decls: []domain.KeyDecl{
			{Key: "seats", Kind: domain.Quota}, {Key: "sso", Kind: domain.Flag},
			{Key: "tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "silver", "gold"}},
		},
	}
	ents := func(l level) []domain.Entitlement {
		return []domain.Entitlement{
			{Key: "seats", Value: domain.QuotaValue(l.quota)}, {Key: "sso", Value: domain.FlagValue(l.flag)},
			{Key: "tier", Value: domain.EnumValue(l.member)},
		}
	}
	if def.present {
		in.defaults = ents(def)
	}
	if sub.present {
		in.sub = &domain.Subscription{PlanCode: "pro", Status: subStatus, GraceUntil: grace}
		in.plan = ents(sub)
	}
	if over.present {
		for _, e := range ents(over) {
			in.overrides = append(in.overrides, domain.Grant{Scope: scope, Key: e.Key, Value: e.Value, Source: domain.FromOverride, Reason: "a deal"})
		}
	}
	return in
}

func TestPrecedenceIsExactlyOneWinnerInAllEightCombinations(t *testing.T) {
	t.Parallel()
	over := level{quota: 5, flag: false, member: "bronze"} // the exception says LESS than the plan
	sub := level{quota: 25, flag: true, member: "silver"}  // the plan says more than the exception
	def := level{quota: 1000, flag: true, member: "gold"}  // the free tier says most of all (a contrived tier: it makes "biggest wins" lose)
	for mask := 0; mask < 8; mask++ {
		o, s, d := over, sub, def
		o.present, s.present, d.present = mask&1 != 0, mask&2 != 0, mask&4 != 0
		r := resolve(inputsFor(o, s, d, domain.Active, nil))

		var want level
		var wantSource domain.Source
		switch {
		case o.present:
			want, wantSource = over, domain.FromOverride
		case s.present:
			want, wantSource = sub, domain.FromSubscription
		case d.present:
			want, wantSource = def, domain.FromDefault
		}
		if mask == 0 {
			if len(r.grants) != 0 {
				t.Errorf("none present (mask %03b): %+v granted something", mask, r.grants)
			}
			continue
		}
		if len(r.grants) != 3 {
			t.Errorf("mask %03b: want exactly one grant per key (3), got %d: %+v", mask, len(r.grants), r.grants)
		}
		for key, wantValue := range map[domain.Key]domain.Value{
			"seats": domain.QuotaValue(want.quota), "sso": domain.FlagValue(want.flag), "tier": domain.EnumValue(want.member),
		} {
			g, ok := r.grants[key]
			if !ok || g.Source != wantSource || g.Value != wantValue {
				t.Errorf("mask %03b (override=%v sub=%v default=%v) key %s: got %+v, want %s %v", mask, o.present, s.present, d.present, key, g, wantSource, wantValue)
			}
		}
	}
}

// A level that is PRESENT but does not apply is absent: a subscription that no longer holds, an override that has ended.
func TestALevelThatDoesNotApplyIsAbsentNotWinning(t *testing.T) {
	t.Parallel()
	over := level{present: true, quota: 5, flag: false, member: "bronze"}
	sub := level{present: true, quota: 25, flag: true, member: "silver"}
	def := level{present: true, quota: 1000, flag: true, member: "gold"}
	pastGrace := ptr(t0.Add(-time.Nanosecond))
	atGraceEnd := ptr(t0)

	for name, c := range map[string]struct {
		status domain.SubStatus
		grace  *time.Time
	}{
		"past_due past its grace":       {domain.PastDue, pastGrace},
		"past_due at the grace instant": {domain.PastDue, atGraceEnd},
		"past_due with no grace":        {domain.PastDue, nil},
		"canceled":                      {domain.Canceled, nil},
		"paused":                        {domain.Paused, nil},
		"incomplete":                    {domain.Incomplete, nil},
	} {
		r := resolve(inputsFor(level{}, sub, def, c.status, c.grace))
		if g := r.grants["seats"]; g.Source != domain.FromDefault || g.Value != domain.QuotaValue(1000) {
			t.Errorf("%s: the plan that does not hold still answered: %+v", name, g)
		}
		if r.plan != "" {
			t.Errorf("%s: a plan that does not hold is named as the scope's plan: %q", name, r.plan)
		}
	}
	// Held by grace: present, and it says when it ends.
	end := ptr(t0.Add(time.Nanosecond))
	r := resolve(inputsFor(level{}, sub, def, domain.PastDue, end))
	if g := r.grants["seats"]; g.Source != domain.FromSubscription || g.ExpiresAt == nil || !g.ExpiresAt.Equal(*end) {
		t.Errorf("a plan held by grace: %+v; want the plan's value ending with the window", g)
	}

	// An override that has ended is gone; one ending in the future is not.
	in := inputsFor(over, sub, def, domain.Active, nil)
	for i := range in.overrides {
		in.overrides[i].ExpiresAt = ptr(t0)
	}
	if g := resolve(in).grants["seats"]; g.Source != domain.FromSubscription {
		t.Errorf("an override at its expiry instant still won: %+v", g)
	}
	for i := range in.overrides {
		in.overrides[i].ExpiresAt = ptr(t0.Add(time.Nanosecond))
	}
	if g := resolve(in).grants["seats"]; g.Source != domain.FromOverride {
		t.Errorf("an override with a nanosecond left lost: %+v", g)
	}
}

// Two grants for one key at one level are what a migration between plans leaves behind. They are merged, never ranked against
// each other, so what the scope gets does not depend on the order the store listed them in.
func TestTwoGrantsForOneKeyAtOneLevelAreMergedNeverRanked(t *testing.T) {
	t.Parallel()
	base := inputsFor(level{}, level{}, level{}, domain.Active, nil)
	ent := func(k string, v domain.Value) domain.Entitlement {
		return domain.Entitlement{Key: domain.Key(k), Value: v}
	}
	both := func(a, b domain.Entitlement) []domain.Entitlement { return []domain.Entitlement{a, b} }
	for name, c := range map[string]struct {
		key  domain.Key
		list []domain.Entitlement
		want domain.Value
	}{
		"quotas take the larger":              {"seats", both(ent("seats", domain.QuotaValue(10)), ent("seats", domain.QuotaValue(25))), domain.QuotaValue(25)},
		"the order listed does not matter":    {"seats", both(ent("seats", domain.QuotaValue(25)), ent("seats", domain.QuotaValue(10))), domain.QuotaValue(25)},
		"unlimited is above every number":     {"seats", both(ent("seats", domain.QuotaValue(1_000_000)), ent("seats", domain.UnlimitedQuota())), domain.UnlimitedQuota()},
		"unlimited, listed first":             {"seats", both(ent("seats", domain.UnlimitedQuota()), ent("seats", domain.QuotaValue(1_000_000))), domain.UnlimitedQuota()},
		"flags OR":                            {"sso", both(ent("sso", domain.FlagValue(false)), ent("sso", domain.FlagValue(true))), domain.FlagValue(true)},
		"enums take the highest declared":     {"tier", both(ent("tier", domain.EnumValue("gold")), ent("tier", domain.EnumValue("silver"))), domain.EnumValue("gold")},
		"enums, the alphabet would say other": {"tier", both(ent("tier", domain.EnumValue("silver")), ent("tier", domain.EnumValue("gold"))), domain.EnumValue("gold")},
	} {
		in := base
		in.defaults = c.list
		if g := resolve(in).grants[c.key]; g.Value != c.want || g.Source != domain.FromDefault {
			t.Errorf("%s: got %+v, want %v from the default level", name, g, c.want)
		}
	}

	// The merged grant's reason and expiry are those of the grant that WON the merge, not the one listed last.
	in := base
	exp := ptr(t0.Add(time.Hour))
	in.overrides = []domain.Grant{
		{Scope: scope, Key: "seats", Value: domain.QuotaValue(9), Source: domain.FromOverride, Reason: "the bigger deal", ExpiresAt: exp},
		{Scope: scope, Key: "seats", Value: domain.QuotaValue(5), Source: domain.FromOverride, Reason: "the smaller deal"},
	}
	// Whichever order the store lists them in.
	for name, list := range map[string][]domain.Grant{"the winner listed first": in.overrides, "the winner listed last": {in.overrides[1], in.overrides[0]}} {
		in.overrides = list
		g := resolve(in).grants["seats"]
		if g.Value != domain.QuotaValue(9) || g.Reason != "the bigger deal" || g.ExpiresAt == nil || !g.ExpiresAt.Equal(*exp) {
			t.Errorf("merged override with %s = %+v; want the bigger deal's value, reason and expiry", name, g)
		}
	}

	// One of two that its key cannot hold is skipped, and the other stands.
	in = base
	in.defaults = both(ent("tier", domain.EnumValue("platinum")), ent("tier", domain.EnumValue("silver")))
	if g := resolve(in).grants["tier"]; g.Value != domain.EnumValue("silver") {
		t.Errorf("an unranked member beside a ranked one: %+v", g)
	}
}

// Skipped values are counted by the level they came from, so an operator can tell a bad plan row from a bad override.
func TestValuesTheirKeyCannotHoldAreCountedByLevel(t *testing.T) {
	t.Parallel()
	in := inputsFor(level{}, level{}, level{}, domain.Active, nil)
	in.defaults = []domain.Entitlement{{Key: "ghost", Value: domain.FlagValue(true)}, {Key: "seats", Value: domain.FlagValue(true)}}
	in.sub = &domain.Subscription{PlanCode: "pro", Status: domain.Active}
	in.plan = []domain.Entitlement{{Key: "tier", Value: domain.EnumValue("platinum")}}
	in.overrides = []domain.Grant{{Scope: scope, Key: "sso", Value: domain.QuotaValue(1), Source: domain.FromOverride, Reason: "x"}}
	r := resolve(in)
	if len(r.grants) != 0 {
		t.Errorf("nothing here is a legal value for its key, yet: %+v", r.grants)
	}
	if r.invalid[domain.FromDefault] != 2 || r.invalid[domain.FromSubscription] != 1 || r.invalid[domain.FromOverride] != 1 {
		t.Errorf("invalid counts by level = %v; want default 2 (an undeclared key, a wrong kind), subscription 1, override 1", r.invalid)
	}
}
