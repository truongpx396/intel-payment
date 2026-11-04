package contracts

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// DemoScope is the scope SeedDemo gives a plan to.
var DemoScope = md.Scope{Realm: "t1", Kind: "org", ID: "o1"}

// SeedDemo puts a small, realistic product into a store, for the tests of the doors and of the binary: five capabilities, four
// plans in catalogue order, a scope on the second of them, and one override that ends. A second realm holds a different product
// under the same codes, so isolation has something to be isolated from.
//
//	sso (flag)  seats (quota)  support_tier (enum bronze < silver < gold)  export (flag)  daily_user_credits (quota)
//	free: seats 3, tier bronze · team: seats 25, tier silver · pro: sso, seats 100, tier gold · enterprise: sso, seats unlimited
//	DemoScope is on team, with export switched on until T0 + 30 days ("launch partner").
func SeedDemo(t testing.TB, m *MemStore) {
	t.Helper()
	for _, realm := range []md.Realm{"t1", "other"} {
		for _, d := range []domain.KeyDecl{
			{Key: "sso", Kind: domain.Flag}, {Key: "export", Kind: domain.Flag}, {Key: "seats", Kind: domain.Quota},
			{Key: "daily_user_credits", Kind: domain.Quota},
			{Key: "support_tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "silver", "gold"}},
		} {
			m.DeclareKey(t, realm, d)
		}
	}
	m.PutPlan(t, "t1", "free", 1, quotaEnt("seats", 3), enumEnt("support_tier", "bronze"), quotaEnt("daily_user_credits", 700))
	m.PutPlan(t, "t1", "team", 2, quotaEnt("seats", 25), enumEnt("support_tier", "silver"), quotaEnt("daily_user_credits", 2000))
	m.PutPlan(t, "t1", "pro", 3, flagEnt("sso", true), quotaEnt("seats", 100), enumEnt("support_tier", "gold"), quotaEnt("daily_user_credits", 20000))
	m.PutPlan(t, "t1", "enterprise", 4, flagEnt("sso", true), quotaEnt("seats", -1), enumEnt("support_tier", "gold"), quotaEnt("daily_user_credits", -1))
	m.Subscribe(t, DemoScope, "team", domain.Active, nil)
	end := T0.Add(30 * 24 * time.Hour)
	g := overrideOf(DemoScope, "export", domain.FlagValue(true), "launch partner", &end)
	if err := m.PutOverride(Ctx(), g, "alice"); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}
	// The other product sells the same codes for more, to a scope with the same id.
	m.PutPlan(t, "other", "team", 1, flagEnt("sso", true), quotaEnt("seats", 5000))
	m.Subscribe(t, md.Scope{Realm: "other", Kind: "org", ID: "o1"}, "team", domain.Active, nil)
}
