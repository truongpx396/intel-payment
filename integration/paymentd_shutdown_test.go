//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R067a: a deploy stops the old paymentd while callers are mid-call. A Record in flight must settle or be
// journalled — never lost, never booked twice — and the listener must stop taking new calls BEFORE it waits for
// the old ones, or a busy service never finishes stopping.
//
// A Record is held in flight deterministically: Redis is told to pause writes, so the hot function the Record
// is waiting on does not run until the pause ends.

func pauseRedisWrites(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	rdb := hotredisClient(addr)
	defer func() { _ = rdb.Close() }()
	must(t, rdb.Do(t.Context(), "CLIENT", "PAUSE", d.Milliseconds(), "WRITE").Err())
}

// dedicatedRedis is a Redis of the test's own. Pausing writes pauses every client of the server, so the tests that
// do it cannot share the package's Redis with the ones running beside them.
func dedicatedRedis(t *testing.T) string {
	t.Helper()
	r := redistest.Start(context.Background(), t, redistest.Standalone)
	rdb := r.Client()
	defer func() { _ = rdb.Close() }()
	_, err := hotredis.EnsureInstalled(t.Context(), rdb)
	must(t, err)
	return r.Addr
}

type httpResult struct {
	code int
	body string
	err  error
}

// post is an HTTP call that reports its outcome on a channel, for a goroutine (t.Fatal is not for those).
func post(url, token, body string, hdr ...string) <-chan httpResult {
	out := make(chan httpResult, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			out <- httpResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			out <- httpResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		out <- httpResult{code: resp.StatusCode, body: string(b)}
	}()
	return out
}

func shutdownRecordBody(scope, resource string) string {
	return `{"scope":` + scope + `,"resource":"` + resource + `","rate_key":"gpt","occurred_at":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `","quantities":[{"unit":"in","amount":1200},{"unit":"out","amount":800}]}`
}

func TestPaymentdDrainsWhatIsInFlightBeforeItStops(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	openScope(t, rd, realm, dsn, sc)
	d := startPaymentd(t, paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0", "PAYMENT_RECORD_TIMEOUT", "20s", "PAYMENT_SHUTDOWN_TIMEOUT", "20s"))
	httpAddr, grpcAddr := d.listening(t, "http"), d.listening(t, "grpc")
	base := "http://" + httpAddr
	awaitReady(t, d, &http.Client{Timeout: 10 * time.Second}, base)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	if code, body := httpDo(t, &http.Client{Timeout: 10 * time.Second}, "POST", base+"/v1/grant", serviceToken, `{"scope":`+scopeJSON+`,"amount":1000,"reason":"purchase"}`, "Idempotency-Key", "g-1"); code != 200 {
		t.Fatalf("grant = %d %s", code, body)
	}

	// Two Records in flight, one on each transport, both waiting on a Redis that will not write for 3 s.
	g, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcclient.WithBearer(serviceToken, grpcclient.OverInsecureTransport()))
	must(t, err)
	t.Cleanup(func() { _ = g.Close() })
	pauseRedisWrites(t, rd, 3*time.Second)
	viaREST := post(base+"/v1/record", serviceToken, shutdownRecordBody(scopeJSON, "llm.chat"), "Idempotency-Key", "r-rest")
	type grpcResult struct {
		rc  domain.Receipt
		err error
	}
	viaGRPC := make(chan grpcResult, 1)
	go func() {
		rc, err := grpcclient.New(g).Record(t.Context(), domain.Event{
			Scope: sc, Resource: "llm.chat", RateKey: "gpt", IdemKey: "r-grpc", OccurredAt: time.Now(),
			Quantities: []domain.Quantity{{Unit: "in", Amount: 600}, {Unit: "out", Amount: 400}},
		})
		viaGRPC <- grpcResult{rc, err}
	}()
	time.Sleep(500 * time.Millisecond) // both have reached the paused Redis
	select {
	case r := <-viaREST:
		t.Fatalf("the REST record finished before the pause ended: %+v", r)
	default:
	}

	signalled := time.Now()
	must(t, d.cmd.Process.Signal(syscall.SIGTERM))

	// It stops taking new calls at once — long before the old ones can finish.
	refused := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", httpAddr, 200*time.Millisecond)
		if err != nil {
			refused = true
			break
		}
		_ = c.Close()
	}
	if !refused {
		t.Error("the listener still accepted connections 2 s after SIGTERM: it waits for the calls in flight while taking new ones, so a busy service never finishes stopping")
	}
	// And what was in flight is answered, not dropped.
	r := <-viaREST
	if r.err == nil && time.Since(signalled) < 2*time.Second {
		t.Fatalf("the REST record was answered while Redis was still paused, so it was not in flight: %+v", r)
	}
	if r.err != nil || (r.code != 200 && r.code != 202) {
		t.Fatalf("the REST record in flight at SIGTERM: code %d, err %v, body %s — it was dropped, not settled or journalled", r.code, r.err, r.body)
	}
	var rest struct {
		Applied, Deferred bool
		Delta, Balance    int64
	}
	must(t, json.Unmarshal([]byte(r.body), &rest))
	gr := <-viaGRPC
	if gr.err != nil {
		t.Fatalf("the gRPC record in flight at SIGTERM: %v — it was dropped, not settled or journalled", gr.err)
	}
	if code := d.waitExit(t, 20*time.Second); code != 0 {
		t.Errorf("paymentd exited %d after draining; logs:\n%s", code, d.logs.String())
	}

	// Each was booked once: the grant less exactly what the receipts say was taken. A record that was journalled
	// instead of settled has taken nothing from the hot balance (the worker books it).
	want := domain.Credits(1000)
	if !rest.Deferred {
		want += domain.Credits(rest.Delta)
	}
	if !gr.rc.Deferred {
		want += gr.rc.Delta
	}
	if got := hotBalance(t, rd, realm, dsn, sc); got != want {
		t.Errorf("hot balance = %d, want %d (the grant less each record's cost, once each)", got, want)
	}
}

func TestPaymentdThatCannotDrainInTimeStopsAnywayAndARetryIsNotBookedTwice(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	openScope(t, rd, realm, dsn, sc)
	env := paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0", "PAYMENT_RECORD_TIMEOUT", "30s", "PAYMENT_SHUTDOWN_TIMEOUT", "1s")
	d := startPaymentd(t, env)
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	if code, body := httpDo(t, c, "POST", base+"/v1/grant", serviceToken, `{"scope":`+scopeJSON+`,"amount":1000,"reason":"purchase"}`, "Idempotency-Key", "g-1"); code != 200 {
		t.Fatalf("grant = %d %s", code, body)
	}

	body := shutdownRecordBody(scopeJSON, "llm.chat")
	pauseRedisWrites(t, rd, 5*time.Second)
	stuck := post(base+"/v1/record", serviceToken, body, "Idempotency-Key", "r-stuck")
	time.Sleep(500 * time.Millisecond)
	must(t, d.cmd.Process.Signal(syscall.SIGTERM))

	// A deploy cannot wait for ever: past the bound it stops, and says what it left behind.
	start := time.Now()
	code := d.waitExit(t, 15*time.Second)
	if code == 0 {
		t.Errorf("paymentd reported a clean stop though a record was still in flight; logs:\n%s", d.logs.String())
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("took %v to stop with a 1 s bound", took)
	}
	if out := d.logs.String(); !strings.Contains(out, "in flight") {
		t.Errorf("the log does not say the drain was cut short with calls in flight:\n%s", out)
	}
	<-stuck // the caller learns the call did not complete, by an error it can retry on

	// The retry, with the same key, after the pause: whether or not the first attempt reached Redis, it is
	// charged once.
	time.Sleep(5 * time.Second)
	d2 := startPaymentd(t, env)
	base2 := d2.httpURL(t)
	awaitReady(t, d2, c, base2)
	code2, retried := httpDo(t, c, "POST", base2+"/v1/record", serviceToken, body, "Idempotency-Key", "r-stuck")
	if code2 != 200 {
		t.Fatalf("the retry = %d %s", code2, retried)
	}
	var rc struct {
		Applied        bool
		Delta, Balance int64
	}
	must(t, json.Unmarshal([]byte(retried), &rc))
	if !rc.Applied || rc.Delta >= 0 {
		t.Fatalf("the retry's receipt does not show a charge: %s", retried)
	}
	if got, want := hotBalance(t, rd, realm, dsn, sc), domain.Credits(1000+rc.Delta); got != want {
		t.Errorf("hot balance = %d after the retry, want %d: the record was charged once, however many attempts reached Redis", got, want)
	}
}
