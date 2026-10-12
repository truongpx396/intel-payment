//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The REST surface in front of the real engine — Redis, Postgres, the journal — for what only the engine can say.

// restStack puts the REST handler in front of a stack's engine, as a caller with record and grant on its realm.
func restStack(t *testing.T, s *stack) http.Handler {
	t.Helper()
	svc, ok := s.meter.(ports.Service)
	if !ok {
		t.Fatal("the engine is not a ports.Service")
	}
	h, err := resthandler.New(resthandler.Config{
		Meter: svc, Balance: svc, Clock: contracts.NewFakeClock(timeNow()),
		Auth: auth.AuthenticatorFunc(func(_ context.Context, c auth.Credential) (auth.Principal, error) {
			if c.Bearer == "t" {
				return auth.Principal{CallerID: "it", Realms: []domain.Realm{s.realm}, Privileges: auth.Record | auth.Grant}, nil
			}
			return auth.Principal{}, auth.ErrUnauthenticated
		}),
	})
	must(t, err)
	return h
}

type rest struct {
	t *testing.T
	h http.Handler
}

func (r rest) do(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%q: %v", rec.Body.String(), err)
	}
	return m
}

func recordBody(sc domain.Scope, occurred string, in int) string {
	return `{"scope":{"kind":"` + sc.Kind + `","id":"` + sc.ID + `"},"resource":"llm.chat","rate_key":"gpt","occurred_at":"` + occurred + `",` +
		`"quantities":[{"unit":"in","amount":` + itoa(in) + `},{"unit":"out","amount":800}]}`
}

// "A retry with the same key and the same request returns the original outcome; the same key with a
// different request is 409 idempotency_conflict" — over HTTP, against the books.
func TestTheIdempotencyKeyOnTheWireIsTheEnginesGuard(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpt{})
	api := rest{t, restStack(t, s)}
	sc := s.scope()
	s.open(sc)
	at := timeNow().Add(-1e9).UTC().Format("2006-01-02T15:04:05.000Z07:00")

	grant := api.do("POST", "/v1/grant", `{"scope":{"kind":"workspace","id":"`+sc.ID+`"},"amount":100000,"reason":"purchase"}`, "Idempotency-Key", "pay-1")
	if grant.Code != 200 || decode(t, grant)["applied"] != true {
		t.Fatalf("%d %s", grant.Code, grant.Body.String())
	}
	if again := api.do("POST", "/v1/grant", `{"scope":{"kind":"workspace","id":"`+sc.ID+`"},"amount":100000,"reason":"purchase"}`, "Idempotency-Key", "pay-1"); decode(t, again)["applied"] != false {
		t.Fatalf("a replayed grant mints nothing: %s", again.Body.String())
	}
	if s.hot(sc) != 100_000 {
		t.Fatalf("minted twice: %d", s.hot(sc))
	}

	first := api.do("POST", "/v1/record", recordBody(sc, at, 1200), "Idempotency-Key", "evt-1")
	m := decode(t, first)
	if first.Code != 200 || m["applied"] != true || m["delta"] != float64(-660) {
		t.Fatalf("%d %s", first.Code, first.Body.String())
	}
	replay := api.do("POST", "/v1/record", recordBody(sc, at, 1200), "Idempotency-Key", "evt-1")
	r := decode(t, replay)
	if replay.Code != 200 || r["applied"] != false || r["seq"] != m["seq"] || r["delta"] != float64(0) {
		t.Fatalf("the same key and request returns the original outcome, applying nothing: %s", replay.Body.String())
	}

	conflict := api.do("POST", "/v1/record", recordBody(sc, at, 1), "Idempotency-Key", "evt-1")
	if conflict.Code != 409 || decode(t, conflict)["error"].(map[string]any)["code"] != "idempotency_conflict" {
		t.Fatalf("the same key with a different request is a conflict, never a second effect: %d %s", conflict.Code, conflict.Body.String())
	}
	later := api.do("POST", "/v1/record", recordBody(sc, timeNow().UTC().Format("2006-01-02T15:04:05.000Z07:00"), 1200), "Idempotency-Key", "evt-1")
	if later.Code != 409 {
		t.Fatalf("occurred_at must be identical on every retry: a different one is a different request: %d %s", later.Code, later.Body.String())
	}
	if got := s.hot(sc); got != 100_000-660 {
		t.Fatalf("charged once: %d", got)
	}
}

func TestTheSameRequestDeliveredConcurrentlyOverHTTPHasOneEffect(t *testing.T) {
	t.Parallel()
	s := newStack(t, stackOpt{})
	api := rest{t, restStack(t, s)}
	sc := s.scope()
	s.open(sc)
	_, err := s.meter.Grant(context.Background(), domain.Grant{Scope: sc, Amount: 100_000, IdemKey: "seed", Reason: "purchase"})
	must(t, err)
	body := recordBody(sc, timeNow().Add(-1e9).UTC().Format("2006-01-02T15:04:05.000Z07:00"), 1200)

	const n = 16
	var applied atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := api.do("POST", "/v1/record", body, "Idempotency-Key", "race")
			if rec.Code != 200 {
				t.Errorf("a concurrent delivery of one key is a replay, never an error: %d %s", rec.Code, rec.Body.String())
				return
			}
			if decode(t, rec)["applied"] == true {
				applied.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if applied.Load() != 1 || s.hot(sc) != 100_000-660 {
		t.Fatalf("exactly-once over HTTP: %d applied, balance %d", applied.Load(), s.hot(sc))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func timeNow() time.Time { return time.Now() }
