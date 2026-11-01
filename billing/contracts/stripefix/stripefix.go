// Package stripefix is the Stripe adapter's test support: deliveries signed the way Stripe signs them, event bodies in
// the shapes Stripe documents for API version 2024-06-20, and a fake Stripe API that records what the adapter asks of
// it. The bodies are built from the documentation, not captured from an account — a sandbox rehearsal before the first
// real card is still owed (the adoption checklist).
package stripefix

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The secrets of the fixtures' two accounts. They are references' targets, never real keys.
//
//nolint:gosec // fixture values, not credentials
const (
	APIKeyA     = "fixture-api-key-a"
	APIKeyB     = "fixture-api-key-b"
	WebhookSecA = "fixture-webhook-secret-a"
	WebhookSecB = "fixture-webhook-secret-b"
	APIVersion  = "2024-06-20"
)

// Request is one call the adapter made to the fake API.
type Request struct {
	Method, Path string
	Form         url.Values
	Header       http.Header
}

// Fixtures is a Stripe world: two accounts, a clock, a catalogue, and a fake API.
type Fixtures struct {
	Clock *mcontracts.FakeClock

	api  *httptest.Server
	mu   sync.Mutex
	reqs []Request

	// Subscriptions is what GET /v1/subscriptions/{id} answers, by id.
	Subscriptions map[string]map[string]any
	// DeclineCharges makes POST /v1/payment_intents answer a card error.
	DeclineCharges bool
	// FailAll makes every API call answer a 500.
	FailAll bool
	// ChargeStatus, when not zero, is the HTTP status POST /v1/payment_intents answers with, as an error.
	ChargeStatus int
	// IntentStatus, when not empty, is the status of the payment intent a charge creates (default "succeeded").
	IntentStatus string
	// ManagedPayments is an account that has Stripe's Managed Payments on by default, as a new sandbox does: a Checkout Session
	// created at the API version this adapter is pinned to is refused (the feature needs a later one), unless the call opts
	// out with managed_payments[enabled]=false. The refusal is what Stripe really answers: a 400 invalid_request_error with
	// no code and no param, only a message.
	ManagedPayments bool
	// ManagedPaymentsEvenOptedOut refuses the session even with the opt-out, as an account Stripe will not exempt would.
	ManagedPaymentsEvenOptedOut bool
	// CheckoutStatus, when not zero, is the HTTP status POST /v1/checkout/sessions answers with, as an error.
	CheckoutStatus int

	customers map[string]string // idempotency key → customer id
	charges   map[string]string
	n         int
}

// New starts the fake API; it is stopped with the test.
func New(t testing.TB) *Fixtures {
	t.Helper()
	f := &Fixtures{Clock: mcontracts.NewFakeClock(contracts.Start), Subscriptions: map[string]map[string]any{},
		customers: map[string]string{}, charges: map[string]string{}}
	f.api = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.api.Close)
	return f
}

// Stop shuts the fake API down, as an outage of Stripe's does.
func (f *Fixtures) Stop() { f.api.Close() }

// APIURL is where the fake API listens.
func (f *Fixtures) APIURL() string { return f.api.URL }

// Requests are the calls made so far.
func (f *Fixtures) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Last is the most recent call to a path prefix with a method.
func (f *Fixtures) Last(method, pathPrefix string) (Request, bool) {
	rs := f.Requests()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Method == method && strings.HasPrefix(rs[i].Path, pathPrefix) {
			return rs[i], true
		}
	}
	return Request{}, false
}

// Secrets resolves the references the fixtures' accounts hold.
func (f *Fixtures) Secrets() ports.SecretResolver { return secrets{} }

type secrets struct{}

func (secrets) Resolve(_ context.Context, ref string) (string, error) {
	switch ref {
	case "env:STRIPE_SECRET_KEY_A":
		return APIKeyA, nil
	case "env:STRIPE_SECRET_KEY_B":
		return APIKeyB, nil
	case "env:STRIPE_WEBHOOK_SECRET_A":
		return WebhookSecA, nil
	case "env:STRIPE_WEBHOOK_SECRET_B":
		return WebhookSecB, nil
	}
	return "", fmt.Errorf("stripefix: no secret behind %q", ref)
}

// Catalog is the world's catalogue, which maps the fixtures' price ids to plans for both accounts.
func (f *Fixtures) Catalog() ports.Catalog {
	c := contracts.NewMemCatalog(&contracts.CallLog{})
	for _, w := range contracts.WorldPlans() {
		for _, acct := range []string{f.Account().ID, f.OtherAccount().ID} {
			c.Add(w.Plan, acct, w.Currency, w.ProviderPriceID)
		}
	}
	return c
}

func (f *Fixtures) account(id, secretKey, webhook string) domain.ProviderAccount {
	return domain.ProviderAccount{ID: id, Provider: "stripe", Realms: []domain.Realm{md.DefaultRealm}, Mode: "test",
		SecretRef: "env:STRIPE_SECRET_KEY_" + secretKey, WebhookSecretRef: "env:STRIPE_WEBHOOK_SECRET_" + webhook, Active: true}
}

// Account implements contracts.ProviderFixtures.
func (f *Fixtures) Account() domain.ProviderAccount { return f.account("stripe-a", "A", "A") }

// OtherAccount implements contracts.ProviderFixtures.
func (f *Fixtures) OtherAccount() domain.ProviderAccount { return f.account("stripe-b", "B", "B") }

// Now implements contracts.ProviderFixtures.
func (f *Fixtures) Now() time.Time { return f.Clock.Now() }

// SignatureHeader computes the Stripe-Signature header for a body: t=<unix>,v1=<hex HMAC-SHA256(secret, t.body)>.
func SignatureHeader(secret string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", at.Unix())
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// Sign returns the headers of a delivery to an account, signed with the secret at the instant.
func (f *Fixtures) Sign(secret string, at time.Time, body []byte) http.Header {
	h := http.Header{}
	h.Set("Stripe-Signature", SignatureHeader(secret, at, body))
	return h
}

// Event wraps an object in Stripe's event envelope.
func (f *Fixtures) Event(id, typ string, created time.Time, object map[string]any) []byte {
	b, err := json.Marshal(map[string]any{"id": id, "object": "event", "api_version": APIVersion, "created": created.Unix(), "livemode": false,
		"type": typ, "data": map[string]any{"object": object}})
	if err != nil {
		panic(err)
	}
	return b
}

// Invoice is an invoice.paid object: a paid invoice for one price, with the payment intent that paid it.
func Invoice(id, customer, paymentIntent, subscription, price string, amount int64) map[string]any {
	inv := map[string]any{"id": id, "object": "invoice", "customer": customer, "payment_intent": paymentIntent, "amount_paid": amount, "amount_due": amount,
		"currency": "usd", "tax": 81, "status": "paid", "hosted_invoice_url": "https://invoice.stripe.test/i/" + id,
		"lines": map[string]any{"data": []any{map[string]any{"id": "il_1", "price": map[string]any{"id": price}}}}}
	if subscription != "" {
		inv["subscription"] = subscription
	}
	return inv
}

// Charge is a charge.refunded object: cumulative amount_refunded, with the refunds newest first.
func Charge(id, customer, paymentIntent, invoice string, amount, refunded int64, refundIDs ...string) map[string]any {
	refunds := make([]any, 0, len(refundIDs))
	for _, r := range refundIDs {
		refunds = append(refunds, map[string]any{"id": r})
	}
	ch := map[string]any{"id": id, "object": "charge", "customer": customer, "payment_intent": paymentIntent, "amount": amount, "amount_refunded": refunded,
		"currency": "usd", "refunds": map[string]any{"data": refunds}}
	if invoice != "" {
		ch["invoice"] = invoice
	}
	return ch
}

// Dispute is a charge.dispute.* object.
func Dispute(id, paymentIntent string, amount int64, status string) map[string]any {
	return map[string]any{"id": id, "object": "dispute", "charge": "ch_1", "payment_intent": paymentIntent, "amount": amount, "currency": "usd", "status": status}
}

// Subscription is a customer.subscription.* object.
func Subscription(id, customer, price, status string, start, end time.Time, cancelAtEnd bool) map[string]any {
	return map[string]any{"id": id, "object": "subscription", "customer": customer, "status": status, "current_period_start": start.Unix(),
		"current_period_end": end.Unix(), "cancel_at_period_end": cancelAtEnd,
		"items": map[string]any{"data": []any{map[string]any{"id": "si_1", "price": map[string]any{"id": price}}}}}
}

// PaymentIntent is a payment_intent.* object.
func PaymentIntent(id, customer string, amount int64, invoice any, method string) map[string]any {
	return map[string]any{"id": id, "object": "payment_intent", "customer": customer, "amount": amount, "currency": "usd", "invoice": invoice,
		"payment_method": method, "last_payment_error": map[string]any{"code": "card_declined", "message": "Your card was declined."}}
}

func (f *Fixtures) signed(id, typ string, at time.Time, object map[string]any) ([]byte, http.Header) {
	body := f.Event(id, typ, at, object)
	return body, f.Sign(WebhookSecA, at, body)
}

// SignedPaymentSucceeded implements contracts.ProviderFixtures: an invoice.paid for $10.00 of the pack, paid by pay-1.
func (f *Fixtures) SignedPaymentSucceeded() ([]byte, http.Header) {
	return f.signed("evt_ok", "invoice.paid", f.Now(), Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
}

// TamperedPaymentSucceeded implements contracts.ProviderFixtures.
func (f *Fixtures) TamperedPaymentSucceeded() ([]byte, http.Header) {
	b, h := f.SignedPaymentSucceeded()
	return []byte(strings.Replace(string(b), `"amount_paid":1000`, `"amount_paid":100000`, 1)), h
}

// SignedAt implements contracts.ProviderFixtures.
func (f *Fixtures) SignedAt(at time.Time) ([]byte, http.Header) {
	return f.signed("evt_at", "invoice.paid", at, Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
}

// SignedWithSecretOf implements contracts.ProviderFixtures.
func (f *Fixtures) SignedWithSecretOf(other domain.ProviderAccount) ([]byte, http.Header) {
	secret := WebhookSecB
	if other.ID == f.Account().ID {
		secret = WebhookSecA
	}
	body := f.Event("evt_other", "invoice.paid", f.Now(), Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	return body, f.Sign(secret, f.Now(), body)
}

// SignedCumulativeRefund implements contracts.ProviderFixtures: a charge refunded `first` and then `second` more.
func (f *Fixtures) SignedCumulativeRefund(first, second int64) ([]byte, http.Header) {
	return f.signed("evt_refund", "charge.refunded", f.Now(), Charge("ch_1", "cus_1", "pay-1", "", 1000, first+second, "re_2", "re_1"))
}

// SignedUnknownType implements contracts.ProviderFixtures.
func (f *Fixtures) SignedUnknownType() ([]byte, http.Header) {
	return f.signed("evt_unknown", "customer.created", f.Now(), map[string]any{"id": "cus_1", "object": "customer"})
}

// Checkout implements contracts.ProviderFixtures.
func (f *Fixtures) Checkout() ports.CheckoutInput {
	return ports.CheckoutInput{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}, Mode: "payment", PlanCode: contracts.PackCode, Currency: "USD",
		ProviderPriceID: contracts.PackPriceID, CustomerID: "cus_1", BillingEmail: "buyer@example.test", SuccessURL: "https://app.example.test/ok",
		CancelURL: "https://app.example.test/no", IdemKey: "checkout-1", Metadata: map[string]string{"plan": contracts.PackCode}}
}

var _ contracts.ProviderFixtures = (*Fixtures)(nil)

// ---------------------------------------------------------------- the fake API --

func (f *Fixtures) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.reqs = append(f.reqs, Request{r.Method, r.URL.Path, r.Form, r.Header.Clone()})
	fail, decline, chargeStatus, intentStatus := f.FailAll, f.DeclineCharges, f.ChargeStatus, f.IntentStatus
	managed, managedAlways, checkoutStatus := f.ManagedPayments, f.ManagedPaymentsEvenOptedOut, f.CheckoutStatus
	f.n++
	n := f.n
	f.mu.Unlock()

	w.Header().Set("Date", f.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	apiErr := func(code int, typ, errCode, msg string) {
		reply(code, map[string]any{"error": map[string]any{"type": typ, "code": errCode, "message": msg}})
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if auth != APIKeyA && auth != APIKeyB {
		apiErr(401, "invalid_request_error", "api_key_invalid", "Invalid API Key provided")
		return
	}
	if fail {
		apiErr(500, "api_error", "", "An unknown error occurred")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/customers":
		f.mu.Lock()
		id, ok := f.customers[key]
		if !ok {
			id = fmt.Sprintf("cus_new_%d", len(f.customers)+1)
			if key != "" {
				f.customers[key] = id
			}
		}
		f.mu.Unlock()
		reply(200, map[string]any{"id": id, "object": "customer"})
	case r.Method == http.MethodPost && path == "/v1/checkout/sessions":
		if checkoutStatus != 0 {
			apiErr(checkoutStatus, "invalid_request_error", "", "Invalid line_items[0]: something Stripe does not like.")
			return
		}
		if (managed && r.Form.Get("managed_payments[enabled]") != "false") || managedAlways {
			apiErr(400, "invalid_request_error", "", "Managed Payments is not supported on API version "+APIVersion+". Update your API version, or set the API Version of this request to 2025-03-31.basil or greater.")
			return
		}
		reply(200, map[string]any{"id": fmt.Sprintf("cs_%d", n), "object": "checkout.session", "url": fmt.Sprintf("https://checkout.stripe.test/c/pay/cs_%d", n)})
	case r.Method == http.MethodPost && path == "/v1/billing_portal/sessions":
		reply(200, map[string]any{"id": "bps_1", "url": "https://billing.stripe.test/p/session/" + r.Form.Get("customer")})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/subscriptions/"):
		f.mu.Lock()
		sub, ok := f.Subscriptions[strings.TrimPrefix(path, "/v1/subscriptions/")]
		f.mu.Unlock()
		if !ok {
			apiErr(404, "invalid_request_error", "resource_missing", "No such subscription")
			return
		}
		reply(200, sub)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/subscriptions/"):
		reply(200, map[string]any{"id": strings.TrimPrefix(path, "/v1/subscriptions/"), "object": "subscription"})
	case r.Method == http.MethodPost && path == "/v1/subscription_schedules":
		start, end := f.Now().Add(-24*time.Hour).Unix(), f.Now().Add(29*24*time.Hour).Unix()
		reply(200, map[string]any{"id": "sub_sched_1", "object": "subscription_schedule", "phases": []any{map[string]any{"start_date": start, "end_date": end,
			"items": []any{map[string]any{"price": "price_pro", "quantity": 1}}}}})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/subscription_schedules/"):
		reply(200, map[string]any{"id": strings.TrimPrefix(path, "/v1/subscription_schedules/"), "object": "subscription_schedule"})
	case r.Method == http.MethodPost && path == "/v1/payment_intents":
		if decline {
			apiErr(402, "card_error", "card_declined", "Your card was declined.")
			return
		}
		if chargeStatus != 0 {
			apiErr(chargeStatus, "api_error", "", "The charge was not made.")
			return
		}
		f.mu.Lock()
		id, ok := f.charges[key]
		if !ok {
			id = fmt.Sprintf("pi_new_%d", len(f.charges)+1)
			if key != "" {
				f.charges[key] = id
			}
		}
		f.mu.Unlock()
		status := "succeeded"
		if intentStatus != "" {
			status = intentStatus
		}
		reply(200, map[string]any{"id": id, "object": "payment_intent", "status": status})
	default:
		apiErr(404, "invalid_request_error", "resource_missing", "Unrecognized request URL: "+r.Method+" "+path)
	}
}
