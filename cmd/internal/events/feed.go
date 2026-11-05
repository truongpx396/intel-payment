package events

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/events/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/events/adapters/driving/eventshttp"
	"github.com/truongpx396/intel-payment/events/app"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// CredentialRealms tells the feed which realms a call may read, from the credential the REST handler's registry authenticated. The door
// imports no transport and no authenticator; this is where its words meet the credential's.
type CredentialRealms struct{}

var _ eventshttp.Caller = CredentialRealms{}

// Realms implements eventshttp.Caller. The registry checks a route's privilege before its handler runs, so the check here is a second
// one: a handler mounted without it would still refuse.
func (CredentialRealms) Realms(ctx context.Context, requested string) ([]string, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, eventshttp.ErrUnauthenticated
	}
	if err := p.Require(auth.Record); err != nil {
		return nil, fmt.Errorf("%w: %w", eventshttp.ErrForbidden, err)
	}
	if requested != "" {
		realm := domain.Realm(requested)
		if err := realm.Validate(); err != nil || !p.Covers(realm) {
			return nil, eventshttp.ErrRealmMismatch
		}
		return []string{string(realm.Or())}, nil
	}
	if p.AnyRealm {
		return nil, eventshttp.ErrRealmRequired
	}
	out := make([]string, 0, len(p.Realms))
	for _, r := range p.Realms {
		out = append(out, string(r.Or()))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

var privileges = map[eventshttp.Privilege]auth.Privilege{eventshttp.Record: auth.Record}

// FeedRoutes is GET /v1/events, mounted on the REST handler's /v1 surface where the registry authenticates every call and checks the
// route's privilege before the handler runs. A route that names no privilege is refused: it would be served to any caller.
func FeedRoutes(pool *pgxpool.Pool, clock mports.Clock, log *slog.Logger) ([]resthandler.Route, error) {
	feed, err := app.NewFeed(postgres.NewStore(pool, clock))
	if err != nil {
		return nil, err
	}
	door, err := eventshttp.New(eventshttp.Config{Feed: feed, Caller: CredentialRealms{}, Logger: log})
	if err != nil {
		return nil, err
	}
	routes := door.Routes()
	out := make([]resthandler.Route, 0, len(routes))
	for _, r := range routes {
		priv, ok := privileges[r.Privilege]
		if !ok {
			return nil, fmt.Errorf("events: %s %s names no privilege, and a /v1 route is never open", r.Method, r.Path)
		}
		out = append(out, resthandler.Route{Method: r.Method, Path: r.Path, Surface: resthandler.V1, Privilege: priv, Handler: r.Handler})
	}
	return out, nil
}
