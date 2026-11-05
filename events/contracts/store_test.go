package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events/contracts"
)

func mem(defect func(*contracts.Defects)) contracts.StoreFactory {
	return func(_ testing.TB, clock *contracts.Clock) *contracts.Backend {
		s := contracts.NewMemStore(clock)
		if defect != nil {
			defect(&s.Defects)
		}
		return &contracts.Backend{Store: s, Seed: s}
	}
}

func TestMemoryStoreHonoursTheStoreContract(t *testing.T) {
	t.Parallel()
	contracts.StoreContract(t, mem(nil))
}

// The suite is held to what it enforces: it passes an honest store and rejects each deliberately broken one by name. A store that hands
// an endpoint another realm's events leaks one product's payments to another, and must not pass because the fake it was tried on was kind.
func TestStoreContractRejectsEveryBrokenStoreByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.Defects)
		caughtBy []string
	}{
		{"records an event once per call, not once per source", func(d *contracts.Defects) { d.AppendEveryCall = true }, []string{contracts.CaseAppendOnce, contracts.CaseAppendTogether}},
		{"issues ids without looking at the last", func(d *contracts.Defects) { d.UnorderedIDs = true }, []string{contracts.CaseIDsIncrease, contracts.CaseFeedWhileWriting}},
		{"serves every realm to the feed", func(d *contracts.Defects) { d.FeedIgnoresRealm = true }, []string{contracts.CaseFeedScoped}},
		{"ignores the feed's type filter", func(d *contracts.Defects) { d.FeedIgnoresTypes = true }, []string{contracts.CaseFeedScoped}},
		{"includes the cursor in the page after it", func(d *contracts.Defects) { d.FeedInclusive = true }, []string{contracts.CaseFeedExactlyOnce}},
		{"serves the feed newest first", func(d *contracts.Defects) { d.FeedBackwards = true }, []string{contracts.CaseFeedExactlyOnce, contracts.CaseIDsIncrease}},
		{"owes an inactive endpoint", func(d *contracts.Defects) { d.FanOutInactive = true }, []string{contracts.CaseFanOut}},
		{"owes an endpoint another realm's events", func(d *contracts.Defects) { d.FanOutAnyRealm = true }, []string{contracts.CaseFanOut}},
		{"owes an endpoint types it did not subscribe to", func(d *contracts.Defects) { d.FanOutAnyType = true }, []string{contracts.CaseFanOut}},
		{"hands a delivery to every claimant", func(d *contracts.Defects) { d.NoLease = true }, []string{contracts.CaseLease}},
		{"owes a delivered delivery again", func(d *contracts.Defects) { d.DeliveredStaysDue = true }, []string{contracts.CaseDeliveredFinal}},
		{"owes a dead delivery again", func(d *contracts.Defects) { d.DeadStaysDue = true }, []string{contracts.CaseDeadIsCounted}},
		{"re-drives a delivery that is not dead", func(d *contracts.Defects) { d.RedeliverAnything = true }, []string{contracts.CaseDeadIsCounted}},
		{"leaves a delivery pending past the window", func(d *contracts.Defects) { d.ExpireNothing = true }, []string{contracts.CaseExpireStale}},
		{"purges an event something is still owed for", func(d *contracts.Defects) { d.PurgeEverything = true }, []string{contracts.CasePurge}},
		{"never purges", func(d *contracts.Defects) { d.PurgeNothing = true }, []string{contracts.CasePurge}},
		{"does not keep an event's data", func(d *contracts.Defects) { d.DropData = true }, []string{contracts.CaseKeepsWhatItGot, contracts.CaseDueIsComplete}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.Cases() {
				if failed, _ := contracts.Probe(60*time.Second, func(t testing.TB) { c.Run(t, mem(b.defect)) }); failed {
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

func TestEveryCaseHasAUniqueNameAndTheHonestStorePassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.Cases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(60*time.Second, func(t testing.TB) { c.Run(t, mem(nil)) }); failed {
			t.Errorf("the honest store fails %q: %v", c.Name, said)
		}
	}
}
