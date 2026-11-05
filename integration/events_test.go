//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R115, end to end: a customer's balance falls past a watch, and a host that registered an endpoint is told — a signed webhook that
// verifies with its secret, carrying an id that is also in the feed it can pull. Real paymentd and payment-worker processes, Redis,
// PostgreSQL and the bus; the host is a local HTTP server.

type webhookDelivery struct {
	header http.Header
	body   []byte
}

func TestALowBalanceReachesAHostAsASignedWebhookAndAsAFeedEntryWithTheSameId(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	if _, err := pool.Exec(t.Context(), `INSERT INTO balance_watches (realm, scope_kind, scope_id, pool, key, threshold) VALUES ($1,$2,$3,'general','host_low',$4)`,
		realm, sc.Kind, sc.ID, workerGrant-100); err != nil {
		t.Fatal(err)
	}

	// The host: an endpoint that records what it is sent.
	var mu sync.Mutex
	var got []webhookDelivery
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, webhookDelivery{r.Header.Clone(), b})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer host.Close()
	const secret = "whsec_integration_secret"
	if _, err := pool.Exec(t.Context(), `INSERT INTO event_endpoints (realm, url, secret_ref, event_types) VALUES ($1, $2, 'env:EVENTS_TEST_SECRET', ARRAY['credits.low'])`, realm, host.URL+"/hook"); err != nil {
		t.Fatal(err)
	}

	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL,
		"EVENTS_TEST_SECRET", secret, "PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS", "true", "PAYMENT_EVENTS_TICK_INTERVAL", "100ms")))
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8")))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	grantWhenReady(t, c, base, scopeJSON)
	record(t, c, base, scopeJSON, "r-low") // the balance falls past the watch

	eventually(t, 30*time.Second, "the host to be told", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 1 })
	mu.Lock()
	first := got[0]
	mu.Unlock()

	// A signature the host can check, with the secret it registered, and with no other.
	if err := events.Verify(first.header.Get(events.HeaderSignature), first.body, []byte(secret), time.Now(), 5*time.Minute); err != nil {
		t.Fatalf("the webhook does not verify with the endpoint's secret: %v\nheaders: %v\nbody: %s", err, first.header, first.body)
	}
	if err := events.Verify(first.header.Get(events.HeaderSignature), first.body, []byte("another"), time.Now(), 5*time.Minute); err == nil {
		t.Fatal("it verifies with a secret that is not the endpoint's")
	}
	var env struct {
		ID, Type, Realm, APIVersion string
		Scope                       struct{ Kind, ID string }
		Data                        map[string]any
	}
	must(t, json.Unmarshal(first.body, &env))
	if env.Type != "credits.low" || env.Realm != realm || env.Scope.ID != sc.ID || env.ID != first.header.Get(events.HeaderEventID) || env.Data["pool"] != "general" {
		t.Fatalf("the envelope: %s", first.body)
	}
	if strings.Contains(string(first.body), "billing_email") {
		t.Fatalf("the envelope leaks: %s", first.body)
	}

	// The same event is in the feed, with the same id and the same bytes, and the cursor leaves nothing after it.
	code, feed := httpDo(t, c, "GET", base+"/v1/events", serviceToken, "")
	if code != 200 {
		t.Fatalf("GET /v1/events = %d %s", code, feed)
	}
	var page struct {
		Data []json.RawMessage
		Next string
	}
	must(t, json.Unmarshal([]byte(feed), &page))
	if len(page.Data) != 1 || string(page.Data[0]) != string(first.body) || page.Next != env.ID {
		t.Fatalf("the feed:\n%s\nthe webhook:\n%s", feed, first.body)
	}
	if code, body := httpDo(t, c, "GET", base+"/v1/events?after="+page.Next, serviceToken, ""); code != 200 || !strings.Contains(body, `"data":[]`) || !strings.Contains(body, page.Next) {
		t.Fatalf("after the cursor: %d %s", code, body)
	}
	if code, _ := httpDo(t, c, "GET", base+"/v1/events", "", ""); code != 401 {
		t.Fatalf("the feed without a credential: %d", code)
	}

	// Observable, and not sent twice.
	wbase := "http://" + w.listening(t, "ops")
	assertMetric(t, c, wbase+"/metrics", `events_recorded_total{type="credits.low"} 1`)
	assertMetric(t, c, wbase+"/metrics", `events_delivery_total{outcome="delivered"} 1`)
	time.Sleep(time.Second)
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("the host was sent the event %d times", n)
	}
}
