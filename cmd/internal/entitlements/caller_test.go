package entitlements_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/truongpx396/intel-payment/cmd/internal/entitlements"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementhttp"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	edomain "github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R105: the doors learn whose realm a call is for from the credential the transport authenticated — the REST handler's registry,
// or the gRPC interceptor — through this one Caller. The realm is authenticated, never asserted; a credential without `record`
// cannot read a customer's entitlements; and a call no credential reached is refused, never served.

func asking(p *auth.Principal) context.Context {
	if p == nil {
		return context.Background()
	}
	return auth.WithPrincipal(context.Background(), *p)
}

func TestTheCredentialDecidesWhoIsAskingAndForWhichRealm(t *testing.T) {
	t.Parallel()
	c := entitlements.CredentialCaller{}
	one := &auth.Principal{CallerID: "host", Realms: []domain.Realm{"t1"}, Privileges: auth.Record}
	two := &auth.Principal{CallerID: "host", Realms: []domain.Realm{"t1", "other"}, Privileges: auth.Record}
	anyRealm := &auth.Principal{CallerID: "local", AnyRealm: true, Privileges: auth.Record}
	scope := func(realm string) domain.Scope {
		return domain.Scope{Realm: domain.Realm(realm), Kind: "org", ID: "o1"}
	}

	for name, tc := range map[string]struct {
		p    *auth.Principal
		in   domain.Scope
		want domain.Realm
		err  error
	}{
		"its only realm, none named":     {one, scope(""), "t1", nil},
		"its realm, named":               {one, scope("t1"), "t1", nil},
		"a realm it does not cover":      {one, scope("other"), "", entitlementwire.ErrRealmMismatch},
		"two realms and none named":      {two, scope(""), "", edomain.ErrInvalid},
		"the second of two, named":       {two, scope("other"), "other", nil},
		"a credential for any realm":     {anyRealm, scope(""), "default", nil},
		"no credential reached the call": {nil, scope("t1"), "", entitlementwire.ErrUnauthenticated},
		"a credential that only meters":  {&auth.Principal{CallerID: "meter", Realms: []domain.Realm{"t1"}, Privileges: auth.Grant}, scope(""), "", entitlementwire.ErrForbidden},
		"a credential for no realm":      {&auth.Principal{CallerID: "nobody", Privileges: auth.Record}, scope(""), "", edomain.ErrInvalid},
	} {
		got, err := c.Resolve(asking(tc.p), tc.in)
		if tc.err != nil {
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: %v; want %v", name, err, tc.err)
			}
			continue
		}
		if err != nil || got.Realm != tc.want || got.Kind != "org" || got.ID != "o1" {
			t.Errorf("%s: %+v, %v; want realm %q", name, got, err, tc.want)
		}
	}
}

// A route that names no privilege would be served to any caller, and a /v1 route is never open.
func TestTheRoutesAreMountedOnTheV1SurfaceWithTheirPrivilege(t *testing.T) {
	t.Parallel()
	h := func(http.ResponseWriter, *http.Request) {}
	got, err := entitlements.V1Routes([]entitlementhttp.Route{
		{Method: "GET", Path: "/v1/entitlements", Privilege: entitlementhttp.Record, Handler: h},
		{Method: "POST", Path: "/v1/entitlements/check", Privilege: entitlementhttp.Record, Handler: h},
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v, %v", got, err)
	}
	for _, r := range got {
		if r.Surface != resthandler.V1 || r.Privilege != auth.Record || r.Handler == nil {
			t.Errorf("%s %s: %+v; want the V1 surface, `record`, and the door's handler", r.Method, r.Path, r)
		}
	}
	if _, err := entitlements.V1Routes([]entitlementhttp.Route{{Method: "GET", Path: "/v1/entitlements", Handler: h}}); err == nil {
		t.Error("a route naming no privilege was mounted")
	}
}
