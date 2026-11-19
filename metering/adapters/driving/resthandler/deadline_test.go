package resthandler_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
)

// A request that waits on the ledger holds one of a small number of pooled connections until the caller gives up, and a caller
// that never does — a hung proxy, a client that went away without closing — holds it for ever. net/http's WriteTimeout closes
// the connection but does not cancel the request's context, so the query it was waiting on carries on. The deadline is the
// router's to give, once, to every route: /v1, the webhook door and the platform alike.

func stallingHandler(sawDeadline *atomic.Bool, cause *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Context().Deadline()
		sawDeadline.Store(ok)
		select {
		case <-r.Context().Done():
			cause.Store(r.Context().Err())
		case <-time.After(5 * time.Second):
			cause.Store(errors.New("nothing cancelled the request in 5s"))
		}
		w.WriteHeader(http.StatusGatewayTimeout)
	}
}

func buildWith(t *testing.T, o resthandler.Options, routes ...resthandler.Route) http.Handler {
	t.Helper()
	reg := resthandler.NewRegistry()
	for _, r := range routes {
		reg.Add(r)
	}
	o.Auth, o.Logger = authenticator, quiet
	h, err := reg.Handler(o)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestARequestIsCancelledWhenItsConfiguredDeadlinePasses(t *testing.T) {
	t.Parallel()
	var saw atomic.Bool
	var cause atomic.Value
	h := buildWith(t, resthandler.Options{RequestTimeout: 50 * time.Millisecond},
		resthandler.Route{Method: "POST", Path: "/v1/stall", Surface: resthandler.V1, Privilege: auth.Record, Handler: stallingHandler(&saw, &cause)})

	start := time.Now()
	do(h, "POST", "/v1/stall", map[string]string{"Authorization": "Bearer record-token"})

	if !saw.Load() {
		t.Fatal("the handler's context carried no deadline: a stalled query holds its connection until the client gives up")
	}
	if err, _ := cause.Load().(error); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the request ended with %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("a 50ms deadline took %v to cancel the request", took)
	}
}

func TestEveryRouteGetsTheDeadlineNotOnlyTheAuthenticatedOnes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		route resthandler.Route
	}{
		{"a webhook", resthandler.Route{Method: "POST", Path: "/webhooks/{provider}/{account}", Surface: resthandler.Webhook}},
		{"a platform route", resthandler.Route{Method: "GET", Path: "/readyz", Surface: resthandler.Platform}},
	} {
		var saw atomic.Bool
		var cause atomic.Value
		tc.route.Handler = stallingHandler(&saw, &cause)
		h := buildWith(t, resthandler.Options{RequestTimeout: 50 * time.Millisecond}, tc.route)
		path := "/readyz"
		if tc.route.Surface == resthandler.Webhook {
			path = "/webhooks/stripe/acct"
		}
		do(h, tc.route.Method, path, nil)
		if !saw.Load() {
			t.Errorf("%s: the handler's context carried no deadline", tc.name)
		}
	}
}

func TestWithoutADeadlineTheRequestKeepsTheCallersOwnContext(t *testing.T) {
	t.Parallel()
	var saw atomic.Bool
	h := buildWith(t, resthandler.Options{},
		resthandler.Route{Method: "POST", Path: "/v1/ok", Surface: resthandler.V1, Privilege: auth.Record, Handler: func(_ http.ResponseWriter, r *http.Request) {
			_, ok := r.Context().Deadline()
			saw.Store(ok)
		}})
	do(h, "POST", "/v1/ok", map[string]string{"Authorization": "Bearer record-token"})
	if saw.Load() {
		t.Fatal("a host that embeds the handler and sets no deadline was given one")
	}
}
