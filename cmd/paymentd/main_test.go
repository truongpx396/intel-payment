package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"), goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop")); err != nil {
			os.Stderr.WriteString("goleak: " + err.Error() + "\n")
			code = 1
		}
	}
	os.Exit(code)
}

func runWith(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	code = run(ctx, args, getenv(env), &out, &errb)
	return code, out.String(), errb.String()
}

// R067: a bad configuration stops the process before it dials anything — a service that came up half-configured
// would serve money on defaults nobody chose. Exit 2 is "configuration or usage"; 1 is "ran and failed".
func TestABadConfigurationStopsTheProcessBeforeItDialsAnything(t *testing.T) {
	t.Parallel()
	// Both stores point at a closed port: if the process dialled them first, this would fail on a dial error
	// (or hang), not on the setting.
	env := with("PAYMENT_BALANCE_REDIS_URL", "redis://127.0.0.1:1/0", "PAYMENT_LEDGER_DSN", "postgres://x:y@127.0.0.1:1/z", "PAYMENT_SETTLEMENT", "eventually")
	start := time.Now()
	code, _, stderr := runWith(t, env)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (configuration); stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "PAYMENT_SETTLEMENT") {
		t.Errorf("stderr does not name the variable:\n%s", stderr)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %v: the process dialled something before it checked its settings", d)
	}
}

func TestAnUnknownCommandIsUsageNotAStart(t *testing.T) {
	t.Parallel()
	code, _, stderr := runWith(t, base(), "migrate")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr does not say how to use it:\n%s", stderr)
	}
}

// The compose healthcheck: the image is distroless, so there is no curl; the binary probes itself. It asks
// /readyz — whether the process may be sent traffic, which is what an orchestrator gates a rollout on.
func TestHealthcheckIsZeroOnlyWhileTheProcessIsReady(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	env := map[string]string{"PAYMENT_HTTP_ADDR": addr}

	if code, _, stderr := runWith(t, env, "healthcheck"); code != 0 {
		t.Errorf("ready: exit code = %d, want 0; stderr: %s", code, stderr)
	}
	status.Store(http.StatusServiceUnavailable)
	if code, _, _ := runWith(t, env, "healthcheck"); code != 1 {
		t.Errorf("not ready: exit code = %d, want 1", code)
	}
	srv.Close()
	if code, _, _ := runWith(t, env, "healthcheck"); code != 1 {
		t.Errorf("nothing listening: exit code = %d, want 1", code)
	}
}

// The configured address is how the process LISTENS (":8080" is every interface); the probe dials loopback.
func TestHealthcheckProbesLoopbackWhenTheListenerIsAnyInterface(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	if code, _, stderr := runWith(t, map[string]string{"PAYMENT_HTTP_ADDR": ":" + port}, "healthcheck"); code != 0 {
		t.Errorf(`":%s" (every interface): exit code = %d, want 0; stderr: %s`, port, code, stderr)
	}
	if code, _, stderr := runWith(t, map[string]string{"PAYMENT_HTTP_ADDR": "0.0.0.0:" + port}, "healthcheck"); code != 0 {
		t.Errorf(`"0.0.0.0:%s": exit code = %d, want 0; stderr: %s`, port, code, stderr)
	}
}

// A probe must answer: a process that accepts the connection and never replies is not healthy.
func TestHealthcheckGivesUpOnAHungProcess(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	start := time.Now()
	code, _, _ := runWith(t, map[string]string{"PAYMENT_HTTP_ADDR": strings.TrimPrefix(srv.URL, "http://")}, "healthcheck")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("took %v to give up", d)
	}
}
