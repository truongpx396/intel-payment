package payments

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
)

// CredentialCaller tells the payment door which realm a call means, from the credential the REST handler authenticated.
// The door imports no transport; this is where its words meet the credential's.
type CredentialCaller struct{}

var _ paymenthttp.Caller = CredentialCaller{}

// Via implements paymenthttp.Caller: the caller the credential identifies.
func (CredentialCaller) Via(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.CallerID
	}
	return ""
}

// Resolve implements paymenthttp.Caller.
func (CredentialCaller) Resolve(r *http.Request, s domain.Scope) (domain.Scope, error) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		return domain.Scope{}, paymenthttp.ErrUnauthenticated
	}
	scope, err := p.Resolve(s)
	switch {
	case errors.Is(err, auth.ErrRealmMismatch):
		return domain.Scope{}, fmt.Errorf("%w: %w", paymenthttp.ErrRealmMismatch, err)
	case err != nil:
		return domain.Scope{}, err
	}
	return scope, nil
}

var privileges = map[paymenthttp.Privilege]auth.Privilege{paymenthttp.Record: auth.Record, paymenthttp.Grant: auth.Grant, paymenthttp.Admin: auth.Admin}

// V1Routes mounts the door's routes on the REST handler's /v1 surface, where the registry authenticates every call and
// checks the route's privilege before the handler runs. A route that names no privilege is refused: it would be served to
// any caller.
func V1Routes(routes []paymenthttp.Route) ([]resthandler.Route, error) {
	out := make([]resthandler.Route, 0, len(routes))
	for _, r := range routes {
		priv, ok := privileges[r.Privilege]
		if !ok {
			return nil, fmt.Errorf("payments: %s %s names no privilege, and a /v1 route is never open", r.Method, r.Path)
		}
		out = append(out, resthandler.Route{Method: r.Method, Path: r.Path, Surface: resthandler.V1, Privilege: priv, Handler: r.Handler})
	}
	return out, nil
}
