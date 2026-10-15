package domain

import "time"

// AutoRecharge is a scope's standing instruction: when the pool's balance crosses below Threshold, charge the saved
// payment method off-session for one pack of PlanCode. Credits still arrive only when the verified payment is
// processed (invariant 18). The method is the provider's reference — never card data.
type AutoRecharge struct {
	Scope             Scope
	Enabled           bool
	Pool              Pool
	Threshold         int64 // credits; the pool is recharged when it falls below this
	PlanCode          string
	Currency          string
	Account           string
	PaymentMethodRef  string
	MaxPerDay         int // distinct charges per scope per UTC day (1..50)
	LastTriggeredAt   *time.Time
	ConsecutiveFailed int    // failed charges since the last good one; three disable the instruction
	DisabledReason    string // why it was switched off, if it was
}
