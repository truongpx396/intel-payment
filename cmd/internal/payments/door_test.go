package payments_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R099a: the door asks "which realm does this credential mean?" and the answer is the REST handler's: the credential the
// registry authenticated, put on the request context. The glue maps its words to the door's and refuses the one thing the
// door must never be left to guess — a call no credential reached.

func requestAs(p *auth.Principal) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/plans", nil)
	if p != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), *p))
	}
	return r
}

func TestTheDoorLearnsTheRealmFromTheAuthenticatedCredential(t *testing.T) {
	t.Parallel()
	one := &auth.Principal{CallerID: "host", Realms: []md.Realm{"acme"}, Privileges: auth.Record}
	two := &auth.Principal{CallerID: "host", Realms: []md.Realm{"acme", "beta"}, Privileges: auth.Record}
	var c paymenthttp.Caller = payments.CredentialCaller{}

	got, err := c.Resolve(requestAs(one), domain.Scope{Kind: "user", ID: "u1"})
	if err != nil || got != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) {
		t.Errorf("one realm, none named = %+v, %v; want the credential's", got, err)
	}
	got, err = c.Resolve(requestAs(two), domain.Scope{Realm: "beta", Kind: "user", ID: "u1"})
	if err != nil || got.Realm != "beta" {
		t.Errorf("two realms, one named = %+v, %v; want beta", got, err)
	}
	if _, err = c.Resolve(requestAs(two), domain.Scope{Kind: "user", ID: "u1"}); !errors.Is(err, md.ErrInvalid) {
		t.Errorf("two realms, none named = %v; want ErrInvalid: a realm is never guessed", err)
	}
	if _, err = c.Resolve(requestAs(one), domain.Scope{Realm: "other", Kind: "user", ID: "u1"}); !errors.Is(err, paymenthttp.ErrRealmMismatch) {
		t.Errorf("a realm outside the credential = %v; want the door's ErrRealmMismatch, which it answers 403", err)
	}
}

func TestACallNoCredentialReachedIsRefusedNotServed(t *testing.T) {
	t.Parallel()
	_, err := payments.CredentialCaller{}.Resolve(requestAs(nil), domain.Scope{Kind: "user", ID: "u1"})
	if !errors.Is(err, paymenthttp.ErrUnauthenticated) {
		t.Fatalf("no principal on the context = %v; want ErrUnauthenticated: a wiring slip must refuse, never serve", err)
	}
}

func TestEachDoorRouteIsMountedOnTheV1SurfaceWithItsPrivilege(t *testing.T) {
	t.Parallel()
	in := []paymenthttp.Route{
		{Method: "GET", Path: "/v1/plans", Privilege: paymenthttp.Record, Handler: func(http.ResponseWriter, *http.Request) {}},
		{Method: "PUT", Path: "/v1/auto-recharge", Privilege: paymenthttp.Grant, Handler: func(http.ResponseWriter, *http.Request) {}},
		{Method: "PUT", Path: "/v1/admin/provider-accounts/{id}", Privilege: paymenthttp.Admin, Handler: func(http.ResponseWriter, *http.Request) {}},
	}
	out, err := payments.V1Routes(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []auth.Privilege{auth.Record, auth.Grant, auth.Admin}
	for i, r := range out {
		if r.Surface != resthandler.V1 || r.Privilege != want[i] || r.Method != in[i].Method || r.Path != in[i].Path || r.Handler == nil {
			t.Errorf("route %d = %+v, want %s %s on /v1 needing %s", i, r, in[i].Method, in[i].Path, want[i])
		}
	}
	if _, err := payments.V1Routes([]paymenthttp.Route{{Method: "GET", Path: "/v1/x", Privilege: 0, Handler: func(http.ResponseWriter, *http.Request) {}}}); err == nil {
		t.Error("a route with no privilege was mounted: it would be served to any caller")
	}
}

func TestTheCredentialsCallerIsRecordedBesideEveryAdminChange(t *testing.T) {
	t.Parallel()
	p := &auth.Principal{CallerID: "billing-console", Realms: []md.Realm{"acme"}, Privileges: auth.Admin}
	if got := (payments.CredentialCaller{}).Via(requestAs(p)); got != "billing-console" {
		t.Errorf("Via = %q, want the credential's caller", got)
	}
	if got := (payments.CredentialCaller{}).Via(requestAs(nil)); got != "" {
		t.Errorf("Via with no credential = %q, want nothing", got)
	}
}
