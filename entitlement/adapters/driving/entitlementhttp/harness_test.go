package entitlementhttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementhttp"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// credential is a Caller standing for what the binary's glue does with the REST handler's authenticated principal: no credential at
// all, a credential without the privilege, or one bound to a set of realms.
type credential struct {
	none, noRecord bool
	realms         []md.Realm
}

func (c credential) Resolve(_ context.Context, s md.Scope) (md.Scope, error) {
	switch {
	case c.none:
		return md.Scope{}, entitlementwire.ErrUnauthenticated
	case c.noRecord:
		return md.Scope{}, entitlementwire.ErrForbidden
	case s.Realm == "" && len(c.realms) == 1:
		s.Realm = c.realms[0]
		return s, nil
	case s.Realm == "":
		return md.Scope{}, domain.ErrInvalid
	}
	for _, r := range c.realms {
		if r == s.Realm {
			return s, nil
		}
	}
	return md.Scope{}, entitlementwire.ErrRealmMismatch
}

var inT1 = credential{realms: []md.Realm{"t1"}}

// logSink collects what the door logs for the operator.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *logSink) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

type rig struct {
	h      http.Handler
	mem    *contracts.MemStore
	faults *contracts.Breakable
	log    *logSink
	door   *entitlementhttp.Door
	e      *app.Entitler
}

// brokenChecker and brokenEntitler fail with whatever they are told, to show what the door says about an error it does not know.
type brokenChecker struct{ err error }

func (b brokenChecker) Check(context.Context, md.Scope, []domain.CheckRequest) ([]domain.CheckResult, error) {
	return nil, b.err
}

// newRig serves the real Entitler over the demo world through the door, as the binary would.
func newRig(t *testing.T, cred entitlementwire.Caller, tweak ...func(*entitlementhttp.Config)) *rig {
	t.Helper()
	mem := contracts.NewMemStore()
	contracts.SeedDemo(t, mem)
	faults := contracts.NewBreakable(mem)
	e, err := app.New(app.Deps{Store: faults, Clock: contracts.NewFakeClock(contracts.T0)})
	if err != nil {
		t.Fatal(err)
	}
	sink := &logSink{}
	cfg := entitlementhttp.Config{Entitler: e, Checker: e, Caller: cred, Logger: slog.New(slog.NewTextHandler(sink, nil)), MaxBody: 4096}
	for _, f := range tweak {
		f(&cfg)
	}
	d, err := entitlementhttp.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, r := range d.Routes() {
		mux.HandleFunc(r.Method+" "+r.Path, r.Handler)
	}
	return &rig{h: mux, mem: mem, faults: faults, log: sink, door: d, e: e}
}

type response struct {
	Code int
	Head http.Header
	Body string
}

func (r *rig) do(t *testing.T, method, target, body string) response {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return response{rec.Code, rec.Header(), rec.Body.String()}
}

func (r *rig) check(t *testing.T, body string) response {
	t.Helper()
	return r.do(t, http.MethodPost, "/v1/entitlements/check", body)
}

type decisions struct {
	Data []entitlementhttp.DecisionJSON `json:"data"`
}

func (r response) decisions(t *testing.T) []entitlementhttp.DecisionJSON {
	t.Helper()
	var d decisions
	if err := json.Unmarshal([]byte(r.Body), &d); err != nil {
		t.Fatalf("the body is not decisions: %v\n%s", err, r.Body)
	}
	return d.Data
}

func (r response) errorCode(t *testing.T) (code, message string) {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal([]byte(r.Body), &e); err != nil {
		t.Fatalf("the body is not an error: %v\n%s", err, r.Body)
	}
	return e.Error.Code, e.Error.Message
}

const scopeJSON = `"scope":{"kind":"org","id":"o1"}`

func checkBody(checks string) string { return `{` + scopeJSON + `,"checks":[` + checks + `]}` }

var errBoom = errors.New("dial tcp 10.1.2.3:5432: connection refused")

var _ ports.Checker = brokenChecker{}

var scopeDemo = contracts.DemoScope

func mustJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
}
