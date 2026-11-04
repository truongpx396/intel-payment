package integration

import (
	"testing"
	"time"

	billing "github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/entitlement/domain"
)

// R101: the entitlement layer cannot import the payment layer, so it states for itself which subscriptions entitle a scope
// (Subscription.Holds), and the payment layer states it too (Sub.HoldsEntitlements, which its grace-expiry sweep is built on).
// Two statements of one rule agree only if something holds them together. This is that: every status the schema allows, every
// grace window relative to the instant asked — absent, an hour left, exactly over, an hour over — compared.
//
// It needs no database, so it runs with every `make test`.
func TestTheEntitlementLayerAndThePaymentLayerAgreeWhichSubscriptionsEntitle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	statuses := []string{"trialing", "active", "past_due", "paused", "canceled", "incomplete"}
	graces := map[string]*time.Time{
		"none":          nil,
		"an hour left":  ptr(now.Add(time.Hour)),
		"a nanosecond":  ptr(now.Add(time.Nanosecond)),
		"exactly over":  ptr(now),
		"an hour over":  ptr(now.Add(-time.Hour)),
		"a long ago":    ptr(now.Add(-1000 * time.Hour)),
		"a year ahead":  ptr(now.Add(365 * 24 * time.Hour)),
		"before it all": ptr(time.Time{}),
	}
	for _, st := range statuses {
		for name, grace := range graces {
			pay := billing.Sub{Status: billing.SubStatus(st), GraceUntil: grace}
			ent := domain.Subscription{Status: domain.SubStatus(st), GraceUntil: grace}
			if got, want := ent.Holds(now), pay.HoldsEntitlements(now); got != want {
				t.Errorf("%s with grace %s: the entitlement layer says %v, the payment layer says %v", st, name, got, want)
			}
		}
	}
	// The same six names, so a status the payment layer adds cannot slip past the entitlement layer unnoticed.
	for _, s := range []billing.SubStatus{billing.Trialing, billing.Active, billing.PastDue, billing.Paused, billing.Canceled, billing.Incomplete} {
		if !domain.SubStatus(s).Valid() {
			t.Errorf("the payment layer's status %q is not one the entitlement layer knows", s)
		}
	}
}

func ptr[T any](v T) *T { return &v }
