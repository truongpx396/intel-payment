package entitlements

import (
	"context"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementhttp"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// CredentialCaller tells the entitlement doors which realm a call means, from the credential the transport authenticated — the REST
// handler's registry, or the gRPC server's interceptor, both of which leave a Principal on the context. The doors import no
// transport and no authenticator; this is where their words meet the credential's.
type CredentialCaller struct{}

var _ entitlementwire.Caller = CredentialCaller{}

// Resolve implements entitlementwire.Caller. The registry checks a REST route's privilege before its handler runs and the gRPC
// interceptor checks none, so the privilege is checked here as well: reading a customer's entitlements needs `record`.
func (CredentialCaller) Resolve(ctx context.Context, s domain.Scope) (domain.Scope, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return domain.Scope{}, entitlementwire.ErrUnauthenticated
	}
	if err := p.Require(auth.Record); err != nil {
		return domain.Scope{}, fmt.Errorf("%w: %w", entitlementwire.ErrForbidden, err)
	}
	scope, err := p.Resolve(s)
	switch {
	case errors.Is(err, auth.ErrRealmMismatch):
		return domain.Scope{}, fmt.Errorf("%w: %w", entitlementwire.ErrRealmMismatch, err)
	case err != nil:
		return domain.Scope{}, err
	}
	return scope, nil
}

var privileges = map[entitlementhttp.Privilege]auth.Privilege{entitlementhttp.Record: auth.Record}

// V1Routes mounts the door's routes on the REST handler's /v1 surface, where the registry authenticates every call and checks the
// route's privilege before the handler runs. A route that names no privilege is refused: it would be served to any caller.
func V1Routes(routes []entitlementhttp.Route) ([]resthandler.Route, error) {
	out := make([]resthandler.Route, 0, len(routes))
	for _, r := range routes {
		priv, ok := privileges[r.Privilege]
		if !ok {
			return nil, fmt.Errorf("entitlements: %s %s names no privilege, and a /v1 route is never open", r.Method, r.Path)
		}
		out = append(out, resthandler.Route{Method: r.Method, Path: r.Path, Surface: resthandler.V1, Privilege: priv, Handler: r.Handler})
	}
	return out, nil
}
