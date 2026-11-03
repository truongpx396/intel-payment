package domain

import "time"

// SubStatus is where a scope's subscription stands. The names are the payment layer's and the schema's: this layer cannot
// import the payment layer, so it states them for itself, and a parity test holds the two together.
type SubStatus string

// The six states a subscription can be in.
const (
	Trialing   SubStatus = "trialing"
	Active     SubStatus = "active"
	PastDue    SubStatus = "past_due"
	Paused     SubStatus = "paused"
	Canceled   SubStatus = "canceled"
	Incomplete SubStatus = "incomplete"
)

// Valid reports whether s is one of the six.
func (s SubStatus) Valid() bool {
	switch s {
	case Trialing, Active, PastDue, Paused, Canceled, Incomplete:
		return true
	}
	return false
}

// Subscription is the part of a scope's paid plan that decides what it is entitled to: which plan, in what state, and until
// when a failed payment is forgiven.
type Subscription struct {
	PlanCode   string
	Status     SubStatus
	GraceUntil *time.Time
}

// Holds reports whether the subscription entitles its scope to what the plan unlocks at now. A failed payment does not
// instantly revoke (payments invariant 13): a past_due subscription holds until the instant GraceUntil is reached, and not
// after. Anything that is not active, trialing or past_due inside grace holds nothing — the scope falls back to the default
// tier.
func (s Subscription) Holds(now time.Time) bool {
	switch s.Status {
	case Active, Trialing:
		return true
	case PastDue:
		return s.GraceUntil != nil && now.Before(*s.GraceUntil)
	}
	return false
}

// Offer is a plan a realm sells, with what it unlocks. It is how a refusal finds the plan that would permit what was asked.
type Offer struct {
	PlanCode     string
	Entitlements []Entitlement
}
