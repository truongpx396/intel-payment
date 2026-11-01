package paymenthttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The stubs embed the port they stand for, so a method a test did not give a behaviour panics: a handler that
// reaches for something the test did not expect fails loudly instead of quietly answering.

type catalogStub struct {
	ports.Catalog
	list func(ctx context.Context, realm domain.Realm, currency string) ([]domain.Plan, error)
}

func (c catalogStub) List(ctx context.Context, realm domain.Realm, currency string) ([]domain.Plan, error) {
	return c.list(ctx, realm, currency)
}

type checkoutStub struct {
	ports.Checkout
	start      func(ctx context.Context, req ports.CheckoutRequest) (string, error)
	portal     func(ctx context.Context, scope domain.Scope, account, returnURL string) (string, error)
	fulfilment func(ctx context.Context, scope domain.Scope) (ports.Fulfilment, error)
}

func (c checkoutStub) Start(ctx context.Context, req ports.CheckoutRequest) (string, error) {
	return c.start(ctx, req)
}

func (c checkoutStub) Portal(ctx context.Context, s domain.Scope, account, returnURL string) (string, error) {
	return c.portal(ctx, s, account, returnURL)
}

func (c checkoutStub) Fulfilment(ctx context.Context, s domain.Scope) (ports.Fulfilment, error) {
	return c.fulfilment(ctx, s)
}

type subsStub struct {
	ports.Subs
	get func(ctx context.Context, s domain.Scope) (domain.Sub, bool, error)
}

func (s subsStub) Get(ctx context.Context, sc domain.Scope) (domain.Sub, bool, error) {
	return s.get(ctx, sc)
}

type changesStub struct {
	ports.Subscriptions
	change func(ctx context.Context, s domain.Scope, planCode string) error
	cancel func(ctx context.Context, s domain.Scope) error
}

func (c changesStub) Change(ctx context.Context, s domain.Scope, planCode string) error {
	return c.change(ctx, s, planCode)
}

func (c changesStub) CancelAtPeriodEnd(ctx context.Context, s domain.Scope) error {
	return c.cancel(ctx, s)
}

type rechargesStub struct {
	ports.AutoRecharges
	get func(ctx context.Context, s domain.Scope) (domain.AutoRecharge, bool, error)
	put func(ctx context.Context, a domain.AutoRecharge) error
}

func (r rechargesStub) Get(ctx context.Context, s domain.Scope) (domain.AutoRecharge, bool, error) {
	return r.get(ctx, s)
}

func (r rechargesStub) Put(ctx context.Context, a domain.AutoRecharge) error { return r.put(ctx, a) }

type paymentsStub struct {
	ports.Payments
	list func(ctx context.Context, s domain.Scope, cursor string, limit int) ([]domain.Payment, string, error)
}

func (p paymentsStub) List(ctx context.Context, s domain.Scope, cursor string, limit int) ([]domain.Payment, string, error) {
	return p.list(ctx, s, cursor, limit)
}

// caller is who the registry authenticated, as the test header X-Realms says it: the realms the credential covers,
// joined with '+'. It resolves a scope's realm the way the real credential does — the credential's own when it has one,
// a refusal for a realm it does not cover, and a refusal to guess when it spans several.
type caller struct{}

func (caller) Resolve(r *http.Request, s domain.Scope) (domain.Scope, error) {
	realms := strings.Split(r.Header.Get("X-Realms"), "+")
	if s.Realm == "" {
		if len(realms) != 1 {
			return domain.Scope{}, md.ErrInvalid
		}
		s.Realm = domain.Realm(realms[0])
		return s, nil
	}
	for _, have := range realms {
		if domain.Realm(have) == s.Realm {
			return s, nil
		}
	}
	return domain.Scope{}, paymenthttp.ErrRealmMismatch
}

func (caller) Via(*http.Request) string { return "test-credential" }

// door is a handler over the routes of a Config, as the REST handler's registry would mount them (minus the
// credential, which the registry checks).
type door struct {
	http.Handler
	routes []paymenthttp.Route
}

func newDoor(t *testing.T, cfg paymenthttp.Config) *door {
	t.Helper()
	if cfg.Caller == nil {
		cfg.Caller = caller{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	d, err := paymenthttp.New(cfg)
	if err != nil {
		t.Fatalf("paymenthttp.New: %v", err)
	}
	mux := http.NewServeMux()
	for _, r := range d.Routes() {
		mux.HandleFunc(r.Method+" "+r.Path, r.Handler)
	}
	return &door{Handler: mux, routes: d.Routes()}
}

// reply is one response, with its body read.
type reply struct {
	Status int
	Body   []byte
	Header http.Header
}

// JSON decodes the body into v.
func (r reply) JSON(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("the body is not the JSON expected: %v\n%s", err, r.Body)
	}
}

// ErrorCode is the code of the error envelope.
func (r reply) ErrorCode(t *testing.T) string {
	t.Helper()
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	r.JSON(t, &e)
	return e.Error.Code
}

// call sends a request as a credential covering realm "acme". headers are alternating names and values.
func (d *door) call(t *testing.T, method, path, body string, headers ...string) reply {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("X-Realms", "acme")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return reply{Status: rec.Code, Body: rec.Body.Bytes(), Header: rec.Result().Header}
}

var errBoom = errors.New("dial tcp 10.0.0.7:5432: connection refused") // an error whose text names the topology

// httpHeaders builds a header from alternating names and values, keeping a name given twice twice.
func httpHeaders(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// serve sends a prepared request through the door.
func serve(d *door, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}
