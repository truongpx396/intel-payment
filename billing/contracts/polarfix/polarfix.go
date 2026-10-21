// Package polarfix is the Polar adapter's test support: deliveries signed the way Polar signs them (the Standard
// Webhooks scheme, and the older Polar HMAC that secrets made before 8 September 2026 still use), event bodies in the
// shapes Polar documents, and a fake Polar API that records what the adapter asks of it. The bodies are built from the
// documentation; the ones under captured/ (see Captured) are real deliveries from a Polar sandbox, which the adapter's
// captured_test.go holds it to. They are of a free order: a paid Polar order, a refund and a subscription renewal have not
// been captured.
//
// For Polar the "provider price id" the engine stores is the Polar PRODUCT id: it is what a checkout is created for and
// what every order and subscription names.
package polarfix

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// The fixtures' secrets: the targets of the references the accounts hold, never real tokens. A webhook secret is
// `whsec_` and the base64 of its key, as Polar issues them.
//
//nolint:gosec // fixture values, not credentials
const (
	TokenA      = "fixture-polar-token-a"
	TokenB      = "fixture-polar-token-b"
	WebhookSecA = "whsec_Zml4dHVyZS1wb2xhci13ZWJob29rLWEtMzItYnl0ZXMhIQ=="
	WebhookSecB = "whsec_Zml4dHVyZS1wb2xhci13ZWJob29rLWItMzItYnl0ZXMhIQ=="
)

// Request is one call the adapter made to the fake API.
type Request struct {
	Method, Path string
	Body         map[string]any
	Header       http.Header
}

// Fixtures is a Polar world.
type Fixtures struct {
	Clock *mcontracts.FakeClock

	api  *httptest.Server
	mu   sync.Mutex
	reqs []Request

	// Subscriptions is what GET /v1/subscriptions/{id} answers.
	Subscriptions map[string]map[string]any
	// ExistingCustomers are the external ids Polar already has a customer for (the Polar id by external id).
	ExistingCustomers map[string]string
	// EmailsInUse are the emails Polar already has a customer for: an email is unique within an organization.
	EmailsInUse map[string]bool
	FailAll     bool
	n           int
}

// New starts the fake API; it is stopped with the test.
func New(t testing.TB) *Fixtures {
	t.Helper()
	f := &Fixtures{Clock: mcontracts.NewFakeClock(contracts.Start), Subscriptions: map[string]map[string]any{},
		ExistingCustomers: map[string]string{}, EmailsInUse: map[string]bool{}}
	f.api = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.api.Close)
	return f
}

// APIURL is where the fake API listens.
func (f *Fixtures) APIURL() string { return f.api.URL }

// Requests are the calls made so far.
func (f *Fixtures) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.reqs...)
}

// Last is the most recent call with the exact method and path.
func (f *Fixtures) Last(method, path string) (Request, bool) {
	rs := f.Requests()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Method == method && rs[i].Path == path {
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
	case "env:POLAR_ACCESS_TOKEN_A":
		return TokenA, nil
	case "env:POLAR_ACCESS_TOKEN_B":
		return TokenB, nil
	case "env:POLAR_WEBHOOK_SECRET_A":
		return WebhookSecA, nil
	case "env:POLAR_WEBHOOK_SECRET_B":
		return WebhookSecB, nil
	}
	return "", fmt.Errorf("polarfix: no secret behind %q", ref)
}

// Catalog maps the world's product ids to plans for both accounts.
func (f *Fixtures) Catalog() ports.Catalog {
	c := contracts.NewMemCatalog(&contracts.CallLog{})
	for _, w := range contracts.WorldPlans() {
		for _, acct := range []string{f.Account().ID, f.OtherAccount().ID} {
			c.Add(w.Plan, acct, w.Currency, w.ProviderPriceID)
		}
	}
	return c
}

func account(id, suffix string) domain.ProviderAccount {
	return domain.ProviderAccount{ID: id, Provider: "polar", Realms: []domain.Realm{md.DefaultRealm}, Mode: "test",
		SecretRef: "env:POLAR_ACCESS_TOKEN_" + suffix, WebhookSecretRef: "env:POLAR_WEBHOOK_SECRET_" + suffix, Active: true}
}

// Account implements contracts.ProviderFixtures.
func (f *Fixtures) Account() domain.ProviderAccount { return account("polar-a", "A") }

// OtherAccount implements contracts.ProviderFixtures.
func (f *Fixtures) OtherAccount() domain.ProviderAccount { return account("polar-b", "B") }

// Now implements contracts.ProviderFixtures.
func (f *Fixtures) Now() time.Time { return f.Clock.Now() }

// StandardKey is the HMAC key of a secret under the Standard Webhooks scheme: the base64 after `whsec_`, decoded.
func StandardKey(secret string) []byte {
	k, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		panic(err)
	}
	return k
}

// LegacyKey is the HMAC key of a secret under Polar's own scheme, which secrets made before 8 September 2026 use: the
// UTF-8 bytes of the whole `whsec_…` string, prefix included.
func LegacyKey(secret string) []byte { return []byte(secret) }

// SignatureWith is the `v1,<base64>` signature of `id.timestamp.body` under key.
func SignatureWith(key []byte, id string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", id, at.Unix())
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Headers are the three headers of a delivery.
func Headers(id string, at time.Time, signatures ...string) http.Header {
	h := http.Header{}
	h.Set("webhook-id", id)
	h.Set("webhook-timestamp", fmt.Sprint(at.Unix()))
	h.Set("webhook-signature", strings.Join(signatures, " "))
	return h
}

// Sign signs a delivery the Standard Webhooks way, as a secret made now is signed.
func Sign(secret, id string, at time.Time, body []byte) http.Header {
	return Headers(id, at, SignatureWith(StandardKey(secret), id, at, body))
}

// SignLegacy signs a delivery the way Polar signed before the Standard Webhooks key.
func SignLegacy(secret, id string, at time.Time, body []byte) http.Header {
	return Headers(id, at, SignatureWith(LegacyKey(secret), id, at, body))
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// Event wraps data in Polar's envelope.
func (f *Fixtures) Event(typ string, at time.Time, data map[string]any) []byte {
	b, err := json.Marshal(map[string]any{"type": typ, "timestamp": stamp(at), "data": data})
	if err != nil {
		panic(err)
	}
	return b
}

// Order is an order object: total is what the customer was charged, tax included (the figure Polar calls
// total_amount); 81 of it is sales tax. A subscription id of "" is the JSON null Polar sends for a one-time order.
func Order(id, customer, product string, total int64, status, subscription string) map[string]any {
	o := map[string]any{"id": id, "status": status, "paid": status == "paid" || status == "partially_refunded" || status == "refunded",
		"total_amount": total, "net_amount": total - 81, "subtotal_amount": total - 81, "discount_amount": 0, "tax_amount": 81,
		"refunded_amount": 0, "refunded_tax_amount": 0, "currency": "usd", "customer_id": customer, "product_id": product,
		"billing_reason": "purchase", "subscription_id": nil, "created_at": stamp(contracts.Start)}
	if subscription != "" {
		o["subscription_id"], o["billing_reason"] = subscription, "subscription_cycle"
	}
	return o
}

// Refunded marks an order refunded to date: refunded of it is the sales tax, so Polar reports the two apart and the
// customer got back their sum.
func Refunded(o map[string]any, net, tax int64) map[string]any {
	o["refunded_amount"], o["refunded_tax_amount"] = net, tax
	return o
}

// Subscription is a subscription object.
func Subscription(id, customer, product, status string, start, end time.Time, cancelAtEnd bool) map[string]any {
	return map[string]any{"id": id, "status": status, "customer_id": customer, "product_id": product, "cancel_at_period_end": cancelAtEnd,
		"current_period_start": stamp(start), "current_period_end": stamp(end), "recurring_interval": "month", "currency": "usd"}
}

func (f *Fixtures) signed(id, typ string, at time.Time, data map[string]any) ([]byte, http.Header) {
	body := f.Event(typ, at, data)
	return body, Sign(WebhookSecA, id, at, body)
}

// SignedPaymentSucceeded implements contracts.ProviderFixtures: a paid order for $10.00 of the pack.
func (f *Fixtures) SignedPaymentSucceeded() ([]byte, http.Header) {
	return f.signed("msg_ok", "order.paid", f.Now(), Order("pay-1", "cus_1", contracts.PackPriceID, 1000, "paid", ""))
}

// TamperedPaymentSucceeded implements contracts.ProviderFixtures.
func (f *Fixtures) TamperedPaymentSucceeded() ([]byte, http.Header) {
	b, h := f.SignedPaymentSucceeded()
	return []byte(strings.Replace(string(b), `"total_amount":1000`, `"total_amount":100000`, 1)), h
}

// SignedAt implements contracts.ProviderFixtures.
func (f *Fixtures) SignedAt(at time.Time) ([]byte, http.Header) {
	return f.signed("msg_at", "order.paid", at, Order("pay-1", "cus_1", contracts.PackPriceID, 1000, "paid", ""))
}

// SignedWithSecretOf implements contracts.ProviderFixtures.
func (f *Fixtures) SignedWithSecretOf(other domain.ProviderAccount) ([]byte, http.Header) {
	secret := WebhookSecB
	if other.ID == f.Account().ID {
		secret = WebhookSecA
	}
	body := f.Event("order.paid", f.Now(), Order("pay-1", "cus_1", contracts.PackPriceID, 1000, "paid", ""))
	return body, Sign(secret, "msg_other", f.Now(), body)
}

// SignedCumulativeRefund implements contracts.ProviderFixtures: Polar reports what an order has been refunded to date,
// the money (refunded_amount) and its sales tax (refunded_tax_amount) apart.
func (f *Fixtures) SignedCumulativeRefund(first, second int64) ([]byte, http.Header) {
	total, tax := first+second, int64(40)
	o := Refunded(Order("pay-1", "cus_1", contracts.PackPriceID, 1000, "partially_refunded", ""), total-tax, tax)
	return f.signed("msg_refund", "order.refunded", f.Now(), o)
}

// SignedUnknownType implements contracts.ProviderFixtures.
func (f *Fixtures) SignedUnknownType() ([]byte, http.Header) {
	return f.signed("msg_unknown", "benefit_grant.created", f.Now(), map[string]any{"id": "bg_1"})
}

// Checkout implements contracts.ProviderFixtures.
func (f *Fixtures) Checkout() ports.CheckoutInput {
	return ports.CheckoutInput{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}, Mode: "payment", PlanCode: contracts.PackCode, Currency: "USD",
		ProviderPriceID: contracts.PackPriceID, Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, CustomerID: "cus_1", BillingEmail: "buyer@example.test",
		SuccessURL: "https://app.example.test/ok", CancelURL: "https://app.example.test/no", IdemKey: "checkout-1", Metadata: map[string]string{"plan": contracts.PackCode}}
}

var _ contracts.ProviderFixtures = (*Fixtures)(nil)

// ---------------------------------------------------------------- the fake API --

func (f *Fixtures) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, Request{r.Method, r.URL.Path, body, r.Header.Clone()})
	fail := f.FailAll
	f.n++
	n := f.n
	f.mu.Unlock()

	w.Header().Set("Date", f.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token != TokenA && token != TokenB {
		reply(401, map[string]any{"error": "Unauthorized", "detail": "The access token is invalid."})
		return
	}
	if fail {
		// Echo the token the way a careless upstream error page might, so a test can see it scrubbed.
		reply(500, map[string]any{"error": "InternalServerError", "detail": "boom while handling " + token})
		return
	}
	path := r.URL.Path
	id := path[strings.LastIndex(path, "/")+1:]
	switch {
	case r.Method == http.MethodPost && path == "/v1/customers/":
		ext, _ := body["external_id"].(string)
		email, _ := body["email"].(string)
		f.mu.Lock()
		_, extTaken := f.ExistingCustomers[ext]
		taken := extTaken || f.EmailsInUse[email]
		if !taken {
			f.ExistingCustomers[ext] = fmt.Sprintf("cus_new_%d", len(f.ExistingCustomers)+1)
			f.EmailsInUse[email] = true
		}
		cid := f.ExistingCustomers[ext]
		f.mu.Unlock()
		if taken {
			reply(422, map[string]any{"detail": []any{map[string]any{"type": "value_error", "loc": []any{"body", "email"}, "msg": "A customer with this email address already exists."}}})
			return
		}
		reply(201, map[string]any{"id": cid, "external_id": ext, "email": email})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/customers/external/"):
		ext := strings.TrimPrefix(path, "/v1/customers/external/")
		f.mu.Lock()
		cid, ok := f.ExistingCustomers[ext]
		f.mu.Unlock()
		if !ok {
			reply(404, map[string]any{"error": "ResourceNotFound", "detail": "Customer not found"})
			return
		}
		reply(200, map[string]any{"id": cid, "external_id": ext})
	case r.Method == http.MethodPost && path == "/v1/checkouts/":
		if ps, _ := body["products"].([]any); len(ps) == 0 {
			reply(422, map[string]any{"detail": []any{map[string]any{"type": "missing", "loc": []any{"body", "products"}, "msg": "Field required"}}})
			return
		}
		reply(201, map[string]any{"id": fmt.Sprintf("co_%d", n), "status": "open", "url": fmt.Sprintf("https://buy.polar.test/checkout/co_%d", n)})
	case r.Method == http.MethodPost && path == "/v1/customer-sessions/":
		reply(201, map[string]any{"id": "cs_1", "customer_portal_url": "https://polar.test/portal/" + fmt.Sprint(body["customer_id"])})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/subscriptions/"):
		f.mu.Lock()
		sub, ok := f.Subscriptions[id]
		f.mu.Unlock()
		if !ok {
			reply(404, map[string]any{"error": "ResourceNotFound", "detail": "Subscription not found"})
			return
		}
		reply(200, sub)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/subscriptions/"):
		f.mu.Lock()
		sub, ok := f.Subscriptions[id]
		f.mu.Unlock()
		switch {
		case !ok:
			reply(404, map[string]any{"error": "ResourceNotFound", "detail": "Subscription not found"})
		case body["cancel_at_period_end"] == true && sub["cancel_at_period_end"] == true:
			reply(403, map[string]any{"error": "AlreadyCanceledSubscription", "detail": "This subscription is already canceled or will be at the end of the period."})
		default:
			reply(200, sub)
		}
	default:
		reply(404, map[string]any{"error": "ResourceNotFound", "detail": "no route " + r.Method + " " + path})
	}
}
