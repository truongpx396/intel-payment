package paymenthttp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

const checkoutBody = `{"scope":{"kind":"user","id":"u1"},"plan_code":"pack_10","currency":"USD","mode":"payment","account":"stripe-test",` +
	`"success_url":"https://app.example/ok","cancel_url":"https://app.example/no","billing_email":"a@example.com"}`

// checkoutDoor serves a checkout and records every request it was asked to start.
func checkoutDoor(t *testing.T, reqs *[]ports.CheckoutRequest, answer func(ports.CheckoutRequest) (string, error)) *door {
	t.Helper()
	return newDoor(t, paymenthttp.Config{Checkout: checkoutStub{start: func(_ context.Context, req ports.CheckoutRequest) (string, error) {
		*reqs = append(*reqs, req)
		return answer(req)
	}}})
}

func ok(ports.CheckoutRequest) (string, error) { return "https://pay.example/session/abc", nil }

// A checkout is what the caller asked to buy, for the scope its credential covers — and nothing else: it names a plan, never a
// price, and it goes to the use case with the Idempotency-Key as its identity.
func TestCheckoutStartsExactlyWhatWasAskedForTheCredentialsRealm(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)

	got := d.call(t, "POST", "/v1/checkout", checkoutBody, "Idempotency-Key", "buy-1")
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", got.Status, got.Body)
	}
	var out struct {
		RedirectURL string `json:"redirect_url"`
	}
	got.JSON(t, &out)
	if out.RedirectURL != "https://pay.example/session/abc" {
		t.Errorf("redirect_url = %q", out.RedirectURL)
	}
	want := ports.CheckoutRequest{
		Scope: domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}, PlanCode: "pack_10", Currency: "USD", Mode: "payment", Account: "stripe-test",
		SuccessURL: "https://app.example/ok", CancelURL: "https://app.example/no", BillingEmail: "a@example.com", IdemKey: "buy-1",
	}
	if len(reqs) != 1 || reqs[0] != want {
		t.Errorf("the use case was asked for %+v, want exactly %+v", reqs, want)
	}
}

// The same buy button pressed twice reaches the use case twice with the same key: the door does not decide a replay, the
// provider does, on the key.
func TestCheckoutARetryCarriesTheSameKeyToTheUseCase(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)
	for range 2 {
		if got := d.call(t, "POST", "/v1/checkout", checkoutBody, "Idempotency-Key", "buy-1"); got.Status != http.StatusOK {
			t.Fatalf("status = %d: %s", got.Status, got.Body)
		}
	}
	if len(reqs) != 2 || reqs[0].IdemKey != "buy-1" || reqs[1].IdemKey != "buy-1" {
		t.Errorf("the two attempts carried %+v, want the one key twice", reqs)
	}
}

func TestCheckoutNeedsExactlyOneNonBlankIdempotencyKey(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)
	for name, hdr := range map[string][]string{"none": nil, "blank": {"Idempotency-Key", "  "}} {
		if got := d.call(t, "POST", "/v1/checkout", checkoutBody, hdr...); got.Status != http.StatusBadRequest || got.ErrorCode(t) != "invalid_request" {
			t.Errorf("%s: %d %s, want 400 invalid_request", name, got.Status, got.Body)
		}
	}
	two := httpHeaders("Idempotency-Key", "a", "Idempotency-Key", "b")
	req, _ := http.NewRequestWithContext(t.Context(), "POST", "/v1/checkout", strings.NewReader(checkoutBody))
	req.Header = two
	req.Header.Set("X-Realms", "acme")
	rec := serve(d, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("two keys: %d, want 400", rec.Code)
	}
	if len(reqs) != 0 {
		t.Errorf("%d checkouts were started without a usable key: a server-made key would make every retry a new session", len(reqs))
	}
}

// The realm is the credential's. A body that names another is refused before anything is sold, and a credential that
// spans several and a body that names none is not guessed at.
func TestCheckoutTheRealmIsTheCredentialsAndNeverTheBodys(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)
	other := strings.Replace(checkoutBody, `"id":"u1"`, `"id":"u1","realm":"other"`, 1)
	if got := d.call(t, "POST", "/v1/checkout", other, "Idempotency-Key", "k"); got.Status != http.StatusForbidden || got.ErrorCode(t) != "realm_mismatch" {
		t.Errorf("a realm outside the credential: %d %s, want 403 realm_mismatch", got.Status, got.Body)
	}
	if got := d.call(t, "POST", "/v1/checkout", checkoutBody, "Idempotency-Key", "k", "X-Realms", "acme+beta"); got.Status != http.StatusBadRequest {
		t.Errorf("two realms and none named: %d %s, want 400", got.Status, got.Body)
	}
	named := strings.Replace(checkoutBody, `"id":"u1"`, `"id":"u1","realm":"beta"`, 1)
	if got := d.call(t, "POST", "/v1/checkout", named, "Idempotency-Key", "k", "X-Realms", "acme+beta"); got.Status != http.StatusOK {
		t.Errorf("the second realm, named: %d %s, want 200", got.Status, got.Body)
	}
	if len(reqs) != 1 || reqs[0].Scope.Realm != "beta" {
		t.Errorf("started %+v, want one checkout in realm beta", reqs)
	}
}

// What a checkout costs is the catalogue's. A request that tries to say — a price, an amount, a credit quantity, a provider's
// price id, a grant — is a request with a field the door does not have, and it is refused by name rather than ignored.
func TestCheckoutARequestCannotNameAPriceACreditsQuantityOrAGrant(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)
	for _, field := range []string{`"amount":1`, `"price":1`, `"credits":999999`, `"provider_price_id":"price_x"`, `"grant":true`, `"idem_key":"k"`, `"customer_id":"cus_x"`} {
		body := strings.Replace(checkoutBody, `{`, `{`+field+`,`, 1)
		got := d.call(t, "POST", "/v1/checkout", body, "Idempotency-Key", "k")
		name := strings.Trim(strings.SplitN(field, ":", 2)[0], `"`)
		if got.Status != http.StatusBadRequest || !strings.Contains(string(got.Body), name) {
			t.Errorf("%s: %d %s, want 400 naming the field", field, got.Status, got.Body)
		}
	}
	if len(reqs) != 0 {
		t.Errorf("%d checkouts were started from a request with a field the door does not have", len(reqs))
	}
}

func TestCheckoutMalformedRequestsAreRefusedBeforeTheUseCase(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)
	for name, body := range map[string]string{
		"no scope":          `{"plan_code":"p","currency":"USD","success_url":"https://a/","cancel_url":"https://a/"}`,
		"not JSON":          `{`,
		"empty":             ``,
		"two documents":     checkoutBody + checkoutBody,
		"trailing garbage":  checkoutBody + ` x`,
		"a string scope":    `{"scope":"user:u1"}`,
		"a number plan":     strings.Replace(checkoutBody, `"pack_10"`, `10`, 1),
		"null":              `null`,
		"an array":          `[]`,
		"a fractional body": strings.Replace(checkoutBody, `"id":"u1"`, `"id":1.5`, 1),
	} {
		if got := d.call(t, "POST", "/v1/checkout", body, "Idempotency-Key", "k"); got.Status != http.StatusBadRequest || got.ErrorCode(t) != "invalid_request" {
			t.Errorf("%s: %d %s, want 400 invalid_request", name, got.Status, got.Body)
		}
	}
	if len(reqs) != 0 {
		t.Errorf("%d checkouts were started from a malformed request", len(reqs))
	}
}

func TestCheckoutABodyOverTheCapIsRefusedAsTooLarge(t *testing.T) {
	t.Parallel()
	var reqs []ports.CheckoutRequest
	d := checkoutDoor(t, &reqs, ok)
	huge := `{"scope":{"kind":"user","id":"` + strings.Repeat("x", 2<<20) + `"}}`
	got := d.call(t, "POST", "/v1/checkout", huge, "Idempotency-Key", "k")
	if got.Status != http.StatusRequestEntityTooLarge || got.ErrorCode(t) != "invalid_request" {
		t.Errorf("a 2 MiB body: %d %.80s, want 413 invalid_request", got.Status, got.Body)
	}
	if len(reqs) != 0 {
		t.Error("a checkout was started from a body over the cap")
	}
}

// Each thing the use case can refuse is a status a client can act on, and nothing else leaks.
func TestCheckoutRefusalsAreStatusesAndTheRestIsFixedText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"an unknown plan or price", fmt.Errorf("%w: pack_99 in USD", domain.ErrUnknownPrice), 422, "unknown_price"},
		{"an account that does not serve the realm", fmt.Errorf("%w: x does not serve realm acme", domain.ErrUnknownAccount), 404, "unknown_account"},
		{"an invalid request", fmt.Errorf("%w: a return URL must be https", md.ErrInvalid), 400, "invalid_request"},
		{"a provider that cannot", domain.ErrUnsupported, 422, "unsupported"},
		{"a key used for another request", md.ErrIdemConflict, 409, "idempotency_conflict"},
		{"the caller went away", context.Canceled, 499, "cancelled"},
		{"the deadline passed", context.DeadlineExceeded, 504, "deadline_exceeded"},
		{"anything else", errBoom, 500, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := &logSink{}
			d := newDoor(t, paymenthttp.Config{Logger: log.logger(), Checkout: checkoutStub{start: func(context.Context, ports.CheckoutRequest) (string, error) { return "", tc.err }}})
			got := d.call(t, "POST", "/v1/checkout", checkoutBody, "Idempotency-Key", "k")
			if got.Status != tc.status || got.ErrorCode(t) != tc.code {
				t.Fatalf("%d %s, want %d %s", got.Status, got.Body, tc.status, tc.code)
			}
			if tc.status == 500 {
				if strings.Contains(string(got.Body), "10.0.0.7") {
					t.Errorf("a 500 leaks the topology: %s", got.Body)
				}
				if !strings.Contains(log.String(), "10.0.0.7") {
					t.Errorf("the log lacks the cause: %q", log.String())
				}
			}
		})
	}
}

func TestCheckoutAnErrorWrappedDeepIsStillFound(t *testing.T) {
	t.Parallel()
	d := newDoor(t, paymenthttp.Config{Checkout: checkoutStub{start: func(context.Context, ports.CheckoutRequest) (string, error) {
		return "", fmt.Errorf("checkout: %w", errors.Join(errBoom, fmt.Errorf("sell: %w", domain.ErrUnknownPrice)))
	}}})
	if got := d.call(t, "POST", "/v1/checkout", checkoutBody, "Idempotency-Key", "k"); got.Status != 422 {
		t.Errorf("a joined, wrapped ErrUnknownPrice = %d %s, want 422", got.Status, got.Body)
	}
}
