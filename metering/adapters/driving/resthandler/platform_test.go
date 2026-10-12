package resthandler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The platform's endpoints (FR-042): /healthz says the process is up and touches nothing; /readyz says it may
// be sent traffic, and is 503 while the schema is behind head or the shard count disagrees with the
// configuration, so a rollout cannot serve money against a half-migrated database or a re-sharded keyspace;
// /metrics exports the observability contract. None needs a credential and none carries CORS.

type stubReady struct {
	report ports.ReadyReport
	calls  atomic.Int64
	block  bool
}

func (r *stubReady) Check(ctx context.Context) ports.ReadyReport {
	r.calls.Add(1)
	if r.block {
		<-ctx.Done()
		return ports.ReadyReport{Migrations: "unknown", Problems: []string{"the check did not finish: " + ctx.Err().Error()}}
	}
	return r.report
}

func platform(t *testing.T, ready ports.Readiness, metrics http.Handler, timeout time.Duration) (http.Handler, *stubMeter) {
	t.Helper()
	stub := &stubMeter{}
	h, err := resthandler.New(resthandler.Config{
		Meter: stub, Balance: stub, Clock: contracts.NewFakeClock(epoch), Logger: quiet,
		Auth: auth.AuthenticatorFunc(func(context.Context, auth.Credential) (auth.Principal, error) {
			return auth.Principal{}, auth.ErrUnauthenticated
		}),
		Ready: ready, Metrics: metrics, ReadyTimeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, stub
}

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzSaysTheProcessIsUpAndTouchesNothing(t *testing.T) {
	t.Parallel()
	ready := &stubReady{report: ports.ReadyReport{Migrations: "unknown", Problems: []string{"postgres is down"}}}
	h, stub := platform(t, ready, nil, 0)
	rec := get(h, "/healthz")
	var body struct {
		Status string `json:"status"`
	}
	mustDecode(t, rec.Body.Bytes(), &body)
	if rec.Code != 200 || body.Status != "ok" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if ready.calls.Load() != 0 || stub.calls.Load() != 0 {
		t.Fatal("a liveness probe that calls a dependency turns that dependency's outage into a restart loop")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("a probe is never cached: %q", rec.Header().Get("Cache-Control"))
	}
}

func TestReadyzIsReadyOnlyAtHeadWithTheConfiguredShardCount(t *testing.T) {
	t.Parallel()
	type resp struct {
		Status     string   `json:"status"`
		Migrations string   `json:"migrations"`
		Shards     int      `json:"shards"`
		Problems   []string `json:"problems"`
	}
	rows := []struct {
		name   string
		report ports.ReadyReport
		code   int
		status string
		mig    string
		probs  string // a substring of the problems, when there are any
	}{
		{"at head, shards agree", ports.ReadyReport{Migrations: "head", Shards: 256}, 200, "ready", "head", ""},
		{"the schema is behind head", ports.ReadyReport{Migrations: "behind", Shards: 256, Problems: []string{"schema is 2 migrations behind head"}}, 503, "not_ready", "behind", "behind head"},
		{"the shard count disagrees with the configuration", ports.ReadyReport{Migrations: "head", Shards: 256, Problems: []string{"configured 128 shards, recorded 256"}}, 503, "not_ready", "head", "128"},
		{"the schema cannot be read at all", ports.ReadyReport{Migrations: "unknown", Problems: []string{"postgres: connection refused"}}, 503, "not_ready", "unknown", "connection refused"},
		{"head, but the hot tier is unreachable", ports.ReadyReport{Migrations: "head", Shards: 256, Problems: []string{"redis: unreachable"}}, 503, "not_ready", "head", "redis"},
		{"behind AND mismatched: both are said", ports.ReadyReport{Migrations: "behind", Shards: 256, Problems: []string{"behind head", "configured 128 shards, recorded 256"}}, 503, "not_ready", "behind", "128"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			h, _ := platform(t, &stubReady{report: row.report}, nil, 0)
			rec := get(h, "/readyz")
			var got resp
			mustDecode(t, rec.Body.Bytes(), &got)
			if rec.Code != row.code || got.Status != row.status || got.Migrations != row.mig || got.Shards != row.report.Shards {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if row.probs != "" && !strings.Contains(strings.Join(got.Problems, "; "), row.probs) {
				t.Fatalf("the problems say what is wrong: %v", got.Problems)
			}
			if row.code == 200 && len(got.Problems) != 0 {
				t.Fatalf("%v", got.Problems)
			}
			if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("%v", rec.Header())
			}
		})
	}
}

// A check that hangs is not ready. A probe must answer, or it cannot gate a rollout.
func TestAReadinessCheckThatHangsIsNotReady(t *testing.T) {
	t.Parallel()
	h, _ := platform(t, &stubReady{block: true}, nil, 50*time.Millisecond)
	start := time.Now()
	rec := get(h, "/readyz")
	if rec.Code != 503 || time.Since(start) > 3*time.Second {
		t.Fatalf("%d after %v", rec.Code, time.Since(start))
	}
}

func TestMetricsServesWhatItIsGiven(t *testing.T) {
	t.Parallel()
	exporter := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("# TYPE metering_admit_denied_total counter\n"))
	})
	h, _ := platform(t, &stubReady{}, exporter, 0)
	rec := get(h, "/metrics")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "metering_admit_denied_total") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestPlatformRoutesNeedNoCredentialAndCarryNoCORS(t *testing.T) {
	t.Parallel()
	exporter := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("x 1\n")) })
	h, _ := platform(t, &stubReady{report: ports.ReadyReport{Migrations: "head", Shards: 1}}, exporter, 0)
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := get(h, path, "Origin", "https://evil.example", "Authorization", "Bearer nope")
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		for k := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Fatalf("%s carries %s", path, k)
			}
		}
	}
}

func TestReadyzAndMetricsExistOnlyWithTheirSources(t *testing.T) {
	t.Parallel()
	h, _ := platform(t, nil, nil, 0)
	if rec := get(h, "/healthz"); rec.Code != 200 {
		t.Fatalf("liveness needs nothing: %d", rec.Code)
	}
	for _, path := range []string{"/readyz", "/metrics"} {
		if rec := get(h, path); rec.Code != 404 {
			t.Fatalf("%s is registered with nothing behind it: %d", path, rec.Code)
		}
	}
}
