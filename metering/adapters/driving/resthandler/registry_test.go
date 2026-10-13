package resthandler_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// rest-api.md: three surfaces, three authentication models, and the split matters more than the routes.
// /v1/* is a host's BACKEND behind a service credential; /webhooks/* is signature only, no JWT, no CORS;
// /healthz, /readyz and /metrics are the platform's. The registry is where that is decided, once, and
// where a check added to the wrong surface is refused.

// A test authenticator: bearer tokens and one client-certificate subject.
var principals = map[string]auth.Principal{
	"record-token": {CallerID: "host-a", Realms: []domain.Realm{"acme"}, Privileges: auth.Record},
	"grant-token":  {CallerID: "minter", Realms: []domain.Realm{"acme"}, Privileges: auth.Grant},
	"admin-token":  {CallerID: "ops", Realms: []domain.Realm{"acme"}, Privileges: auth.Admin},
}

// quiet swallows the log lines of failures the tests cause on purpose.
var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

var authenticator = auth.AuthenticatorFunc(func(_ context.Context, c auth.Credential) (auth.Principal, error) {
	if p, ok := principals[c.Bearer]; ok && c.Bearer != "" {
		return p, nil
	}
	if len(c.PeerCertificates) > 0 && c.PeerCertificates[0].Subject.CommonName == "host-cert" {
		return auth.Principal{CallerID: "host-cert", Realms: []domain.Realm{"acme"}, Privileges: auth.Record}, nil
	}
	if c.Bearer == "backend-down" {
		return auth.Principal{}, errors.New("credential store: connection refused")
	}
	return auth.Principal{}, auth.ErrUnauthenticated
})

// who records the caller a handler saw.
type seen struct {
	reached atomic.Int64
	caller  atomic.Value // auth.Principal
	hadAny  atomic.Bool
}

func (s *seen) handler(w http.ResponseWriter, r *http.Request) {
	s.reached.Add(1)
	if p, ok := auth.FromContext(r.Context()); ok {
		s.caller.Store(p)
		s.hadAny.Store(true)
	}
	_, _ = w.Write([]byte(`{"ok":true,"id":"` + r.PathValue("id") + `"}`))
}

func build(t *testing.T, routes ...resthandler.Route) http.Handler {
	t.Helper()
	reg := resthandler.NewRegistry()
	for _, r := range routes {
		reg.Add(r)
	}
	h, err := reg.Handler(resthandler.Options{Auth: authenticator, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not the one error shape: %q (%v)", rec.Body.String(), err)
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("an error has a code and a message: %q", rec.Body.String())
	}
	return body.Error.Code
}

func noCORS(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Fatalf("a CORS header (%s) on a route that must carry none", k)
		}
	}
}

func TestV1RoutesAreBehindCallerAuth(t *testing.T) {
	t.Parallel()
	s := &seen{}
	h := build(t,
		resthandler.Route{Method: "POST", Path: "/v1/spend", Surface: resthandler.V1, Privilege: auth.Record, Handler: s.handler},
		resthandler.Route{Method: "POST", Path: "/v1/mint", Surface: resthandler.V1, Privilege: auth.Grant, Handler: s.handler},
		resthandler.Route{Method: "GET", Path: "/v1/things/{id}", Surface: resthandler.V1, Privilege: auth.Record, Handler: s.handler},
	)
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "host-cert"}}

	rows := []struct {
		name    string
		method  string
		path    string
		hdr     map[string]string
		tls     *tls.ConnectionState
		status  int
		code    string
		reached int64
	}{
		{"no credential", "POST", "/v1/spend", nil, nil, 401, "unauthenticated", 0},
		{"a wrong token is refused, not treated as anonymous", "POST", "/v1/spend", map[string]string{"Authorization": "Bearer nope"}, nil, 401, "unauthenticated", 0},
		{"another scheme is not a bearer", "POST", "/v1/spend", map[string]string{"Authorization": "Basic cmVjb3JkLXRva2Vu"}, nil, 401, "unauthenticated", 0},
		{"a bearer with no token", "POST", "/v1/spend", map[string]string{"Authorization": "Bearer "}, nil, 401, "unauthenticated", 0},
		{"a credential store that is down is not an open door", "POST", "/v1/spend", map[string]string{"Authorization": "Bearer backend-down"}, nil, 500, "internal", 0},
		{"the right privilege", "POST", "/v1/spend", map[string]string{"Authorization": "Bearer record-token"}, nil, 200, "", 1},
		{"the scheme is case-insensitive", "POST", "/v1/spend", map[string]string{"Authorization": "bearer record-token"}, nil, 200, "", 1},
		{"a client certificate is a credential", "POST", "/v1/spend", nil, &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}, 200, "", 1},
		{"record cannot mint", "POST", "/v1/mint", map[string]string{"Authorization": "Bearer record-token"}, nil, 403, "forbidden", 0},
		{"grant cannot spend", "POST", "/v1/spend", map[string]string{"Authorization": "Bearer grant-token"}, nil, 403, "forbidden", 0},
		{"admin is neither", "POST", "/v1/mint", map[string]string{"Authorization": "Bearer admin-token"}, nil, 403, "forbidden", 0},
		{"grant can mint", "POST", "/v1/mint", map[string]string{"Authorization": "Bearer grant-token"}, nil, 200, "", 1},
		{"a path parameter reaches the handler", "GET", "/v1/things/abc", map[string]string{"Authorization": "Bearer record-token"}, nil, 200, "", 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := &seen{}
			h := build(t,
				resthandler.Route{Method: "POST", Path: "/v1/spend", Surface: resthandler.V1, Privilege: auth.Record, Handler: s.handler},
				resthandler.Route{Method: "POST", Path: "/v1/mint", Surface: resthandler.V1, Privilege: auth.Grant, Handler: s.handler},
				resthandler.Route{Method: "GET", Path: "/v1/things/{id}", Surface: resthandler.V1, Privilege: auth.Record, Handler: s.handler},
			)
			req := httptest.NewRequest(row.method, row.path, strings.NewReader(`{}`))
			for k, v := range row.hdr {
				req.Header.Set(k, v)
			}
			req.TLS = row.tls
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != row.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, row.status, rec.Body.String())
			}
			if row.code != "" {
				if got := errorCode(t, rec); got != row.code {
					t.Fatalf("code = %q, want %q", got, row.code)
				}
			}
			if row.status == 401 && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("a 401 says how to authenticate")
			}
			if s.reached.Load() != row.reached {
				t.Fatalf("handler reached %d times, want %d", s.reached.Load(), row.reached)
			}
			noCORS(t, rec)
		})
	}

	t.Run("the handler sees who is calling", func(t *testing.T) {
		t.Parallel()
		rec := do(h, "GET", "/v1/things/xyz", map[string]string{"Authorization": "Bearer record-token"})
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"xyz"`) {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if p, _ := s.caller.Load().(auth.Principal); p.CallerID != "host-a" {
			t.Fatalf("the handler saw %+v", p)
		}
	})
}

// The webhook invariant (11): a provider speaks signatures, not bearer tokens, and a browser never calls
// it. A JWT check there rejects the provider; a CORS policy there is meaningless and invites a browser.
func TestWebhookRoutesCarryNoAuthAndNoCORS(t *testing.T) {
	t.Parallel()
	for name, hdr := range map[string]map[string]string{
		"with no credential":                  nil,
		"with a bearer that would be refused": {"Authorization": "Bearer nope"},
		"with a bearer that would be valid":   {"Authorization": "Bearer record-token"},
		"from a browser origin":               {"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"},
		"with a provider's signature":         {"Stripe-Signature": "t=1,v1=abc"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &seen{}
			h := build(t, resthandler.Route{Method: "POST", Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: s.handler})
			rec := do(h, "POST", "/webhooks/stripe/acct-1", hdr)
			if rec.Code != 200 {
				t.Fatalf("a webhook delivery is reached whatever it carries: %d %s", rec.Code, rec.Body.String())
			}
			noCORS(t, rec)
			if s.reached.Load() != 1 {
				t.Fatalf("reached %d times", s.reached.Load())
			}
		})
	}
	t.Run("and the handler is given no caller: authentication is not attempted", func(t *testing.T) {
		t.Parallel()
		sw := &seen{}
		h := build(t, resthandler.Route{Method: "POST", Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: sw.handler})
		do(h, "POST", "/webhooks/stripe/acct-1", map[string]string{"Authorization": "Bearer record-token"})
		if sw.hadAny.Load() {
			t.Fatal("a webhook handler was handed a caller identity it never asked for")
		}
	})
}

func TestAPlantedAuthOrCORSCheckOnAWebhookIsRejected(t *testing.T) {
	t.Parallel()
	noop := func(http.ResponseWriter, *http.Request) {}
	planted := map[string]resthandler.Route{
		"a JWT check": {Method: "POST", Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: noop,
			Middleware: []resthandler.Middleware{resthandler.RequireAuth(authenticator)}},
		"a CORS policy": {Method: "POST", Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: noop,
			Middleware: []resthandler.Middleware{resthandler.CORS("*")}},
	}
	for name, route := range planted {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := resthandler.NewRegistry()
			reg.Add(route)
			err := reg.Validate()
			if err == nil || !strings.Contains(err.Error(), "/webhooks/{provider}/{account}") {
				t.Fatalf("the planted check must be refused, naming the route: %v", err)
			}
			if _, herr := reg.Handler(resthandler.Options{Auth: authenticator}); herr == nil {
				t.Fatal("a registry that fails validation must not build a handler")
			}
		})
	}
}

func TestEveryRouteIsOnTheSurfaceItsPathBelongsTo(t *testing.T) {
	t.Parallel()
	noop := func(http.ResponseWriter, *http.Request) {}
	bad := map[string]resthandler.Route{
		"a /v1 path declared a webhook":       {Method: "POST", Path: "/v1/admit", Surface: resthandler.Webhook, Handler: noop},
		"a /v1 path declared platform":        {Method: "GET", Path: "/v1/health", Surface: resthandler.Platform, Handler: noop},
		"a webhook path declared /v1":         {Method: "POST", Path: "/webhooks/x/y", Surface: resthandler.V1, Privilege: auth.Record, Handler: noop},
		"a platform path declared /v1":        {Method: "GET", Path: "/healthz", Surface: resthandler.V1, Privilege: auth.Record, Handler: noop},
		"a /v1 route that names no privilege": {Method: "POST", Path: "/v1/anything", Surface: resthandler.V1, Handler: noop},
		"a path on no surface":                {Method: "GET", Path: "/debug/pprof", Surface: resthandler.Platform, Handler: noop},
		"a route with no handler":             {Method: "GET", Path: "/v1/x", Surface: resthandler.V1, Privilege: auth.Record},
		"a route with no method":              {Path: "/v1/x", Surface: resthandler.V1, Privilege: auth.Record, Handler: noop},
	}
	for name, route := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := resthandler.NewRegistry()
			reg.Add(route)
			if err := reg.Validate(); err == nil {
				t.Fatalf("%+v was accepted", route)
			}
		})
	}
	t.Run("the same method and path twice", func(t *testing.T) {
		t.Parallel()
		reg := resthandler.NewRegistry()
		r := resthandler.Route{Method: "POST", Path: "/v1/admit", Surface: resthandler.V1, Privilege: auth.Record, Handler: noop}
		reg.Add(r)
		reg.Add(r)
		if err := reg.Validate(); err == nil {
			t.Fatal("two handlers for one route: which one answers is a coin toss")
		}
	})
	t.Run("a well-formed table passes", func(t *testing.T) {
		t.Parallel()
		reg := resthandler.NewRegistry()
		reg.Add(resthandler.Route{Method: "POST", Path: "/v1/admit", Surface: resthandler.V1, Privilege: auth.Record, Handler: noop})
		reg.Add(resthandler.Route{Method: "POST", Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: noop})
		reg.Add(resthandler.Route{Method: "GET", Path: "/healthz", Surface: resthandler.Platform, Handler: noop})
		reg.Add(resthandler.Route{Method: "GET", Path: "/readyz", Surface: resthandler.Platform, Handler: noop})
		reg.Add(resthandler.Route{Method: "GET", Path: "/metrics", Surface: resthandler.Platform, Handler: noop})
		if err := reg.Validate(); err != nil {
			t.Fatal(err)
		}
		if n := len(reg.Routes()); n != 5 {
			t.Fatalf("Routes() lists what was added: %d", n)
		}
	})
}

func TestPlatformRoutesAreOpenAndCarryNoCORS(t *testing.T) {
	t.Parallel()
	s := &seen{}
	h := build(t, resthandler.Route{Method: "GET", Path: "/healthz", Surface: resthandler.Platform, Handler: s.handler})
	rec := do(h, "GET", "/healthz", map[string]string{"Origin": "https://evil.example"})
	if rec.Code != 200 || s.reached.Load() != 1 {
		t.Fatalf("%d", rec.Code)
	}
	noCORS(t, rec)
}

func TestUnknownRoutesAndMethodsAnswerInTheOneErrorShape(t *testing.T) {
	t.Parallel()
	s := &seen{}
	h := build(t,
		resthandler.Route{Method: "POST", Path: "/v1/spend", Surface: resthandler.V1, Privilege: auth.Record, Handler: s.handler},
		resthandler.Route{Method: "GET", Path: "/v1/spend", Surface: resthandler.V1, Privilege: auth.Record, Handler: s.handler},
	)
	t.Run("an unknown path is 404, with or without a credential: whether a route exists is not for strangers to probe", func(t *testing.T) {
		t.Parallel()
		for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer record-token"}} {
			rec := do(h, "GET", "/v1/nope", hdr)
			if rec.Code != 404 || errorCode(t, rec) != "not_found" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("a wrong method is 405 with Allow, and no credential is needed to be told", func(t *testing.T) {
		t.Parallel()
		rec := do(h, "DELETE", "/v1/spend", nil)
		if rec.Code != 405 || errorCode(t, rec) != "method_not_allowed" {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
			t.Fatalf("Allow = %q", allow)
		}
	})
	t.Run("a CORS preflight is not answered: a browser must not be able to call /v1", func(t *testing.T) {
		t.Parallel()
		rec := do(h, "OPTIONS", "/v1/spend", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
		if rec.Code != 405 {
			t.Fatalf("%d", rec.Code)
		}
		noCORS(t, rec)
	})
}

func TestAPanicIsOneInternalErrorAndTheNextRequestIsServed(t *testing.T) {
	t.Parallel()
	ok := &seen{}
	h := build(t,
		resthandler.Route{Method: "POST", Path: "/v1/boom", Surface: resthandler.V1, Privilege: auth.Record, Handler: func(http.ResponseWriter, *http.Request) { panic("boom: nil map in the pricer") }},
		resthandler.Route{Method: "POST", Path: "/v1/fine", Surface: resthandler.V1, Privilege: auth.Record, Handler: ok.handler},
	)
	hdr := map[string]string{"Authorization": "Bearer record-token"}
	rec := do(h, "POST", "/v1/boom", hdr)
	if rec.Code != 500 || errorCode(t, rec) != "internal" || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := do(h, "POST", "/v1/fine", hdr); rec.Code != 200 {
		t.Fatalf("the service survives one bad request: %d", rec.Code)
	}
}

func TestAnAuthenticatorIsRequiredForV1(t *testing.T) {
	t.Parallel()
	reg := resthandler.NewRegistry()
	reg.Add(resthandler.Route{Method: "POST", Path: "/v1/admit", Surface: resthandler.V1, Privilege: auth.Record, Handler: func(http.ResponseWriter, *http.Request) {}})
	if _, err := reg.Handler(resthandler.Options{}); err == nil {
		t.Fatal("a registry with /v1 routes and no way to authenticate must not build: it would serve money to anyone")
	}
}
