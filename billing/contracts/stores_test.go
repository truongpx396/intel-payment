package contracts_test

import (
	"testing"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
)

// The in-memory stores every processor and checkout suite stands on are held to the same contracts as the
// PostgreSQL ones: a fake that is kinder than the real store would make those suites pass for the wrong reason.

func TestMemoryInboxHonoursTheInboxContract(t *testing.T) {
	t.Parallel()
	contracts.InboxContract(t, func(t testing.TB, clock *mcontracts.FakeClock, maxAttempts int) ports.Inbox {
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
