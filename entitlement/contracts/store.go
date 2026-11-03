package contracts

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The names of the cases of StoreContract.
const (
	CaseKeysAsDeclared    = "a realm's keys come back as declared, and each enum's order is the order it was declared in, not the alphabet's"
	CaseStoreRealms       = "keys, defaults, plans and overrides never cross a realm (invariant 10)"
	CaseOverrideRoundTrip = "an override is read back whole, and writing it again replaces it: one per scope and key"
	CaseOverridesScoped   = "an override belongs to its scope alone"
	CaseSubscriptionLive  = "a scope's subscription is its live one, grace window and all; a canceled one is not live"
	CasePlansCatalogue    = "the plans on sale are listed in catalogue order with what each unlocks, and a plan off sale is not listed"
	CaseOverrideNeedsWhy  = "an override that does not say who and why, or is not an override, is refused and stored nowhere (invariant 5)"
	CasePlanOffSale       = "a plan taken off sale still entitles its subscribers, and an unknown plan entitles nothing without being an error"
)

// Backend is a store under test and the means to arrange it.
type Backend struct {
	Store ports.EntitlementStore
	Seed  Seeder
}

// StoreFactory builds a fresh, empty backend for one case.
type StoreFactory func(t testing.TB) *Backend

func scopeIn(r md.Realm, id string) md.Scope { return md.Scope{Realm: r, Kind: "org", ID: id} }

func overrideOf(s md.Scope, k string, v domain.Value, reason string, exp *time.Time) domain.Grant {
	return domain.Grant{Scope: s, Key: domain.Key(k), Value: v, Source: domain.FromOverride, ExpiresAt: exp, Reason: reason}
}

// StoreCases are the subtests of StoreContract: what an EntitlementStore must keep true whatever it is built on.
func StoreCases() []Case[StoreFactory] {
	return []Case[StoreFactory]{
		{CaseKeysAsDeclared, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			b.Seed.DeclareKey(t, realm, domain.KeyDecl{Key: "region", Kind: domain.Enum, EnumOrder: []string{"us", "eu", "ap"}})
			keys, err := b.Store.Keys(Ctx(), realm)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, d := range keys {
				names = append(names, string(d.Key))
				if err := d.Validate(); err != nil {
					t.Fatalf("a declaration read back is not valid: %+v: %v", d, err)
				}
			}
			if want := []string{"daily_user_credits", "export", "region", "seats", "sso", "support_tier"}; !slices.Equal(names, want) {
				t.Fatalf("Keys = %v, want %v (ordered by key)", names, want)
			}
			for _, d := range keys {
				switch d.Key {
				case "region":
					if !slices.Equal(d.EnumOrder, []string{"us", "eu", "ap"}) {
						t.Fatalf("region's order = %v; the declared order is the ranking and it is not alphabetical", d.EnumOrder)
					}
				case "support_tier":
					if !slices.Equal(d.EnumOrder, []string{"bronze", "silver", "gold"}) {
						t.Fatalf("support_tier's order = %v", d.EnumOrder)
					}
				default:
					if len(d.EnumOrder) != 0 {
						t.Fatalf("%s is not an enum and has no order: %v", d.Key, d.EnumOrder)
					}
				}
			}
			// Declaring a key again replaces its declaration: the order is data and can be corrected.
			b.Seed.DeclareKey(t, realm, domain.KeyDecl{Key: "region", Kind: domain.Enum, EnumOrder: []string{"ap", "eu", "us"}})
			keys, _ = b.Store.Keys(Ctx(), realm)
			for _, d := range keys {
				if d.Key == "region" && !slices.Equal(d.EnumOrder, []string{"ap", "eu", "us"}) {
					t.Fatalf("a corrected declaration is not what is read: %v", d.EnumOrder)
				}
			}
		}},
		{CaseStoreRealms, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			b.Seed.DeclareKey(t, otherRealm, domain.KeyDecl{Key: "sso", Kind: domain.Flag})
			b.Seed.PutDefaults(t, realm, quotaEnt("seats", 3))
			b.Seed.PutDefaults(t, otherRealm, quotaEnt("seats", 300))
			b.Seed.PutPlan(t, realm, "pro", 1, quotaEnt("seats", 25))
			b.Seed.PutPlan(t, otherRealm, "pro", 1, quotaEnt("seats", 2500))
			b.Seed.PutPlan(t, otherRealm, "only-there", 2, quotaEnt("seats", 1))
			if err := b.Store.PutOverride(Ctx(), overrideOf(scopeIn(otherRealm, "o1"), "export", domain.FlagValue(true), "other", nil), "alice"); err != nil {
				t.Fatal(err)
			}

			if ks, _ := b.Store.Keys(Ctx(), realm); len(ks) != 5 {
				t.Fatalf("realm t1 has 5 declared keys, Keys returned %d", len(ks))
			}
			if ds, _ := b.Store.Defaults(Ctx(), realm); len(ds) != 1 || ds[0].Value != domain.QuotaValue(3) {
				t.Fatalf("Defaults(t1) = %+v, want its own seats=3", ds)
			}
			if es, _ := b.Store.PlanEntitlements(Ctx(), realm, "pro"); len(es) != 1 || es[0].Value != domain.QuotaValue(25) {
				t.Fatalf("PlanEntitlements(t1, pro) = %+v, want its own 25", es)
			}
			if es, _ := b.Store.PlanEntitlements(Ctx(), realm, "only-there"); len(es) != 0 {
				t.Fatalf("a plan of another realm entitled something here: %+v", es)
			}
			if ps, _ := b.Store.Plans(Ctx(), realm); len(ps) != 1 || ps[0].PlanCode != "pro" {
				t.Fatalf("Plans(t1) = %+v, want only its own pro", ps)
			}
			if os, _ := b.Store.Overrides(Ctx(), scopeIn(realm, "o1")); len(os) != 0 {
				t.Fatalf("another realm's override for the same id is visible here: %+v", os)
			}
			if os, _ := b.Store.Overrides(Ctx(), scopeIn(otherRealm, "o1")); len(os) != 1 {
				t.Fatalf("the other realm lost its override: %+v", os)
			}
			// The same id in another product holds a subscription; this one's scope has none.
			b.Seed.Subscribe(t, scopeIn(otherRealm, "o1"), "pro", domain.Active, nil)
			if sub, ok, _ := b.Store.Subscription(Ctx(), scopeIn(realm, "o1")); ok {
				t.Fatalf("another realm's subscription is this scope's: %+v", sub)
			}
			if _, ok, _ := b.Store.Subscription(Ctx(), scopeIn(otherRealm, "o1")); !ok {
				t.Fatal("the other realm lost its own subscription")
			}
		}},
		{CaseOverrideRoundTrip, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			s := scopeIn(realm, "o1")
			exp := T0.Add(90 * 24 * time.Hour).Truncate(time.Microsecond)
			want := overrideOf(s, "seats", domain.QuotaValue(500), "enterprise deal, approved by dana", &exp)
			if err := b.Store.PutOverride(Ctx(), want, "alice"); err != nil {
				t.Fatal(err)
			}
			got, err := b.Store.Overrides(Ctx(), s)
			if err != nil || len(got) != 1 {
				t.Fatalf("Overrides = %+v, %v; want the one written", got, err)
			}
			g := got[0]
			if g.Scope != s || g.Key != "seats" || g.Value != domain.QuotaValue(500) || g.Source != domain.FromOverride ||
				g.Reason != want.Reason || g.ExpiresAt == nil || !g.ExpiresAt.Equal(exp) {
				t.Fatalf("read back %+v, wrote %+v", g, want)
			}

			if err := b.Store.PutOverride(Ctx(), overrideOf(s, "seats", domain.UnlimitedQuota(), "raised again", nil), "alice"); err != nil {
				t.Fatal(err)
			}
			if err := b.Store.PutOverride(Ctx(), overrideOf(s, "export", domain.FlagValue(true), "pilot", nil), "alice"); err != nil {
				t.Fatal(err)
			}
			got, _ = b.Store.Overrides(Ctx(), s)
			if len(got) != 2 {
				t.Fatalf("one override per scope and key: want 2 rows (seats replaced, export added), got %+v", got)
			}
			if got[0].Key != "export" || got[1].Key != "seats" {
				t.Fatalf("overrides are ordered by key: %+v", got)
			}
			if got[1].Value != domain.UnlimitedQuota() || got[1].Reason != "raised again" || got[1].ExpiresAt != nil {
				t.Fatalf("the second write did not replace the first: %+v", got[1])
			}
		}},
		{CaseOverrideNeedsWhy, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			s := scopeIn(realm, "o1")
			subscription := overrideOf(s, "export", domain.FlagValue(true), "pilot", nil)
			subscription.Source = domain.FromSubscription
			for name, c := range map[string]struct {
				g     domain.Grant
				actor string
			}{
				"no reason":                    {overrideOf(s, "export", domain.FlagValue(true), "", nil), "alice"},
				"a reason of only spaces":      {overrideOf(s, "export", domain.FlagValue(true), "  \t ", nil), "alice"},
				"no actor":                     {overrideOf(s, "export", domain.FlagValue(true), "pilot", nil), ""},
				"an actor of only spaces":      {overrideOf(s, "export", domain.FlagValue(true), "pilot", nil), "   "},
				"a grant that is not override": {subscription, "alice"},
				"an unset value":               {overrideOf(s, "export", domain.Value{}, "pilot", nil), "alice"},
			} {
				if err := b.Store.PutOverride(Ctx(), c.g, c.actor); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("%s: PutOverride = %v; want ErrInvalid", name, err)
				}
			}
			if os, err := b.Store.Overrides(Ctx(), s); err != nil || len(os) != 0 {
				t.Fatalf("a refused override was stored: %+v, %v", os, err)
			}
			// An explained one is accepted, so the refusals above are about what they say they are about.
			if err := b.Store.PutOverride(Ctx(), overrideOf(s, "export", domain.FlagValue(true), "pilot", nil), "alice"); err != nil {
				t.Fatal(err)
			}
		}},
		{CaseOverridesScoped, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			if err := b.Store.PutOverride(Ctx(), overrideOf(scopeIn(realm, "o1"), "export", domain.FlagValue(true), "pilot", nil), "alice"); err != nil {
				t.Fatal(err)
			}
			if os, _ := b.Store.Overrides(Ctx(), scopeIn(realm, "o2")); len(os) != 0 {
				t.Fatalf("a neighbour's override: %+v", os)
			}
			if os, _ := b.Store.Overrides(Ctx(), md.Scope{Realm: realm, Kind: "user", ID: "o1"}); len(os) != 0 {
				t.Fatalf("an override for an org reached a user with the same id: %+v", os)
			}
		}},
		{CaseSubscriptionLive, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			b.Seed.PutPlan(t, realm, "pro", 1, flagEnt("sso", true))
			s := scopeIn(realm, "o1")
			if _, ok, err := b.Store.Subscription(Ctx(), s); err != nil || ok {
				t.Fatalf("a scope that never subscribed has none: ok %v, %v", ok, err)
			}
			b.Seed.Subscribe(t, s, "pro", domain.Active, nil)
			if sub, ok, err := b.Store.Subscription(Ctx(), s); err != nil || !ok || sub.PlanCode != "pro" || sub.Status != domain.Active || sub.GraceUntil != nil {
				t.Fatalf("Subscription = %+v, %v, %v", sub, ok, err)
			}
			grace := T0.Add(72 * time.Hour).Truncate(time.Microsecond)
			b.Seed.Subscribe(t, s, "pro", domain.PastDue, &grace)
			sub, ok, err := b.Store.Subscription(Ctx(), s)
			if err != nil || !ok || sub.Status != domain.PastDue || sub.GraceUntil == nil || !sub.GraceUntil.Equal(grace) {
				t.Fatalf("a past_due subscription keeps its grace window: %+v, %v, %v", sub, ok, err)
			}
			b.Seed.Subscribe(t, s, "pro", domain.Paused, nil)
			if sub, ok, _ := b.Store.Subscription(Ctx(), s); !ok || sub.Status != domain.Paused {
				t.Fatalf("a paused subscription is live (it holds nothing, which is the Entitler's call): %+v, %v", sub, ok)
			}
			b.Seed.Unsubscribe(t, s)
			if sub, ok, err := b.Store.Subscription(Ctx(), s); err != nil || ok {
				t.Fatalf("a canceled subscription is not live: %+v, %v, %v", sub, ok, err)
			}
			if _, ok, _ := b.Store.Subscription(Ctx(), scopeIn(realm, "o2")); ok {
				t.Fatal("a neighbour's subscription")
			}
		}},
		{CasePlansCatalogue, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			b.Seed.PutPlan(t, realm, "enterprise", 3, quotaEnt("seats", -1))
			b.Seed.PutPlan(t, realm, "b-team", 2, quotaEnt("seats", 25), flagEnt("sso", true))
			b.Seed.PutPlan(t, realm, "a-team", 2, quotaEnt("seats", 10))
			b.Seed.PutPlan(t, realm, "free", 1, quotaEnt("seats", 3))
			b.Seed.PutPlan(t, realm, "legacy", 0, quotaEnt("seats", 999))
			b.Seed.RetirePlan(t, realm, "legacy")
			ps, err := b.Store.Plans(Ctx(), realm)
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, p := range ps {
				codes = append(codes, p.PlanCode)
			}
			if want := []string{"free", "a-team", "b-team", "enterprise"}; !slices.Equal(codes, want) {
				t.Fatalf("Plans = %v, want %v: catalogue order, ties by code, nothing off sale", codes, want)
			}
			if len(ps[2].Entitlements) != 2 {
				t.Fatalf("an offer carries what the plan unlocks: %+v", ps[2])
			}
		}},
		{CasePlanOffSale, func(t testing.TB, mk StoreFactory) {
			b := mk(t)
			declareAll(t, b.Seed, realm)
			b.Seed.PutPlan(t, realm, "legacy", 1, flagEnt("sso", true))
			b.Seed.RetirePlan(t, realm, "legacy")
			es, err := b.Store.PlanEntitlements(Ctx(), realm, "legacy")
			if err != nil || len(es) != 1 {
				t.Fatalf("a retired plan must still entitle its subscribers: %+v, %v", es, err)
			}
			if es, err := b.Store.PlanEntitlements(Ctx(), realm, "no-such-plan"); err != nil || len(es) != 0 {
				t.Fatalf("an unknown plan has no entitlements and is not an error: %+v, %v", es, err)
			}
		}},
	}
}

// StoreContract runs every case against an implementation of ports.EntitlementStore.
func StoreContract(t *testing.T, mk StoreFactory) {
	t.Helper()
	runCases(t, StoreCases(), mk)
}
