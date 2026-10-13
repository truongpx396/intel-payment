package contracts

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Ingress is stage 1 as the suite sees it: it answers a delivery, and it retries the deliveries it parked.
type Ingress interface {
	ports.WebhookIngress
	// Retry verifies the parked deliveries again (billing.webhook.tick).
	Retry(ctx context.Context) error
}

// IngressFactory builds the ingress under test over a harness's collaborators: the harness's provider (registered
// as "fake"), accounts, inbox, metrics and clock, with MaxWebhookBody as the body limit.
type IngressFactory func(t testing.TB, h *Harness) Ingress

// IngressCase is one subtest of IngressContract.
type IngressCase struct {
	Name string
	Run  func(t testing.TB, mk IngressFactory)
}

const (
	metricReceived     = "billing_webhook_received_total"
	metricVerifyFailed = "billing_webhook_verify_failed_total"
)

func handle(ing Ingress, provider, account string, body []byte, hdr http.Header) ports.IngressResult {
	return ing.Handle(Ctx(), provider, account, body, hdr)
}

func wantStatus(t testing.TB, got ports.IngressResult, status int, what string) {
	t.Helper()
	if got.Status != status {
		t.Fatalf("%s: status %d (%s), want %d", what, got.Status, got.Code, status)
	}
}

// moneyOrStore are the collaborators that must stay untouched until a delivery is verified.
var storePrefixes = []string{"inbox.", "customers.", "payments.", "subs.", "catalog.", "meter.", "lots."}

// IngressCases are the subtests of IngressContract, in order.
func IngressCases() []IngressCase {
	return []IngressCase{
		{"verifies before anything", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Log.Reset()
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, "a genuine delivery")
			calls := h.Log.Calls()
			v := h.Log.Index("provider.VerifyWebhook")
			if v < 0 {
				t.Fatalf("a delivery was acknowledged without being verified: %v", calls)
			}
			for _, c := range calls[:v] {
				for _, p := range storePrefixes {
					if strings.HasPrefix(c, p) {
						t.Fatalf("%s was touched before the signature was verified (invariant 1): %v", c, calls)
					}
				}
			}

			bad, badHdr := h.Fixtures().TamperedPaymentSucceeded()
			h.Log.Reset()
			wantStatus(t, handle(ing, "fake", AccountID, bad, badHdr), 400, "a tampered delivery")
			if touched := h.Log.Touched(storePrefixes...); len(touched) > 0 {
				t.Fatalf("a forged delivery touched %v: nothing may be read, written or looked up for it", touched)
			}
		}},
		{"a forged delivery is 400, stores nothing and is counted as a security event", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			fx := h.Fixtures()
			tampered, tHdr := fx.TamperedPaymentSucceeded()
			signed, _ := fx.SignedPaymentSucceeded()
			other, oHdr := fx.SignedWithSecretOf(fx.OtherAccount())
			for name, d := range map[string]struct {
				body []byte
				hdr  http.Header
			}{"tampered": {tampered, tHdr}, "unsigned": {signed, nil}, "another account's secret": {other, oHdr}} {
				res := handle(ing, "fake", AccountID, d.body, d.hdr)
				if res.Status != 400 || res.Code != "invalid_signature" {
					t.Fatalf("%s: got %d %q, want 400 invalid_signature", name, res.Status, res.Code)
				}
			}
			if n := h.Inbox.Count() + len(h.Inbox.Parked()); n != 0 {
				t.Fatalf("a forged delivery left %d rows in the inbox", n)
			}
			if got := h.Metrics.Counter(metricVerifyFailed, mports.Labels{"provider": "fake"}); got != 3 {
				t.Fatalf("%s{provider=fake} = %d, want 3: a signature failure is a security event", metricVerifyFailed, got)
			}
		}},
		{"persists before acknowledging", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))

			h.Inbox.FailPersist(errors.New("database is down"))
			res := handle(ing, "fake", AccountID, body, hdr)
			if res.Status < 500 || res.Status > 599 {
				t.Fatalf("persisting failed and the delivery was answered %d: a 2xx now loses the payment, only a 5xx makes the provider retry (invariant 5)", res.Status)
			}
			h.Inbox.FailPersist(nil)

			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, "the provider's retry")
			if n := h.Inbox.Count(); n != 1 {
				t.Fatalf("200 was returned with %d events in the inbox: the acknowledgement came before the write", n)
			}
		}},
		{"a replay answers 200 and persists once", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, "the first delivery")
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, "the replay: an error here makes the provider retry for days")

			var wg sync.WaitGroup
			results := make([]int, 8)
			for i := range results {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i] = handle(ing, "fake", AccountID, body, hdr).Status
				}()
			}
			wg.Wait()
			for i, s := range results {
				if s != 200 {
					t.Errorf("concurrent delivery %d answered %d", i, s)
				}
			}
			if n := h.Inbox.Count(); n != 1 {
				t.Fatalf("10 deliveries of one event persisted %d rows", n)
			}
			if got := h.Metrics.Counter(metricReceived, mports.Labels{"provider": "fake", "result": "new"}); got != 1 {
				t.Fatalf("%s{result=new} = %d, want 1", metricReceived, got)
			}
			if got := h.Metrics.Counter(metricReceived, mports.Labels{"provider": "fake", "result": "duplicate"}); got != 9 {
				t.Fatalf("%s{result=duplicate} = %d, want 9", metricReceived, got)
			}
		}},
		{"ingress never touches money", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			for _, fe := range []FakeEvent{
				h.PaymentSucceeded("evt_1", "pay-1"),
				{ID: "evt_2", Type: string(domain.PaymentRefunded), Created: h.Now(), Customer: "cus_1", Payment: "pay-1", Refund: "re_1", Refunded: 300, Amount: 1000, Currency: "USD"},
				{ID: "evt_3", Type: string(domain.DisputeFundsWithdrawn), Created: h.Now(), Customer: "cus_1", Payment: "pay-1", Dispute: "dp_1"},
				{ID: "evt_4", Type: string(domain.SubscriptionUpdated), Created: h.Now(), Customer: "cus_1", Subscription: "sub_1", Status: "active"},
			} {
				body, hdr := h.Deliver(fe)
				wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, fe.ID)
			}
			if touched := h.Log.Touched("meter.", "payments.", "subs.", "customers.", "catalog.", "lots."); len(touched) > 0 {
				t.Fatalf("stage 1 must only persist; it touched %v", touched)
			}
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("the ingress granted credits: fulfilment is stage 2's, from the inbox")
			}
		}},
		{"an unknown, inactive or foreign account is 404 and nothing is verified", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			for name, route := range map[string][2]string{
				"an account that does not exist":     {"fake", "nope"},
				"an inactive account":                {"fake", InactiveID},
				"an account of another provider":     {"fake", ElsewhereID},
				"a provider nobody registered":       {"ghost", AccountID},
				"a route that names neither":         {"", ""},
				"an account with a path in its name": {"fake", "../acct"},
			} {
				res := handle(ing, route[0], route[1], body, hdr)
				if res.Status != 404 || res.Code != "unknown_account" {
					t.Errorf("%s: got %d %q, want 404 unknown_account", name, res.Status, res.Code)
				}
			}
			if n := h.Provider.VerifyCalls(); n != 0 {
				t.Fatalf("a delivery to an account that is not there was verified %d times: the route names the secret", n)
			}
			if h.Inbox.Count()+len(h.Inbox.Parked()) != 0 {
				t.Fatal("a 404 left a row behind")
			}
			for _, s := range h.Metrics.Series() {
				if strings.Contains(s, "ghost") || strings.Contains(s, "nope") {
					t.Fatalf("metric series %q is keyed on what an unauthenticated caller typed: unbounded cardinality", s)
				}
			}
		}},
		{"unavailable verification parks the raw body and answers 202", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Provider.SetDown(true)
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 202, "verification is down")
			parked := h.Inbox.Parked()
			if len(parked) != 1 {
				t.Fatalf("a delivery that could be neither verified nor rejected was dropped: %d parked (invariant 14)", len(parked))
			}
			if string(parked[0].ParkedBody) != string(body) {
				t.Fatal("the parked body is not the raw body, byte for byte: it could never be verified later")
			}
			if parked[0].ParkedHeader.Get(SignatureHeader) != hdr.Get(SignatureHeader) {
				t.Fatal("the parked headers lost the signature")
			}
			if h.Inbox.Count() != 0 {
				t.Fatal("an unverified delivery was filed as a verified event")
			}
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 202, "the provider's redelivery")
			if n := len(h.Inbox.Parked()); n != 1 {
				t.Fatalf("a redelivery of the same bytes parked %d rows", n)
			}
			if got := h.Metrics.Counter(metricReceived, mports.Labels{"provider": "fake", "result": "parked"}); got < 1 {
				t.Fatalf("%s{result=parked} = %d: a parked delivery must be visible", metricReceived, got)
			}
		}},
		{"a parked delivery is verified later and filed like any other", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Provider.SetDown(true)
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 202, "verification is down")
			h.Provider.SetDown(false)

			if err := ing.Retry(Ctx()); err != nil {
				t.Fatal(err)
			}
			if n := h.Inbox.Count(); n != 1 {
				t.Fatalf("a parked delivery that now verifies must become an event; the inbox holds %d", n)
			}
			if n := len(h.Inbox.Parked()); n != 0 {
				t.Fatalf("%d deliveries are still parked", n)
			}
			for _, r := range h.Inbox.Rows() {
				if r.ParkedBody != nil {
					t.Fatal("the raw body must be deleted the moment it verifies (PII)")
				}
			}
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, "the provider's own redelivery afterwards")
			if n := h.Inbox.Count(); n != 1 {
				t.Fatalf("the redelivery made it %d events", n)
			}
		}},
		{"a parked delivery that fails verification is rejected, never processed", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			bad, hdr := h.Fixtures().TamperedPaymentSucceeded()
			h.Provider.SetDown(true)
			wantStatus(t, handle(ing, "fake", AccountID, bad, hdr), 202, "verification is down: a forgery looks like anything else")
			h.Provider.SetDown(false)

			if err := ing.Retry(Ctx()); err != nil {
				t.Fatal(err)
			}
			if h.Inbox.Count() != 0 {
				t.Fatal("a delivery that failed verification on retry became an event: parked is not trusted")
			}
			if n := len(h.Inbox.Parked()); n != 0 {
				t.Fatalf("a forgery stays parked forever (%d rows)", n)
			}
			rejected := 0
			for _, r := range h.Inbox.Rows() {
				if r.Status == RowRejected {
					rejected++
					if r.ParkedBody != nil {
						t.Fatal("a rejected delivery's body must be dropped")
					}
				}
			}
			if rejected != 1 {
				t.Fatalf("%d rejected rows, want 1", rejected)
			}
			if got := h.Metrics.Counter(metricVerifyFailed, mports.Labels{"provider": "fake"}); got != 1 {
				t.Fatalf("%s = %d, want 1: a late forgery is still a security event", metricVerifyFailed, got)
			}
		}},
		{"a parked delivery stays parked while verification is down, and backs off", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Provider.SetDown(true)
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 202, "verification is down")

			if err := ing.Retry(Ctx()); err != nil {
				t.Fatal(err)
			}
			parked := h.Inbox.Parked()
			if len(parked) != 1 || h.Inbox.Count() != 0 {
				t.Fatalf("still unverifiable: want it parked (%d) and not an event (%d)", len(parked), h.Inbox.Count())
			}
			if !parked[0].NextAttemptAt.After(h.Clock.Now()) {
				t.Fatal("a retry that finds verification still down must schedule the next attempt, or it hammers a service that is down")
			}
			calls := h.Provider.VerifyCalls()
			if err := ing.Retry(Ctx()); err != nil {
				t.Fatal(err)
			}
			if h.Provider.VerifyCalls() != calls {
				t.Fatal("a parked delivery was retried before its time")
			}
		}},
		{"an oversized body is refused before it is verified", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			huge := []byte(strings.Repeat("x", MaxWebhookBody+1))
			wantStatus(t, handle(ing, "fake", AccountID, huge, nil), 413, "a body over the limit")
			if h.Provider.VerifyCalls() != 0 {
				t.Fatal("an oversized body reached the verifier")
			}
			if res := handle(ing, "fake", AccountID, huge[:MaxWebhookBody], nil); res.Status == 413 {
				t.Fatal("a body of exactly the limit is within it")
			}
		}},
		{"an event that names another account than the route's is refused", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			h.Provider.ReturnWrongAccount(true)
			body, hdr := h.Deliver(h.PaymentSucceeded("evt_1", "pay-1"))
			res := handle(ing, "fake", AccountID, body, hdr)
			if res.Status < 500 {
				t.Fatalf("an adapter defect was answered %d: it must fail closed and be retried or paged, not filed", res.Status)
			}
			if h.Inbox.Count() != 0 {
				t.Fatal("an event for the wrong account was persisted")
			}
		}},
		{"an ignored event is persisted and acknowledged", func(t testing.TB, mk IngressFactory) {
			h := NewHarness()
			ing := mk(t, h)
			body, hdr := h.Fixtures().SignedUnknownType()
			wantStatus(t, handle(ing, "fake", AccountID, body, hdr), 200, "a type nobody subscribed to")
			rows := h.Inbox.Rows()
			if len(rows) != 1 || rows[0].Event.Type != domain.Ignored {
				t.Fatalf("want the Ignored event persisted once, got %+v", rows)
			}
		}},
	}
}

// IngressContract is the specification of stage 1 of the webhook flow (invariants 1, 5, 6, 14; FR-018, FR-020):
// verify before anything, persist before acknowledging, answer a replay 200, and park what cannot be verified
// with its raw body instead of dropping it or trusting it. It runs against the app over a fake provider and
// in-memory stores.
func IngressContract(t *testing.T, mk IngressFactory) {
	t.Helper()
	for _, c := range IngressCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}
