package domain

import (
	"strings"
	"time"
)

// Live reports whether the status counts toward the one-live-subscription-per-scope rule.
func (s Sub) Live() bool {
	switch s.Status {
	case Trialing, Active, PastDue, Paused:
		return true
	}
	return false
}

// HoldsEntitlements reports whether the subscription entitles its scope to what the plan unlocks at `now`.
// A failed payment does not instantly revoke (invariant 13): a past_due subscription holds until the instant
// GraceUntil is reached, and not after.
func (s Sub) HoldsEntitlements(now time.Time) bool {
	switch s.Status {
	case Active, Trialing:
		return true
	case PastDue:
		return s.GraceUntil != nil && now.Before(*s.GraceUntil)
	}
	return false
}

// GraceDue reports whether a past_due subscription's grace window has ended and the end has not been announced
// yet — the sweep's work: publish the expiry once, so entitlement caches evict at once rather than at their TTL.
func (s Sub) GraceDue(now time.Time) bool {
	return s.Status == PastDue && s.GraceUntil != nil && !now.Before(*s.GraceUntil) && s.GraceLapsedAt == nil
}

// Apply is the pure model of the store's conditional write (Subs.ApplyIfNewer): s is the state held, next is the
// provider's CURRENT object and at is the provider's timestamp of it. It returns the state to hold and whether
// next was applied. Delivery order never decides state (invariant 15): the newer instant wins, whichever
// arrived first.
//
// Providers stamp events to the second, so equal instants are normal. They are broken by a fixed total order —
// the more final status first, then every remaining field — so that any permutation of any set of events
// converges to the same state, and a redelivery of the state already held is not applied (and so not announced
// twice). An object of a different subscription is not this one's to overwrite.
func (s Sub) Apply(next Sub, at time.Time) (Sub, bool) {
	next.LastEventAt = at
	if s.ProviderSubscriptionID == "" && s.LastEventAt.IsZero() {
		return next, true
	}
	if s.Account != next.Account || s.ProviderSubscriptionID != next.ProviderSubscriptionID {
		return s, false
	}
	if compareState(next, s) > 0 {
		return next, true
	}
	return s, false
}

// statusRank orders statuses by how final they are: when two events share an instant, the later in a
// subscription's life wins.
func statusRank(s SubStatus) int {
	switch s {
	case Incomplete:
		return 0
	case Trialing:
		return 1
	case Active:
		return 2
	case PastDue:
		return 3
	case Paused:
		return 4
	case Canceled:
		return 5
	}
	return -1
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

func compareTimePtr(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.Compare(*b)
}

// compareState is a total order on the versions of one subscription: the instant, then how final the status
// is, then every other field in a fixed order. Equal means identical.
func compareState(a, b Sub) int {
	if c := a.LastEventAt.Compare(b.LastEventAt); c != 0 {
		return c
	}
	if ra, rb := statusRank(a.Status), statusRank(b.Status); ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	for _, c := range []int{
		strings.Compare(a.PlanCode, b.PlanCode),
		strings.Compare(string(a.Scope.Realm), string(b.Scope.Realm)),
		strings.Compare(a.Scope.Kind, b.Scope.Kind),
		strings.Compare(a.Scope.ID, b.Scope.ID),
		a.CurrentPeriodStart.Compare(b.CurrentPeriodStart),
		a.CurrentPeriodEnd.Compare(b.CurrentPeriodEnd),
		compareBool(a.CancelAtPeriodEnd, b.CancelAtPeriodEnd),
		compareTimePtr(a.GraceUntil, b.GraceUntil),
		compareTimePtr(a.GraceLapsedAt, b.GraceLapsedAt),
	} {
		if c != 0 {
			return c
		}
	}
	return 0
}
