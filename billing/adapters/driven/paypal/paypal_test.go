package paypal_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driven/paypal"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/paypalfix"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

const verifyPath = "/v1/notifications/verify-webhook-signature"

func setup(t *testing.T, mutate ...func(*paypal.Config)) (*paypal.Provider, *paypalfix.Fixtures) {
	t.Helper()
	fx := paypalfix.New(t)
	cfg := paypal.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, BaseURL: fx.APIURL()}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := paypal.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, fx
}

func ctx() context.Context { return context.Background() }

// fixedSecrets resolves the references it holds to the values it is given, so a test can hold the adapter to a secret of any shape.
type fixedSecrets map[string]string

func (m fixedSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := m[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no secret behind %q", ref)
}

// ---------------------------------------------------------------- the contract --

func TestPayPalHoldsTheProviderContract(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	contracts.ProviderContract(t, p, fx)
}

func TestPayPalIsNamedPayPal(t *testing.T) {
	t.Parallel()
	p, _ := setup(t)
	if p.Name() != "paypal" {
		t.Fatalf("Name = %q", p.Name())
	}
}

func TestPayPalIsBuiltOnlyWithWhatItNeeds(t *testing.T) {
	t.Parallel()
	fx := paypalfix.New(t)
	ok := paypal.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock}
	if _, err := paypal.New(ok); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*paypal.Config){
		"no secret resolver":         func(c *paypal.Config) { c.Secrets = nil },
		"no catalogue":               func(c *paypal.Config) { c.Catalog = nil },
		"no clock":                   func(c *paypal.Config) { c.Clock = nil },
		"a negative tolerance":       func(c *paypal.Config) { c.Tolerance = -time.Second },
		"a base URL that is not one": func(c *paypal.Config) { c.BaseURL = "api-m.paypal.com" },
	} {
		c := ok
		mutate(&c)
		if _, err := paypal.New(c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// ------------------------------------------------------------ remote verification --

func paidCapture() map[string]any {
	return paypalfix.Capture("pay-1", paypalfix.CustomID("cus_1", contracts.PackPriceID), "10.00")
}

// PayPal checks the signature of the exact bytes it sent, so the event must reach it unchanged: not re-encoded, not
// re-indented, not with its & and < turned into escapes.
func TestPayPalIsAskedToVerifyTheBodyByteForByteWithTheHeadersAndTheWebhookId(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := []byte(fmt.Sprintf("{\n  \"id\": \"WH-EVT-bytes\",\n  \"event_type\": \"PAYMENT.CAPTURE.COMPLETED\",\n  \"create_time\": %q,\n  \"summary\": \"Fish & Chips <3 \\u00e9 paid\",\n"+
		"  \"resource\": {\"id\": \"pay-1\", \"status\": \"COMPLETED\", \"amount\": {\"currency_code\": \"USD\", \"value\": \"10.00\"}, \"custom_id\": %q}\n}",
		fx.Now().UTC().Format("2006-01-02T15:04:05.000Z"), paypalfix.CustomID("cus_1", contracts.PackPriceID)))
	hdr := paypalfix.Headers(paypalfix.WebhookIDA, "tx-bytes", fx.Now(), body)
	e, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr)
	if err != nil || e.Type != domain.PaymentSucceeded {
		t.Fatalf("a genuine delivery with awkward bytes: %+v, %v", e, err)
	}
	req, ok := fx.Last(http.MethodPost, verifyPath)
	if !ok {
		t.Fatal("PayPal was never asked to verify the delivery")
	}
	if !strings.Contains(string(req.Raw), `"webhook_event":`+string(body)) {
		t.Fatalf("the event did not reach PayPal byte for byte:\nsent:  %s\nwant it to contain: %s", req.Raw, body)
	}
	for field, want := range map[string]string{"auth_algo": "SHA256withRSA", "cert_url": paypalfix.CertURL, "transmission_id": "tx-bytes",
		"transmission_time": hdr.Get("PAYPAL-TRANSMISSION-TIME"), "transmission_sig": hdr.Get("PAYPAL-TRANSMISSION-SIG"), "webhook_id": paypalfix.WebhookIDA} {
		if req.Body[field] != want {
			t.Errorf("%s = %v, want %q: it comes from the header or the account, never the body", field, req.Body[field], want)
		}
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer tok-A-") {
		t.Errorf("Authorization = %q", req.Header.Get("Authorization"))
	}
}

func TestAnythingPayPalDoesNotVouchForIsAnInvalidSignature(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*paypalfix.Fixtures){
		"a FAILURE verdict":        func(f *paypalfix.Fixtures) { f.Verdict = "FAILURE" },
		"a verdict nobody defined": func(f *paypalfix.Fixtures) { f.Verdict = "PENDING" },
		"a request PayPal rejects": func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusBadRequest },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, fx := setup(t)
			set(fx)
			body, hdr := fx.SignedPaymentSucceeded()
			e, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr)
			if !errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrVerificationUnavailable) {
				t.Fatalf("err = %v, want ErrInvalidSignature", err)
			}
			if !isZero(e) {
				t.Fatalf("an error came with an event: %+v", e)
			}
		})
	}
}

func isZero(e domain.Event) bool { return e.ProviderEventID == "" && e.Type == "" && e.Account == "" }

// When PayPal cannot answer, the delivery is neither genuine nor forged: it is not rejected (that would drop a payment the
// provider will not resend) and not trusted (that would let a forgery through while PayPal is down).
func TestWhenPayPalCannotAnswerTheDeliveryIsNeitherRejectedNorTrusted(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*paypalfix.Fixtures){
		"a 500 from the verify call":               func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusInternalServerError },
		"a 503 from the verify call":               func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusServiceUnavailable },
		"a 429 from the verify call":               func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusTooManyRequests },
		"a 401 from the verify call":               func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusUnauthorized },
		"a 403 from the verify call":               func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusForbidden },
		"a token service that is down":             func(f *paypalfix.Fixtures) { f.TokenStatus = http.StatusServiceUnavailable },
		"a token call PayPal rejects as malformed": func(f *paypalfix.Fixtures) { f.TokenStatus = http.StatusBadRequest },
		"a token call PayPal cannot process":       func(f *paypalfix.Fixtures) { f.TokenStatus = http.StatusUnprocessableEntity },
		"credentials PayPal does not accept":       func(f *paypalfix.Fixtures) { f.TokenStatus = http.StatusUnauthorized },
		"a PayPal that is not answering":           func(f *paypalfix.Fixtures) { f.Stop() },
		"tokens that stay unauthorized":            func(f *paypalfix.Fixtures) { f.RejectAllTokens = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, fx := setup(t)
			set(fx)
			body, hdr := fx.SignedPaymentSucceeded()
			e, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr)
			if !errors.Is(err, domain.ErrVerificationUnavailable) || errors.Is(err, domain.ErrInvalidSignature) {
				t.Fatalf("err = %v, want ErrVerificationUnavailable", err)
			}
			if !isZero(e) {
				t.Fatalf("an error came with an event: %+v", e)
			}
		})
	}
}

func TestACancelledContextIsAnUnavailableVerificationAndPayPalIsNotCalled(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	c, cancel := context.WithCancel(ctx())
	cancel()
	body, hdr := fx.SignedPaymentSucceeded()
	_, err := p.VerifyWebhook(c, fx.Account(), body, hdr)
	if !errors.Is(err, domain.ErrVerificationUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v: a caller that gave up has neither accepted nor rejected the delivery", err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("PayPal was called: %v", fx.Requests())
	}
}

// What can be refused without PayPal is refused without PayPal: a missing header, a time that is not one, a time outside the
// tolerance. Nothing is sent, not even for a token.
func TestWhatNeedsNoAnswerFromPayPalIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body, good := fx.SignedPaymentSucceeded()
	without := func(h string) http.Header {
		c := good.Clone()
		c.Del(h)
		return c
	}
	with := func(h, v string) http.Header {
		c := good.Clone()
		c.Set(h, v)
		return c
	}
	for name, hdr := range map[string]http.Header{
		"no headers":                             nil,
		"no auth algorithm":                      without("PAYPAL-AUTH-ALGO"),
		"no certificate url":                     without("PAYPAL-CERT-URL"),
		"no transmission id":                     without("PAYPAL-TRANSMISSION-ID"),
		"no transmission signature":              without("PAYPAL-TRANSMISSION-SIG"),
		"no transmission time":                   without("PAYPAL-TRANSMISSION-TIME"),
		"an empty signature":                     with("PAYPAL-TRANSMISSION-SIG", ""),
		"a time that is not a time":              with("PAYPAL-TRANSMISSION-TIME", "yesterday"),
		"a time with no zone":                    with("PAYPAL-TRANSMISSION-TIME", "2026-03-01T00:00:00"),
		"a time five minutes and a second ago":   with("PAYPAL-TRANSMISSION-TIME", fx.Now().Add(-5*time.Minute-time.Second).UTC().Format(time.RFC3339)),
		"a time five minutes and a second ahead": with("PAYPAL-TRANSMISSION-TIME", fx.Now().Add(5*time.Minute+time.Second).UTC().Format(time.RFC3339)),
	} {
		_, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr)
		if !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("PayPal was called for deliveries that could be refused on their face: %d requests", len(fx.Requests()))
	}
}

func TestTheTimestampToleranceIsExactlyItsBound(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	const tol = 5 * time.Minute
	body := fx.Event("WH-EVT-t", "PAYMENT.CAPTURE.COMPLETED", fx.Now(), paidCapture())
	for name, tc := range map[string]struct {
		offset time.Duration
		ok     bool
	}{
		"exactly at the bound, past":      {-tol, true},
		"exactly at the bound, future":    {tol, true},
		"a second outside, in the past":   {-tol - time.Second, false},
		"a second outside, in the future": {tol + time.Second, false},
		"now":                             {0, true},
	} {
		_, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-t", fx.Now().Add(tc.offset), body))
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !tc.ok && !errors.Is(err, domain.ErrInvalidSignature):
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}

// The event is spliced into the request to PayPal as it is. A body that is not exactly one JSON value could carry fields of
// its own into that request — a webhook_id of the sender's choosing, so that PayPal vouches for an event it signed for the
// sender's own webhook — so it is never sent.
func TestABodyThatCouldChangeTheVerificationRequestIsNeverSent(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	good := fx.Event("WH-EVT-i", "PAYMENT.CAPTURE.COMPLETED", fx.Now(), paidCapture())
	for name, body := range map[string][]byte{
		"a value followed by fields of its own": []byte(`null,"webhook_id":"` + paypalfix.WebhookIDB + `"`),
		"two values":                            []byte(`{}{}`),
		"a value and trailing text":             append(append([]byte(nil), good...), []byte(" garbage")...),
		"a truncated value":                     good[:len(good)/2],
		"not JSON":                              []byte(`<html>`),
		"nothing":                               nil,
	} {
		hdr := paypalfix.Headers(paypalfix.WebhookIDA, "tx-i", fx.Now(), body)
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("PayPal was sent %d requests built from a body that was not one JSON value", len(fx.Requests()))
	}
}

func TestEachAccountIsVerifiedAgainstItsOwnWebhookWithItsOwnToken(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	a, b := fx.Account(), fx.OtherAccount()
	body, hdrA := fx.SignedPaymentSucceeded()
	if _, err := p.VerifyWebhook(ctx(), a, body, hdrA); err != nil {
		t.Fatalf("A's delivery on A: %v", err)
	}
	if _, err := p.VerifyWebhook(ctx(), b, body, hdrA); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Fatalf("A's delivery on B: err = %v: B's webhook is not the one it was signed for", err)
	}
	bodyB := fx.Event("WH-EVT-b", "PAYMENT.CAPTURE.COMPLETED", fx.Now(), paidCapture())
	if _, err := p.VerifyWebhook(ctx(), b, bodyB, paypalfix.Headers(paypalfix.WebhookIDB, "tx-b", fx.Now(), bodyB)); err != nil {
		t.Fatalf("B's delivery on B: %v", err)
	}
	reqs := fx.Requests()
	var last paypalfix.Request
	for _, r := range reqs {
		if r.Path == verifyPath {
			last = r
		}
	}
	if !strings.HasPrefix(last.Header.Get("Authorization"), "Bearer tok-B-") || last.Body["webhook_id"] != paypalfix.WebhookIDB {
		t.Fatalf("B's verification used %q for webhook %v: each account has its own credentials and its own webhook", last.Header.Get("Authorization"), last.Body["webhook_id"])
	}
	if n := fx.TokensIssued(); n != 2 {
		t.Fatalf("%d tokens were issued for two accounts", n)
	}
}

func TestTheAccessTokenIsReusedUntilItIsAboutToExpire(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	verify := func() {
		t.Helper()
		body, hdr := fx.SignedPaymentSucceeded()
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		verify()
	}
	if n := fx.Count(http.MethodPost, "/v1/oauth2/token"); n != 1 {
		t.Fatalf("%d token requests for three verifications: a token lives for hours", n)
	}
	req, _ := fx.Last(http.MethodPost, "/v1/oauth2/token")
	user, pass, ok := basic(req.Header)
	if !ok || user+":"+pass != paypalfix.CredentialsA || !strings.Contains(string(req.Raw), "grant_type=client_credentials") ||
		!strings.HasPrefix(req.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		t.Fatalf("token request = %q %v %v", req.Raw, req.Header, ok)
	}
	fx.Clock.Advance(paypalfix.ExpiresIn*time.Second - 2*time.Minute) // still good, with room
	verify()
	if n := fx.TokensIssued(); n != 1 {
		t.Fatalf("a token with two minutes to live was replaced (%d issued)", n)
	}
	fx.Clock.Advance(90 * time.Second) // inside the margin before it expires: replaced rather than risked
	verify()
	if n := fx.TokensIssued(); n != 2 {
		t.Fatalf("%d tokens issued, want a new one once the old was within its margin of expiring", n)
	}
}

func basic(h http.Header) (user, pass string, ok bool) {
	r := &http.Request{Header: h}
	return r.BasicAuth()
}

func TestATokenPayPalWithdrewIsReplacedOnceAndTheCallRetried(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body, hdr := fx.SignedPaymentSucceeded()
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); err != nil {
		t.Fatal(err)
	}
	fx.RevokeTokens()
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); err != nil {
		t.Fatalf("a withdrawn token must be replaced and the call made again: %v", err)
	}
	if n := fx.TokensIssued(); n != 2 {
		t.Fatalf("%d tokens issued, want 2", n)
	}
	fx.RejectAllTokens = true
	before := fx.TokensIssued()
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); !errors.Is(err, domain.ErrVerificationUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if n := fx.TokensIssued() - before; n > 1 {
		t.Fatalf("%d tokens were fetched trying to get past a 401: it is one replacement, not a loop", n)
	}
}

func TestAnUnresolvableCredentialOrWebhookIdIsOurFailureAndNeverCarriesASecret(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body, hdr := fx.SignedPaymentSucceeded()
	for name, mutate := range map[string]func(*domain.ProviderAccount){
		"credentials":    func(a *domain.ProviderAccount) { a.SecretRef = "env:NOT_CONFIGURED" },
		"the webhook id": func(a *domain.ProviderAccount) { a.WebhookSecretRef = "env:NOT_CONFIGURED" },
	} {
		acct := fx.Account()
		mutate(&acct)
		_, err := p.VerifyWebhook(ctx(), acct, body, hdr)
		if err == nil || errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrVerificationUnavailable) {
			t.Errorf("%s: err = %v: a reference we cannot resolve is a misconfiguration to alert on, not a forged delivery and not an outage", name, err)
		}
	}
	// Credentials that are not `client_id:client_secret`.
	odd := fixedSecrets{"env:PAYPAL_CREDENTIALS_A": "no-colon-in-here", "env:PAYPAL_WEBHOOK_ID_A": paypalfix.WebhookIDA}
	p2, _ := setup(t, func(c *paypal.Config) { c.Secrets = odd })
	_, err := p2.VerifyWebhook(ctx(), fx.Account(), body, hdr)
	if err == nil || errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrVerificationUnavailable) || strings.Contains(err.Error(), "no-colon-in-here") {
		t.Errorf("credentials with no colon: err = %v", err)
	}
}

func TestNeitherTheTokenNorTheClientSecretIsEverInAnError(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*paypalfix.Fixtures){
		"a verify call that fails": func(f *paypalfix.Fixtures) { f.VerifyStatus = http.StatusInternalServerError },
		"another call that fails":  func(f *paypalfix.Fixtures) { f.FailAll = true },
		"a token call that fails":  func(f *paypalfix.Fixtures) { f.TokenStatus = http.StatusUnauthorized },
	} {
		p, fx := setup(t)
		set(fx)
		body, hdr := fx.SignedPaymentSucceeded()
		_, err1 := p.VerifyWebhook(ctx(), fx.Account(), body, hdr)
		_, err2 := p.CreateCheckout(ctx(), fx.Account(), fx.Checkout())
		for _, err := range []error{err1, err2} {
			if err != nil && (strings.Contains(err.Error(), "tok-A-") || strings.Contains(err.Error(), "fixture-secret-a")) {
				t.Errorf("%s: the error carries a secret: %v", name, err)
			}
		}
	}
}

func TestAVerifiedBodyThatIsNotAnEventIsAnErrorNotAPartialEvent(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	ok := `{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED","create_time":"2026-03-01T00:00:00.000Z","resource":`
	capture := `{"id":"pay-1","status":"COMPLETED","amount":{"currency_code":"USD","value":"10.00"},"custom_id":"cus_1~` + contracts.PackPriceID + `"}`
	for name, body := range map[string][]byte{
		"an array":                                   []byte(`[]`),
		"an empty object":                            []byte(`{}`),
		"no id":                                      []byte(`{"event_type":"PAYMENT.CAPTURE.COMPLETED","create_time":"2026-03-01T00:00:00.000Z","resource":` + capture + `}`),
		"no event type":                              []byte(`{"id":"WH-1","create_time":"2026-03-01T00:00:00.000Z","resource":` + capture + `}`),
		"no create time":                             []byte(`{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED","resource":` + capture + `}`),
		"a create time that is not a time":           []byte(`{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED","create_time":"soon","resource":` + capture + `}`),
		"no resource":                                []byte(`{"id":"WH-1","event_type":"PAYMENT.CAPTURE.COMPLETED","create_time":"2026-03-01T00:00:00.000Z"}`),
		"a resource that is not an object":           []byte(ok + `"x"}`),
		"a capture with no id":                       []byte(ok + `{"status":"COMPLETED","amount":{"currency_code":"USD","value":"10.00"},"custom_id":"cus_1~p"}}`),
		"a capture with no amount":                   []byte(ok + `{"id":"pay-1","status":"COMPLETED","custom_id":"cus_1~p"}}`),
		"an amount that is not a number":             []byte(ok + `{"id":"pay-1","status":"COMPLETED","amount":{"currency_code":"USD","value":"ten"},"custom_id":"cus_1~p"}}`),
		"a negative amount":                          []byte(ok + `{"id":"pay-1","status":"COMPLETED","amount":{"currency_code":"USD","value":"-1.00"},"custom_id":"cus_1~p"}}`),
		"an amount with too many decimals":           []byte(ok + `{"id":"pay-1","status":"COMPLETED","amount":{"currency_code":"USD","value":"1.001"},"custom_id":"cus_1~p"}}`),
		"an amount in no currency":                   []byte(ok + `{"id":"pay-1","status":"COMPLETED","amount":{"value":"10.00"},"custom_id":"cus_1~p"}}`),
		"a completed capture that is not":            []byte(ok + `{"id":"pay-1","status":"PENDING","amount":{"currency_code":"USD","value":"10.00"},"custom_id":"cus_1~p"}}`),
		"a refund with no capture":                   []byte(`{"id":"WH-1","event_type":"PAYMENT.CAPTURE.REFUNDED","create_time":"2026-03-01T00:00:00.000Z","resource":{"id":"r1","amount":{"currency_code":"USD","value":"1.00"},"seller_payable_breakdown":{"total_refunded_amount":{"currency_code":"USD","value":"1.00"}}}}`),
		"a refund with no cumulative total":          []byte(`{"id":"WH-1","event_type":"PAYMENT.CAPTURE.REFUNDED","create_time":"2026-03-01T00:00:00.000Z","resource":{"id":"r1","amount":{"currency_code":"USD","value":"1.00"},"links":[{"rel":"up","href":"https://api.paypal.test/v2/payments/captures/pay-1"}]}}`),
		"a subscription with no id":                  []byte(`{"id":"WH-1","event_type":"BILLING.SUBSCRIPTION.ACTIVATED","create_time":"2026-03-01T00:00:00.000Z","resource":{"status":"ACTIVE","custom_id":"cus_1~p"}}`),
		"a dispute resolved without a known outcome": []byte(`{"id":"WH-1","event_type":"CUSTOMER.DISPUTE.RESOLVED","create_time":"2026-03-01T00:00:00.000Z","resource":{"dispute_id":"PP-D-1","disputed_transactions":[{"seller_transaction_id":"pay-1"}],"dispute_outcome":{"outcome_code":"SOMETHING_NEW"}}}`),
	} {
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-n", fx.Now(), body))
		if err == nil || errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrVerificationUnavailable) {
			t.Errorf("%s: err = %v: a delivery that verified and is nonsense is our adapter's problem, not a forgery and not an outage", name, err)
		}
		if !isZero(e) {
			t.Errorf("%s: a partial event came with the error: %+v", name, e)
		}
	}
}

// ---------------------------------------------------------------- normalization --

func verify(t *testing.T, p *paypal.Provider, fx *paypalfix.Fixtures, id, typ string, resource map[string]any) domain.Event {
	t.Helper()
	body := fx.Event(id, typ, fx.Now(), resource)
	e, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-"+id, fx.Now(), body))
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	if e.Provider != "paypal" || e.Account != fx.Account().ID || e.ProviderEventID != id || !e.CreatedAt.Equal(fx.Now()) || e.RawHash == "" {
		t.Fatalf("%s: envelope = %+v", typ, e)
	}
	return e
}

func money(minor int64) *domain.Money { return &domain.Money{MinorUnits: minor, Currency: "USD"} }

func TestEachPayPalEventIsNormalizedIntoTheEnginesVocabulary(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, next := fx.Now().Add(-24*time.Hour), fx.Now().Add(29*24*time.Hour)
	ours := paypalfix.CustomID("cus_1", contracts.ProPriceID)
	fx.Subscriptions["I-SUB1"] = paypalfix.Subscription("I-SUB1", contracts.ProPriceID, "ACTIVE", ours, start, next)
	fx.Subscriptions["I-FOREIGN"] = paypalfix.Subscription("I-FOREIGN", contracts.ProPriceID, "ACTIVE", "", start, next)

	t.Run("a completed capture is a payment keyed on the capture, for the customer and product the order was made with", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "WH-1", "PAYMENT.CAPTURE.COMPLETED", paypalfix.Capture("cap-9", paypalfix.CustomID("cus_1", contracts.PackPriceID), "10.00"))
		o := e.Object
		if e.Type != domain.PaymentSucceeded || o.ProviderPaymentID != "cap-9" || o.ProviderCustomerID != "cus_1" || o.ProviderPriceID != contracts.PackPriceID ||
			o.ProviderSubscriptionID != "" || o.Amount == nil || *o.Amount != *money(1000) || o.TaxAmount != nil {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
	})
	t.Run("a capture the order was not made by this service for is not ours and grants nothing", func(t *testing.T) {
		t.Parallel()
		for _, custom := range []string{"", "someone-elses-order", "~price", "cus_1~", "cus_1"} {
			if e := verify(t, p, fx, "WH-2"+custom, "PAYMENT.CAPTURE.COMPLETED", paypalfix.Capture("cap-2", custom, "10.00")); e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
				t.Fatalf("custom_id %q → %+v: a PayPal account sells other things too, and a payment is never guessed to be ours", custom, e)
			}
		}
	})
	t.Run("the first tilde splits the customer from the product, which may have tildes of its own", func(t *testing.T) {
		t.Parallel()
		// A customer id never has a tilde (CreateCheckout refuses one), so the first is always the split.
		e := verify(t, p, fx, "WH-3", "PAYMENT.CAPTURE.COMPLETED", paypalfix.Capture("cap-3", "cus_1~price~with~tildes", "10.00"))
		if e.Object.ProviderCustomerID != "cus_1" || e.Object.ProviderPriceID != "price~with~tildes" {
			t.Fatalf("object = %+v", e.Object)
		}
	})
	t.Run("amounts are decimal strings in the currency's own exponent, read without floats", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			value, currency string
			minor           int64
		}{{"10.00", "USD", 1000}, {"10.5", "USD", 1050}, {"0.05", "EUR", 5}, {"0.00", "USD", 0}, {"1000", "JPY", 1000}, {"12", "HUF", 12}, {"99999999999999.99", "USD", 9999999999999999}} {
			c := paypalfix.Capture("cap-4", paypalfix.CustomID("cus_1", contracts.PackPriceID), tc.value)
			c["amount"] = map[string]any{"currency_code": tc.currency, "value": tc.value}
			e := verify(t, p, fx, "WH-4"+tc.value+tc.currency, "PAYMENT.CAPTURE.COMPLETED", c)
			if *e.Object.Amount != (domain.Money{MinorUnits: tc.minor, Currency: tc.currency}) {
				t.Fatalf("%s %s → %+v, want %d minor units", tc.value, tc.currency, *e.Object.Amount, tc.minor)
			}
		}
	})
	t.Run("a denied or declined capture is a failed payment", func(t *testing.T) {
		t.Parallel()
		for typ, reason := range map[string]string{"PAYMENT.CAPTURE.DENIED": "denied", "PAYMENT.CAPTURE.DECLINED": "declined"} {
			c := paypalfix.Capture("cap-5", paypalfix.CustomID("cus_1", contracts.PackPriceID), "10.00")
			c["status"] = strings.ToUpper(reason)
			e := verify(t, p, fx, "WH-5"+reason, typ, c)
			if e.Type != domain.PaymentFailed || e.Object.ProviderPaymentID != "cap-5" || e.Object.FailureReason != reason || e.Object.ProviderCustomerID != "cus_1" || e.Object.Amount.MinorUnits != 1000 {
				t.Fatalf("%s → %+v / %+v", typ, e, e.Object)
			}
		}
	})
	t.Run("a pending capture and an approved order grant nothing: a checkout return never fulfils", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"PAYMENT.CAPTURE.PENDING", "CHECKOUT.ORDER.APPROVED", "CHECKOUT.ORDER.COMPLETED"} {
			if e := verify(t, p, fx, "WH-6"+typ, typ, paidCapture()); e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
				t.Fatalf("%s → %+v (invariant 2)", typ, e)
			}
		}
	})
	t.Run("a refund reports the total refunded from the capture to date, against the capture it names", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "WH-7", "PAYMENT.CAPTURE.REFUNDED", paypalfix.Refund("refund-2", "cap-9", "2.00", "7.00"))
		o := e.Object
		if e.Type != domain.PaymentRefunded || o.ProviderPaymentID != "cap-9" || o.ProviderRefundID != "refund-2" || o.AmountRefunded == nil || *o.AmountRefunded != *money(700) || o.ProviderCustomerID != "" {
			t.Fatalf("event = %+v object = %+v: this refund is 2.00 and the capture has been refunded 7.00 in all", e, o)
		}
	})
	t.Run("a subscription's payment is a payment of the subscription, for the plan PayPal says and the customer the subscription was made for", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "WH-8", "PAYMENT.SALE.COMPLETED", paypalfix.Sale("sale-1", "I-SUB1", "20.00"))
		o := e.Object
		if e.Type != domain.PaymentSucceeded || o.ProviderPaymentID != "sale-1" || o.ProviderSubscriptionID != "I-SUB1" || o.ProviderCustomerID != "cus_1" ||
			o.ProviderPriceID != contracts.ProPriceID || *o.Amount != *money(2000) {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
		if _, ok := fx.Last(http.MethodGet, "/v1/billing/subscriptions/I-SUB1"); !ok {
			t.Fatal("the subscription was not read: a sale names only it, not the customer or the plan")
		}
	})
	t.Run("a sale that belongs to no subscription, or to one that is not ours, is ignored", func(t *testing.T) {
		t.Parallel()
		for name, sale := range map[string]map[string]any{"no subscription": paypalfix.Sale("sale-2", "", "20.00"), "a foreign subscription": paypalfix.Sale("sale-3", "I-FOREIGN", "20.00")} {
			if e := verify(t, p, fx, "WH-9"+name, "PAYMENT.SALE.COMPLETED", sale); e.Type != domain.Ignored {
				t.Fatalf("%s → %+v", name, e)
			}
		}
	})
	t.Run("a subscription event carries the subscription, the customer, the plan and the period", func(t *testing.T) {
		t.Parallel()
		for typ, want := range map[string]domain.EventType{
			"BILLING.SUBSCRIPTION.CREATED": domain.SubscriptionUpdated, "BILLING.SUBSCRIPTION.ACTIVATED": domain.SubscriptionUpdated,
			"BILLING.SUBSCRIPTION.UPDATED": domain.SubscriptionUpdated, "BILLING.SUBSCRIPTION.SUSPENDED": domain.SubscriptionUpdated,
			"BILLING.SUBSCRIPTION.RE-ACTIVATED": domain.SubscriptionUpdated,
			"BILLING.SUBSCRIPTION.CANCELLED":    domain.SubscriptionDeleted, "BILLING.SUBSCRIPTION.EXPIRED": domain.SubscriptionDeleted,
		} {
			e := verify(t, p, fx, "WH-10"+typ, typ, paypalfix.Subscription("I-SUB1", contracts.ProPriceID, "SUSPENDED", ours, start, next))
			o := e.Object
			if e.Type != want || o.ProviderSubscriptionID != "I-SUB1" || o.ProviderCustomerID != "cus_1" || o.ProviderPriceID != contracts.ProPriceID || o.Status != "SUSPENDED" ||
				o.PeriodStart == nil || !o.PeriodStart.Equal(start) || o.PeriodEnd == nil || !o.PeriodEnd.Equal(next) || o.CancelAtPeriodEnd {
				t.Fatalf("%s → %+v / %+v", typ, e, o)
			}
		}
	})
	t.Run("a subscription that is not ours is ignored", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "WH-11", "BILLING.SUBSCRIPTION.ACTIVATED", paypalfix.Subscription("I-X", contracts.ProPriceID, "ACTIVE", "someone-elses", start, next)); e.Type != domain.Ignored {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a failed subscription payment is a failed payment of the subscription, with PayPal's reason", func(t *testing.T) {
		t.Parallel()
		s := paypalfix.Subscription("I-SUB1", contracts.ProPriceID, "ACTIVE", ours, start, next)
		s["billing_info"].(map[string]any)["last_failed_payment"] = map[string]any{"reason_code": "PAYMENT_DENIED", "amount": map[string]any{"currency_code": "USD", "value": "20.00"}}
		e := verify(t, p, fx, "WH-12", "BILLING.SUBSCRIPTION.PAYMENT.FAILED", s)
		o := e.Object
		if e.Type != domain.PaymentFailed || o.ProviderSubscriptionID != "I-SUB1" || o.ProviderCustomerID != "cus_1" || o.FailureReason != "PAYMENT_DENIED" || o.ProviderPaymentID != "" {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
	})
	t.Run("a reversal is the funds withdrawn from a dispute keyed on the capture", func(t *testing.T) {
		t.Parallel()
		withUp := paypalfix.Refund("REV-1", "cap-9", "10.00", "10.00") // the capture is the resource's "up" link
		e := verify(t, p, fx, "WH-13", "PAYMENT.CAPTURE.REVERSED", withUp)
		if e.Type != domain.DisputeFundsWithdrawn || e.Object.ProviderPaymentID != "cap-9" || e.Object.DisputeID != "dp:cap-9" || e.Object.ProviderCustomerID != "" {
			t.Fatalf("event = %+v", e)
		}
		itself := map[string]any{"id": "cap-8", "status": "REVERSED", "amount": map[string]any{"currency_code": "USD", "value": "10.00"}} // the capture is the resource
		if e := verify(t, p, fx, "WH-14", "PAYMENT.CAPTURE.REVERSED", itself); e.Object.ProviderPaymentID != "cap-8" || e.Object.DisputeID != "dp:cap-8" {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a dispute opens against the transaction it disputes, and every phase of it shares one id", func(t *testing.T) {
		t.Parallel()
		opened := verify(t, p, fx, "WH-15", "CUSTOMER.DISPUTE.CREATED", paypalfix.Dispute("PP-D-1", "cap-9", "OPEN", ""))
		if opened.Type != domain.DisputeOpened || opened.Object.ProviderPaymentID != "cap-9" || opened.Object.DisputeID != "dp:cap-9" {
			t.Fatalf("event = %+v", opened)
		}
		withdrawn := verify(t, p, fx, "WH-16", "PAYMENT.CAPTURE.REVERSED", paypalfix.Refund("REV-1", "cap-9", "10.00", "10.00"))
		if opened.Object.DisputeID != withdrawn.Object.DisputeID {
			t.Fatalf("the phases of one dispute have two ids (%q, %q): the reinstatement could never find the clawback", opened.Object.DisputeID, withdrawn.Object.DisputeID)
		}
	})
	t.Run("a resolved dispute is won or lost by PayPal's outcome", func(t *testing.T) {
		t.Parallel()
		for outcome, want := range map[string]string{"RESOLVED_BUYER_FAVOUR": "lost", "ACCEPTED": "lost", "RESOLVED_SELLER_FAVOUR": "won", "RESOLVED_WITH_PAYOUT": "won", "CANCELED_BY_BUYER": "won", "DENIED": "won"} {
			e := verify(t, p, fx, "WH-17"+outcome, "CUSTOMER.DISPUTE.RESOLVED", paypalfix.Dispute("PP-D-1", "cap-9", "RESOLVED", outcome))
			if e.Type != domain.DisputeClosed || e.Object.Status != want || e.Object.DisputeID != "dp:cap-9" || e.Object.ProviderPaymentID != "cap-9" {
				t.Fatalf("%s → %+v / %+v, want %q", outcome, e, e.Object, want)
			}
		}
	})
	t.Run("a dispute update moves no credits", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "WH-18", "CUSTOMER.DISPUTE.UPDATED", paypalfix.Dispute("PP-D-1", "cap-9", "UNDER_REVIEW", "")); e.Type != domain.Ignored {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a refund of a subscription's payment carries no cumulative total, so it is ignored and not guessed", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "WH-19", "PAYMENT.SALE.REFUNDED", map[string]any{"id": "r1", "sale_id": "sale-1", "amount": map[string]any{"total": "5.00", "currency": "USD"}}); e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("traffic nobody subscribed to is ignored with nothing in it", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"CATALOG.PRODUCT.CREATED", "BILLING.PLAN.UPDATED", "PAYMENT.AUTHORIZATION.CREATED", "something.new"} {
			if e := verify(t, p, fx, "WH-20"+typ, typ, map[string]any{"id": "x"}); e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
				t.Fatalf("%s → %+v", typ, e)
			}
		}
	})
	t.Run("a redelivery of the same payment names the same payment", func(t *testing.T) {
		t.Parallel()
		a := verify(t, p, fx, "WH-21a", "PAYMENT.CAPTURE.COMPLETED", paidCapture())
		b := verify(t, p, fx, "WH-21b", "PAYMENT.CAPTURE.COMPLETED", paidCapture())
		if a.Object.ProviderPaymentID != b.Object.ProviderPaymentID || a.ProviderEventID == b.ProviderEventID {
			t.Fatalf("one payment, two events: %+v vs %+v", a, b)
		}
	})
}

// A sale names only its subscription. Reading it is PayPal's to do: if PayPal cannot answer, the delivery is parked and
// tried again, not guessed at and not dropped.
func TestASalesSubscriptionThatCannotBeReadParksTheDeliveryAndNeverGuesses(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(*paypalfix.Fixtures){
		"PayPal failing": func(f *paypalfix.Fixtures) { f.FailAll = true },
	} {
		p, fx := setup(t)
		fx.Subscriptions["I-SUB1"] = paypalfix.Subscription("I-SUB1", contracts.ProPriceID, "ACTIVE", paypalfix.CustomID("cus_1", contracts.ProPriceID), fx.Now(), fx.Now().Add(time.Hour))
		set(fx)
		body := fx.Event("WH-S", "PAYMENT.SALE.COMPLETED", fx.Now(), paypalfix.Sale("sale-1", "I-SUB1", "20.00"))
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-s", fx.Now(), body))
		if !errors.Is(err, domain.ErrVerificationUnavailable) || !isZero(e) {
			t.Errorf("%s: %+v, %v", name, e, err)
		}
	}
	p, fx := setup(t) // PayPal does not have the subscription at all
	body := fx.Event("WH-S2", "PAYMENT.SALE.COMPLETED", fx.Now(), paypalfix.Sale("sale-1", "I-NOPE", "20.00"))
	if e, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-s2", fx.Now(), body)); err == nil ||
		errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrVerificationUnavailable) || !errors.Is(err, md.ErrNotFound) || !isZero(e) {
		t.Errorf("a sale of a subscription PayPal does not know: %+v, %v", e, err)
	}
}

// ------------------------------------------------------------------ the API --

func TestEnsureCustomerMakesAnOpaqueStableIdWithoutCallingPayPal(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := ports.CustomerInput{Scope: md.Scope{Realm: "default", Kind: "workspace", ID: "w1"}, BillingEmail: "buyer@example.test", IdemKey: "customer:paypal-a:default/workspace:w1"}
	id1, err := p.EnsureCustomer(ctx(), fx.Account(), in)
	if err != nil || id1 == "" {
		t.Fatalf("EnsureCustomer = %q, %v", id1, err)
	}
	if id2, _ := p.EnsureCustomer(ctx(), fx.Account(), in); id2 != id1 {
		t.Fatalf("the same key gave %q then %q: the customer is the key, so two first checkouts at once are one", id1, id2)
	}
	other := in
	other.IdemKey = "customer:paypal-a:default/workspace:w2"
	if id3, _ := p.EnsureCustomer(ctx(), fx.Account(), other); id3 == id1 {
		t.Fatal("two keys share a customer id")
	}
	if strings.ContainsAny(id1, "~/ ?#:") || strings.Contains(id1, "w1") {
		t.Fatalf("id = %q: it is carried in a custom_id after a tilde, and says nothing of the scope", id1)
	}
	if _, err := p.EnsureCustomer(ctx(), fx.Account(), ports.CustomerInput{}); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("no key: err = %v", err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("PayPal has no customers, yet it was called: %v", fx.Requests())
	}
}

func TestACheckoutForAPaymentIsAnOrderPayPalCapturesWhenTheBuyerApproves(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := fx.Checkout()
	url, err := p.CreateCheckout(ctx(), fx.Account(), in)
	if err != nil || !strings.HasPrefix(url, "https://www.paypal.test/checkoutnow?token=ORD-") {
		t.Fatalf("%q, %v", url, err)
	}
	req, ok := fx.Last(http.MethodPost, "/v2/checkout/orders")
	if !ok {
		t.Fatal("no order was created")
	}
	b := req.Body
	units, _ := b["purchase_units"].([]any)
	if len(units) != 1 || b["intent"] != "CAPTURE" || b["processing_instruction"] != "ORDER_COMPLETE_ON_PAYMENT_APPROVAL" {
		t.Fatalf("body = %v: the order must be captured when the buyer approves, or the buyer approves and is never charged", b)
	}
	unit, _ := units[0].(map[string]any)
	amt, _ := unit["amount"].(map[string]any)
	if unit["custom_id"] != paypalfix.CustomID("cus_1", contracts.PackPriceID) || unit["reference_id"] != contracts.PackCode || amt["currency_code"] != "USD" || amt["value"] != "10.00" {
		t.Fatalf("purchase unit = %v: the capture carries back only the custom_id, which names the customer and the product", unit)
	}
	exp := experience(b)
	if exp["return_url"] != in.SuccessURL || exp["cancel_url"] != in.CancelURL || exp["user_action"] != "PAY_NOW" || exp["shipping_preference"] != "NO_SHIPPING" {
		t.Fatalf("experience_context = %v", exp)
	}
	if req.Header.Get("PayPal-Request-Id") != in.IdemKey || !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer tok-A-") {
		t.Fatalf("header = %v", req.Header)
	}
	if again, _ := p.CreateCheckout(ctx(), fx.Account(), in); again != url {
		t.Fatalf("a double-clicked buy button made two orders: %q then %q", url, again)
	}
}

func experience(body map[string]any) map[string]any {
	src, _ := body["payment_source"].(map[string]any)
	pp, _ := src["paypal"].(map[string]any)
	exp, _ := pp["experience_context"].(map[string]any)
	return exp
}

func TestACheckoutForASubscriptionIsAPlanTheBuyerApproves(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := fx.Checkout()
	in.Mode, in.ProviderPriceID = "subscription", contracts.ProPriceID
	url, err := p.CreateCheckout(ctx(), fx.Account(), in)
	if err != nil || !strings.HasPrefix(url, "https://www.paypal.test/webapps/billing/subscriptions?ba_token=I-SUB-") {
		t.Fatalf("%q, %v", url, err)
	}
	req, _ := fx.Last(http.MethodPost, "/v1/billing/subscriptions")
	b := req.Body
	sub, _ := b["subscriber"].(map[string]any)
	app, _ := b["application_context"].(map[string]any)
	if b["plan_id"] != contracts.ProPriceID || b["custom_id"] != paypalfix.CustomID("cus_1", contracts.ProPriceID) || sub["email_address"] != "buyer@example.test" ||
		app["return_url"] != in.SuccessURL || app["cancel_url"] != in.CancelURL || app["user_action"] != "SUBSCRIBE_NOW" || app["shipping_preference"] != "NO_SHIPPING" {
		t.Fatalf("body = %v", b)
	}
	if req.Header.Get("PayPal-Request-Id") != in.IdemKey {
		t.Fatalf("header = %v", req.Header)
	}
}

func TestACheckoutThatCouldNotBeAttributedOrChargedIsRefusedBeforePayPalIsAsked(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	mutate := func(f func(*ports.CheckoutInput)) ports.CheckoutInput {
		in := fx.Checkout()
		f(&in)
		return in
	}
	long := strings.Repeat("x", 300)
	for name, in := range map[string]ports.CheckoutInput{
		"no customer, so the capture could never be attributed": mutate(func(c *ports.CheckoutInput) { c.CustomerID = "" }),
		"a customer id with a tilde":                            mutate(func(c *ports.CheckoutInput) { c.CustomerID = "cus~1" }),
		"no product":                                            mutate(func(c *ports.CheckoutInput) { c.ProviderPriceID = "" }),
		"a payment of nothing":                                  mutate(func(c *ports.CheckoutInput) { c.Amount = domain.Money{Currency: "USD"} }),
		"a payment in no currency":                              mutate(func(c *ports.CheckoutInput) { c.Amount = domain.Money{MinorUnits: 1000} }),
		"an order whose custom_id is too long for PayPal":       mutate(func(c *ports.CheckoutInput) { c.ProviderPriceID = long }),
		"a subscription whose custom_id is too long":            mutate(func(c *ports.CheckoutInput) { c.Mode, c.ProviderPriceID = "subscription", strings.Repeat("p", 130) }),
		"a mode nobody defined":                                 mutate(func(c *ports.CheckoutInput) { c.Mode = "barter" }),
	} {
		if _, err := p.CreateCheckout(ctx(), fx.Account(), in); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("PayPal was called: %v", fx.Requests())
	}
}

func TestAZeroDecimalCurrencyIsSentWithoutADecimalPoint(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := fx.Checkout()
	in.Amount = domain.Money{MinorUnits: 1500, Currency: "JPY"}
	if _, err := p.CreateCheckout(ctx(), fx.Account(), in); err != nil {
		t.Fatal(err)
	}
	req, _ := fx.Last(http.MethodPost, "/v2/checkout/orders")
	unit := req.Body["purchase_units"].([]any)[0].(map[string]any)
	if amt := unit["amount"].(map[string]any); amt["value"] != "1500" || amt["currency_code"] != "JPY" {
		t.Fatalf("amount = %v: PayPal refuses a fraction of a yen", amt)
	}
}

// What PayPal cannot do server-side it says so and is not called: saving a method (no setup checkout), charging one,
// a portal (PayPal has none), cancelling at the period's end (PayPal cancels at once), and a plan change (PayPal's revision
// needs the buyer's approval, which the port has no way to ask for).
func TestWhatPayPalCannotDoForTheServerIsUnsupportedAndPayPalIsNotCalled(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	saving := fx.Checkout()
	saving.Mode = "setup"
	_, errSetup := p.CreateCheckout(ctx(), fx.Account(), saving)
	_, errPortal := p.CreatePortalSession(ctx(), fx.Account(), "cus_1", "https://app.example.test/")
	errCancel := p.CancelAtPeriodEnd(ctx(), fx.Account(), "I-SUB1")
	_, errCharge := p.ChargeSaved(ctx(), fx.Account(), ports.OffSessionInput{CustomerID: "cus_1", PaymentMethodRef: "pm", Amount: *money(1000), IdemKey: "k"})
	for name, err := range map[string]error{"setup": errSetup, "portal": errPortal, "cancel at period end": errCancel, "saved charge": errCharge} {
		if !errors.Is(err, domain.ErrUnsupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, policy := range []domain.PlanChangePolicy{domain.AtPeriodEnd, domain.ImmediateProrate, domain.ImmediateNoProrate} {
		if err := p.ChangePlan(ctx(), fx.Account(), "I-SUB1", contracts.TeamPriceID, policy); !errors.Is(err, domain.ErrUnsupported) {
			t.Errorf("change plan (%s): err = %v", policy, err)
		}
	}
	if err := p.ChangePlan(ctx(), fx.Account(), "I-SUB1", contracts.TeamPriceID, "whenever"); !errors.Is(err, md.ErrInvalid) {
		t.Errorf("a policy nobody defined: err = %v", err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("PayPal was called: %v", fx.Requests())
	}
}

func TestFetchSubscriptionIsPayPalsCurrentObjectInTheEnginesTerms(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, next := fx.Now().Add(-24*time.Hour).Truncate(time.Second), fx.Now().Add(29*24*time.Hour).Truncate(time.Second)
	ours := paypalfix.CustomID("cus_1", contracts.ProPriceID)
	fx.Subscriptions["I-SUB1"] = paypalfix.Subscription("I-SUB1", contracts.ProPriceID, "SUSPENDED", ours, start, next)
	sub, at, err := p.FetchSubscription(ctx(), fx.Account(), "I-SUB1")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ProviderSubscriptionID != "I-SUB1" || sub.Account != fx.Account().ID || sub.PlanCode != contracts.ProCode || sub.Status != domain.PastDue ||
		sub.CancelAtPeriodEnd || !sub.CurrentPeriodStart.Equal(start) || !sub.CurrentPeriodEnd.Equal(next) {
		t.Fatalf("sub = %+v", sub)
	}
	if !at.Equal(fx.Now()) {
		t.Fatalf("at = %v, want the provider's own clock (its Date header) %v", at, fx.Now())
	}
	for paypalStatus, want := range map[string]domain.SubStatus{"APPROVAL_PENDING": domain.Incomplete, "APPROVED": domain.Incomplete, "ACTIVE": domain.Active,
		"SUSPENDED": domain.PastDue, "CANCELLED": domain.Canceled, "EXPIRED": domain.Canceled, "SOMETHING_NEW": domain.Incomplete} {
		fx.Subscriptions["I-SUB2"] = paypalfix.Subscription("I-SUB2", contracts.ProPriceID, paypalStatus, ours, start, next)
		if got, _, _ := p.FetchSubscription(ctx(), fx.Account(), "I-SUB2"); got.Status != want {
			t.Errorf("PayPal's %q = %q, want %q", paypalStatus, got.Status, want)
		}
	}
	// A renewed subscription's period starts at its last payment, not at the subscription's own start.
	renewed := paypalfix.Subscription("I-SUB5", contracts.ProPriceID, "ACTIVE", ours, start.Add(-60*24*time.Hour), next)
	lastPaid := start.Add(-time.Hour)
	renewed["billing_info"].(map[string]any)["last_payment"] = map[string]any{"time": lastPaid.UTC().Format("2006-01-02T15:04:05.000Z")}
	fx.Subscriptions["I-SUB5"] = renewed
	if got, _, err := p.FetchSubscription(ctx(), fx.Account(), "I-SUB5"); err != nil || !got.CurrentPeriodStart.Equal(lastPaid.Truncate(time.Millisecond)) {
		t.Errorf("a renewed subscription: period start %v (%v), want its last payment %v", got.CurrentPeriodStart, err, lastPaid)
	}
	// Before the first payment there is no last payment: the period starts when the subscription did.
	first := paypalfix.Subscription("I-SUB3", contracts.ProPriceID, "ACTIVE", ours, start, next)
	delete(first["billing_info"].(map[string]any), "last_payment")
	fx.Subscriptions["I-SUB3"] = first
	if got, _, err := p.FetchSubscription(ctx(), fx.Account(), "I-SUB3"); err != nil || !got.CurrentPeriodStart.Equal(start) {
		t.Errorf("a subscription not yet paid: %+v, %v", got, err)
	}
	fx.Subscriptions["I-SUB4"] = paypalfix.Subscription("I-SUB4", "P-NOBODY-SELLS", "ACTIVE", ours, start, next)
	if got, _, err := p.FetchSubscription(ctx(), fx.Account(), "I-SUB4"); err != nil || got.PlanCode != "" {
		t.Errorf("a plan the catalogue does not sell: %+v, %v; want no plan code, which the processor ignores rather than guesses", got, err)
	}
	if _, _, err := p.FetchSubscription(ctx(), fx.Account(), "I-MISSING"); !errors.Is(err, md.ErrNotFound) {
		t.Errorf("a subscription PayPal does not have: %v, want ErrNotFound", err)
	}
}

func TestFetchedStateIsDatedByPayPalsClockNotOurs(t *testing.T) {
	t.Parallel()
	fx := paypalfix.New(t)
	ours := mcontracts.NewFakeClock(fx.Now().Add(7 * 24 * time.Hour))
	p, err := paypal.New(paypal.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: ours, BaseURL: fx.APIURL()})
	if err != nil {
		t.Fatal(err)
	}
	fx.Subscriptions["I-SUB1"] = paypalfix.Subscription("I-SUB1", contracts.ProPriceID, "ACTIVE", paypalfix.CustomID("cus_1", contracts.ProPriceID), fx.Now(), fx.Now().Add(time.Hour))
	if _, at, err := p.FetchSubscription(ctx(), fx.Account(), "I-SUB1"); err != nil || !at.Equal(fx.Now()) {
		t.Fatalf("at = %v (%v), want PayPal's own %v", at, err, fx.Now())
	}
}

func TestAnAPIFailureIsATypedErrorOfPayPals(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	fx.FailAll = true
	_, err := p.CreateCheckout(ctx(), fx.Account(), fx.Checkout())
	var api *paypal.APIError
	if !errors.As(err, &api) || api.Status != 500 || api.Name != "INTERNAL_SERVER_ERROR" || api.DebugID != "dbg-1" {
		t.Fatalf("err = %v, want an *APIError with status 500 and PayPal's debug id for support", err)
	}
	if errors.Is(err, domain.ErrVerificationUnavailable) {
		t.Fatalf("a failed checkout is not an unavailable verification: %v", err)
	}
}

func TestACancelledContextStopsACheckout(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	c, cancel := context.WithCancel(ctx())
	cancel()
	if _, err := p.CreateCheckout(c, fx.Account(), fx.Checkout()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatal("a cancelled context still reached PayPal")
	}
}

type recorder struct{ hosts []string }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.hosts = append(r.hosts, req.URL.Scheme+"://"+req.URL.Host)
	return nil, errors.New("recorded")
}

func TestTheAPIHostFollowsTheAccountsModeAndNothingElse(t *testing.T) {
	t.Parallel()
	fx := paypalfix.New(t)
	rt := &recorder{}
	p, err := paypal.New(paypal.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, HTTP: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	body, hdr := fx.SignedPaymentSucceeded()
	for mode, want := range map[string]string{"test": paypal.SandboxURL, "live": paypal.ProductionURL} {
		acct := fx.Account()
		acct.Mode = mode
		rt.hosts = nil
		_, _ = p.VerifyWebhook(ctx(), acct, body, hdr)
		if len(rt.hosts) == 0 || rt.hosts[0] != want {
			t.Errorf("mode %q went to %v, want %s: a test-mode account must never reach production, nor a live one the sandbox", mode, rt.hosts, want)
		}
	}
	acct := fx.Account()
	acct.Mode = "staging"
	rt.hosts = nil
	if _, err := p.VerifyWebhook(ctx(), acct, body, hdr); !errors.Is(err, md.ErrInvalid) || len(rt.hosts) != 0 {
		t.Errorf("an account of unknown mode: err = %v, called %v", err, rt.hosts)
	}
}

// ---------------------------------------------------------------- amounts --

func TestAmountsAreDecimalStringsInTheCurrencysOwnExponent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		minor    int64
		currency string
		text     string
	}{{1000, "USD", "10.00"}, {5, "EUR", "0.05"}, {0, "USD", "0.00"}, {1, "GBP", "0.01"}, {1500, "JPY", "1500"}, {7, "HUF", "7"}, {99, "TWD", "99"}, {12345678901, "USD", "123456789.01"}} {
		got, err := paypal.FormatAmount(tc.minor, tc.currency)
		if err != nil || got != tc.text {
			t.Errorf("FormatAmount(%d, %s) = %q, %v; want %q", tc.minor, tc.currency, got, err, tc.text)
		}
		back, err := paypal.ParseAmount(tc.text, tc.currency)
		if err != nil || back != tc.minor {
			t.Errorf("ParseAmount(%q, %s) = %d, %v; want %d", tc.text, tc.currency, back, err, tc.minor)
		}
	}
	if _, err := paypal.FormatAmount(-1, "USD"); err == nil {
		t.Error("a negative amount was formatted")
	}
	if _, err := paypal.FormatAmount(1, ""); err == nil {
		t.Error("an amount in no currency was formatted")
	}
	// 184467440737095517 × 100 is 2^64 + 84: without the check on the high word it would read as eighty-four cents.
	for _, bad := range []string{"184467440737095517.00", "184467440737095517", "", ".", "1.", ".5", "-1.00", "+1.00", "1e3", "1,000.00", " 1.00", "1.00 ", "1.001", "0x10", "ten", "1.0.0", "९.००"} {
		if n, err := paypal.ParseAmount(bad, "USD"); err == nil {
			t.Errorf("ParseAmount(%q, USD) = %d, want an error", bad, n)
		}
	}
	if n, err := paypal.ParseAmount("1000.5", "JPY"); err == nil {
		t.Errorf("a fraction of a yen was accepted: %d", n)
	}
	if n, err := paypal.ParseAmount("92233720368547758.08", "USD"); err == nil {
		t.Errorf("an amount past int64 was accepted: %d", n)
	}
}

var grammar = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// reference is the amount in minor units by arbitrary-precision arithmetic, or false if the text is not an amount the
// adapter should take: not the grammar, more decimals than the exponent, or past int64.
func reference(text string, exponent int) (int64, bool) {
	if !grammar.MatchString(text) {
		return 0, false
	}
	if _, frac, ok := strings.Cut(text, "."); ok && len(frac) > exponent {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return 0, false
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return r.Num().Int64(), true
}

func FuzzParseAmountAgreesWithArbitraryPrecisionArithmetic(f *testing.F) {
	for _, s := range []string{"184467440737095517.00", "10.00", "0.05", "0", "1000", "9223372036854775807", "92233720368547758.07", "92233720368547758.08", "1.001", "1.", ".5", "-1", "1e3", "00012.30", "٣.٠٠"} {
		f.Add(s, true)
		f.Add(s, false)
	}
	f.Fuzz(func(t *testing.T, text string, decimal bool) {
		currency, exponent := "USD", 2
		if !decimal {
			currency, exponent = "JPY", 0
		}
		got, err := paypal.ParseAmount(text, currency)
		want, ok := reference(text, exponent)
		switch {
		case ok && (err != nil || got != want):
			t.Fatalf("ParseAmount(%q, %s) = %d, %v; want %d", text, currency, got, err, want)
		case !ok && err == nil:
			t.Fatalf("ParseAmount(%q, %s) = %d, want an error", text, currency, got)
		}
	})
}

func FuzzFormatAmountThenParseIsTheIdentity(f *testing.F) {
	for _, n := range []int64{0, 1, 5, 99, 100, 1000, 123456789, 9223372036854775807} {
		f.Add(n, true)
		f.Add(n, false)
	}
	f.Fuzz(func(t *testing.T, minor int64, decimal bool) {
		currency, exponent := "EUR", 2
		if !decimal {
			currency, exponent = "HUF", 0
		}
		text, err := paypal.FormatAmount(minor, currency)
		if minor < 0 {
			if err == nil {
				t.Fatalf("FormatAmount(%d) = %q, want an error", minor, text)
			}
			return
		}
		if err != nil {
			t.Fatalf("FormatAmount(%d, %s): %v", minor, currency, err)
		}
		if want, ok := reference(text, exponent); !ok || want != minor {
			t.Fatalf("FormatAmount(%d, %s) = %q, which is %d by exact arithmetic", minor, currency, text, want)
		}
		if back, err := paypal.ParseAmount(text, currency); err != nil || back != minor {
			t.Fatalf("ParseAmount(%q) = %d, %v; want %d", text, back, err, minor)
		}
	})
}
