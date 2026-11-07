package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func memAdminOps(defect func(*contracts.MemAdminOpDefects)) contracts.AdminOpFactory {
	return func(_ testing.TB, clock *contracts.FakeClock) ports.AdminOperationStore {
		m := contracts.NewMemAdminOps(clock)
		if defect != nil {
			defect(&m.Defects)
		}
		return m
	}
}

func TestMemoryAdminOpsHonourTheContract(t *testing.T) {
	t.Parallel()
	contracts.AdminOpContract(t, memAdminOps(nil))
}

// The suite is held to what it enforces: it passes an honest queue, and it rejects each deliberately broken one by name. A queue that makes
// an adjustment twice on a retry, hands one operation to two workers, or records an outcome for a worker that lost its claim moves money
// wrongly, and none may pass because the fake it was tried against was kind.
func TestAdminOpContractRejectsEveryBrokenStoreByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemAdminOpDefects)
		caughtBy []string
	}{
		{"makes a new operation for every request", func(d *contracts.MemAdminOpDefects) { d.NewOperationOnReplay = true }, []string{contracts.CaseAOOnce, contracts.CaseAOOnceTogether}},
		{"knows a key without its realm", func(d *contracts.MemAdminOpDefects) { d.IgnoresRealmInKey = true }, []string{contracts.CaseAORealmIsolation}},
		{"reads an operation from any realm", func(d *contracts.MemAdminOpDefects) { d.ReadsAcrossRealms = true }, []string{contracts.CaseAORealmIsolation}},
		{"accepts a key reused for a different request", func(d *contracts.MemAdminOpDefects) { d.AcceptsKeyReuse = true }, []string{contracts.CaseAOKeyReuse}},
		{"claims the newest request first", func(d *contracts.MemAdminOpDefects) { d.ClaimsNewestFirst = true }, []string{contracts.CaseAOOldestFirst}},
		{"claims an operation another worker holds", func(d *contracts.MemAdminOpDefects) { d.ClaimsLiveLease = true }, []string{contracts.CaseAOClaimTogether, contracts.CaseAOLeaseLapses}},
		{"never lets a dead worker's claim lapse", func(d *contracts.MemAdminOpDefects) { d.NeverLapses = true }, []string{contracts.CaseAOLeaseLapses}},
		{"does not count the attempts", func(d *contracts.MemAdminOpDefects) { d.ForgetsAttempts = true }, []string{contracts.CaseAOLeaseLapses}},
		{"records an outcome for an attempt that lost its claim", func(d *contracts.MemAdminOpDefects) { d.FinishesAnyAttempt = true }, []string{contracts.CaseAOFinishOnce}},
		{"records a second outcome", func(d *contracts.MemAdminOpDefects) { d.FinishesTwice = true }, []string{contracts.CaseAOFinishOnce}},
		{"claims a failed operation again", func(d *contracts.MemAdminOpDefects) { d.RetriesFailed = true }, []string{contracts.CaseAOFailedStays}},
		{"counts finished operations as waiting", func(d *contracts.MemAdminOpDefects) { d.CountsDoneInBacklog = true }, []string{contracts.CaseAOBacklog}},
		{"loses a result", func(d *contracts.MemAdminOpDefects) { d.DropsResult = true }, []string{contracts.CaseAOFinishOnce, contracts.CaseAOJSONRoundTrip}},
		{"accepts a lease that is not positive", func(d *contracts.MemAdminOpDefects) { d.AnyLease = true }, []string{contracts.CaseAOLeaseIsPositive}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.AdminOpCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memAdminOps(b.defect)) }); failed {
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

func TestEveryAdminOpCaseHasAUniqueNameAndTheHonestStorePassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.AdminOpCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memAdminOps(nil)) }); failed {
			t.Errorf("the honest store fails %q: %v", c.Name, said)
		}
	}
}
