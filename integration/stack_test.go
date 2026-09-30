//go:build integration

// Package integration wires the real adapters — Redis Functions, PostgreSQL, the configuration
// cache — under the real Meter, and asserts the behaviours no single package can: an outage that
// defers usage and a recovery that applies it exactly once, a cold account rebuilt from the books,
// a price change between two retries of one event.
package integration

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/internal/chaos"
	"github.com/truongpx396/intel-payment/internal/pgtest"
	"github.com/truongpx396/intel-payment/metering"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	pricingtable "github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var (
	pg    *pgtest.Postgres
	redis *redistest.Redis
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if pg, err = pgtest.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if redis, err = redistest.Run(ctx, redistest.Standalone); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = pg.Terminate(ctx)
		os.Exit(1)
	}
	rdb := redis.Client()
	if _, err := hotredis.EnsureInstalled(ctx, rdb); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = rdb.Close()
	code := m.Run()
	_ = redis.Terminate(ctx)
	_ = pg.Terminate(ctx)
	if code == 0 {
		if err := goleak.Find(
			goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}

var seq atomic.Int64

// stack is one product's wiring: its own database, its own realm, and a Redis reached through a
// proxy that can be cut.
type stack struct {
	t       *testing.T
	realm   domain.Realm
	pool    *pgxpool.Pool
	books   *pgadapter.Books
	journal *pgadapter.Journal
	config  *pgadapter.ConfigStore
	store   *hotredis.Store
	proxy   *chaos.Proxy
	met     *metrics
	cfg     metering.Config
	deps    app.Deps
	meter   ports.Meter
	replay  *app.JournalReplayer
}

type stackOpt struct {
	cfg    metering.Config
	quotas ports.QuotaSource
}

func newStack(t *testing.T, o stackOpt) *stack {
	t.Helper()
	ctx := context.Background()
	s := &stack{t: t, realm: domain.Realm(fmt.Sprintf("r%d", seq.Add(1))), met: newMetrics()}

	var err error
	if s.pool, err = pgadapter.Connect(ctx, pg.NewDB(t)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.pool.Close)
	s.books, s.journal = pgadapter.NewBooks(s.pool), pgadapter.NewJournal(s.pool)
	if s.config, err = pgadapter.NewConfigStore(ctx, s.pool, pgadapter.ConfigOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.config.Close)

	if s.proxy, err = chaos.NewProxy(redis.Addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.proxy.Close)

	s.cfg = o.cfg
	s.cfg.Realm, s.cfg.BalanceRedisURL, s.cfg.LedgerDSN = s.realm, "redis://x", "postgres://x"
	s.cfg = s.cfg.WithDefaults()
	s.cfg.AdmitTimeout, s.cfg.RecordTimeout = 400*time.Millisecond, 400*time.Millisecond

	rdb := hotredisClient(s.proxy.Addr())
	t.Cleanup(func() { _ = rdb.Close() })
	s.store, err = hotredis.New(rdb, hotredis.Options{Config: s.cfg, Pools: s.config, Limits: s.config, Clock: system.Clock{}})
	if err != nil {
		t.Fatal(err)
	}
	must(t, s.books.EnsureHotConfig(ctx, s.cfg.Shards))

	s.deps = app.Deps{
		Pricers: ports.PricerRegistry{"table": pricingtable.New(0)}, Balance: s.store, Books: s.books,
		Limits: s.config, Pools: s.config, RateCards: s.config, Journal: s.journal, Bus: &nullBus{},
		Quotas: o.quotas, Clock: system.Clock{}, IDs: &system.IDs{}, Metrics: s.met,
	}
	if s.meter, err = app.New(s.cfg, s.deps); err != nil {
		t.Fatal(err)
	}
	if s.replay, err = app.NewJournalReplayer(s.cfg, s.deps); err != nil {
		t.Fatal(err)
	}
	s.seedRealm()
	return s
}

func (s *stack) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("%s: %v", sql, err)
	}
}

func (s *stack) seedRealm() {
	r := string(s.realm)
	s.exec(`INSERT INTO credit_pools (realm, name, priority) VALUES ($1,'promo',10)`, r)
	s.exec(`INSERT INTO rate_cards (realm, version, pricer) VALUES ($1,'v1','table')`, r)
	s.exec(`INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size, cost_micros_per_block) VALUES
		($1,'v1','gpt','in',150000,1000000,100000), ($1,'v1','gpt','out',600000,1000000,0)`, r)
	s.exec(`INSERT INTO limits (realm, name, unit, max, window_kind, deny_code, sort_order) VALUES ($1,'scope_balance','credit',0,'balance','payment_required',0)`, r)
}

// open opens the scope's shard in Redis the way the worker does at start: hot_shards decides the
// generation, and `open` installs it only if absent.
func (s *stack) open(sc domain.Scope) {
	s.t.Helper()
	ctx := context.Background()
	sh := sc.Normalized().Shard(s.cfg.Shards)
	rec, err := s.books.EnsureShard(ctx, sh)
	must(s.t, err)
	must(s.t, s.store.OpenShard(ctx, sh, rec.Gen))
}

func (s *stack) scope() domain.Scope {
	return domain.Scope{Realm: s.realm, Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
}

func (s *stack) event(sc domain.Scope, key string) domain.Event {
	return domain.Event{Scope: sc, Resource: "llm.chat", RateKey: "gpt", IdemKey: key, OccurredAt: time.Now(),
		Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}}}
}

// hot is the scope's hot balance across pools, straight from Redis (bypassing the proxy's state).
func (s *stack) hot(sc domain.Scope) domain.Credits {
	s.t.Helper()
	direct := hotredisClient(redis.Addr)
	defer func() { _ = direct.Close() }()
	st, err := hotredis.New(direct, hotredis.Options{Config: s.cfg, Pools: s.config, Limits: s.config, Clock: system.Clock{}})
	must(s.t, err)
	acct, err := st.Snapshot(context.Background(), sc)
	must(s.t, err)
	var sum domain.Credits
	for _, v := range acct.Pools {
		sum += v
	}
	return sum
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// nullBus accepts and forgets: these tests are about money, not notifications.
type nullBus struct{ ports.Bus }

func (*nullBus) Publish(context.Context, string, []byte) error { return nil }

type metrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64 // the last value set
}

func newMetrics() *metrics { return &metrics{counts: map[string]int64{}, gauges: map[string]int64{}} }
func (m *metrics) Count(name string, n int64, _ ports.Labels) {
	m.mu.Lock()
	m.counts[name] += n
	m.mu.Unlock()
}
func (m *metrics) Gauge(name string, v int64, _ ports.Labels) {
	m.mu.Lock()
	m.gauges[name] = v
	m.mu.Unlock()
}
func (m *metrics) gauge(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gauges[name]
}
func (*metrics) Observe(string, float64, ports.Labels) {}
func (m *metrics) get(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name]
}
