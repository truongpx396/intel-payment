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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/contracts/stripefix"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R098: payments run in the processes. A provider's signed delivery reaches paymentd's webhook route, is verified and
// persisted before it is acknowledged, and payment-worker — a different process, over the same database — fulfils it by
// minting credits through the engine. Nothing here is a package test: the binaries are built and run with nothing but
// PAYMENT_* variables and the references a provider account holds, against the real Postgres and Redis.

const (
	itAccount   = "stripe-main"
	itCustomer  = "cus_it"
	itPriceID   = "price_it_pack"
	itPackCode  = "pack-it"
	itCredits   = 5000
	itWebhookSk = "whsec_integration_test_secret"
	itAPIKey    = "sk_test_integration_only"
)

// seedPayments gives the realm a Stripe account, a pack on sale through it, and a customer that is the scope.
func seedPayments(t *testing.T, pool *pgxpool.Pool, realm string, sc domain.Scope) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref) VALUES ($1,'stripe',ARRAY[$2]::text[],'test','env:STRIPE_SECRET_KEY','env:STRIPE_WEBHOOK_SECRET')`, itAccount, realm)
	exec(`INSERT INTO plans (realm, code, name, kind, credit_allotment, pool) VALUES ($1,$2,'Credit pack','one_time',$3,'general')`, realm, itPackCode, itCredits)
	exec(`INSERT INTO plan_prices (plan_id, currency, minor_units) SELECT id, 'USD', 1000 FROM plans WHERE realm = $1 AND code = $2`, realm, itPackCode)
	exec(`INSERT INTO plan_provider_prices (plan_id, provider_account, currency, provider_price_id) SELECT id, $3, 'USD', $4 FROM plans WHERE realm = $1 AND code = $2`, realm, itPackCode, itAccount, itPriceID)
	exec(`INSERT INTO billing_customers (realm, scope_kind, scope_id, provider_account, provider_customer_id) VALUES ($1,$2,$3,$4,$5)`, realm, sc.Kind, sc.ID, itAccount, itCustomer)
}

func paymentsEnv(env map[string]string) map[string]string {
	env["STRIPE_SECRET_KEY"], env["STRIPE_WEBHOOK_SECRET"] = itAPIKey, itWebhookSk
	env["PAYMENT_WEBHOOK_TICK_INTERVAL"] = "100ms"
	return env
}

// delivery is a signed invoice.paid for the pack, as Stripe would send it now.
func delivery(t *testing.T, eventID, paymentIntent string) (string, string) {
	t.Helper()
	fx := stripefix.New(t)
	now := time.Now()
	body := fx.Event(eventID, "invoice.paid", now, stripefix.Invoice("in_"+eventID, itCustomer, paymentIntent, "", itPriceID, 1000))
	return string(body), stripefix.SignatureHeader(itWebhookSk, now, body)
}

func balanceOf(t *testing.T, c *http.Client, base string, sc domain.Scope) int64 {
	t.Helper()
	code, body := httpDo(t, c, "GET", fmt.Sprintf("%s/v1/balance?scope_kind=%s&scope_id=%s", base, sc.Kind, sc.ID), serviceToken, "")
	if code != 200 {
		return -1
	}
	var b struct{ Total int64 }
	must(t, json.Unmarshal([]byte(body), &b))
	return b.Total
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

func TestAProvidersSignedPaymentIsVerifiedAtTheDoorAndFulfilledByTheWorkerExactlyOnce(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	seedPayments(t, pool, realm, sc)

	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL)))
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8")))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	hook := base + "/webhooks/stripe/" + itAccount

	body, sig := delivery(t, "evt_it_1", "pi_it_1")
	code, answer := httpDo(t, c, "POST", hook, "", body, "Stripe-Signature", sig, "Content-Type", "application/json")
	if code != http.StatusOK {
		t.Fatalf("a genuine delivery was answered %d %s", code, answer)
	}
	// The same delivery again, twice at once, as a provider retries: acknowledged, never fulfilled twice.
	for range 3 {
		if code, answer := httpDo(t, c, "POST", hook, "", body, "Stripe-Signature", sig); code != http.StatusOK {
			t.Fatalf("a replay was answered %d %s", code, answer)
		}
	}

	eventually(t, 30*time.Second, "the worker to mint the pack's credits", func() bool { return balanceOf(t, c, base, sc) == itCredits })
	time.Sleep(500 * time.Millisecond) // long enough for a duplicate grant to show, at a 100 ms tick
	if got := balanceOf(t, c, base, sc); got != itCredits {
		t.Fatalf("the balance is %d after replays, want %d: a replayed delivery was fulfilled again", got, itCredits)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_events WHERE provider_account = $1`, itAccount); n != 1 {
		t.Errorf("%d events in the inbox for four deliveries of one event, want 1", n)
	}
	var status string
	var granted int64
	must(t, pool.QueryRow(context.Background(), `SELECT status, credits_granted FROM payments WHERE provider_account = $1 AND provider_payment_id = 'pi_it_1'`, itAccount).Scan(&status, &granted))
	if status != "succeeded" || granted != itCredits {
		t.Errorf("the payment is %s with %d credits granted, want succeeded with %d", status, granted, itCredits)
	}
	eventually(t, 30*time.Second, "the worker to book the grant", func() bool { return bookedFor(t, pool, realm, sc).sum == itCredits })

	// What the door refuses leaves nothing behind.
	forged := strings.Replace(body, `"amount_paid":1000`, `"amount_paid":100000`, 1)
	for name, tc := range map[string]struct {
		method, path, body, sig string
		want                    int
	}{
		"a tampered body":       {"POST", "/webhooks/stripe/" + itAccount, forged, sig, 400},
		"no signature":          {"POST", "/webhooks/stripe/" + itAccount, body, "", 400},
		"an unknown account":    {"POST", "/webhooks/stripe/nobody", body, sig, 404},
		"an unknown provider":   {"POST", "/webhooks/nopay/" + itAccount, body, sig, 404},
		"another provider's":    {"POST", "/webhooks/polar/" + itAccount, body, sig, 404},
		"a body over the limit": {"POST", "/webhooks/stripe/" + itAccount, strings.Repeat("x", 2<<20), sig, 413},
		"a GET":                 {"GET", "/webhooks/stripe/" + itAccount, "", "", 405},
	} {
		hdr := []string{}
		if tc.sig != "" {
			hdr = append(hdr, "Stripe-Signature", tc.sig)
		}
		if code, answer := httpDo(t, c, tc.method, base+tc.path, "", tc.body, hdr...); code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, answer, tc.want)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_events WHERE provider_account = $1`, itAccount); n != 1 {
		t.Errorf("%d events in the inbox after refused deliveries, want still 1", n)
	}
	if got := balanceOf(t, c, base, sc); got != itCredits {
		t.Errorf("the balance moved to %d on refused deliveries", got)
	}

	// The webhook route asks for no credential, and /v1 still does.
	if code, _ := httpDo(t, c, "GET", base+"/v1/balance?scope_kind=workspace&scope_id=x", "", ""); code != http.StatusUnauthorized {
		t.Errorf("/v1/balance with no credential = %d, want 401: mounting the webhook route must not open the rest", code)
	}

	// Both processes report what they did, under the names an operator alerts on.
	assertMetric(t, c, base+"/metrics", `billing_webhook_received_total{provider="stripe",result="new"} 1`)
	assertMetric(t, c, base+"/metrics", `billing_webhook_received_total{provider="stripe",result="duplicate"} 3`)
	assertMetric(t, c, base+"/metrics", `billing_webhook_verify_failed_total{provider="stripe"} 2`)
	wbase := "http://" + w.listening(t, "ops")
	assertMetric(t, c, wbase+"/metrics", `billing_webhook_processed_total{outcome="processed",provider="stripe"} 1`)
	assertMetric(t, c, wbase+"/metrics", `billing_grant_applied_total`)
}

func assertMetric(t *testing.T, c *http.Client, url, line string) {
	t.Helper()
	eventually(t, 15*time.Second, line, func() bool {
		_, body := httpDo(t, c, "GET", url, "", "")
		return strings.Contains(body, line)
	})
}

func TestALowBalanceReachesAutoTopUpInTheWorker(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	// A host's own low-balance watch, with no auto top-up instruction behind it: the message must reach the handler
	// (which finds nothing to do) by way of the hot tier, the writer, the bus and the worker's subscription.
	if _, err := pool.Exec(context.Background(), `INSERT INTO balance_watches (realm, scope_kind, scope_id, pool, key, threshold) VALUES ($1,$2,$3,'general','host_low',$4)`,
		realm, sc.Kind, sc.ID, workerGrant-100); err != nil {
		t.Fatal(err)
	}

	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL)))
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8")))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)

	grantWhenReady(t, c, base, scopeJSON)
	record(t, c, base, scopeJSON, "r-low") // 660 credits: the balance falls past the watch

	wbase := "http://" + w.listening(t, "ops")
	assertMetric(t, c, wbase+"/metrics", `billing_autorecharge_total{result="ignored"} 1`)
}
