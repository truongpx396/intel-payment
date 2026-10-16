package app_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// checkoutUnderTest builds the checkout over a harness's collaborators.
var checkoutUnderTest contracts.CheckoutFactory = func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
	t.Helper()
	c, err := app.NewCheckout(app.CheckoutDeps{Providers: h.Providers(), Accounts: h.Accounts, Catalog: h.Catalog, Customers: h.Customers,
		Payments: h.Payments, Tax: tax, Clock: h.Clock, ReturnHosts: []string{contracts.ReturnHost}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckoutContract(t *testing.T) {
	t.Parallel()
	contracts.CheckoutContract(t, checkoutUnderTest)
}

// The checkout suite is shown to reject a checkout that grants on a return, and collaborators broken in the ways
// that sell the wrong thing or lose the buyer.
func TestCheckoutContractRejectsEveryBrokenCheckoutByName(t *testing.T) {
	t.Parallel()
	const (
		returns = "a return from checkout grants nothing, and fulfilment is the server's report (2)"
		mapped  = "a customer is created once and mapped to its scope before a payment can arrive"
		once    = "two first checkouts at once create one customer"
		modes   = "payment, subscription and setup each return a hosted page and grant nothing (2)"
	)
	broken := []struct {
		name     string
		build    contracts.CheckoutFactory
		caughtBy string
	}{
		{"a checkout that grants when the buyer comes back", func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
			return grantsOnReturn{checkoutUnderTest(t, h, tax), h}
		}, returns},
		{"a checkout that grants when it starts", func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
			return grantsOnStart{checkoutUnderTest(t, h, tax), h}
		}, modes},
		{"customers that are lost before the payment arrives", func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
			h.Customers.DropUpserts = true
			return checkoutUnderTest(t, h, tax)
		}, mapped},
		{"a provider that ignores the customer key", func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
			h.Provider.NonIdempotentCustomers = true
			return checkoutUnderTest(t, h, tax)
		}, once},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.CheckoutCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, b.build) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			if !slices.Contains(rejected, b.caughtBy) {
				t.Fatalf("the suite PASSED %s on %q (rejected only by %q)", b.name, b.caughtBy, rejected)
			}
		})
	}
}

func TestCheckoutContractPassesEveryCaseOnTheCheckoutUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.CheckoutCases() {
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, checkoutUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

type grantsOnReturn struct {
	ports.Checkout
	h *contracts.Harness
}

func (g grantsOnReturn) Fulfilment(ctx context.Context, s domain.Scope) (ports.Fulfilment, error) {
	_, _ = g.h.Meter.Grant(ctx, md.Grant{Scope: s, Amount: contracts.PackCredits, IdemKey: "on-return", Reason: "purchase"})
	return g.Checkout.Fulfilment(ctx, s)
}

type grantsOnStart struct {
	ports.Checkout
	h *contracts.Harness
}

func (g grantsOnStart) Start(ctx context.Context, r ports.CheckoutRequest) (string, error) {
	url, err := g.Checkout.Start(ctx, r)
	if err == nil {
		_, _ = g.h.Meter.Grant(ctx, md.Grant{Scope: r.Scope, Amount: contracts.PackCredits, IdemKey: "on-start", Reason: "purchase"})
	}
	return url, err
}

// A provider that prices a purchase itself (PayPal's orders) must be told the price, and the price is the
// catalogue's for the plan in the currency — never anything the request said (invariant 4).
func TestTheProviderIsToldTheCataloguesPriceForThePlanInTheCurrency(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	c := checkoutUnderTest(t, h, nil)
	for plan, want := range map[string]domain.Money{contracts.PackCode: {MinorUnits: 1000, Currency: "USD"}, contracts.ProCode: {MinorUnits: 2000, Currency: "USD"}} {
		if _, err := c.Start(contracts.Ctx(), h.CheckoutRequest(plan)); err != nil {
			t.Fatal(err)
		}
		all := h.Provider.Checkouts()
		if got := all[len(all)-1].Amount; got != want {
			t.Errorf("%s: the provider was told %+v, want the catalogue's %+v", plan, got, want)
		}
	}
	// A setup checkout sells nothing: no amount.
	req := h.CheckoutRequest("")
	req.Mode = "setup"
	if _, err := c.Start(contracts.Ctx(), req); err != nil {
		t.Fatal(err)
	}
	all := h.Provider.Checkouts()
	if got := all[len(all)-1].Amount; got != (domain.Money{}) {
		t.Errorf("setup: the provider was told an amount: %+v", got)
	}
}
