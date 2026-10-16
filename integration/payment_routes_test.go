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
