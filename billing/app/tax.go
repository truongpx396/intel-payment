package app

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// NewTaxStrategy names who computes tax (FR-028, D9). The engine never does — there is no compute-it-here mode,
// because rate tables and filing obligations are a specialist product — so a mode that is not one of the three is
// refused, and external_hook needs the host's service to call.
//
//   - provider_managed: the provider, as merchant of record or through its tax product. Adds nothing.
//   - none: nobody; prices are tax-inclusive or tax is out of scope. Adds nothing.
//   - external_hook: the host's own service, called before the checkout is created.
func NewTaxStrategy(mode domain.TaxMode, hook ports.TaxHook) (ports.TaxStrategy, error) {
	switch mode {
	case domain.TaxProviderManaged, domain.TaxNone:
		return passthroughTax{mode}, nil
	case domain.TaxExternalHook:
		if hook == nil {
			return nil, fmt.Errorf("%w: the external_hook tax strategy needs the host's tax service: without one, tax is silently nobody's", md.ErrInvalid)
		}
		return hookTax{hook}, nil
	}
	return nil, fmt.Errorf("%w: tax strategy %q is not provider_managed, external_hook or none (there is no mode that computes tax here)", md.ErrInvalid, mode)
}

// passthroughTax adds nothing to the checkout: the provider computes tax, or nobody does.
type passthroughTax struct{ mode domain.TaxMode }

func (p passthroughTax) Mode() domain.TaxMode                               { return p.mode }
func (passthroughTax) Annotate(context.Context, *ports.CheckoutInput) error { return nil }

// hookTax calls the host's tax service before a checkout. The service may annotate the checkout — a tax code, a
// calculation reference — but not change what is sold: scope, plan, price, currency, customer, mode and redirects
// were resolved from the catalogue and the customer records (invariant 4), and a tax service has no say in them.
type hookTax struct{ hook ports.TaxHook }

func (hookTax) Mode() domain.TaxMode { return domain.TaxExternalHook }

// sold is what a checkout is selling, and to whom: what the hook may not change.
type sold struct {
	scope                                                      domain.Scope
	mode, plan, currency, price, customer, success, cancel, ik string
	email                                                      string
}

func soldOf(in *ports.CheckoutInput) sold {
	return sold{in.Scope, in.Mode, in.PlanCode, in.Currency, in.ProviderPriceID, in.CustomerID, in.SuccessURL, in.CancelURL, in.IdemKey, in.BillingEmail}
}

func (h hookTax) Annotate(ctx context.Context, in *ports.CheckoutInput) error {
	before := soldOf(in)
	if err := h.hook(ctx, in); err != nil {
		return fmt.Errorf("the host's tax service: %w", err)
	}
	if soldOf(in) != before {
		return fmt.Errorf("%w: the host's tax service changed what is being sold: tax is annotated here, not decided", md.ErrInvalid)
	}
	return nil
}
