package domain

import (
	"fmt"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// TaxMode says WHO computes tax. The engine never does: there is no compute-it-here mode, because rate tables and
// filing obligations are a specialist product (FR-028, D9).
type TaxMode string

// The three tax modes.
const (
	TaxProviderManaged TaxMode = "provider_managed" // the provider, as merchant of record or via its tax product (default)
	TaxExternalHook    TaxMode = "external_hook"    // a host-supplied service, called before checkout
	TaxNone            TaxMode = "none"             // nobody: prices are tax-inclusive or out of scope
)

// Valid reports whether m is one of the three.
func (m TaxMode) Valid() bool {
	switch m {
	case TaxProviderManaged, TaxExternalHook, TaxNone:
		return true
	}
	return false
}

// ParseTaxMode reads a configured mode, refusing anything else; empty is the default.
func ParseTaxMode(s string) (TaxMode, error) {
	if s == "" {
		return TaxProviderManaged, nil
	}
	if m := TaxMode(s); m.Valid() {
		return m, nil
	}
	return "", fmt.Errorf("%w: tax strategy %q is not provider_managed, external_hook or none", md.ErrInvalid, s)
}

// PlanChangePolicy is what a mid-period plan change does (D11). Proration is the provider's math, which already
// owns the invoice; the policy only chooses when and whether it applies.
type PlanChangePolicy string

// The three policies.
const (
	AtPeriodEnd        PlanChangePolicy = "at_period_end"        // default: needs no proration at all
	ImmediateProrate   PlanChangePolicy = "immediate_prorate"    // now, the provider prorates
	ImmediateNoProrate PlanChangePolicy = "immediate_no_prorate" // now, no proration
)

// Valid reports whether p is one of the three.
func (p PlanChangePolicy) Valid() bool {
	switch p {
	case AtPeriodEnd, ImmediateProrate, ImmediateNoProrate:
		return true
	}
	return false
}

// ParsePlanChangePolicy reads a configured policy, refusing anything else; empty is the default.
func ParsePlanChangePolicy(s string) (PlanChangePolicy, error) {
	if s == "" {
		return AtPeriodEnd, nil
	}
	if p := PlanChangePolicy(s); p.Valid() {
		return p, nil
	}
	return "", fmt.Errorf("%w: plan change policy %q is not at_period_end, immediate_prorate or immediate_no_prorate", md.ErrInvalid, s)
}
