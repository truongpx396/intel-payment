package contracts

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The names of the cases of EntitlerContract. A teeth test names the case that must reject a broken implementation, and a
// string copied between files drifts: these are the one spelling.
const (
	CaseUndeclared        = "an undeclared key is denied on every method, and denying it is not an error (invariant 1)"
	CaseOutage            = "a store outage is an error on every method and never a permit (invariant 2)"
	CaseOverrideBeats     = "an override beats the subscription, whichever way it points (precedence)"
	CaseGraceHolds        = "past_due inside the grace window keeps what the plan unlocks (payments invariant 13)"
	CaseGraceLapses       = "when the grace window closes the scope falls back to the free tier, at that instant"
	CaseNotHeld           = "a canceled, incomplete or paused subscription holds nothing"
	CaseFreeTier          = "a scope with no subscription gets the free tier and nothing else"
	CaseRefusalActionable = "a refused quota names why in words a person can act on, and the plan that would permit it (invariant 7)"
	CaseUnlimited         = "unlimited is -1 and is not a number (invariant 6)"
	CaseUpgradeAtOnce     = "an upgrade, an override and a lapse are visible at once, not after a TTL (invariant 9)"
	CaseProvenance        = "Resolve reports where every grant came from, and is what the checks enforce (invariants 5, 8)"
	CaseRealms            = "realms do not leak (invariant 10)"
	CaseExpiry            = "an override ends at its expiry instant, with nobody announcing it"
	CaseWrongQuestion     = "asking a key the wrong kind of question is an error, never a guess"
	CaseInvalidRow        = "a stored value its key cannot hold grants nothing and the lower level answers"
)

const (
	realm      md.Realm = "t1"
	otherRealm md.Realm = "other"
)

var errBoom = errors.New("connection refused")

func flagEnt(k string, on bool) domain.Entitlement {
	return domain.Entitlement{Key: domain.Key(k), Value: domain.FlagValue(on)}
}
func quotaEnt(k string, n int64) domain.Entitlement {
	return domain.Entitlement{Key: domain.Key(k), Value: domain.QuotaValue(n)}
}
func enumEnt(k, member string) domain.Entitlement {
	return domain.Entitlement{Key: domain.Key(k), Value: domain.EnumValue(member)}
}

// declareAll declares the capabilities every case uses to a realm.
func declareAll(t testing.TB, s Seeder, r md.Realm) {
	t.Helper()
	for _, d := range []domain.KeyDecl{
		{Key: "sso", Kind: domain.Flag},
		{Key: "export", Kind: domain.Flag},
		{Key: "seats", Kind: domain.Quota},
		{Key: "daily_user_credits", Kind: domain.Quota},
		{Key: "support_tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "silver", "gold"}},
	} {
		s.DeclareKey(t, r, d)
	}
}

func newWorld(t testing.TB, mk WorldFactory) *World {
	t.Helper()
	w := mk(t)
	declareAll(t, w.Seed, w.Scope.Realm)
	return w
}

// changed announces a change to the scope the way the payment layer does.
func (w *World) changed(reason string) {
	if w.Announce != nil {
		w.Announce(reason)
	}
}

func (w *World) allowed(t testing.TB, k domain.Key) bool {
	t.Helper()
	ok, err := w.Entitler.Allowed(Ctx(), w.Scope, k)
	if err != nil {
		t.Fatalf("Allowed(%s): %v", k, err)
	}
	return ok
}

func (w *World) override(t testing.TB, k string, v domain.Value, reason string, expires *time.Time) {
	t.Helper()
	g := domain.Grant{Scope: w.Scope, Key: domain.Key(k), Value: v, Source: domain.FromOverride, ExpiresAt: expires, Reason: reason}
	if err := w.Store.PutOverride(Ctx(), g, "alice"); err != nil {
		t.Fatalf("PutOverride(%s): %v", k, err)
	}
}

// EntitlerCases are the subtests of EntitlerContract: the skeleton of entitlement-ports.md § Contract-test skeleton, made
// to run against any Entitler, plus declared keys, enum order, and the expiry of an override.
func EntitlerCases() []Case[WorldFactory] {
	return []Case[WorldFactory]{
		{CaseUndeclared, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			// A stored entitlement for a key nobody declared is what a typo or a half-finished migration leaves behind.
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("ghost", true))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			for _, k := range []domain.Key{"no_such_capability", "ghost"} {
				if w.allowed(t, k) {
					t.Fatalf("Allowed(%s): an undeclared key must fail closed", k)
				}
				if _, ok, err := w.Entitler.Quota(Ctx(), w.Scope, k); err != nil || ok {
					t.Fatalf("Quota(%s) = ok %v, err %v; want !ok and no error", k, ok, err)
				}
				if _, ok, err := w.Entitler.Choice(Ctx(), w.Scope, k); err != nil || ok {
					t.Fatalf("Choice(%s) = ok %v, err %v; want !ok and no error", k, ok, err)
				}
				d, err := w.Entitler.Consume(Ctx(), w.Scope, k, 0, 1)
				if err != nil || d.Allowed || d.Reason == "" {
					t.Fatalf("Consume(%s) = %+v, %v; want a refusal that says why, and no error", k, d, err)
				}
			}
			gs, err := w.Entitler.Resolve(Ctx(), w.Scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range gs {
				if g.Key == "ghost" {
					t.Fatalf("Resolve reports a grant for a key the realm never declared: %+v", g)
				}
			}
		}},
		{CaseOutage, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true), quotaEnt("seats", 25), enumEnt("support_tier", "gold"))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			w.Faults.BreakWith(errBoom)

			if ok, err := w.Entitler.Allowed(Ctx(), w.Scope, "sso"); err == nil || ok || !errors.Is(err, errBoom) || !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("Allowed = %v, %v; want an error that is ErrUnavailable and keeps its cause, and no permit (invariant 2)", ok, err)
			}
			if _, ok, err := w.Entitler.Quota(Ctx(), w.Scope, "seats"); err == nil || ok {
				t.Fatalf("Quota = ok %v, err %v; want an error", ok, err)
			}
			if _, ok, err := w.Entitler.Choice(Ctx(), w.Scope, "support_tier"); err == nil || ok {
				t.Fatalf("Choice = ok %v, err %v; want an error", ok, err)
			}
			if d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", 0, 1); err == nil || d.Allowed {
				t.Fatalf("Consume = %+v, %v; want an error and no permit", d, err)
			}
			if _, err := w.Entitler.Resolve(Ctx(), w.Scope); err == nil {
				t.Fatal("Resolve answered during an outage")
			}

			w.Faults.Heal()
			if !w.allowed(t, "sso") {
				t.Fatal("the store is back and the plan unlocks sso: the outage must not outlive itself")
			}

			// A store that is only PARTLY down is the outage that tempts an answer from what was read. Each read the answer
			// depends on, failing alone, fails the answer: an overrides table that cannot be read could be hiding the override
			// that says no.
			w.override(t, "sso", domain.FlagValue(false), "suspended by finance", nil)
			for _, method := range []string{"Keys", "Defaults", "Subscription", "PlanEntitlements", "Overrides"} {
				w.Faults.BreakOnly(errBoom, method)
				if ok, err := w.Entitler.Allowed(Ctx(), w.Scope, "sso"); err == nil || ok || !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("with only %s failing, Allowed = %v, %v; want an error and no permit (invariant 2)", method, ok, err)
				}
				if d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", 0, 1); err == nil || d.Allowed {
					t.Fatalf("with only %s failing, Consume = %+v, %v; want an error and no permit", method, d, err)
				}
			}
			w.Faults.Heal()

			// The catalogue is only where a refusal looks for a way out: when it cannot be read the refusal stands, without a path.
			w.Seed.PutPlan(t, realm, "team", 2, quotaEnt("seats", 5))
			w.Seed.Subscribe(t, w.Scope, "team", domain.Active, nil)
			w.Faults.BreakOnly(errBoom, "Plans")
			if d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", 5, 1); err != nil || d.Allowed || d.Reason == "" || d.UpgradePath != "" {
				t.Fatalf("with only Plans failing, a seat past the limit = %+v, %v; want a refusal that says why and names no plan", d, err)
			}
			if d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", 4, 1); err != nil || !d.Allowed {
				t.Fatalf("with only Plans failing, a seat within the limit = %+v, %v; want it allowed: the catalogue is not needed to say yes", d, err)
			}
		}},
		{CaseOverrideBeats, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "free", 1, flagEnt("sso", false))
			w.Seed.Subscribe(t, w.Scope, "free", domain.Active, nil)
			w.override(t, "sso", domain.FlagValue(true), "2026-Q3 enterprise pilot, approved by sales", nil)
			w.changed("override")
			if !w.allowed(t, "sso") {
				t.Fatal("a negotiated override must survive the plan: yes despite the plan")
			}

			w.Seed.PutPlan(t, realm, "pro", 2, flagEnt("sso", true))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			w.override(t, "sso", domain.FlagValue(false), "contract dispute, suspended by finance", nil)
			w.changed("override")
			if w.allowed(t, "sso") {
				t.Fatal("an override that says no must beat a plan that says yes: no despite the plan")
			}
		}},
		{CaseGraceHolds, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true), quotaEnt("seats", 25))
			w.Seed.PutDefaults(t, realm, flagEnt("sso", false), quotaEnt("seats", 3))
			grace := T0.Add(72 * time.Hour)
			w.Seed.Subscribe(t, w.Scope, "pro", domain.PastDue, &grace)
			if !w.allowed(t, "sso") {
				t.Fatal("a transient decline must not revoke inside the grace window")
			}
			if limit, ok, err := w.Entitler.Quota(Ctx(), w.Scope, "seats"); err != nil || !ok || limit != 25 {
				t.Fatalf("Quota(seats) = %d, %v, %v inside grace; want the plan's 25", limit, ok, err)
			}
			gs, err := w.Entitler.Resolve(Ctx(), w.Scope)
			if err != nil {
				t.Fatal(err)
			}
			g := findGrant(t, gs, "sso")
			if g.Source != domain.FromSubscription || g.ExpiresAt == nil || !g.ExpiresAt.Equal(grace) {
				t.Fatalf("a grant held by grace must say it ends with the grace window; got %+v", g)
			}
		}},
		{CaseGraceLapses, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true))
			w.Seed.PutDefaults(t, realm, flagEnt("sso", false))
			grace := T0.Add(time.Hour)
			w.Seed.Subscribe(t, w.Scope, "pro", domain.PastDue, &grace)
			if !w.allowed(t, "sso") {
				t.Fatal("inside grace the plan holds")
			}
			w.Clock.Advance(time.Hour - time.Nanosecond)
			if !w.allowed(t, "sso") {
				t.Fatal("a nanosecond before the window closes the plan still holds")
			}
			w.Clock.Advance(time.Nanosecond)
			w.changed("grace_expired")
			if w.allowed(t, "sso") {
				t.Fatal("entitlements must lapse the instant the grace window closes")
			}
			w2 := newWorld(t, mk)
			w2.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true))
			w2.Seed.PutDefaults(t, realm, flagEnt("sso", false))
			past := T0.Add(-time.Hour)
			w2.Seed.Subscribe(t, w2.Scope, "pro", domain.PastDue, &past)
			if w2.allowed(t, "sso") {
				t.Fatal("a window that closed an hour ago holds nothing")
			}
		}},
		{CaseNotHeld, func(t testing.TB, mk WorldFactory) {
			for _, status := range []domain.SubStatus{domain.Canceled, domain.Incomplete, domain.Paused} {
				w := newWorld(t, mk)
				w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true))
				w.Seed.PutDefaults(t, realm, flagEnt("sso", false))
				w.Seed.Subscribe(t, w.Scope, "pro", status, nil)
				if w.allowed(t, "sso") {
					t.Fatalf("a %s subscription entitled its scope", status)
				}
			}
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true))
			w.Seed.PutDefaults(t, realm, flagEnt("sso", false))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			w.Seed.Unsubscribe(t, w.Scope)
			w.changed("subscription")
			if w.allowed(t, "sso") {
				t.Fatal("a subscription that was canceled still entitled its scope")
			}
		}},
		{CaseFreeTier, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutDefaults(t, realm, flagEnt("export", false), quotaEnt("seats", 3), enumEnt("support_tier", "bronze"))
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true), quotaEnt("seats", 25))
			if w.allowed(t, "sso") || w.allowed(t, "export") {
				t.Fatal("the free tier granted what only a plan unlocks")
			}
			if limit, ok, err := w.Entitler.Quota(Ctx(), w.Scope, "seats"); err != nil || !ok || limit != 3 {
				t.Fatalf("Quota(seats) = %d, %v, %v; want the free tier's 3", limit, ok, err)
			}
			if m, ok, err := w.Entitler.Choice(Ctx(), w.Scope, "support_tier"); err != nil || !ok || m != "bronze" {
				t.Fatalf("Choice(support_tier) = %q, %v, %v; want the free tier's bronze", m, ok, err)
			}
			if _, ok, err := w.Entitler.Quota(Ctx(), w.Scope, "daily_user_credits"); err != nil || ok {
				t.Fatalf("Quota(daily_user_credits) = ok %v, err %v; nothing grants it, so it is !ok and not an error", ok, err)
			}
		}},
		{CaseRefusalActionable, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "team", 1, quotaEnt("seats", 25))
			w.Seed.PutPlan(t, realm, "business", 2, quotaEnt("seats", 100))
			w.Seed.PutPlan(t, realm, "enterprise", 3, quotaEnt("seats", -1))
			w.Seed.Subscribe(t, w.Scope, "team", domain.Active, nil)

			d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", 25, 1)
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed {
				t.Fatal("the 26th seat on a 25-seat plan must be refused")
			}
			if d.Reason == "" || !strings.Contains(d.Reason, "25") || !strings.Contains(d.Reason, "seats") {
				t.Fatalf("a refusal must say what the plan includes and what is used, in words: %q (invariant 7)", d.Reason)
			}
			if d.UpgradePath != "business" {
				t.Fatalf("UpgradePath = %q; the first plan in the catalogue that permits it is business", d.UpgradePath)
			}
			if d.Limit != 25 || d.Current != 25 || d.Remaining != 0 {
				t.Fatalf("a refusal reports the numbers it judged by: %+v", d)
			}
			// Business cannot take 101 seats: only the plan with no ceiling can.
			if d, _ := w.Entitler.Consume(Ctx(), w.Scope, "seats", 99, 2); d.Allowed || d.UpgradePath != "enterprise" {
				t.Fatalf("101 seats: %+v; only enterprise permits it", d)
			}
			// What is allowed carries no refusal.
			if d, _ := w.Entitler.Consume(Ctx(), w.Scope, "seats", 24, 1); !d.Allowed || d.Reason != "" || d.UpgradePath != "" || d.Remaining != 0 {
				t.Fatalf("the 25th seat of 25: %+v; want allowed, nothing to explain, no room left", d)
			}
			// A limit that is an OVERRIDE outranks every plan: upgrading would not lift it, so no plan is named.
			w.override(t, "seats", domain.QuotaValue(5), "capped while the audit runs, approved by dana", nil)
			w.changed("override")
			d, err = w.Entitler.Consume(Ctx(), w.Scope, "seats", 5, 1)
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed || d.UpgradePath != "" || !strings.Contains(d.Reason, "5") || !strings.Contains(d.Reason, "account") {
				t.Fatalf("a seat past an override's 5: %+v; want a refusal that says the account is limited and names no plan", d)
			}
			// With nowhere better to go, the refusal still explains itself and does not invent a path.
			w2 := newWorld(t, mk)
			w2.Seed.PutPlan(t, realm, "team", 1, quotaEnt("seats", 25))
			w2.Seed.Subscribe(t, w2.Scope, "team", domain.Active, nil)
			if d, _ := w2.Entitler.Consume(Ctx(), w2.Scope, "seats", 25, 1); d.Allowed || d.Reason == "" || d.UpgradePath != "" {
				t.Fatalf("no plan permits it: %+v; want a reason and no upgrade path", d)
			}
			// A key the plan does not include at all is a refusal with a way out too.
			w3 := newWorld(t, mk)
			w3.Seed.PutPlan(t, realm, "free", 1)
			w3.Seed.PutPlan(t, realm, "team", 2, quotaEnt("seats", 25))
			w3.Seed.Subscribe(t, w3.Scope, "free", domain.Active, nil)
			if d, _ := w3.Entitler.Consume(Ctx(), w3.Scope, "seats", 0, 1); d.Allowed || d.Reason == "" || d.UpgradePath != "team" {
				t.Fatalf("a plan with no seats quota: %+v; want a refusal that names team", d)
			}
		}},
		{CaseUnlimited, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "enterprise", 1, quotaEnt("seats", -1))
			w.Seed.Subscribe(t, w.Scope, "enterprise", domain.Active, nil)
			d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", 1_000_000, 1)
			if err != nil {
				t.Fatal(err)
			}
			if !d.Allowed || d.Remaining != -1 || d.Limit != -1 {
				t.Fatalf("unlimited mishandled: %+v", d)
			}
			if limit, ok, err := w.Entitler.Quota(Ctx(), w.Scope, "seats"); err != nil || !ok || limit != -1 {
				t.Fatalf("Quota = %d, %v, %v; want -1 and ok: unlimited is a grant, not an absence", limit, ok, err)
			}
		}},
		{CaseUpgradeAtOnce, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "free", 1, flagEnt("sso", false), flagEnt("export", false))
			w.Seed.PutPlan(t, realm, "pro", 2, flagEnt("sso", true), flagEnt("export", false))
			w.Seed.Subscribe(t, w.Scope, "free", domain.Active, nil)
			if w.allowed(t, "sso") { // warm whatever the implementation keeps
				t.Fatal("free does not include sso")
			}
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil) // the upgrade
			w.changed("subscription")
			if !w.allowed(t, "sso") {
				t.Fatal("a customer who just paid must not wait out a cache TTL (invariant 9)")
			}

			if w.allowed(t, "export") {
				t.Fatal("pro does not include export")
			}
			w.override(t, "export", domain.FlagValue(true), "comp for the outage", nil)
			w.changed("override")
			if !w.allowed(t, "export") {
				t.Fatal("an override written must be visible at once")
			}
		}},
		{CaseProvenance, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutDefaults(t, realm, quotaEnt("seats", 3), quotaEnt("daily_user_credits", 1000), enumEnt("support_tier", "bronze"))
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true), quotaEnt("seats", 25), enumEnt("support_tier", "silver"))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			end := T0.Add(30 * 24 * time.Hour)
			w.override(t, "daily_user_credits", domain.QuotaValue(50_000), "negotiated at renewal, approved by dana", &end)
			w.override(t, "export", domain.FlagValue(true), "pilot", nil)

			gs, err := w.Entitler.Resolve(Ctx(), w.Scope)
			if err != nil {
				t.Fatal(err)
			}
			want := map[domain.Key]domain.Source{
				"daily_user_credits": domain.FromOverride, "export": domain.FromOverride,
				"seats": domain.FromSubscription, "sso": domain.FromSubscription, "support_tier": domain.FromSubscription,
			}
			if len(gs) != len(want) {
				t.Fatalf("Resolve returned %d grants, want %d (one per key granted): %+v", len(gs), len(want), gs)
			}
			keys := make([]string, len(gs))
			for i, g := range gs {
				keys[i] = string(g.Key)
				if g.Source == "" {
					t.Fatalf("grant %q has no source — unanswerable in support", g.Key)
				}
				if g.Source != want[g.Key] {
					t.Fatalf("grant %q came from %q, want %q", g.Key, g.Source, want[g.Key])
				}
				if g.Source == domain.FromOverride && g.Reason == "" {
					t.Fatalf("an override must say why: %+v", g)
				}
				if g.Source != domain.FromOverride && g.Reason != "" {
					t.Fatalf("only an override carries a reason: %+v", g)
				}
				if g.Scope != w.Scope {
					t.Fatalf("grant %q is for scope %+v, not the one asked about", g.Key, g.Scope)
				}
			}
			if !slices.IsSorted(keys) {
				t.Fatalf("Resolve is ordered by key so that a plan card is stable: %v", keys)
			}
			credits := findGrant(t, gs, "daily_user_credits")
			if credits.ExpiresAt == nil || !credits.ExpiresAt.Equal(end) || credits.Reason != "negotiated at renewal, approved by dana" {
				t.Fatalf("an override's expiry and reason survive Resolve: %+v", credits)
			}

			// Invariant 8: the UI reads the resolution the server enforces. Every grant Resolve reports is what the checks answer.
			for _, g := range gs {
				switch g.Value.Kind {
				case domain.Flag:
					if got := w.allowed(t, g.Key); got != g.Value.Bool {
						t.Fatalf("Resolve says %s = %v and Allowed says %v", g.Key, g.Value.Bool, got)
					}
				case domain.Quota:
					if limit, ok, err := w.Entitler.Quota(Ctx(), w.Scope, g.Key); err != nil || !ok || limit != g.Value.Int {
						t.Fatalf("Resolve says %s = %d and Quota says %d, %v, %v", g.Key, g.Value.Int, limit, ok, err)
					}
				case domain.Enum:
					if m, ok, err := w.Entitler.Choice(Ctx(), w.Scope, g.Key); err != nil || !ok || m != g.Value.Text {
						t.Fatalf("Resolve says %s = %q and Choice says %q, %v, %v", g.Key, g.Value.Text, m, ok, err)
					}
				}
			}
		}},
		{CaseRealms, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			declareAll(t, w.Seed, otherRealm)
			other := md.Scope{Realm: otherRealm, Kind: w.Scope.Kind, ID: w.Scope.ID} // the same id, in another product

			// Another product sells a plan with sso, gives it away on its free tier, and has granted this id an override.
			w.Seed.PutPlan(t, otherRealm, "pro", 1, flagEnt("sso", true), quotaEnt("seats", 500))
			w.Seed.PutDefaults(t, otherRealm, flagEnt("sso", true), quotaEnt("seats", 500))
			w.Seed.Subscribe(t, other, "pro", domain.Active, nil)
			ov := domain.Grant{Scope: other, Key: "export", Value: domain.FlagValue(true), Source: domain.FromOverride, Reason: "other realm's deal"}
			if err := w.Store.PutOverride(Ctx(), ov, "alice"); err != nil {
				t.Fatal(err)
			}
			// This product has the same codes, with less.
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", false), quotaEnt("seats", 5))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)

			if w.allowed(t, "sso") || w.allowed(t, "export") {
				t.Fatal("another realm's plan, free tier or override granted a capability here")
			}
			if limit, _, err := w.Entitler.Quota(Ctx(), w.Scope, "seats"); err != nil || limit != 5 {
				t.Fatalf("Quota(seats) = %d, %v; want this realm's 5, not the other's 500", limit, err)
			}
			if d, _ := w.Entitler.Consume(Ctx(), w.Scope, "seats", 5, 1); d.Allowed {
				t.Fatalf("a seat past this realm's 5 was admitted: %+v", d)
			}
			gs, err := w.Entitler.Resolve(Ctx(), w.Scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range gs {
				if g.Scope.Realm != realm || (g.Source == domain.FromOverride) {
					t.Fatalf("Resolve reports a grant from elsewhere: %+v", g)
				}
			}
			// And the other realm still has what is its own.
			if ok, err := w.Entitler.Allowed(Ctx(), other, "sso"); err != nil || !ok {
				t.Fatalf("the other realm lost its own entitlement: %v, %v", ok, err)
			}
		}},
		{CaseExpiry, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutDefaults(t, realm, flagEnt("export", false))
			end := T0.Add(time.Hour)
			w.override(t, "export", domain.FlagValue(true), "a one-hour trial of export", &end)
			if !w.allowed(t, "export") {
				t.Fatal("an unexpired override applies")
			}
			w.Clock.Advance(time.Hour - time.Nanosecond)
			if !w.allowed(t, "export") {
				t.Fatal("a nanosecond before it ends, an override still applies")
			}
			w.Clock.Advance(time.Nanosecond) // nobody announces this: no tick expires an override
			if w.allowed(t, "export") {
				t.Fatal("an override applied after its expiry instant")
			}
		}},
		{CaseWrongQuestion, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true), quotaEnt("seats", 25), enumEnt("support_tier", "gold"))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			if ok, err := w.Entitler.Allowed(Ctx(), w.Scope, "seats"); ok || !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Allowed(a quota) = %v, %v; want false and ErrInvalid", ok, err)
			}
			if _, ok, err := w.Entitler.Quota(Ctx(), w.Scope, "sso"); ok || !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Quota(a flag) = ok %v, %v; want !ok and ErrInvalid", ok, err)
			}
			if _, ok, err := w.Entitler.Choice(Ctx(), w.Scope, "seats"); ok || !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Choice(a quota) = ok %v, %v; want !ok and ErrInvalid", ok, err)
			}
			if d, err := w.Entitler.Consume(Ctx(), w.Scope, "sso", 0, 1); d.Allowed || !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Consume(a flag) = %+v, %v; want a refusal and ErrInvalid", d, err)
			}
			if d, err := w.Entitler.Consume(Ctx(), w.Scope, "seats", -1, 1); d.Allowed || !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Consume(a negative count) = %+v, %v; want a refusal and ErrInvalid", d, err)
			}
		}},
		{CaseInvalidRow, func(t testing.TB, mk WorldFactory) {
			w := newWorld(t, mk)
			w.Seed.PutDefaults(t, realm, flagEnt("sso", false), enumEnt("support_tier", "bronze"))
			// The plan says "platinum", which the key never declared, and gives sso a number.
			w.Seed.PutPlan(t, realm, "pro", 1, enumEnt("support_tier", "platinum"), quotaEnt("sso", 1))
			w.Seed.Subscribe(t, w.Scope, "pro", domain.Active, nil)
			if w.allowed(t, "sso") {
				t.Fatal("a quota stored under a flag key was read as permission")
			}
			if m, ok, err := w.Entitler.Choice(Ctx(), w.Scope, "support_tier"); err != nil || !ok || m != "bronze" {
				t.Fatalf("Choice = %q, %v, %v; the undeclared member must grant nothing and the free tier answers", m, ok, err)
			}
			gs, err := w.Entitler.Resolve(Ctx(), w.Scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range gs {
				if g.Value.Text == "platinum" || (g.Key == "sso" && g.Value.Kind != domain.Flag) {
					t.Fatalf("Resolve reports a value its key cannot hold: %+v", g)
				}
			}
		}},
	}
}

func findGrant(t testing.TB, gs []domain.Grant, k domain.Key) domain.Grant {
	t.Helper()
	for _, g := range gs {
		if g.Key == k {
			return g
		}
	}
	t.Fatalf("Resolve has no grant for %q: %+v", k, gs)
	return domain.Grant{}
}

// EntitlerContract runs every case against an implementation of ports.Entitler. mk builds a fresh world for each, so the
// cases run in parallel and none depends on another's side effects.
func EntitlerContract(t *testing.T, mk WorldFactory) {
	t.Helper()
	runCases(t, EntitlerCases(), mk)
}
