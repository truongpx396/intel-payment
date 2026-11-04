package entitlements

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	entpg "github.com/truongpx396/intel-payment/entitlement/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementgrpc"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementhttp"
	entapp "github.com/truongpx396/intel-payment/entitlement/app"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Common is what paymentd gives the entitlement layer.
type Common struct {
	Pool          *pgxpool.Pool
	Clock         mports.Clock
	Metrics       mports.Metrics
	Bus           mports.Bus
	Log           *slog.Logger
	Settings      envcfg.Entitlements
	SubjectPrefix string // PAYMENT_SUBJECT_PREFIX
}

// Stack is the entitlement layer, open over a database.
type Stack struct {
	// Entitler answers a host's questions; Quotas is the same resolution as metering's QuotaSource, so a limit sized by a plan is
	// sized by what the Entitler enforces.
	Entitler *entapp.Entitler
	Quotas   mports.QuotaSource

	watcher *entapp.Watcher
}

// GroupFor names a replica's subscription to the announcements that evict its cache. Every replica needs a group of its own, since
// a shared one would hand each announcement to one of them, and a stable one, since a bus stream is only trimmed up to what every
// group has acknowledged.
func GroupFor(instance string) string { return "entitlement-cache-" + instance }

// Open builds the Entitler over the database and, when it has a cache, joins the announcements that keep it current.
func Open(ctx context.Context, c Common) (*Stack, error) {
	e, err := entapp.New(entapp.Deps{Store: entpg.New(c.Pool), Clock: c.Clock, Metrics: c.Metrics, CacheTTL: c.Settings.CacheTTL})
	if err != nil {
		return nil, err
	}
	s := &Stack{Entitler: e, Quotas: entapp.NewQuotaSource(e)}
	if c.Settings.CacheTTL <= 0 {
		c.Log.Info("entitlements are not cached: every check reads the database", "PAYMENT_ENTITLEMENT_CACHE_TTL", c.Settings.CacheTTL.String())
		return s, nil
	}
	instance := c.Settings.InstanceID
	if instance == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("entitlements: PAYMENT_INSTANCE_ID is not set and the hostname cannot be read, so this replica has no name to subscribe under: %w", err)
		}
		instance = sanitise(host) // an empty hostname becomes "replica": sanitise never returns nothing
	}
	group := GroupFor(instance)
	if s.watcher, err = e.Watch(ctx, c.Bus, c.SubjectPrefix, group); err != nil {
		return nil, fmt.Errorf("entitlements: this replica cannot subscribe to the announcements that keep its cache current, so it would serve stale entitlements for up to the TTL after every change: %w", err)
	}
	c.Log.Info("entitlement cache is kept current", "group", group, "ttl", c.Settings.CacheTTL.String(),
		"note", "a retired replica's group must be deleted from the bus, or the stream is never trimmed past it")
	return s, nil
}

// Close ends the subscription.
func (s *Stack) Close() {
	if s.watcher != nil {
		_ = s.watcher.Close()
	}
}

// sanitise makes a hostname usable as an instance id: the characters a consumer-group name allows.
func sanitise(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.TrimLeft(b.String(), "-._")
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		return "replica"
	}
	return out
}

// REST is the entitlement routes, mounted on the REST handler's /v1 surface behind the credential.
func (s *Stack) REST(log *slog.Logger) ([]resthandler.Route, error) {
	door, err := entitlementhttp.New(entitlementhttp.Config{Entitler: s.Entitler, Checker: s.Entitler, Caller: CredentialCaller{}, Logger: log})
	if err != nil {
		return nil, err
	}
	return V1Routes(door.Routes())
}

// GRPC is entitlement.v1.Entitlement, to be registered on a server whose interceptor has authenticated the call.
func (s *Stack) GRPC(log *slog.Logger) (*entitlementgrpc.Server, error) {
	return entitlementgrpc.New(entitlementgrpc.Config{Entitler: s.Entitler, Checker: s.Entitler, Caller: CredentialCaller{}, Logger: log})
}

// StartRelay runs the config relay: it forwards the database's configuration notifications onto the bus as
// <prefix>.config.<realm>, which every replica's cache listens for. A relay that cannot start is an error at start — a worker that
// is not relaying leaves every cache to its TTL — and one that later fails to publish is logged and carries on.
func StartRelay(ctx context.Context, pool *pgxpool.Pool, bus mports.Bus, prefix string, log *slog.Logger) (*pgadapter.ConfigRelay, error) {
	r, err := pgadapter.NewConfigRelay(ctx, pool, bus, prefix, pgadapter.RelayOptions{
		OnError: func(err error) { log.Error("config relay", "error", err.Error()) },
	})
	if err != nil {
		return nil, fmt.Errorf("entitlements: the config relay cannot start, so no replica would hear of a configuration change before its cache TTL passed: %w", err)
	}
	return r, nil
}
