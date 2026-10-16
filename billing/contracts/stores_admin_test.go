package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/ports"
)

func memWriter(defect func(*contracts.MemConfigWriter)) contracts.ConfigWriterFactory {
	return func(testing.TB) ports.ConfigWriter {
		w := contracts.NewMemConfigWriter(contracts.WorldAccounts()...)
		if defect != nil {
			defect(w)
		}
		return w
	}
}

func TestMemoryConfigWriterHonoursTheConfigWriterContract(t *testing.T) {
	t.Parallel()
	contracts.ConfigWriterContract(t, memWriter(nil))
}

// The contract is held to what it enforces: it passes an honest writer, and it rejects each deliberately broken one — by name.
// A writer that lets a mapping be replaced would orphan every subscriber still renewing at the old price, and must not pass
// because the fake it was tried against was kind.
func TestConfigWriterContractRejectsEveryBrokenWriterByName(t *testing.T) {
	t.Parallel()
	const (
		unique   = "a plan code is unique within a realm, and creating it again changes nothing"
		valid    = "a plan that is not valid is refused and stored nowhere"
		replaced = "a mapping is never replaced: a subscriber renews at the old price and a payment for an unmapped one is ignored"
		shared   = "a provider's price belongs to one plan"
		provider = "an account is created and replaced, inactive ones are listed, and its provider never changes"
		account  = "an account that is not valid is refused and stored nowhere"
		repoint  = "a customer links a scope to an existing provider customer, and is never re-pointed"
		racing   = "however many create one plan code at once, exactly one wins"
	)
	broken := []struct {
		name     string
		defect   func(*contracts.MemConfigWriter)
		caughtBy []string
	}{
		{"overwrites a plan that exists", func(w *contracts.MemConfigWriter) { w.Defects.OverwriteOnCreate = true }, []string{unique}},
		{"stores a plan that is not valid", func(w *contracts.MemConfigWriter) { w.Defects.SkipValidation = true }, []string{valid, account}},
		{"replaces a mapping silently", func(w *contracts.MemConfigWriter) { w.Defects.ReplaceMapping = true }, []string{replaced}},
		{"sells two plans with one provider price", func(w *contracts.MemConfigWriter) { w.Defects.ShareProviderPrice = true }, []string{shared}},
		{"re-points a scope at another provider customer", func(w *contracts.MemConfigWriter) { w.Defects.RepointCustomer = true }, []string{repoint}},
		{"rewrites an account's provider", func(w *contracts.MemConfigWriter) { w.Defects.ChangeProvider = true }, []string{provider}},
		{"checks for a plan and writes it later", func(w *contracts.MemConfigWriter) { w.Defects.LoseAnUpdate = true }, []string{racing}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.ConfigWriterCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memWriter(b.defect)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a writer that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryConfigWriterCaseHasAUniqueNameAndTheHonestWriterPassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.ConfigWriterCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memWriter(nil)) }); failed {
			t.Errorf("the honest writer fails %q: %v", c.Name, said)
		}
	}
}
