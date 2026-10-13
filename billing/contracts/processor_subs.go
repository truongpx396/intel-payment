package contracts

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// entitlementEvents is the reasons billing gave for telling the entitlement layer a scope changed.
func entitlementReasons(h *Harness, scope md.Scope) []string {
	var out []string
	for _, m := range h.Bus.On("billing.entitlement." + scope.SubjectToken()) {
		if r, ok := m["reason"].(string); ok {
			out = append(out, r)
		}
	}
	return out
}

// SubscriptionCases are the subtests of ProcessorSubscriptions (invariants 12, 13, 15; FR-025, FR-026).
func SubscriptionCases() []ProcessorCase {
	t1 := Start.Add(time.Hour)
	return []ProcessorCase{
		{"out-of-order subscription events converge to the newest state (15)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenProviderSub("sub-1", domain.Canceled, t1)                                      // what the provider says now
			h.Persisted(h.SubscriptionEvent("evt_a", "sub-1", domain.SubscriptionUpdated, Start)) // an old notice, delivered late
			h.Persisted(h.SubscriptionEvent("evt_b", "sub-1", domain.SubscriptionDeleted, t1))
			h.Rounds(t, p, 2, time.Minute)
			wantRow(t, h, "evt_a", RowProcessed)
			wantRow(t, h, "evt_b", RowProcessed)
			got := h.Subs.Row("sub-1")
			if got.Status != domain.Canceled || !got.LastEventAt.Equal(t1) || got.Scope != h.Scope || got.Account != AccountID || got.PlanCode != ProCode {
				t.Fatalf("subscription = %+v: delivery order decided state (invariant 15)", got)
			}
			if n := count(entitlementReasons(h, h.Scope), "subscription"); n != 1 {
				t.Fatalf("the change was announced %d times, want 1: the second event found nothing new", n)
			}
		}},
		{"a stale read of the provider never overwrites a newer state", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			held := h.GivenProviderSub("sub-1", domain.Canceled, t1)
			held.LastEventAt = t1
			h.Subs.Put(held)
			h.GivenProviderSub("sub-1", domain.Active, Start) // a lagging replica answers with older state
			h.Persisted(h.SubscriptionEvent("evt_a", "sub-1", domain.SubscriptionUpdated, t1))
			h.Tick(t, p)
			wantRow(t, h, "evt_a", RowProcessed)
			if got := h.Subs.Row("sub-1"); got.Status != domain.Canceled || !got.LastEventAt.Equal(t1) {
				t.Fatalf("an older state replaced a newer one: %+v", got)
			}
			if len(entitlementReasons(h, h.Scope)) != 0 {
				t.Fatal("a change that was not applied was announced")
			}
		}},
		{"a failed payment holds entitlements until GraceUntil and then suspends (13)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenProviderSub("sub-2", domain.PastDue, Start)
			h.Persisted(h.PaymentFailed("evt_f", "in-9", "sub-2"))
			h.Clock.Advance(3 * time.Hour) // the worker gets to it late: the window is the provider's to date, not ours
			h.Tick(t, p)

			grace := Start.Add(GraceWindow)
			s := h.Subs.Row("sub-2")
			if s.Status != domain.PastDue || s.GraceUntil == nil || !s.GraceUntil.Equal(grace) {
				t.Fatalf("subscription = %+v, want past_due with GraceUntil %s", s, grace)
			}
			if !s.HoldsEntitlements(grace.Add(-time.Second)) || s.HoldsEntitlements(grace) {
				t.Fatal("entitlements must hold until the instant GraceUntil is reached, and not after")
			}
			pay := h.Payments.Row("in-9")
			if pay.Status != domain.Failed || pay.FailureReason != "card_declined" || pay.CreditsGranted != 0 {
				t.Fatalf("payment = %+v", pay)
			}
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("a failed payment moved credits: it holds, it does not revoke")
			}
			if !contains(paymentEvents(h, h.Scope), "failed") || count(entitlementReasons(h, h.Scope), "subscription") != 1 {
				t.Fatalf("announced %v / %v", paymentEvents(h, h.Scope), entitlementReasons(h, h.Scope))
			}

			// Inside the window nothing is announced; at its instant the end is announced once.
			h.Clock.Advance(grace.Add(-time.Second).Sub(h.Clock.Now()))
			h.Tick(t, p)
			if count(entitlementReasons(h, h.Scope), "grace_expired") != 0 {
				t.Fatal("the grace was announced over before it ended")
			}
			h.Clock.Advance(time.Second)
			h.Tick(t, p)
			if n := count(entitlementReasons(h, h.Scope), "grace_expired"); n != 1 {
				t.Fatalf("the end of the grace was announced %d times at its instant, want 1", n)
			}
			if h.Subs.Row("sub-2").GraceLapsedAt == nil {
				t.Fatal("the announcement was not recorded, so every sweep would repeat it")
			}
			h.Rounds(t, p, 2, time.Hour)
			h.Tick(t, mk(t, h)) // another processor
			if n := count(entitlementReasons(h, h.Scope), "grace_expired"); n != 1 {
				t.Fatalf("the end of the grace was announced %d times in all, want once", n)
			}
		}},
		{"a past_due state carries the grace forward, and a recovery clears it", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenProviderSub("sub-2", domain.PastDue, Start)
			h.Persisted(h.PaymentFailed("evt_f", "in-9", "sub-2"))
			h.Tick(t, p)
			grace := Start.Add(GraceWindow)

			t2 := Start.Add(time.Hour)
			h.GivenProviderSub("sub-2", domain.PastDue, t2) // a newer notice of the same trouble
			h.Persisted(h.SubscriptionEvent("evt_u", "sub-2", domain.SubscriptionUpdated, t2))
			h.Rounds(t, p, 1, time.Minute)
			s := h.Subs.Row("sub-2")
			if !s.LastEventAt.Equal(t2) || s.GraceUntil == nil || !s.GraceUntil.Equal(grace) {
				t.Fatalf("subscription = %+v: a newer past_due must not move the grace window", s)
			}

			t3 := Start.Add(2 * time.Hour)
			h.GivenProviderSub("sub-2", domain.Active, t3) // the customer updated their card
			h.Persisted(h.SubscriptionEvent("evt_r", "sub-2", domain.SubscriptionUpdated, t3))
			h.Rounds(t, p, 1, time.Minute)
			if s := h.Subs.Row("sub-2"); s.Status != domain.Active || s.GraceUntil != nil {
				t.Fatalf("subscription = %+v: recovery clears the grace", s)
			}
		}},
		{"a failed payment while the provider still says active sets no grace", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenProviderSub("sub-2", domain.Active, Start)
			h.Persisted(h.PaymentFailed("evt_f", "in-9", "sub-2"))
			h.Tick(t, p)
			if s := h.Subs.Row("sub-2"); s.Status != domain.Active || s.GraceUntil != nil {
				t.Fatalf("subscription = %+v: state is the provider's, not ours", s)
			}
			if h.Payments.Row("in-9").Status != domain.Failed {
				t.Fatal("the failed payment is still recorded")
			}
		}},
		{"a cancellation is announced and a deleted subscription is canceled", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenProviderSub("sub-3", domain.Canceled, Start)
			h.Persisted(h.SubscriptionEvent("evt_d", "sub-3", domain.SubscriptionDeleted, Start))
			h.Tick(t, p)
			if s := h.Subs.Row("sub-3"); s.Status != domain.Canceled {
				t.Fatalf("subscription = %+v", s)
			}
			if !contains(paymentEvents(h, h.Scope), "canceled") || !contains(entitlementReasons(h, h.Scope), "subscription") {
				t.Fatalf("announced %v / %v", paymentEvents(h, h.Scope), entitlementReasons(h, h.Scope))
			}
		}},
		{"status is written only when the processor handles the event (12)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			s := h.GivenProviderSub("sub-1", domain.Active, Start)
			s.CancelAtPeriodEnd = true
			h.Provider.SetSubscription("sub-1", s, Start) // the provider has been told to cancel at period end
			if got := h.Subs.Row("sub-1"); got.ProviderSubscriptionID != "" {
				t.Fatalf("the subscription appeared before any event was processed: %+v", got)
			}
			if touched := h.Log.Touched("subs."); len(touched) != 0 {
				t.Fatalf("subscriptions were touched before the processor ran: %v", touched)
			}
			h.Persisted(h.SubscriptionEvent("evt_a", "sub-1", domain.SubscriptionUpdated, Start))
			h.Tick(t, p)
			if got := h.Subs.Row("sub-1"); !got.CancelAtPeriodEnd || got.Status != domain.Active {
				t.Fatalf("subscription = %+v: local state follows the verified event", got)
			}
		}},
		{"a subscription on a plan the catalogue does not know is ignored, not guessed", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			s := h.GivenProviderSub("sub-1", domain.Active, Start)
			s.PlanCode = "" // the adapter could not map the provider's price to a plan
			h.Provider.SetSubscription("sub-1", s, Start)
			h.Persisted(h.SubscriptionEvent("evt_a", "sub-1", domain.SubscriptionUpdated, Start))
			h.Tick(t, p)
			wantRow(t, h, "evt_a", RowIgnored)
			if h.Subs.Row("sub-1").ProviderSubscriptionID != "" {
				t.Fatal("a subscription was written with no plan")
			}
			if got := h.Metrics.Counter(metricIgnored, fake("reason", "unknown_price")); got != 1 {
				t.Fatalf("%s{unknown_price} = %d, want 1", metricIgnored, got)
			}
		}},
		{"a failed one-time payment is recorded and touches no subscription", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentFailed("evt_f", "pay-7", "")
			fe.Price, fe.Amount = PackPriceID, 1000
			h.Persisted(fe)
			h.Tick(t, p)
			pay := h.Payments.Row("pay-7")
			if pay.Status != domain.Failed || pay.Kind != "one_time" || pay.CreditsGranted != 0 {
				t.Fatalf("payment = %+v", pay)
			}
			// (Every tick also lists the grace windows that have ended: that is the sweep, not this payment.)
			if touched := h.Log.Touched("subs.Get", "subs.ApplyIfNewer", "subs.MarkGraceLapsed"); len(touched) != 0 {
				t.Fatalf("a one-time payment touched a subscription: %v", touched)
			}
		}},
		{"a failure does not undo a payment that succeeded, whatever the order", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_s", "pay-1"))
			h.Clock.Advance(time.Second)
			fe := h.PaymentFailed("evt_f", "pay-1", "")
			fe.Price, fe.Amount = PackPriceID, 1000
			h.Persisted(fe)
			h.Rounds(t, p, 2, time.Minute)
			wantRow(t, h, "evt_s", RowProcessed)
			wantRow(t, h, "evt_f", RowProcessed)
			if got := h.Payments.Row("pay-1"); got.Status != domain.Succeeded || got.CreditsGranted != PackCredits {
				t.Fatalf("payment = %+v: a failed attempt followed a success, and the success stands", got)
			}
		}},
		{"a payment that failed and then succeeded is fulfilled once", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentFailed("evt_f", "pay-1", "")
			fe.Price, fe.Amount = PackPriceID, 1000
			h.Persisted(fe)
			h.Tick(t, p)
			if got := h.Payments.Row("pay-1"); got.Status != domain.Failed || got.CreditsGranted != 0 {
				t.Fatalf("payment = %+v after the first attempt", got)
			}
			h.Clock.Advance(time.Second)
			h.Persisted(h.PaymentSucceeded("evt_s", "pay-1")) // the customer's next attempt, under the same payment
			h.Rounds(t, p, 1, time.Minute)
			got := h.Payments.Row("pay-1")
			if got.Status != domain.Succeeded || got.FailureReason != "" || got.CreditsGranted != PackCredits || h.Meter.Grants("pay-1") != 1 {
				t.Fatalf("payment = %+v, grants %d: a payment that eventually succeeds is granted what it bought, once", got, h.Meter.Grants("pay-1"))
			}
		}},
		{"a fulfilled payment is announced once, as succeeded or renewed", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_a", "pay-1"))
			h.Clock.Advance(time.Second)
			h.Persisted(h.PaymentSucceeded("evt_b", "pay-1")) // the same purchase under a second event
			h.Clock.Advance(time.Second)
			h.Persisted(h.SubscriptionPaymentSucceeded("evt_c", "in-1", "sub-1"))
			h.Rounds(t, p, 3, time.Minute)
			ev := paymentEvents(h, h.Scope)
			if count(ev, "succeeded") != 1 || count(ev, "renewed") != 1 {
				t.Fatalf("announced %v, want one succeeded (the pack) and one renewed (the invoice)", ev)
			}
		}},
	}
}

// ProcessorSubscriptions is the specification of subscription state (invariants 12, 13, 15; FR-025, FR-026): state
// is the provider's current object, applied only if newer, so delivery order never decides it; a failed payment
// holds entitlements through a grace window and then they lapse, announced once; and status has one writer.
func ProcessorSubscriptions(t *testing.T, mk ProcessorFactory) {
	t.Helper()
	runCases(t, SubscriptionCases(), mk)
}
