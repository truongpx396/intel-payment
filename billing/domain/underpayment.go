package domain

// UnderpaymentPolicy is what the processor does with a verified payment that settled for less than its plan costs.
//
// A plan's credits are minted from the catalogue and never from the amount paid, so a provider-side coupon that takes a pack
// from $10.00 to $5.00 still grants the whole pack. The three policies differ only in how far that goes. Each counts what it does
// (billing_payment_underpaid_total), so none of them lets revenue be given away without a signal.
type UnderpaymentPolicy string

const (
	// UnderpaymentHoldZero is the default: a payment of nothing for a plan that costs money (a trial invoice, a coupon of
	// 100%) is held, and a discount above nothing is granted.
	UnderpaymentHoldZero UnderpaymentPolicy = "hold_zero"
	// UnderpaymentHold holds every payment below the plan's price. It is for a deployment that never runs a coupon: anything
	// that settled for less is a price or a promotion code nobody meant to have.
	UnderpaymentHold UnderpaymentPolicy = "hold"
	// UnderpaymentGrant holds nothing. It is for a deployment that uses a provider's trial as free credits on purpose.
	UnderpaymentGrant UnderpaymentPolicy = "grant"
)

// Valid reports whether p is one of the three policies, spelt exactly.
func (p UnderpaymentPolicy) Valid() bool {
	switch p {
	case UnderpaymentHoldZero, UnderpaymentHold, UnderpaymentGrant:
		return true
	}
	return false
}

// Holds reports whether a payment of paid minor units that is already known to be below its plan's price is held under p.
func (p UnderpaymentPolicy) Holds(paid int64) bool {
	switch p {
	case UnderpaymentHold:
		return true
	case UnderpaymentGrant:
		return false
	default: // UnderpaymentHoldZero, and nothing else reaches here: an unknown policy is refused where it is given
		return paid <= 0
	}
}
