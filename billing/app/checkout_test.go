package app_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// unbuiltCheckout is the checkout as it is before it exists: it answers nothing, so every case fails on what it
// claims and not on a compile error. The variable below that binds it is the only edit T085 makes to the wiring.
type unbuiltCheckout struct{}

func (unbuiltCheckout) Start(context.Context, ports.CheckoutRequest) (string, error) { return "", nil }
func (unbuiltCheckout) Portal(context.Context, domain.Scope, string, string) (string, error) {
	return "", nil
}
func (unbuiltCheckout) Fulfilment(context.Context, domain.Scope) (ports.Fulfilment, error) {
	return ports.Fulfilment{}, nil
}

// checkoutUnderTest builds the checkout over a harness's collaborators.
var checkoutUnderTest contracts.CheckoutFactory = func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
	return unbuiltCheckout{}
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
		returns  = "a return from checkout grants nothing, and fulfilment is the server's report (2)"
		catalogd = "the price and credits come from the catalogue, never from the request (4)"
		mapped   = "a customer is created once and mapped to its scope before a payment can arrive"
		once     = "two first checkouts at once create one customer"
		modes    = "payment, subscription and setup each return a hosted page and grant nothing (2)"
	)
	fallback := domain.Plan{Realm: md.DefaultRealm, Code: "pack", Kind: domain.OneTime, Active: true,
		Prices: map[string]domain.Money{"USD": {MinorUnits: 1, Currency: "USD"}}}
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
		{"a catalogue that answers any plan with a default one", func(t testing.TB, h *contracts.Harness, tax ports.TaxStrategy) ports.Checkout {
			h.Catalog.FallbackPlan = &fallback
			return checkoutUnderTest(t, h, tax)
		}, catalogd},
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
