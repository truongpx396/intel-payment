//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts/stripefix"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R099d: the routes a host's backend calls to sell are served by paymentd, behind the credential, with the privilege the
// contract gives each — in the process, over the real Postgres. A credential that can only meter cannot spend a customer's
// money; a call no credential reached is refused; the realm is the credential's. Nothing here reaches a provider: a checkout
// for a plan nobody sells is refused by the catalogue before any provider is asked.

const readOnlyToken = "integration-read-only-token"

func TestPaymentdServesTheHostsPaymentRoutesBehindTheCredentialAndItsPrivilege(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	seedPayments(t, pool, realm, sc)

	env := paymentsEnv(paymentdEnv(realm, dsn))
	env["PAYMENT_SERVICE_TOKENS"] += ",viewer:" + auth.HashToken(readOnlyToken) + ":" + realm + ":record"
	d := startPaymentd(t, env)
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	q := fmt.Sprintf("scope_kind=%s&scope_id=%s", sc.Kind, sc.ID)

	// The catalogue: the pack the realm sells, by its slug — and nothing of the provider's.
	code, body := httpDo(t, c, "GET", base+"/v1/plans?currency=USD", readOnlyToken, "")
	if code != http.StatusOK || !strings.Contains(body, `"code":"`+itPackCode+`"`) || !strings.Contains(body, `"USD":1000`) {
		t.Fatalf("GET /v1/plans = %d %s, want the pack priced 1000 USD", code, body)
	}
	if strings.Contains(body, itPriceID) || strings.Contains(body, itAccount) {
		t.Errorf("the catalogue names the provider's price or account: %s", body)
	}

	// Reads of a scope that has bought nothing.
	for path, want := range map[string]string{
		"/v1/subscription?" + q: "null", "/v1/auto-recharge?" + q: "null",
		"/v1/payments?" + q: `{"data":[],"next":null}`, "/v1/fulfilment?" + q: `{"kind":"none"}`,
	} {
		if code, body := httpDo(t, c, "GET", base+path, readOnlyToken, ""); code != 200 || strings.TrimSpace(body) != want {
			t.Errorf("GET %s = %d %s, want 200 %s", path, code, body, want)
		}
	}

	// No credential, and a credential for the wrong privilege.
	if code, body := httpDo(t, c, "GET", base+"/v1/plans?currency=USD", "", ""); code != http.StatusUnauthorized {
		t.Errorf("a call with no credential = %d %s, want 401", code, body)
	}
	instruction := fmt.Sprintf(`{"scope":{"kind":%q,"id":%q},"enabled":true,"pool":"general","threshold":500,"plan_code":%q,"currency":"USD","account":%q,"payment_method_ref":"pm_it","max_per_day":3}`,
		sc.Kind, sc.ID, itPackCode, itAccount)
	for _, call := range []struct{ method, path, body string }{
		{"PUT", "/v1/auto-recharge", instruction},
		{"POST", "/v1/subscription/change", fmt.Sprintf(`{"scope":{"kind":%q,"id":%q},"plan_code":"x"}`, sc.Kind, sc.ID)},
		{"POST", "/v1/subscription/cancel", fmt.Sprintf(`{"scope":{"kind":%q,"id":%q}}`, sc.Kind, sc.ID)},
	} {
		if code, body := httpDo(t, c, call.method, base+call.path, readOnlyToken, call.body); code != http.StatusForbidden || !strings.Contains(body, `"forbidden"`) {
			t.Errorf("%s %s with a record-only credential = %d %s, want 403 forbidden", call.method, call.path, code, body)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM auto_recharge WHERE realm = $1`, realm); n != 0 {
		t.Errorf("%d instructions were stored by a credential that cannot spend", n)
	}

	// A standing instruction, by a credential that can: stored, and read back without the method's reference.
	code, body = httpDo(t, c, "PUT", base+"/v1/auto-recharge", serviceToken, instruction)
	if code != http.StatusOK || !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"has_payment_method":true`) || strings.Contains(body, "pm_it") {
		t.Errorf("PUT /v1/auto-recharge = %d %s", code, body)
	}
	var ref string
	must(t, pool.QueryRow(context.Background(), `SELECT payment_method_ref FROM auto_recharge WHERE realm = $1 AND scope_id = $2`, realm, sc.ID).Scan(&ref))
	if ref != "pm_it" {
		t.Errorf("the stored method reference is %q", ref)
	}

	// The realm is the credential's: a body that names another is refused before anything is sold.
	other := strings.Replace(instruction, `"kind"`, `"realm":"someone-else","kind"`, 1)
	if code, body := httpDo(t, c, "PUT", base+"/v1/auto-recharge", serviceToken, other); code != http.StatusForbidden || !strings.Contains(body, "realm_mismatch") {
		t.Errorf("a realm outside the credential = %d %s, want 403 realm_mismatch", code, body)
	}

	// A checkout for a plan nobody sells is refused by the catalogue, before any provider is asked.
	checkout := fmt.Sprintf(`{"scope":{"kind":%q,"id":%q},"plan_code":"no-such-pack","currency":"USD","success_url":"https://app.example/ok","cancel_url":"https://app.example/no"}`, sc.Kind, sc.ID)
	code, body = httpDo(t, c, "POST", base+"/v1/checkout", readOnlyToken, checkout, "Idempotency-Key", "buy-1")
	var e struct{ Error struct{ Code string } }
	must(t, json.Unmarshal([]byte(body), &e))
	if code != http.StatusUnprocessableEntity || e.Error.Code != "unknown_price" {
		t.Errorf("a checkout of a plan nobody sells = %d %s, want 422 unknown_price", code, body)
	}
	if code, body := httpDo(t, c, "POST", base+"/v1/checkout", readOnlyToken, checkout); code != http.StatusBadRequest {
		t.Errorf("a checkout with no Idempotency-Key = %d %s, want 400", code, body)
	}
}

func TestPaymentdRefusesToStartUnderASellingSettingThatCannotWork(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	for name, value := range map[string]string{"PAYMENT_TAX_STRATEGY": "external_hook", "PAYMENT_PLAN_CHANGE_POLICY": "whenever"} {
		d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, name, value)))
		if code := d.waitExit(t, 30*time.Second); code != 2 {
			t.Errorf("%s=%s: paymentd exited %d, want 2 (a refusal)", name, value, code)
		}
		if !strings.Contains(d.logs.String(), name) {
			t.Errorf("%s=%s: the refusal does not name the variable:\n%s", name, value, d.logs.String())
		}
	}
}

// R101a: payment configuration is data with an admin API, and it is the API — not SQL — that makes a deployment able to sell:
// an account, a plan, the provider's price that sells it, and a customer. Every change names a person and a reason and is
// attributed by the database, an account created through it is the one the webhook door then verifies against, and a
// credential that can only meter touches none of it.
func TestPaymentdAdministersThePaymentConfigurationAndAttributesEveryChange(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	env := paymentsEnv(paymentdEnv(realm, dsn))
	env["PAYMENT_SERVICE_TOKENS"] += ",viewer:" + auth.HashToken(readOnlyToken) + ":" + realm + ":record"
	d := startPaymentd(t, env)
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	const acct, price = "stripe-admin", "price_admin_pack"
	who := `"actor":"ops@example.test","reason":"onboarding"`
	status := func(method, path, token, body string) (int, string) {
		return httpDo(t, c, method, base+path, token, body)
	}

	// A credential that can only meter cannot read or write configuration.
	for _, call := range []struct{ method, path, body string }{
		{"GET", "/v1/admin/plans", ""}, {"GET", "/v1/admin/provider-accounts", ""},
		{"PUT", "/v1/admin/provider-accounts/" + acct, ""}, {"POST", "/v1/admin/plans", ""},
	} {
		if code, body := status(call.method, call.path, readOnlyToken, call.body); code != http.StatusForbidden {
			t.Errorf("%s %s with a record-only credential = %d %s, want 403", call.method, call.path, code, body)
		}
	}

	// An account this deployment cannot use is refused, whoever asks.
	account := func(provider, ref string, realms ...string) string {
		quoted := make([]string, len(realms))
		for i, r := range realms {
			quoted[i] = `"` + r + `"`
		}
		return fmt.Sprintf(`{%s,"provider":%q,"realms":[%s],"mode":"test","secret_ref":%q,"webhook_secret_ref":"env:STRIPE_WEBHOOK_SECRET","active":true}`,
			who, provider, strings.Join(quoted, ","), ref)
	}
	for name, body := range map[string]string{
		"a provider with no adapter":   account("quickbooks", "env:STRIPE_SECRET_KEY", realm),
		"a scheme nothing resolves":    account("stripe", "vault:secret/stripe", realm),
		"a realm the credential lacks": account("stripe", "env:STRIPE_SECRET_KEY", realm, "someone-else"),
	} {
		if code, out := status("PUT", "/v1/admin/provider-accounts/"+acct, serviceToken, body); code < 400 || code >= 500 {
			t.Errorf("%s: %d %s, want a 4xx", name, code, out)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM provider_accounts WHERE id = $1`, acct); n != 0 {
		t.Fatalf("%d accounts stored from requests that were refused", n)
	}

	// Onboarding through the API: the account, a plan, the provider's price that sells it, and a customer.
	if code, out := status("PUT", "/v1/admin/provider-accounts/"+acct, serviceToken, account("stripe", "env:STRIPE_SECRET_KEY", realm)); code != http.StatusCreated {
		t.Fatalf("PUT account = %d %s, want 201", code, out)
	}
	planBody := fmt.Sprintf(`{%s,"code":"pack-admin","name":"Admin pack","kind":"one_time","credit_allotment":%d,"prices":{"USD":1000}}`, who, itCredits)
	if code, out := status("POST", "/v1/admin/plans", serviceToken, planBody); code != http.StatusCreated {
		t.Fatalf("POST plan = %d %s, want 201", code, out)
	}
	if code, out := status("POST", "/v1/admin/plans", serviceToken, planBody); code != http.StatusConflict {
		t.Errorf("POST the same plan again = %d %s, want 409", code, out)
	}
	mapping := "/v1/admin/plans/pack-admin/provider-prices/" + acct + "/USD"
	if code, out := status("PUT", mapping, serviceToken, fmt.Sprintf(`{%s,"provider_price_id":%q}`, who, price)); code != http.StatusCreated {
		t.Fatalf("PUT provider price = %d %s, want 201", code, out)
	}
	if code, out := status("PUT", mapping, serviceToken, fmt.Sprintf(`{%s,"provider_price_id":"price_replacement"}`, who)); code != http.StatusConflict {
		t.Errorf("replacing a mapping = %d %s, want 409: a subscriber still renews at the old price", code, out)
	}
	customer := fmt.Sprintf(`{%s,"scope":{"kind":"workspace","id":"w-admin"},"account":%q,"provider_customer_id":%q}`, who, acct, itCustomer)
	if code, out := status("PUT", "/v1/admin/customers", serviceToken, customer); code != http.StatusCreated {
		t.Fatalf("PUT customer = %d %s, want 201", code, out)
	}
	if code, out := status("PUT", "/v1/admin/customers", serviceToken, strings.Replace(customer, itCustomer, "cus_other", 1)); code != http.StatusConflict {
		t.Errorf("re-pointing a customer = %d %s, want 409", code, out)
	}

	// What was written is what is sold: the client's catalogue shows the pack by its slug, and not the provider's price.
	code, out := status("GET", "/v1/plans?currency=USD", readOnlyToken, "")
	if code != http.StatusOK || !strings.Contains(out, `"code":"pack-admin"`) || strings.Contains(out, price) {
		t.Errorf("GET /v1/plans = %d %s", code, out)
	}
	// ... and the admin sees the mapping.
	if code, out := status("GET", "/v1/admin/plans/pack-admin", serviceToken, ""); code != http.StatusOK || !strings.Contains(out, price) {
		t.Errorf("GET admin plan = %d %s, want the provider's price id", code, out)
	}

	// The account made through the API is the one the door verifies against.
	body, _ := delivery(t, "evt_admin_1", "pi_admin_1")
	body = strings.ReplaceAll(body, itPriceID, price)
	sig := resign(t, body)
	if code, out := httpDo(t, c, "POST", base+"/webhooks/stripe/"+acct, "", body, "Stripe-Signature", sig, "Content-Type", "application/json"); code != http.StatusOK {
		t.Errorf("a signed delivery for the account made through the API = %d %s, want 200", code, out)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_events WHERE provider_account = $1`, acct); n != 1 {
		t.Errorf("%d events in the inbox for the account, want 1", n)
	}

	// Every change is attributed: the person, the reason, and the credential it came through.
	for _, want := range []struct{ kind, action string }{
		{"provider_accounts", "config.insert"}, {"plans", "config.insert"}, {"plan_provider_prices", "config.insert"}, {"billing_customers", "customer.link"},
	} {
		var actor, reason string
		err := pool.QueryRow(context.Background(), `SELECT actor, coalesce(reason,'') FROM audit_log WHERE subject_kind = $1 AND action = $2 AND actor LIKE 'ops@example.test%' LIMIT 1`, want.kind, want.action).Scan(&actor, &reason)
		if err != nil || actor != "ops@example.test (via integration)" || reason != "onboarding" {
			t.Errorf("audit of %s %s = %q %q (%v), want the person, the reason and the credential", want.kind, want.action, actor, reason, err)
		}
	}
}

// resign signs a body the way Stripe would have, now, with the secret the test gives the process.
func resign(t *testing.T, body string) string {
	t.Helper()
	return stripefix.SignatureHeader(itWebhookSk, time.Now(), []byte(body))
}
