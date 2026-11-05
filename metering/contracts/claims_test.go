package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/contracts"
)

func memClaims(defect func(*contracts.MemClaimerDefects)) contracts.ClaimerFactory {
	return func(testing.TB) contracts.ClaimerBackend {
		m := contracts.NewMemClaimer()
		if defect != nil {
			defect(&m.Defects)
		}
		return contracts.ClaimerBackend{Claims: m, Kill: m.Kill}
	}
}

func TestMemoryClaimerHonoursTheContract(t *testing.T) {
	t.Parallel()
	contracts.ClaimerContract(t, memClaims(nil))
}

// The suite is held to what it enforces. A claim that excludes no one runs every tick twice; one that a dead holder keeps means the work
// it left half done is never finished.
func TestClaimerContractRejectsEveryBrokenClaimerByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemClaimerDefects)
		caughtBy []string
	}{
		{"excludes no one", func(d *contracts.MemClaimerDefects) { d.NoExclusion = true }, []string{contracts.CaseClaimExclusive, contracts.CaseClaimTogether}},
		{"is one lock for every key", func(d *contracts.MemClaimerDefects) { d.OneLockForAll = true }, []string{contracts.CaseClaimIndependent}},
		{"gives nothing back on release", func(d *contracts.MemClaimerDefects) { d.ReleaseNothing = true }, []string{contracts.CaseClaimReleased}},
		{"lets a late second release free a later holder", func(d *contracts.MemClaimerDefects) { d.ReleaseFrees = true }, []string{contracts.CaseClaimReleased}},
		{"lets a dead holder keep its claim", func(d *contracts.MemClaimerDefects) { d.DeadKeepsClaim = true }, []string{contracts.CaseClaimDeadHolder}},
		{"claims for a cancelled context", func(d *contracts.MemClaimerDefects) { d.IgnoresContext = true }, []string{contracts.CaseClaimCancelled}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.ClaimerCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memClaims(b.defect)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a claimer that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryClaimerCaseHasAUniqueNameAndTheHonestClaimerPassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.ClaimerCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memClaims(nil)) }); failed {
			t.Errorf("the honest claimer fails %q: %v", c.Name, said)
		}
	}
}
