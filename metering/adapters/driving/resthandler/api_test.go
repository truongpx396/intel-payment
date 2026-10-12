package resthandler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// stubMeter is a programmable Meter that counts every call: a refusal at the edge must be provable as
// "the engine was never reached".
type stubMeter struct {
	calls atomic.Int64

	admit    func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error)
	record   func(context.Context, domain.Event) (domain.Receipt, error)
	grant    func(context.Context, domain.Grant) (domain.Receipt, error)
	transfer func(context.Context, domain.Transfer) (domain.TransferReceipt, error)
	balance  func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error)

	mu       sync.Mutex
	scope    domain.Scope
	admitReq domain.AdmitRequest
	event    domain.Event
	gotGrant domain.Grant
	gotXfer  domain.Transfer
}

var (
	_ ports.Meter         = (*stubMeter)(nil)
	_ ports.BalanceReader = (*stubMeter)(nil)
)

func (m *stubMeter) Admit(ctx context.Context, s domain.Scope, r domain.AdmitRequest) (domain.Admission, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.scope, m.admitReq = s, r
	m.mu.Unlock()
	if m.admit == nil {
		return domain.Admission{Allowed: true, Headroom: 7}, nil
	}
	return m.admit(ctx, s, r)
}

func (m *stubMeter) Record(ctx context.Context, e domain.Event) (domain.Receipt, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.event = e
	m.mu.Unlock()
	if m.record == nil {
		return domain.Receipt{IdemKey: e.IdemKey, Applied: true, Delta: -660, Seq: 1}, nil
	}
	return m.record(ctx, e)
}

func (m *stubMeter) Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.gotGrant = g
	m.mu.Unlock()
	if m.grant == nil {
		return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Delta: g.Amount, Seq: 1}, nil
	}
	return m.grant(ctx, g)
}

func (m *stubMeter) Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.gotXfer = t
	m.mu.Unlock()
	if m.transfer == nil {
		return domain.TransferReceipt{IdemKey: t.IdemKey, Applied: true, Amount: t.Amount, Status: domain.TransferInTransit}, nil
	}
	return m.transfer(ctx, t)
}

func (m *stubMeter) Balance(ctx context.Context, s domain.Scope) (map[domain.Pool]domain.Credits, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.scope = s
	m.mu.Unlock()
	if m.balance == nil {
		return map[domain.Pool]domain.Credits{domain.GeneralPool: 5}, nil
	}
	return m.balance(ctx, s)
}

var epoch = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// credentials the tests use.
var tokens = map[string]auth.Principal{
	"all":    {CallerID: "host-a", Realms: []domain.Realm{"acme"}, Privileges: auth.Record | auth.Grant},
	"record": {CallerID: "spender", Realms: []domain.Realm{"acme"}, Privileges: auth.Record},
	"grant":  {CallerID: "minter", Realms: []domain.Realm{"acme"}, Privileges: auth.Grant},
	"two":    {CallerID: "host-b", Realms: []domain.Realm{"acme", "globex"}, Privileges: auth.Record | auth.Grant},
}

type apiOpts struct {
	ledger    ports.LedgerReader
	credits   ports.CreditsReader
	transfers ports.TransferReader
	legacy429 bool
	upgrade   func(context.Context, domain.Scope) string
	maxBody   int64
	logs      io.Writer
}

type api struct {
	h    http.Handler
	stub *stubMeter
	logs *bytes.Buffer
	mu   sync.Mutex
}

func newAPI(t *testing.T, stub *stubMeter, o apiOpts) *api {
	t.Helper()
	a := &api{stub: stub, logs: &bytes.Buffer{}}
	sink := o.logs
	if sink == nil {
		sink = &syncWriter{a: a}
	}
	h, err := resthandler.New(resthandler.Config{
		Meter: stub, Balance: stub, Clock: contracts.NewFakeClock(epoch), Logger: slog.New(slog.NewTextHandler(sink, nil)),
		Auth: auth.AuthenticatorFunc(func(_ context.Context, c auth.Credential) (auth.Principal, error) {
			if p, ok := tokens[c.Bearer]; ok {
				return p, nil
			}
			return auth.Principal{}, auth.ErrUnauthenticated
		}),
		Ledger: o.ledger, Transfers: o.transfers, Credits: o.credits,
		Legacy429ForLimits: o.legacy429, UpgradeURL: o.upgrade, MaxBodyBytes: o.maxBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.h = h
	return a
}

type syncWriter struct{ a *api }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.a.mu.Lock()
	defer w.a.mu.Unlock()
	return w.a.logs.Write(p)
}

func (a *api) logged() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logs.String()
}

// call makes a request as the bearer token.
func (a *api) call(token, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// The bodies and routes of the five metering calls, valid for the engine.
const (
	scopeJSON = `{"kind":"workspace","id":"w1"}`
	scope2    = `{"kind":"workspace","id":"w2"}`
)

type route struct {
	name, method, path, body string
	mutating                 bool
}

var (
	admitRoute    = route{"admit", "POST", "/v1/admit", `{"scope":` + scopeJSON + `,"resource":"llm.chat","max_cost":10}`, false}
	recordRoute   = route{"record", "POST", "/v1/record", `{"scope":` + scopeJSON + `,"resource":"llm.chat","rate_key":"gpt","occurred_at":"2026-09-29T12:00:00Z","quantities":[{"unit":"in","amount":1200},{"unit":"out","amount":800}]}`, true}
	grantRoute    = route{"grant", "POST", "/v1/grant", `{"scope":` + scopeJSON + `,"amount":100,"reason":"purchase"}`, true}
	transferRoute = route{"transfer", "POST", "/v1/transfer", `{"from":` + scopeJSON + `,"to":` + scope2 + `,"amount":5,"reason":"allocation"}`, true}
	balanceRoute  = route{"balance", "GET", "/v1/balance?scope_kind=workspace&scope_id=w1", "", false}

	allRoutes = []route{admitRoute, recordRoute, grantRoute, transferRoute, balanceRoute}
)

// do makes the route's call as token, with an Idempotency-Key where the route has an operation behind it.
func (a *api) do(token string, r route) *httptest.ResponseRecorder {
	if r.mutating {
		return a.call(token, r.method, r.path, r.body, "Idempotency-Key", "k-1")
	}
	return a.call(token, r.method, r.path, r.body)
}

type errEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
	Allowed  *bool          `json:"allowed"`
	Exceeded map[string]any `json:"exceeded"`
	Headroom *int64         `json:"headroom"`
	Blocked  *bool          `json:"blocked"`
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errEnvelope {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("an error is JSON: Content-Type = %q", ct)
	}
	var e errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not JSON: %q (%v)", rec.Body.String(), err)
	}
	if e.Error.Code == "" || e.Error.Message == "" {
		t.Fatalf("an error has a code and a message: %q", rec.Body.String())
	}
	return e
}

func mustTime(s string) time.Time {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return v
}

func mustDecode(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("not the JSON we expected: %q (%v)", raw, err)
	}
}

// newRequest builds a route's request carrying ctx, as token.
func newRequest(ctx context.Context, r route, token string) *http.Request {
	req := httptest.NewRequestWithContext(ctx, r.method, r.path, strings.NewReader(r.body))
	req.Header.Set("Authorization", "Bearer "+token)
	if r.mutating {
		req.Header.Set("Idempotency-Key", "k-1")
	}
	return req
}

func serve(a *api, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// await receives from ch or fails the test: a handler that never reaches the engine must fail a test, not hang it.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
