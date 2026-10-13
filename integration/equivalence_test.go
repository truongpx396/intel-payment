//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/truongpx396/intel-payment/metering"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcserver"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// FR-044: an in-process Meter and a remote one are the same value to the code that calls them.
// contracts.MeterEquivalence is one suite, run twice over the same real stack — Redis, Postgres, the
// writer's journal — differing only in what the Meter under test is.

// TestMeterEquivalenceInProcess is the control: the suite against app.Meter directly. It passes today,
// which shows the suite asserts nothing the engine does not already do.
func TestMeterEquivalenceInProcess(t *testing.T) {
	t.Parallel()
	contracts.MeterEquivalence(t, func(t *testing.T, o contracts.MeterOptions) contracts.MeterHarness {
		s, h := equivalenceStack(t, o)
		h.Meter = s.meter
		h.Balance = func(_ context.Context, sc domain.Scope) (map[domain.Pool]domain.Credits, error) {
			return h.EngineBalance(t, sc), nil
		}
		return h
	})
}

// TestMeterEquivalenceOverTheWire is the claim: the same suite through the transport — grpcclient dialling
// grpcserver, which fronts the very engine the in-process mode calls. Nothing in the suite changed when the
// double that answered "not built yet" gave way to this pair (T069): the assertions are the same ones.
func TestMeterEquivalenceOverTheWire(t *testing.T) {
	t.Parallel()
	contracts.MeterEquivalence(t, func(t *testing.T, o contracts.MeterOptions) contracts.MeterHarness {
		s, h := equivalenceStack(t, o)
		client := serveOverTheWire(t, s, h)
		h.Meter = client
		h.Balance = client.Balance
		return h
	})
}

// serveOverTheWire puts a gRPC server in front of the stack's engine and returns a client on an in-memory
// connection to it. The caller is a credential with record and grant over the harness's two realms.
func serveOverTheWire(t *testing.T, s *stack, h contracts.MeterHarness) *grpcclient.Client {
	t.Helper()
	svc, ok := s.meter.(ports.Service)
	if !ok {
		t.Fatal("the engine is not a ports.Service")
	}
	srv, err := grpcserver.New(svc, svc)
	must(t, err)
	caller := auth.Principal{CallerID: "equivalence", Realms: []domain.Realm{h.Realm, h.OtherRealm}, Privileges: auth.Record | auth.Grant}
	g := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		return next(auth.WithPrincipal(ctx, caller), req)
	}))
	srv.Register(g)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(t, err)
	t.Cleanup(func() { _ = conn.Close(); g.Stop(); _ = lis.Close() })
	return grpcclient.New(conn)
}

// equivalenceStack builds one realm's real stack for a subtest, and a harness whose levers drive it. The
// caller binds Meter and Balance: those are the transport under test.
func equivalenceStack(t *testing.T, o contracts.MeterOptions) (*stack, contracts.MeterHarness) {
	t.Helper()
	s := newStack(t, stackOpt{cfg: metering.Config{NegativeBalance: o.Policy, AdmitFail: o.AdmitFail}})
	for i, l := range o.Limits {
		s.addLimit(l, 10*(i+1))
	}
	s.awaitLimits(1 + len(o.Limits))
	return s, contracts.MeterHarness{
		Engine:        s.meter,
		EngineBalance: func(t *testing.T, sc domain.Scope) map[domain.Pool]domain.Credits { t.Helper(); return s.pools(sc) },
		Realm:         s.realm,
		OtherRealm:    s.realm + "-other",
		CardVersion:   "v1",
		Now:           time.Now,
		Open:          func(t *testing.T, sc domain.Scope) { t.Helper(); s.open(sc) },
		Outage: func(t *testing.T) func() {
			t.Helper()
			s.proxy.Cut()
			return s.proxy.Restore
		},
		Replay: func(t *testing.T) {
			t.Helper()
			res, err := s.replay.Replay(context.Background(), 10)
			must(t, err)
			if res.Claimed == 0 {
				t.Fatalf("the journal replay found nothing to apply: %+v", res)
			}
		},
	}
}

// pools is the scope's hot balance per pool, read straight from Redis — bypassing the proxy, so it is
// the oracle even while the proxy is cut.
func (s *stack) pools(sc domain.Scope) map[domain.Pool]domain.Credits {
	s.t.Helper()
	direct := hotredisClient(redis.Addr)
	defer func() { _ = direct.Close() }()
	st, err := hotredis.New(direct, hotredis.Options{Config: s.cfg, Pools: s.config, Limits: s.config, Clock: system.Clock{}})
	must(s.t, err)
	acct, err := st.Snapshot(context.Background(), sc)
	must(s.t, err)
	return acct.Pools
}

// addLimit configures one more ceiling for the realm, the way an operator's row would.
func (s *stack) addLimit(l domain.Limit, order int) {
	s.t.Helper()
	var subject, resource, entitlement, dur any
	if l.Subject != "" {
		subject = l.Subject
	}
	if l.Resource != "" {
		resource = l.Resource
	}
	if l.MaxEntitlement != "" {
		entitlement = l.MaxEntitlement
	}
	if l.Dur != 0 {
		dur = int(l.Dur / time.Second)
	}
	tz, warn := l.TZ, l.WarnAt
	if tz == "" {
		tz = "UTC"
	}
	if warn == 0 {
		warn = 0.8
	}
	s.exec(`INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, dur_seconds, tz, warn_at, deny_code, resource, sort_order)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		string(s.realm), l.Name, subject, string(l.Unit), l.Max, entitlement, l.Window.String(), dur, tz, warn, l.DenyCode, resource, order)
}

// awaitLimits waits for the configuration cache to learn of n limits from the database's own notification.
func (s *stack) awaitLimits(n int) {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ls, err := s.config.Limits(context.Background(), s.realm)
		if err == nil && len(ls) == n {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the configuration cache never saw %d limits (has %d, err %v)", n, len(ls), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
