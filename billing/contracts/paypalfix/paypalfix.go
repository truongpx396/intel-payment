// Package paypalfix is the PayPal adapter's test support: deliveries as PayPal sends them (an envelope with an event
// type and a resource, five PAYPAL-* headers), and a fake PayPal API that does what PayPal does with them — it answers
// "verify this signature" by checking the delivery against the webhook it was configured for — and records what the
// adapter asks of it. The bodies these helpers build are from the documentation; the ones under captured/ (see Captured) are the
// PayPal webhook simulator's, which are not a buyer's payment. A real capture, refund and dispute have not been captured:
// that needs a buyer to approve an order in a browser.
//
// PayPal signs with RSA and a certificate the adapter never sees, so the fake signs with a private scheme of its own:
// an HMAC over the transmission id, the time, the webhook id and the CRC32 of the event's exact bytes. What the fixtures
// can show is that the adapter forwards the delivery faithfully and obeys the verdict — not that PayPal's crypto holds.
package paypalfix

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
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

// The fixtures' credentials: the targets of the references the accounts hold, never real ones. A PayPal account's API
// credentials are `client_id:client_secret`; its webhook "secret" is the id of the webhook as configured in the
// developer portal, which PayPal needs to verify a delivery.
//
//nolint:gosec // fixture values, not credentials
const (
	CredentialsA = "fixture-client-a:fixture-secret-a"
	CredentialsB = "fixture-client-b:fixture-secret-b"
	WebhookIDA   = "WH-FIXTUREA0000000"
	WebhookIDB   = "WH-FIXTUREB0000000"
	signingKey   = "the-fake-paypals-private-signing-key"
	// CertURL is where the certificate of every fixture delivery is said to be.
	CertURL = "https://api.paypal.test/v1/notifications/certs/CERT-FIXTURE"
	// ExpiresIn is how long a token the fake issues lives, in seconds.
	ExpiresIn = 32400
)

// Request is one call the adapter made to the fake API.
type Request struct {
	Method, Path string
	Raw          []byte
	Body         map[string]any
	Header       http.Header
}

// Fixtures is a PayPal world.
type Fixtures struct {
	Clock *mcontracts.FakeClock

	api  *httptest.Server
	mu   sync.Mutex
	reqs []Request

	// Subscriptions is what GET /v1/billing/subscriptions/{id} answers.
	Subscriptions map[string]map[string]any
	// VerifyStatus, when not zero, is the HTTP status the verify call answers with instead of a verdict.
	VerifyStatus int
	// TokenStatus, when not zero, is the HTTP status the token call answers with instead of a token.
	TokenStatus int
	// FailAll makes every API call other than token and verify answer 500, echoing the token it was given.
	FailAll bool
	// Verdict, when not empty, is the verification_status the verify call answers with whatever the delivery is.
	Verdict string
	// RejectAllTokens makes every token, old or new, unauthorized.
	RejectAllTokens bool
	tokens          int
	minToken        int // tokens issued before this one have been revoked
	n               int
	orders          map[string]string // PayPal-Request-Id → order or subscription id
}

// New starts the fake API; it is stopped with the test.
func New(t testing.TB) *Fixtures {
	t.Helper()
	f := &Fixtures{Clock: mcontracts.NewFakeClock(contracts.Start), Subscriptions: map[string]map[string]any{}, orders: map[string]string{}}
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

// Count is how many calls were made to the exact method and path.
func (f *Fixtures) Count(method, path string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
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

// TokensIssued is how many access tokens the fake has handed out.
func (f *Fixtures) TokensIssued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

// Stop shuts the fake API down, as an outage of PayPal's does.
func (f *Fixtures) Stop() { f.api.Close() }

// RevokeTokens makes every token issued so far unauthorized, as PayPal does when one is withdrawn or expires early.
func (f *Fixtures) RevokeTokens() {
	f.mu.Lock()
	f.minToken = f.tokens + 1
	f.mu.Unlock()
}

// Secrets resolves the references the fixtures' accounts hold.
func (f *Fixtures) Secrets() ports.SecretResolver { return secrets{} }

type secrets struct{}

func (secrets) Resolve(_ context.Context, ref string) (string, error) {
	switch ref {
	case "env:PAYPAL_CREDENTIALS_A":
		return CredentialsA, nil
	case "env:PAYPAL_CREDENTIALS_B":
		return CredentialsB, nil
	case "env:PAYPAL_WEBHOOK_ID_A":
		return WebhookIDA, nil
	case "env:PAYPAL_WEBHOOK_ID_B":
		return WebhookIDB, nil
	}
	return "", fmt.Errorf("paypalfix: no secret behind %q", ref)
}

// Catalog maps the world's price ids to plans for both accounts. For PayPal a price id is a billing plan id, or, for a
// one-time purchase, whatever identifier the catalogue gives it.
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
	return domain.ProviderAccount{ID: id, Provider: "paypal", Realms: []domain.Realm{md.DefaultRealm}, Mode: "test",
		SecretRef: "env:PAYPAL_CREDENTIALS_" + suffix, WebhookSecretRef: "env:PAYPAL_WEBHOOK_ID_" + suffix, Active: true}
}

// Account implements contracts.ProviderFixtures.
func (f *Fixtures) Account() domain.ProviderAccount { return account("paypal-a", "A") }

// OtherAccount implements contracts.ProviderFixtures.
func (f *Fixtures) OtherAccount() domain.ProviderAccount { return account("paypal-b", "B") }

// Now implements contracts.ProviderFixtures.
func (f *Fixtures) Now() time.Time { return f.Clock.Now() }

// ---------------------------------------------------------------- signing --

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func expectedSignature(webhookID, transmissionID, transmissionTime string, event []byte) string {
	mac := hmac.New(sha256.New, []byte(signingKey))
	fmt.Fprintf(mac, "%s|%s|%s|%d", transmissionID, transmissionTime, webhookID, crc32.ChecksumIEEE(event))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Headers are the five headers of a delivery of body to webhookID, transmitted at `at`.
func Headers(webhookID, transmissionID string, at time.Time, body []byte) http.Header {
	h := http.Header{}
	h.Set("PAYPAL-AUTH-ALGO", "SHA256withRSA")
	h.Set("PAYPAL-CERT-URL", CertURL)
	h.Set("PAYPAL-TRANSMISSION-ID", transmissionID)
	h.Set("PAYPAL-TRANSMISSION-TIME", stamp(at))
	h.Set("PAYPAL-TRANSMISSION-SIG", expectedSignature(webhookID, transmissionID, stamp(at), body))
	return h
}

// Event wraps a resource in PayPal's envelope.
func (f *Fixtures) Event(id, typ string, at time.Time, resource map[string]any) []byte {
	b, err := json.Marshal(map[string]any{"id": id, "event_version": "1.0", "create_time": stamp(at), "resource_type": "resource",
		"event_type": typ, "summary": "fixture " + typ, "resource": resource})
	if err != nil {
		panic(err)
	}
	return b
}

func amount(value string) map[string]any {
	return map[string]any{"currency_code": "USD", "value": value}
}

// Capture is a capture resource: value is a decimal string, as PayPal sends amounts; customID is what the order was made with.
func Capture(id, customID, value string) map[string]any {
	return map[string]any{"id": id, "status": "COMPLETED", "amount": amount(value), "final_capture": true, "custom_id": customID,
		"links": []any{map[string]any{"href": "https://api.paypal.test/v2/payments/captures/" + id, "rel": "self", "method": "GET"},
			map[string]any{"href": "https://api.paypal.test/v2/checkout/orders/ORD-1", "rel": "up", "method": "GET"}}}
}

// Refund is a refund resource of a capture: value is this refund alone and total is everything refunded to date.
func Refund(id, captureID, value, total string) map[string]any {
	return map[string]any{"id": id, "status": "COMPLETED", "amount": amount(value),
		"seller_payable_breakdown": map[string]any{"gross_amount": amount(value), "total_refunded_amount": amount(total)},
		"links": []any{map[string]any{"href": "https://api.paypal.test/v2/payments/refunds/" + id, "rel": "self", "method": "GET"},
			map[string]any{"href": "https://api.paypal.test/v2/payments/captures/" + captureID, "rel": "up", "method": "GET"}}}
}

// Sale is the payment of one cycle of a subscription.
func Sale(id, subscriptionID, total string) map[string]any {
	return map[string]any{"id": id, "state": "completed", "billing_agreement_id": subscriptionID,
		"amount": map[string]any{"total": total, "currency": "USD", "details": map[string]any{"subtotal": total}}}
}

// Subscription is a subscription resource.
func Subscription(id, plan, status, customID string, start, next time.Time) map[string]any {
	return map[string]any{"id": id, "plan_id": plan, "status": status, "custom_id": customID, "start_time": stamp(start),
		"billing_info": map[string]any{"next_billing_time": stamp(next), "failed_payments_count": 0,
			"last_payment": map[string]any{"amount": amount("20.00"), "time": stamp(start)}}}
}

// Dispute is a dispute resource over a capture; outcome is PayPal's outcome_code, or "" for a dispute not decided.
func Dispute(id, captureID, status, outcome string) map[string]any {
	d := map[string]any{"dispute_id": id, "status": status, "dispute_life_cycle_stage": "CHARGEBACK",
		"disputed_transactions": []any{map[string]any{"seller_transaction_id": captureID, "gross_amount": amount("10.00")}}}
	if outcome != "" {
		d["dispute_outcome"] = map[string]any{"outcome_code": outcome}
	}
	return d
}

// CustomID is what a checkout for a customer and a price is made with, and what the capture of it carries back.
func CustomID(customer, price string) string { return customer + "~" + price }

func (f *Fixtures) signed(id, typ string, at time.Time, resource map[string]any) ([]byte, http.Header) {
	body := f.Event(id, typ, f.Now(), resource)
	return body, Headers(WebhookIDA, "tx-"+id, at, body)
}

// SignedPaymentSucceeded implements contracts.ProviderFixtures: a completed capture of $10.00 of the pack.
func (f *Fixtures) SignedPaymentSucceeded() ([]byte, http.Header) {
	return f.signed("WH-EVT-ok", "PAYMENT.CAPTURE.COMPLETED", f.Now(), Capture("pay-1", CustomID("cus_1", contracts.PackPriceID), "10.00"))
}

// TamperedPaymentSucceeded implements contracts.ProviderFixtures.
func (f *Fixtures) TamperedPaymentSucceeded() ([]byte, http.Header) {
	b, h := f.SignedPaymentSucceeded()
	return []byte(strings.Replace(string(b), `"value":"10.00"`, `"value":"1000.00"`, 1)), h
}

// SignedAt implements contracts.ProviderFixtures.
func (f *Fixtures) SignedAt(at time.Time) ([]byte, http.Header) {
	return f.signed("WH-EVT-at", "PAYMENT.CAPTURE.COMPLETED", at, Capture("pay-1", CustomID("cus_1", contracts.PackPriceID), "10.00"))
}

// SignedWithSecretOf implements contracts.ProviderFixtures: PayPal signed it for the webhook of the other account.
func (f *Fixtures) SignedWithSecretOf(other domain.ProviderAccount) ([]byte, http.Header) {
	id := WebhookIDB
	if other.ID == f.Account().ID {
		id = WebhookIDA
	}
	body := f.Event("WH-EVT-other", "PAYMENT.CAPTURE.COMPLETED", f.Now(), Capture("pay-1", CustomID("cus_1", contracts.PackPriceID), "10.00"))
	return body, Headers(id, "tx-other", f.Now(), body)
}

func decimal(minor int64) string { return fmt.Sprintf("%d.%02d", minor/100, minor%100) }

// SignedCumulativeRefund implements contracts.ProviderFixtures: PayPal reports a refund and, with it, the total
// refunded from the capture to date.
func (f *Fixtures) SignedCumulativeRefund(first, second int64) ([]byte, http.Header) {
	return f.signed("WH-EVT-refund", "PAYMENT.CAPTURE.REFUNDED", f.Now(), Refund("refund-2", "pay-1", decimal(second), decimal(first+second)))
}

// SignedUnknownType implements contracts.ProviderFixtures.
func (f *Fixtures) SignedUnknownType() ([]byte, http.Header) {
	return f.signed("WH-EVT-unknown", "CATALOG.PRODUCT.CREATED", f.Now(), map[string]any{"id": "PROD-1"})
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
	f.reqs = append(f.reqs, Request{r.Method, r.URL.Path, raw, body, r.Header.Clone()})
	f.n++
	n := f.n
	f.mu.Unlock()

	w.Header().Set("Date", f.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/token" {
		f.token(r, raw, reply)
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	account, num := tokenAccount(auth)
	f.mu.Lock()
	revoked := f.RejectAllTokens || num < f.minToken
	f.mu.Unlock()
	if account == "" || revoked {
		reply(401, map[string]any{"name": "AUTHENTICATION_FAILURE", "message": "Authentication failed due to invalid authentication credentials or a missing Authorization header."})
		return
	}
	f.mu.Lock()
	failAll, verifyStatus, verdict := f.FailAll, f.VerifyStatus, f.Verdict
	f.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/notifications/verify-webhook-signature":
		if verifyStatus != 0 {
			reply(verifyStatus, map[string]any{"name": "INTERNAL_SERVER_ERROR", "message": "An internal service error has occurred while handling " + auth})
			return
		}
		reply(200, map[string]any{"verification_status": f.verdict(raw, verdict)})
		return
	case failAll:
		reply(500, map[string]any{"name": "INTERNAL_SERVER_ERROR", "message": "boom while handling " + auth, "debug_id": "dbg-1"})
	case r.Method == http.MethodPost && path == "/v2/checkout/orders":
		reply(201, f.created(r, "ORD", n, "payer-action", "https://www.paypal.test/checkoutnow?token=%s"))
	case r.Method == http.MethodPost && path == "/v1/billing/subscriptions":
		reply(201, f.created(r, "I-SUB", n, "approve", "https://www.paypal.test/webapps/billing/subscriptions?ba_token=%s"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/billing/subscriptions/"):
		f.mu.Lock()
		sub, ok := f.Subscriptions[strings.TrimPrefix(path, "/v1/billing/subscriptions/")]
		f.mu.Unlock()
		if !ok {
			reply(404, map[string]any{"name": "RESOURCE_NOT_FOUND", "message": "The specified resource does not exist.", "debug_id": "dbg-2"})
			return
		}
		reply(200, sub)
	default:
		reply(404, map[string]any{"name": "RESOURCE_NOT_FOUND", "message": "no route " + r.Method + " " + path})
	}
}

// created answers a create-order or create-subscription call, once per PayPal-Request-Id: the same id is the same
// object, as PayPal's idempotency makes it.
func (f *Fixtures) created(r *http.Request, prefix string, n int, rel, hrefFormat string) map[string]any {
	key := r.Header.Get("PayPal-Request-Id")
	f.mu.Lock()
	id, seen := f.orders[key]
	if !seen || key == "" {
		id = fmt.Sprintf("%s-%d", prefix, n)
		if key != "" {
			f.orders[key] = id
		}
	}
	f.mu.Unlock()
	return map[string]any{"id": id, "status": "CREATED", "links": []any{
		map[string]any{"href": "https://api.paypal.test/v2/resource/" + id, "rel": "self", "method": "GET"},
		map[string]any{"href": fmt.Sprintf(hrefFormat, id), "rel": rel, "method": "GET"}}}
}

// tokenAccount is the account a fixture token was issued to ("A" or "B", or "" for a token nobody issued) and its number.
func tokenAccount(token string) (string, int) {
	for _, a := range []string{"A", "B"} {
		if rest, ok := strings.CutPrefix(token, "tok-"+a+"-"); ok {
			n := 0
			for _, c := range rest {
				if c < '0' || c > '9' {
					return "", 0
				}
				n = n*10 + int(c-'0')
			}
			return a, n
		}
	}
	return "", 0
}

func (f *Fixtures) token(r *http.Request, raw []byte, reply func(int, any)) {
	f.mu.Lock()
	status := f.TokenStatus
	f.mu.Unlock()
	if status != 0 {
		reply(status, map[string]any{"error": "server_error", "error_description": "token service down"})
		return
	}
	user, pass, _ := r.BasicAuth()
	suffix := ""
	switch user + ":" + pass {
	case CredentialsA:
		suffix = "A"
	case CredentialsB:
		suffix = "B"
	}
	form, _ := url.ParseQuery(string(raw))
	if suffix == "" || form.Get("grant_type") != "client_credentials" {
		reply(401, map[string]any{"error": "invalid_client", "error_description": "Client Authentication failed"})
		return
	}
	f.mu.Lock()
	f.tokens++
	n := f.tokens
	f.mu.Unlock()
	reply(200, map[string]any{"access_token": fmt.Sprintf("tok-%s-%d", suffix, n), "token_type": "Bearer", "expires_in": ExpiresIn})
}

// verdict is what PayPal says of a delivery: SUCCESS if the signature is the one it would have made for exactly these
// bytes of the event, at this webhook, and FAILURE otherwise.
func (f *Fixtures) verdict(raw []byte, forced string) string {
	if forced != "" {
		return forced
	}
	var req struct {
		AuthAlgo         string          `json:"auth_algo"`
		CertURL          string          `json:"cert_url"`
		TransmissionID   string          `json:"transmission_id"`
		TransmissionSig  string          `json:"transmission_sig"`
		TransmissionTime string          `json:"transmission_time"`
		WebhookID        string          `json:"webhook_id"`
		WebhookEvent     json.RawMessage `json:"webhook_event"`
	}
	if json.Unmarshal(raw, &req) != nil || req.AuthAlgo == "" || req.CertURL == "" || req.TransmissionID == "" || req.TransmissionSig == "" ||
		req.TransmissionTime == "" || req.WebhookID == "" || len(req.WebhookEvent) == 0 {
		return "FAILURE"
	}
	want := expectedSignature(req.WebhookID, req.TransmissionID, req.TransmissionTime, req.WebhookEvent)
	if req.AuthAlgo != "SHA256withRSA" || req.CertURL != CertURL || !hmac.Equal([]byte(want), []byte(req.TransmissionSig)) {
		return "FAILURE"
	}
	return "SUCCESS"
}
