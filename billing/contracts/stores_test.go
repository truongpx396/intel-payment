package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
)

// The in-memory stores every processor and checkout suite stands on are held to the same contracts as the
// PostgreSQL ones: a fake that is kinder than the real store would make those suites pass for the wrong reason.

func TestMemoryInboxHonoursTheInboxContract(t *testing.T) {
	t.Parallel()
	contracts.InboxContract(t, func(_ testing.TB, clock *mcontracts.FakeClock, maxAttempts int) ports.Inbox {
		in := contracts.NewMemInbox(clock, &contracts.CallLog{})
		in.MaxAttempts = maxAttempts
		return in
	})
}

func TestMemoryPaymentsHonourThePaymentsContract(t *testing.T) {
	t.Parallel()
	contracts.PaymentsContract(t, func(testing.TB) ports.Payments { return contracts.NewMemPayments(&contracts.CallLog{}) })
}

func TestMemorySubscriptionsHonourTheSubsContract(t *testing.T) {
	t.Parallel()
	contracts.SubsContract(t, func(testing.TB) ports.Subs { return contracts.NewMemSubs(&contracts.CallLog{}) })
}

func TestMemoryCustomersHonourTheCustomersContract(t *testing.T) {
	t.Parallel()
	contracts.CustomersContract(t, func(testing.TB) ports.Customers { return contracts.NewMemCustomers(&contracts.CallLog{}) })
}

func TestMemoryCatalogHonoursTheCatalogContract(t *testing.T) {
	t.Parallel()
	contracts.CatalogContract(t, func(testing.TB) (ports.Catalog, contracts.CatalogSeeder) {
		c := contracts.NewMemCatalog(&contracts.CallLog{})
		return c, func(w contracts.WorldPlan) { c.Add(w.Plan, w.Account, w.Currency, w.ProviderPriceID) }
	})
}

func TestMemoryAccountsHonourTheAccountsContract(t *testing.T) {
	t.Parallel()
	contracts.AccountsContract(t, func(testing.TB) (ports.Accounts, contracts.AccountSeeder) {
		a := contracts.NewMemAccounts(&contracts.CallLog{})
		return a, func(acct domain.ProviderAccount) { a.Put(acct) }
	})
}

func TestMemoryAutoRechargesHonourTheAutoRechargesContract(t *testing.T) {
	t.Parallel()
	contracts.AutoRechargesContract(t, func(testing.TB) ports.AutoRecharges { return contracts.NewMemAutoRecharges(&contracts.CallLog{}) })
}

// The store contract is held to the rule it enforces: it passes an honest store, and it rejects each deliberately broken
// one — by name. A store that charges twice must not pass because the fake it was tried against was kind.
func TestAutoRechargeContractRejectsEveryBrokenStoreByName(t *testing.T) {
	t.Parallel()
	const (
		oneSlot  = "concurrent triggers for one scope claim one slot"
		bounded  = "however many triggers fire at once, a day never hands out more than max_per_day slots"
		numbered = "claiming numbers the day's slots, a duplicate resumes, and the day is bounded by max_per_day"
		ledger   = "the ledger is stored: what a claim did is what the next read says"
		edit     = "editing the instruction never refills the day, and the owner enabling it clears the failures"
		settling = "settling clears the slot, counts failures, switches off on the third and says so once"
		stale    = "a slot nobody settled is replaced once it is stale, and still counts"
	)
	broken := []struct {
		name     string
		defect   func(*contracts.MemAutoRecharges)
		caughtBy []string
	}{
		{"claims without holding the lock", func(s *contracts.MemAutoRecharges) { s.Racy = true }, []string{oneSlot, bounded}},
		{"forgets the ledger it computed", func(s *contracts.MemAutoRecharges) { s.ForgetLedger = true }, []string{numbered, ledger}},
		{"refills the day when the instruction is edited", func(s *contracts.MemAutoRecharges) { s.ResetLedgerOnPut = true }, []string{edit}},
		{"never switches an instruction off", func(s *contracts.MemAutoRecharges) { s.NeverDisables = true }, []string{settling}},
		{"resumes an abandoned slot for ever", func(s *contracts.MemAutoRecharges) { s.NeverStale = true }, []string{stale}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.AutoRechargeCases() {
				mk := func(testing.TB) ports.AutoRecharges {
					s := contracts.NewMemAutoRecharges(&contracts.CallLog{})
					b.defect(s)
					return s
				}
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, mk) }); failed {
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

func TestEveryAutoRechargeCaseHasAUniqueNameAndTheHonestStorePassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.AutoRechargeCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		mk := func(testing.TB) ports.AutoRecharges { return contracts.NewMemAutoRecharges(&contracts.CallLog{}) }
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, mk) }); failed {
			t.Errorf("the honest store fails %q: %v", c.Name, said)
		}
	}
}
