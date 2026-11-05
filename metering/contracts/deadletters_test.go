package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func memDeadLetters(defect func(*contracts.MemDeadLetterDefects)) contracts.DeadLetterFactory {
	return func(_ testing.TB, clock *contracts.FakeClock) ports.DeadLetterStore {
		m := contracts.NewMemDeadLetters(clock)
		if defect != nil {
			defect(&m.Defects)
		}
		return m
	}
}

func TestMemoryDeadLettersHonourTheContract(t *testing.T) {
	t.Parallel()
	contracts.DeadLetterContract(t, memDeadLetters(nil))
}

// The suite is held to what it enforces: it passes an honest store, and it rejects each deliberately broken one by name. A store
// that parks the same envelope twice pages twice; one that forgets a replay re-publishes a message an operator already replayed;
// one that deletes on replay loses the only record of what failed. None may pass because the fake it was tried against was kind.
func TestDeadLetterContractRejectsEveryBrokenStoreByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemDeadLetterDefects)
		caughtBy []string
	}{
		{"parks every delivery of an envelope as a new letter", func(d *contracts.MemDeadLetterDefects) { d.ParkEveryCall = true }, []string{contracts.CaseDLParkedOnce, contracts.CaseDLParkedOnceTogether}},
		{"knows a letter by its source id alone", func(d *contracts.MemDeadLetterDefects) { d.KeyOnSourceAlone = true }, []string{contracts.CaseDLIdentity}},
		{"rewrites a letter when it is parked again", func(d *contracts.MemDeadLetterDefects) { d.OverwriteOnRepark = true }, []string{contracts.CaseDLParkedOnce}},
		{"loses a payload's trailing zero bytes", func(d *contracts.MemDeadLetterDefects) { d.TrimPayload = true }, []string{contracts.CaseDLParkedWhole}},
		{"claims a replayed letter again", func(d *contracts.MemDeadLetterDefects) { d.ForgetReplayed = true }, []string{contracts.CaseDLReplayedIsKept}},
		{"grants a claim while another is live", func(d *contracts.MemDeadLetterDefects) { d.IgnoreLiveClaim = true }, []string{contracts.CaseDLClaimOnce, contracts.CaseDLClaimTogether}},
		{"never lets a dead holder's claim lapse", func(d *contracts.MemDeadLetterDefects) { d.NeverLapse = true }, []string{contracts.CaseDLLapsedClaim}},
		{"keeps a claim that was given back", func(d *contracts.MemDeadLetterDefects) { d.KeepClaimOnRelease = true }, []string{contracts.CaseDLReleasedClaim}},
		{"deletes a letter when its replay finishes", func(d *contracts.MemDeadLetterDefects) { d.DeleteOnFinish = true }, []string{contracts.CaseDLReplayedIsKept, contracts.CaseDLNothingIsEverDeleted}},
		{"un-replays a letter when a late release arrives", func(d *contracts.MemDeadLetterDefects) { d.UnreplayOnRelease = true }, []string{contracts.CaseDLReplayedIsKept}},
		{"counts replayed letters as open", func(d *contracts.MemDeadLetterDefects) { d.CountReplayed = true }, []string{contracts.CaseDLOpenCounts}},
		{"leaves a realm with nothing open out of the count", func(d *contracts.MemDeadLetterDefects) { d.OmitEmptyRealms = true }, []string{contracts.CaseDLOpenCounts}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.DeadLetterCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memDeadLetters(b.defect)) }); failed {
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

func TestEveryDeadLetterCaseHasAUniqueNameAndTheHonestStorePassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.DeadLetterCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memDeadLetters(nil)) }); failed {
			t.Errorf("the honest store fails %q: %v", c.Name, said)
		}
	}
}
