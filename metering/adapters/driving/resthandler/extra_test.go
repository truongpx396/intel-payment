package resthandler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/contracts"
)

// R094: the REST handler is the one HTTP door of the service, and a host that composes more than metering into it — the
// payment webhook receiver is the first — adds its routes through Config.Extra. The table judges them exactly as it
// judges its own: a webhook route carries no authentication, a /v1 route always does, and nothing is declared twice.
// Metering imports nothing of what the routes do.

// extra builds the production handler with the given extra routes, or the error that refused them.
func extra(routes ...resthandler.Route) (http.Handler, error) {
	stub := &stubMeter{}
	return resthandler.New(resthandler.Config{
		Meter: stub, Balance: stub, Clock: contracts.NewFakeClock(epoch), Logger: quiet,
		Auth: auth.AuthenticatorFunc(func(_ context.Context, c auth.Credential) (auth.Principal, error) {
			if p, ok := principals[c.Bearer]; ok && c.Bearer != "" {
				return p, nil
			}
			return auth.Principal{}, auth.ErrUnauthenticated
		}),
		Extra: routes,
	})
}

func TestAnExtraWebhookRouteIsServedWithItsPathValuesAndNoCredential(t *testing.T) {
	t.Parallel()
	var s seen
	var got atomic.Value
	h, err := extra(resthandler.Route{Method: http.MethodPost, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook,
		Handler: func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got.Store(r.PathValue("provider") + "|" + r.PathValue("account") + "|" + string(b))
			s.handler(w, r)
		}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe/acct-main", strings.NewReader(`{"raw":"body"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || s.reached.Load() != 1 {
		t.Fatalf("a signed delivery carries no service credential and must reach the route: status %d, reached %d", rec.Code, s.reached.Load())
	}
	if want := `stripe|acct-main|{"raw":"body"}`; got.Load() != want {
		t.Fatalf("the route saw %v, want %q", got.Load(), want)
	}
	if s.hadAny.Load() {
		t.Fatal("a webhook route was given a caller: nothing authenticated it, and it must not look as if something did")
	}
}

func TestAnExtraRouteIsHeldToTheRulesOfItsSurface(t *testing.T) {
	t.Parallel()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	for name, route := range map[string]resthandler.Route{
		"a webhook route that asks for a credential": {Method: http.MethodPost, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook,
			Middleware: []resthandler.Middleware{resthandler.RequireAuth(auth.AuthenticatorFunc(func(context.Context, auth.Credential) (auth.Principal, error) {
				return auth.Principal{}, nil
			}))}, Handler: ok},
		"a webhook route with CORS":               {Method: http.MethodPost, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Middleware: []resthandler.Middleware{resthandler.CORS("*")}, Handler: ok},
		"a route on none of the surfaces":         {Method: http.MethodPost, Path: "/payments/hook", Surface: resthandler.Webhook, Handler: ok},
		"a route that repeats one the table has":  {Method: http.MethodPost, Path: "/v1/admit", Surface: resthandler.V1, Privilege: auth.Record, Handler: ok},
		"a route with no handler":                 {Method: http.MethodPost, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook},
		"a webhook route that names a privilege":  {Method: http.MethodPost, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Privilege: auth.Admin, Handler: ok},
		"a /v1 route that names no privilege":     {Method: http.MethodGet, Path: "/v1/extra", Surface: resthandler.V1, Handler: ok},
		"a webhook route declared on /v1's path":  {Method: http.MethodPost, Path: "/v1/extra", Surface: resthandler.Webhook, Handler: ok},
		"a platform route that carries auth":      {Method: http.MethodGet, Path: "/metrics", Surface: resthandler.Platform, Middleware: []resthandler.Middleware{resthandler.RequireAuth(authenticator)}, Handler: ok},
		"a route with a method the service lacks": {Method: http.MethodTrace, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: ok},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if h, err := extra(route); err == nil {
				t.Fatalf("the table accepted %s; it must be refused at start, not discovered by a request (handler %v)", name, h != nil)
			}
		})
	}
}

func TestAnExtraV1RouteStillRequiresItsPrivilege(t *testing.T) {
	t.Parallel()
	var s seen
	h, err := extra(resthandler.Route{Method: http.MethodGet, Path: "/v1/extra/{id}", Surface: resthandler.V1, Privilege: auth.Grant, Handler: s.handler})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		hdr  map[string]string
		want int
	}{
		"no credential":            {nil, http.StatusUnauthorized},
		"a credential that cannot": {map[string]string{"Authorization": "Bearer record-token"}, http.StatusForbidden},
		"one that can":             {map[string]string{"Authorization": "Bearer grant-token"}, http.StatusOK},
	} {
		if rec := do(h, http.MethodGet, "/v1/extra/7", tc.hdr); rec.Code != tc.want {
			t.Fatalf("%s: status %d, want %d", name, rec.Code, tc.want)
		}
	}
	if s.reached.Load() != 1 {
		t.Fatalf("the route was reached %d times; only the one authorized call may reach it", s.reached.Load())
	}
}

func TestAnExtraRouteAnswersTheWrongMethodAs405WithItsAllowList(t *testing.T) {
	t.Parallel()
	var s seen
	h, err := extra(resthandler.Route{Method: http.MethodPost, Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook, Handler: s.handler})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(h, http.MethodGet, "/webhooks/stripe/acct", nil)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost || s.reached.Load() != 0 {
		t.Fatalf("a GET to the webhook path: status %d, Allow %q, reached %d; want 405, POST, 0", rec.Code, rec.Header().Get("Allow"), s.reached.Load())
	}
}

func TestWithNoExtraRoutesTheTableIsTheOneItWas(t *testing.T) {
	t.Parallel()
	with, err := resthandler.BuildRegistry(resthandler.Config{Meter: &stubMeter{}, Balance: &stubMeter{}, Clock: contracts.NewFakeClock(epoch), Auth: authenticator, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range with.Routes() {
		if r.Surface == resthandler.Webhook {
			t.Fatalf("%s is served though nothing added it: the service would receive deliveries for a payment layer it does not have", r)
		}
	}
}
