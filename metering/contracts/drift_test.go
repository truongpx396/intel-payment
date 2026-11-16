package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/contracts"
)

func memDrift(defect func(*contracts.MemDriftDefects)) contracts.DriftFactory {
	return func(testing.TB) contracts.DriftWorld {
		m := contracts.NewMemDrift()
		if defect != nil {
			defect(&m.Defects)
		}
		return m.World()
	}
}

func TestMemoryDriftReaderHonoursTheContract(t *testing.T) {
	t.Parallel()
	contracts.DriftContract(t, memDrift(nil))
}

// What an operator reads here decides whether a balance is trusted, so a reader that shows another realm's accounts, counts them, or hides the
// worst finding is worse than none. The suite is held to what it enforces: it passes an honest reader and rejects each broken one by name.
func TestDriftContractRejectsEveryBrokenReaderByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemDriftDefects)
		caughtBy []string
	}{
		{"lists every realm's findings", func(d *contracts.MemDriftDefects) { d.ShowsEveryRealm = true }, []string{contracts.CaseDRRealm}},
		{"counts every realm's findings", func(d *contracts.MemDriftDefects) { d.CountsEveryRealm = true }, []string{contracts.CaseDRRealm}},
		{"lets a shard narrow nothing", func(d *contracts.MemDriftDefects) { d.IgnoresTheShard = true }, []string{contracts.CaseDRShard}},
		{"takes shard zero for no shard", func(d *contracts.MemDriftDefects) { d.ShardZeroIsNoShard = true }, []string{contracts.CaseDRShard}},
		{"gives the oldest run first", func(d *contracts.MemDriftDefects) { d.OldestFirst = true }, []string{contracts.CaseDRNewestFirst}},
		{"gives every run whatever the limit", func(d *contracts.MemDriftDefects) { d.IgnoresTheLimit = true }, []string{contracts.CaseDRNewestFirst}},
		{"gives every run when no limit is asked", func(d *contracts.MemDriftDefects) { d.NoDefaultLimit = true }, []string{contracts.CaseDRNewestFirst}},
		{"takes any limit", func(d *contracts.MemDriftDefects) { d.AllowsAnyLimit = true }, []string{contracts.CaseDRLimitBounds}},
		{"ignores the filter for runs with findings", func(d *contracts.MemDriftDefects) { d.IgnoresWithFindings = true }, []string{contracts.CaseDRWithFindings}},
		{"keeps a run that found something of another realm", func(d *contracts.MemDriftDefects) { d.AnyRealmsFindings = true }, []string{contracts.CaseDRWithFindings}},
		{"takes the limit before the filter", func(d *contracts.MemDriftDefects) { d.LimitBeforeFilter = true }, []string{contracts.CaseDRWithFindings}},
		{"lists every finding, however many", func(d *contracts.MemDriftDefects) { d.NoCap = true }, []string{contracts.CaseDRWorstFirst}},
		{"counts only the findings it listed", func(d *contracts.MemDriftDefects) { d.CountsOnlyWhatShown = true }, []string{contracts.CaseDRWorstFirst}},
		{"gives the smallest drift first", func(d *contracts.MemDriftDefects) { d.BestFirst = true }, []string{contracts.CaseDRWhole, contracts.CaseDRWorstFirst}},
		{"nets a plus and a minus into one", func(d *contracts.MemDriftDefects) { d.NetsFindings = true }, []string{contracts.CaseDRWhole, contracts.CaseDRWorstFirst}},
		{"loses healed and alarmed", func(d *contracts.MemDriftDefects) { d.DropsFlags = true }, []string{contracts.CaseDRWhole}},
		{"takes a realm that is not one", func(d *contracts.MemDriftDefects) { d.AcceptsAnyRealm = true }, []string{contracts.CaseDREmptyAndRealms}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.DriftCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memDrift(b.defect)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a reader that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryDriftCaseHasAUniqueNameAndTheHonestReaderPassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.DriftCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memDrift(nil)) }); failed {
			t.Errorf("the honest reader fails %q: %v", c.Name, said)
		}
	}
}
