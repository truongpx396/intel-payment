package app_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// ---- tax ----

// unbuiltTax answers every mode and annotates nothing, so the cases that need a strategy to DO something fail on
// what they claim. The variable below that binds it is the only edit T089 makes to the wiring.
type unbuiltTax struct{ mode domain.TaxMode }

func (u unbuiltTax) Mode() domain.TaxMode                               { return u.mode }
func (unbuiltTax) Annotate(context.Context, *ports.CheckoutInput) error { return nil }

var taxUnderTest contracts.TaxFactory = func(mode domain.TaxMode, _ ports.TaxHook) (ports.TaxStrategy, error) {
	return unbuiltTax{mode}, nil
}

func TestTaxContract(t *testing.T) {
	t.Parallel()
	contracts.TaxContract(t, taxUnderTest)
}

func TestTaxContractRejectsEveryBrokenStrategyByName(t *testing.T) {
	t.Parallel()
	const (
		adds    = "provider_managed and none add nothing the provider does not compute"
		called  = "external_hook is called before the checkout with what is being sold, and may annotate it"
		closed  = "a failing hook fails the checkout closed"
		notSold = "a hook cannot change what is sold (4)"
	)
	// Each wraps the strategy under test and breaks it in one way.
	broken := []struct {
		name     string
		wrap     func(base contracts.TaxFactory) contracts.TaxFactory
		caughtBy string
	}{
		{"a strategy that never calls the host's hook", func(base contracts.TaxFactory) contracts.TaxFactory {
			return func(mode domain.TaxMode, _ ports.TaxHook) (ports.TaxStrategy, error) {
				return base(mode, func(context.Context, *ports.CheckoutInput) error { return nil })
			}
		}, called},
		{"a strategy that swallows the hook's failure", func(base contracts.TaxFactory) contracts.TaxFactory {
			return func(mode domain.TaxMode, hook ports.TaxHook) (ports.TaxStrategy, error) {
				return base(mode, func(ctx context.Context, in *ports.CheckoutInput) error { _ = hook(ctx, in); return nil })
			}
		}, closed},
		{"a strategy that lets the hook rewrite the price", func(_ contracts.TaxFactory) contracts.TaxFactory {
			return func(mode domain.TaxMode, hook ports.TaxHook) (ports.TaxStrategy, error) {
				return rewritesFreely{mode: mode, hook: hook}, nil
			}
		}, notSold},
		{"provider_managed that calls the host's hook anyway", func(_ contracts.TaxFactory) contracts.TaxFactory {
			return func(mode domain.TaxMode, hook ports.TaxHook) (ports.TaxStrategy, error) {
				return callsHookAlways{mode: mode, hook: hook}, nil
			}
		}, adds},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.TaxCases() {
				if failed, _ := contracts.Probe(10*time.Second, func(t testing.TB) { c.Run(t, b.wrap(taxUnderTest)) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			if !slices.Contains(rejected, b.caughtBy) {
				t.Fatalf("the suite PASSED %s on %q (rejected only by %q)", b.name, b.caughtBy, rejected)
			}
		})
	}
}

func TestTaxContractPassesEveryCaseOnTheStrategyUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.TaxCases() {
		if failed, said := contracts.Probe(10*time.Second, func(t testing.TB) { c.Run(t, taxUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

type rewritesFreely struct {
	mode domain.TaxMode
	hook ports.TaxHook
}

func (r rewritesFreely) Mode() domain.TaxMode { return r.mode }
func (r rewritesFreely) Annotate(ctx context.Context, in *ports.CheckoutInput) error {
	if r.mode != domain.TaxExternalHook {
		return nil
	}
	return r.hook(ctx, in)
}

type callsHookAlways struct {
	mode domain.TaxMode
	hook ports.TaxHook
}

func (c callsHookAlways) Mode() domain.TaxMode { return c.mode }
func (c callsHookAlways) Annotate(ctx context.Context, in *ports.CheckoutInput) error {
	if c.hook == nil {
		return nil
	}
	return c.hook(ctx, in)
}
