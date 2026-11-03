package domain_test

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
)

// R101: the entitlement layer cannot import the payment layer, so it states for itself which subscription states entitle a
// scope. The rule must be the payment layer's (Sub.HoldsEntitlements, payments invariant 13); a parity test in integration/
// holds the two together. A failed payment does not instantly revoke: past_due holds until the instant GraceUntil, and not after.
func TestWhichSubscriptionsHoldTheirPlansEntitlements(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	cases := []struct {
		name   string
		status domain.SubStatus
		grace  *time.Time
		held   bool
	}{
		{"active", domain.Active, nil, true},
		{"active, a stale grace window is irrelevant", domain.Active, at(-time.Hour), true},
		{"trialing", domain.Trialing, nil, true},
		{"past_due a nanosecond inside grace", domain.PastDue, at(time.Nanosecond), true},
		{"past_due exactly at the end of grace", domain.PastDue, at(0), false}, // the instant it ends is already over
		{"past_due a nanosecond past grace", domain.PastDue, at(-time.Nanosecond), false},
		{"past_due with no grace window", domain.PastDue, nil, false},
		{"paused", domain.Paused, at(time.Hour), false},
		{"canceled", domain.Canceled, at(time.Hour), false},
		{"incomplete", domain.Incomplete, at(time.Hour), false},
		{"a status nobody defined", "unpaid", at(time.Hour), false},
		{"no status at all", "", at(time.Hour), false},
	}
	for _, c := range cases {
		s := domain.Subscription{PlanCode: "pro", Status: c.status, GraceUntil: c.grace}
		if got := s.Holds(now); got != c.held {
			t.Errorf("%s: Holds = %v, want %v", c.name, got, c.held)
		}
	}
}

func TestSubStatusNamesAreThePaymentLayersAndTheSchemas(t *testing.T) {
	t.Parallel()
	for want, s := range map[string]domain.SubStatus{
		"trialing": domain.Trialing, "active": domain.Active, "past_due": domain.PastDue,
		"paused": domain.Paused, "canceled": domain.Canceled, "incomplete": domain.Incomplete,
	} {
		if string(s) != want || !s.Valid() {
			t.Errorf("%q: valid=%v", s, s.Valid())
		}
	}
	if domain.SubStatus("unpaid").Valid() || domain.SubStatus("").Valid() {
		t.Error("a status the schema's CHECK refuses is not a status")
	}
}
