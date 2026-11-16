package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/contracts"
)

func memBooksUnblock(defect func(*contracts.MemBooksUnblockDefects)) contracts.BooksUnblockFactory {
	return func(testing.TB) contracts.BooksUnblockWorld {
		m := contracts.NewMemBooksUnblock()
		if defect != nil {
			defect(&m.Defects)
		}
		return m.World()
	}
}

func memHotUnblock(defect func(*contracts.MemHotUnblockDefects)) contracts.HotUnblockFactory {
	return func(testing.TB) contracts.HotUnblockWorld {
		m := contracts.NewMemHotUnblock()
		if defect != nil {
			defect(&m.Defects)
		}
		return m.World()
	}
}

func TestMemoryBooksUnblockHonoursTheContract(t *testing.T) {
	t.Parallel()
	contracts.BooksUnblockContract(t, memBooksUnblock(nil))
}

func TestMemoryHotUnblockHonoursTheContract(t *testing.T) {
	t.Parallel()
	contracts.HotUnblockContract(t, memHotUnblock(nil))
}

// An unblock lets an account spend again, so a clearer that clears a neighbour, or says it cleared what it did not, lets someone spend who
// must not. The suites are held to what they enforce: each passes an honest clearer and rejects every deliberately broken one by name.
func TestBooksUnblockContractRejectsEveryBrokenClearerByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemBooksUnblockDefects)
		caughtBy []string
	}{
		{"clears every scope of the realm", func(d *contracts.MemBooksUnblockDefects) { d.ClearsTheWholeRealm = true }, []string{contracts.CaseBUOnlyThat}},
		{"matches a scope in any realm", func(d *contracts.MemBooksUnblockDefects) { d.ForgetsTheRealm = true }, []string{contracts.CaseBUOnlyThat}},
		{"says it cleared a block whether or not there was one", func(d *contracts.MemBooksUnblockDefects) { d.ReportsAlways = true }, []string{contracts.CaseBUNotBlocked, contracts.CaseBUUnknown, contracts.CaseBUTwice}},
		{"writes a watermark for a scope the books had none for", func(d *contracts.MemBooksUnblockDefects) { d.InventsAWatermark = true }, []string{contracts.CaseBUUnknown}},
		{"zeroes the applied sequence", func(d *contracts.MemBooksUnblockDefects) { d.ResetsProgress = true }, []string{contracts.CaseBUClears, contracts.CaseBUTwice}},
		{"reads, looks away and writes: two callers both see the block", func(d *contracts.MemBooksUnblockDefects) { d.NotAtomic = true }, []string{contracts.CaseBUTogether}},
		{"clears for a caller who has gone", func(d *contracts.MemBooksUnblockDefects) { d.IgnoresCancel = true }, []string{contracts.CaseBUCancelled}},
		{"does not check that the scope is one", func(d *contracts.MemBooksUnblockDefects) { d.AcceptsAnyScope = true }, []string{contracts.CaseBUInvalidScope}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.BooksUnblockCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memBooksUnblock(b.defect)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a clearer that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestHotUnblockContractRejectsEveryBrokenClearerByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		defect   func(*contracts.MemHotUnblockDefects)
		caughtBy []string
	}{
		{"reaches into an account of an older generation", func(d *contracts.MemHotUnblockDefects) { d.ClearsAStaleAccount = true }, []string{contracts.CaseHUStale}},
		{"clears in a frozen shard", func(d *contracts.MemHotUnblockDefects) { d.ClearsWhileFrozen = true }, []string{contracts.CaseHUFrozen}},
		{"says it cleared a block whether or not there was one", func(d *contracts.MemHotUnblockDefects) { d.ReportsAlways = true }, []string{contracts.CaseHUNotBlocked, contracts.CaseHUNoAccount, contracts.CaseHUTwice, contracts.CaseHUStale}},
		{"brings an account into being for a scope that had none", func(d *contracts.MemHotUnblockDefects) { d.MakesAnAccount = true }, []string{contracts.CaseHUNoAccount}},
		{"reads, looks away and writes: two callers both see the block", func(d *contracts.MemHotUnblockDefects) { d.NotAtomic = true }, []string{contracts.CaseHUTogether}},
		{"advances the account's sequence", func(d *contracts.MemHotUnblockDefects) { d.MovesTheSequence = true }, []string{contracts.CaseHUClears}},
		{"answers a scope with no account as an error", func(d *contracts.MemHotUnblockDefects) { d.RefusesACold = true }, []string{contracts.CaseHUNoAccount}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.HotUnblockCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memHotUnblock(b.defect)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a clearer that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryUnblockCaseHasAUniqueNameAndTheHonestClearersPassThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.BooksUnblockCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memBooksUnblock(nil)) }); failed {
			t.Errorf("the honest books clearer fails %q: %v", c.Name, said)
		}
	}
	for _, c := range contracts.HotUnblockCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memHotUnblock(nil)) }); failed {
			t.Errorf("the honest hot clearer fails %q: %v", c.Name, said)
		}
	}
}
