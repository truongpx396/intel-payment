package billing_test

import (
	"context"
	"net/http"
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driven/polar"
	"github.com/truongpx396/intel-payment/billing/adapters/driven/stripe"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/polarfix"
	"github.com/truongpx396/intel-payment/billing/contracts/stripefix"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// SC-004: the provider contract runs against EVERY adapter, and a new adapter is not registered until it passes.
// Each row builds an adapter and the fixtures that sign deliveries the way that provider does; until an adapter exists
// its row holds a stub that answers "not implemented", so the contract fails on what it claims. Building the adapter
// replaces its row, and the only edit to this file is that row.

// unbuilt is a provider adapter before it exists.
type unbuilt struct{ name domain.Provider }

func (u unbuilt) Name() domain.Provider { return u.name }
func (unbuilt) EnsureCustomer(context.Context, domain.ProviderAccount, ports.CustomerInput) (string, error) {
	return "", domain.ErrNotImplemented
}
func (unbuilt) CreateCheckout(context.Context, domain.ProviderAccount, ports.CheckoutInput) (string, error) {
	return "", domain.ErrNotImplemented
}
func (unbuilt) CreatePortalSession(context.Context, domain.ProviderAccount, string, string) (string, error) {
	return "", domain.ErrNotImplemented
}
func (unbuilt) VerifyWebhook(context.Context, domain.ProviderAccount, []byte, http.Header) (domain.Event, error) {
	return domain.Event{}, domain.ErrNotImplemented
}
func (unbuilt) FetchSubscription(context.Context, domain.ProviderAccount, string) (domain.Sub, time.Time, error) {
	return domain.Sub{}, time.Time{}, domain.ErrNotImplemented
}
func (unbuilt) ChangePlan(context.Context, domain.ProviderAccount, string, string, domain.PlanChangePolicy) error {
	return domain.ErrNotImplemented
}
func (unbuilt) CancelAtPeriodEnd(context.Context, domain.ProviderAccount, string) error {
	return domain.ErrNotImplemented
}
func (unbuilt) ChargeSaved(context.Context, domain.ProviderAccount, ports.OffSessionInput) (string, error) {
	return "", domain.ErrNotImplemented
}

// adapter builds one provider adapter, with the fixtures that sign for it.
type adapter func(t *testing.T) (ports.PaymentProvider, contracts.ProviderFixtures)

var adapters = map[string]adapter{
	"stripe": func(t *testing.T) (ports.PaymentProvider, contracts.ProviderFixtures) {
		fx := stripefix.New(t)
		p, err := stripe.New(stripe.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, BaseURL: fx.APIURL()})
		if err != nil {
			t.Fatal(err)
		}
		return p, fx
	},
	"polar": func(t *testing.T) (ports.PaymentProvider, contracts.ProviderFixtures) {
		fx := polarfix.New(t)
		p, err := polar.New(polar.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, BaseURL: fx.APIURL()})
		if err != nil {
			t.Fatal(err)
		}
		return p, fx
	},
	"paypal": func(*testing.T) (ports.PaymentProvider, contracts.ProviderFixtures) {
		return unbuilt{"paypal"}, contracts.NewHarness().Fixtures()
	},
}

func TestEveryProviderAdapterPassesTheContract(t *testing.T) {
	t.Parallel()
	for name, build := range adapters {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, fx := build(t)
			contracts.ProviderContract(t, p, fx)
		})
	}
}

// A new adapter is not registered until it passes: an adapter directory that is not a row above fails here, so it
// cannot ship without ever meeting the contract.
func TestEveryAdapterDirectoryIsInTheRegistry(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir("adapters/driven")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "postgres" { // the books, not a provider
			dirs = append(dirs, e.Name())
		}
	}
	var rows []string
	for name := range adapters {
		rows = append(rows, name)
	}
	sort.Strings(dirs)
	sort.Strings(rows)
	for _, d := range dirs {
		if !slices.Contains(rows, d) {
			t.Errorf("billing/adapters/driven/%s is not in the provider registry: add a row, and the contract runs against it (SC-004)", d)
		}
	}
	for _, r := range rows {
		if !slices.Contains(dirs, r) {
			t.Errorf("the registry has a row %q and no adapter directory", r)
		}
	}
}
