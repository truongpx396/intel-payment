// Package domain is the fiat layer's vocabulary and its pure rules: provider accounts, plans, customers,
// subscriptions, payments and the normalized provider event — and the arithmetic that must be exact (the
// cumulative refund formula), the state machines that must converge (subscription state under any delivery
// order, a dispute's two phases) and the instants that must be unambiguous (the end of a grace window).
//
// It imports only metering's domain, and reads no clock and no store: every function takes what it needs, so
// the same computation runs in the processor, in a replay and in a test (payments contract, domain types).
package domain
