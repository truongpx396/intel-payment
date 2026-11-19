package contracts

import (
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// AnnouncementCases are the subtests of the announcement suite. A processor commits what an event did and then announces it,
// and an event whose announcement failed is retried: "a notification is never lost to a bus that blinked". The retry runs the
// event again over a payment or a subscription that already holds the change, so a processor that announces only the change
// it makes itself announces nothing the second time, and the notification is lost for good — a host never told a payment
// succeeded, a customer who just upgraded kept on a cached entitlement until its TTL, an auto top-up's slot never released.
func AnnouncementCases() []ProcessorCase {
	blink := errors.New("the bus blinked")
	// retried fails the first attempt's announcement, then brings the bus back and lets the inbox's retry run.
	retried := func(t testing.TB, h *Harness, p ports.WebhookProcessor, eventID string) {
		t.Helper()
		h.Bus.FailPublish(blink)
		h.Tick(t, p)
		wantRow(t, h, eventID, RowFailed)
		h.Bus.FailPublish(nil)
		h.Rounds(t, p, 3, time.Hour)
		wantRow(t, h, eventID, RowProcessed)
	}
	return []ProcessorCase{
		{"a payment fulfilled while the bus is down is announced, once, when its event is retried", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			retried(t, h, p, "evt_1")
			if got := count(paymentEvents(h, h.Scope), "succeeded"); got != 1 {
				t.Fatalf("the payment was announced %d times (%q), want once: the grant was made and the host was never told", got, paymentEvents(h, h.Scope))
			}
			if h.Meter.Grants("pay-1") != 1 {
				t.Fatalf("grants = %d, want 1", h.Meter.Grants("pay-1"))
			}
		}},
		{"a refund applied while the bus is down is announced, once, when its event is retried", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-1", 1000, PackCredits)
			h.Persisted(h.Refund("evt_1", "pay-1", "re-1", 400))
			retried(t, h, p, "evt_1")
			if got := count(paymentEvents(h, h.Scope), "refunded"); got != 1 {
				t.Fatalf("the refund was announced %d times (%q), want once", got, paymentEvents(h, h.Scope))
			}
		}},
		{"a dispute applied while the bus is down is announced, once, when its event is retried", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-1", 1000, PackCredits)
			h.Persisted(h.Dispute("evt_1", domain.DisputeOpened, "pay-1", "dp-1"))
			retried(t, h, p, "evt_1")
			if got := count(paymentEvents(h, h.Scope), "disputed"); got != 1 {
				t.Fatalf("the dispute was announced %d times (%q), want once", got, paymentEvents(h, h.Scope))
			}
		}},
		{"a subscription written while the bus is down evicts the entitlement caches, once, when its event is retried", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			t1 := Start.Add(time.Hour)
			h.GivenProviderSub("sub-1", domain.Active, t1)
			h.Persisted(h.SubscriptionEvent("evt_1", "sub-1", domain.SubscriptionUpdated, t1))
			retried(t, h, p, "evt_1")
			if got := count(entitlementReasons(h, h.Scope), "subscription"); got != 1 {
				t.Fatalf("the subscription was announced %d times, want once: a customer who just paid keeps the entitlements they had until the cache's TTL", got)
			}
		}},
		{"the slot of an auto top-up whose success could not be announced is released when the event is retried", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			w.Recharges.SetFailures(w.Scope, 2) // two earlier refusals: a confirmed good charge clears them
			_, key := w.rechargeInFlight(t, "pi_1")
			w.Persisted(w.PaymentSucceeded("evt_1", "pi_1"))
			retried(t, w.Harness, p, "evt_1")
			if n := w.Meter.Grants(key); n != 1 {
				t.Fatalf("the credits were granted %d times", n)
			}
			if got := w.instruction(t); got.InflightN != 0 || got.ConsecutiveFailed != 0 {
				t.Fatalf("instruction = %+v: the charge was confirmed, so its slot is released and the failures cleared — not left held until it goes stale", got)
			}
		}},
		{"an event redelivered after its payment was fulfilled and announced announces nothing more", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Tick(t, p)
			h.Persisted(h.PaymentSucceeded("evt_2", "pay-1")) // invoice.paid and checkout.session.completed, for one purchase
			h.Tick(t, p)
			wantRow(t, h, "evt_2", RowProcessed)
			if got := count(paymentEvents(h, h.Scope), "succeeded"); got != 1 {
				t.Fatalf("one purchase was announced %d times (%q), want once", got, paymentEvents(h, h.Scope))
			}
		}},
	}
}

// ProcessorAnnouncements is the specification of what is announced when an event is retried.
func ProcessorAnnouncements(t *testing.T, mk ProcessorFactory) {
	t.Helper()
	runCases(t, AnnouncementCases(), mk)
}
