package billing_test

import (
	"os"
	"slices"
	"sort"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driven/paypal"
	"github.com/truongpx396/intel-payment/billing/adapters/driven/polar"
	"github.com/truongpx396/intel-payment/billing/adapters/driven/stripe"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/paypalfix"
	"github.com/truongpx396/intel-payment/billing/contracts/polarfix"
	"github.com/truongpx396/intel-payment/billing/contracts/stripefix"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// SC-004: the provider contract runs against EVERY adapter, and a new adapter is not registered until it passes.
// Each row builds an adapter and the fixtures that sign deliveries the way that provider does. A new provider starts as a
// row whose adapter is a stub answering domain.ErrNotImplemented, so the contract fails on what it claims; building the
// adapter replaces the stub, and the only edit to this file is that row.

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
	"paypal": func(t *testing.T) (ports.PaymentProvider, contracts.ProviderFixtures) {
		fx := paypalfix.New(t)
		p, err := paypal.New(paypal.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, BaseURL: fx.APIURL()})
		if err != nil {
			t.Fatal(err)
		}
		return p, fx
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
