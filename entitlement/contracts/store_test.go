package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/contracts"
)

func memBackend(defect func(*contracts.MemDefects)) contracts.StoreFactory {
	return func(testing.TB) *contracts.Backend {
		s := contracts.NewMemStore()
		if defect != nil {
			defect(&s.Defects)
		}
		return &contracts.Backend{Store: s, Seed: s}
	}
}

func TestMemoryStoreHonoursTheStoreContract(t *testing.T) {
	t.Parallel()
	contracts.StoreContract(t, memBackend(nil))
}

// The store contract is held to what it enforces: it passes an honest store, and it rejects each deliberately broken one by
// name. A store that forgets the realm would hand one product another's plans, and must not pass because the fake it was tried
// against was kind.
func TestStoreContractRejectsEveryBrokenStoreByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemDefects)
		caughtBy []string
	}{
		{"forgets the realm", func(d *contracts.MemDefects) { d.IgnoreRealm = true }, []string{contracts.CaseStoreRealms}},
		{"sorts an enum's order alphabetically", func(d *contracts.MemDefects) { d.SortEnumOrder = true }, []string{contracts.CaseKeysAsDeclared}},
		{"lists a plan that is off sale", func(d *contracts.MemDefects) { d.ListRetiredPlans = true }, []string{contracts.CasePlansCatalogue}},
		{"adds an override instead of replacing it", func(d *contracts.MemDefects) { d.AppendOverrides = true }, []string{contracts.CaseOverrideRoundTrip}},
		{"forgets an override's expiry", func(d *contracts.MemDefects) { d.ClampExpiry = true }, []string{contracts.CaseOverrideRoundTrip}},
		{"returns no overrides", func(d *contracts.MemDefects) { d.IgnoreOverrides = true }, []string{contracts.CaseOverrideRoundTrip}},
		{"stores an override that says neither who nor why", func(d *contracts.MemDefects) { d.AcceptUnexplained = true }, []string{contracts.CaseOverrideNeedsWhy}},
		{"matches an override by id alone", func(d *contracts.MemDefects) { d.MatchByID = true }, []string{contracts.CaseOverridesScoped}},
		{"reports a canceled subscription as live", func(d *contracts.MemDefects) { d.LiveCanceled = true }, []string{contracts.CaseSubscriptionLive}},
		{"forgets the grace window", func(d *contracts.MemDefects) { d.DropGrace = true }, []string{contracts.CaseSubscriptionLive}},
		{"reports every subscription as active", func(d *contracts.MemDefects) { d.PromoteToActive = true }, []string{contracts.CaseSubscriptionLive}},
		{"stops entitling the subscribers of a plan taken off sale", func(d *contracts.MemDefects) { d.ForgetRetired = true }, []string{contracts.CasePlanOffSale}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.StoreCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memBackend(b.defect)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a store that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryStoreCaseHasAUniqueNameAndTheHonestStorePassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.StoreCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memBackend(nil)) }); failed {
			t.Errorf("the honest store fails %q: %v", c.Name, said)
		}
	}
}
