package contracts

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ------------------------------------------------------------------- tax --

// TaxFactory builds a tax strategy for a mode; the hook is the host's service under external_hook.
type TaxFactory func(mode domain.TaxMode, hook ports.TaxHook) (ports.TaxStrategy, error)

// TaxCase is one subtest of TaxContract.
type TaxCase struct {
	Name string
	Run  func(t testing.TB, mk TaxFactory)
}

func taxInput(h *Harness) ports.CheckoutInput {
	return ports.CheckoutInput{Scope: h.Scope, Mode: "payment", PlanCode: PackCode, Currency: "USD", ProviderPriceID: PackPriceID, CustomerID: "cus_1",
		BillingEmail: "buyer@example.test", SuccessURL: "https://" + ReturnHost + "/ok", CancelURL: "https://" + ReturnHost + "/no", IdemKey: "checkout-1",
		Metadata: map[string]string{"plan": PackCode}}
}

// TaxCases are the subtests of TaxContract (FR-028, D9).
func TaxCases() []TaxCase {
	return []TaxCase{
		{"provider_managed and none add nothing the provider does not compute", func(t testing.TB, mk TaxFactory) {
			h := NewHarness()
			for _, mode := range []domain.TaxMode{domain.TaxProviderManaged, domain.TaxNone} {
				calls := 0
				s, err := mk(mode, func(context.Context, *ports.CheckoutInput) error { calls++; return nil })
				if err != nil || s.Mode() != mode {
					t.Fatalf("%s: strategy %v, %v", mode, s, err)
				}
				in := taxInput(h)
				want := taxInput(h)
				if err := s.Annotate(Ctx(), &in); err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				if !reflect.DeepEqual(in, want) {
					t.Fatalf("%s changed the checkout: %+v", mode, in)
				}
				if calls != 0 {
					t.Fatalf("%s called the host's tax hook: only external_hook does", mode)
				}
			}
		}},
		{"external_hook is called before the checkout with what is being sold, and may annotate it", func(t testing.TB, mk TaxFactory) {
			h := NewHarness()
			var seen ports.CheckoutInput
			calls := 0
			s, err := mk(domain.TaxExternalHook, func(_ context.Context, in *ports.CheckoutInput) error {
				calls++
				seen = *in
				in.Metadata["tax_ref"] = "calc-1"
				return nil
			})
			if err != nil || s.Mode() != domain.TaxExternalHook {
				t.Fatalf("strategy %v, %v", s, err)
			}
			in := taxInput(h)
			if err := s.Annotate(Ctx(), &in); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || seen.PlanCode != PackCode || seen.ProviderPriceID != PackPriceID || seen.Scope != h.Scope || seen.Currency != "USD" {
				t.Fatalf("the hook was called %d times with %+v", calls, seen)
			}
			if in.Metadata["tax_ref"] != "calc-1" || in.Metadata["plan"] != PackCode {
				t.Fatalf("metadata = %v: the hook's annotation must reach the checkout, beside what was there", in.Metadata)
			}
		}},
		{"a failing hook fails the checkout closed", func(t testing.TB, mk TaxFactory) {
			h := NewHarness()
			boom := errors.New("the tax service is down")
			s, err := mk(domain.TaxExternalHook, func(context.Context, *ports.CheckoutInput) error { return boom })
			if err != nil {
				t.Fatal(err)
			}
			in := taxInput(h)
			if err := s.Annotate(Ctx(), &in); !errors.Is(err, boom) {
				t.Fatalf("Annotate = %v: a checkout whose tax cannot be settled must not be created", err)
			}
		}},
		{"a hook cannot change what is sold (4)", func(t testing.TB, mk TaxFactory) {
			h := NewHarness()
			for name, mutate := range map[string]func(*ports.CheckoutInput){
				"the price":    func(in *ports.CheckoutInput) { in.ProviderPriceID = "price_cheap" },
				"the plan":     func(in *ports.CheckoutInput) { in.PlanCode = "free" },
				"the currency": func(in *ports.CheckoutInput) { in.Currency = "VND" },
				"the scope":    func(in *ports.CheckoutInput) { in.Scope.ID = "victim" },
				"the customer": func(in *ports.CheckoutInput) { in.CustomerID = "cus_other" },
				"the mode":     func(in *ports.CheckoutInput) { in.Mode = "setup" },
				"the redirect": func(in *ports.CheckoutInput) { in.SuccessURL = "https://evil.example/" },
			} {
				s, err := mk(domain.TaxExternalHook, func(_ context.Context, in *ports.CheckoutInput) error { mutate(in); return nil })
				if err != nil {
					t.Fatal(err)
				}
				in := taxInput(h)
				if err := s.Annotate(Ctx(), &in); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("a hook that rewrote %s: Annotate = %v; want ErrInvalid — tax is annotated, not decided here", name, err)
				}
			}
		}},
		{"there is no fourth mode, and external_hook needs a hook", func(t testing.TB, mk TaxFactory) {
			for _, mode := range []domain.TaxMode{"compute_it_here", "", "PROVIDER_MANAGED"} {
				if s, err := mk(mode, func(context.Context, *ports.CheckoutInput) error { return nil }); err == nil {
					t.Errorf("mode %q was accepted (%v): rate tables and filing duties are a specialist product", mode, s)
				}
			}
			if s, err := mk(domain.TaxExternalHook, nil); err == nil {
				t.Errorf("external_hook with no hook was accepted (%v): tax would silently be nobody's", s)
			}
		}},
	}
}

// TaxContract is the specification of the tax seam (FR-028, D9): it names who computes tax, and the engine never
// does. provider_managed and none add nothing; external_hook calls the host's service before checkout, fails the
// checkout closed when it fails, and may annotate the checkout but not change what is sold.
func TaxContract(t *testing.T, mk TaxFactory) {
	t.Helper()
	for _, c := range TaxCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}
