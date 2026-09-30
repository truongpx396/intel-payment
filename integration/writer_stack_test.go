//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/internal/chaos"
	"github.com/truongpx396/intel-payment/metering"
	pgarchive "github.com/truongpx396/intel-payment/metering/adapters/driven/archive/postgres"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/natsjetstream"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// writerShards spreads scopes over the whole shard space so that parallel tests sharing one Redis
// almost never share a shard — and a stream — with each other.
const writerShards = 16384

// wopts configure one writer stack.
type wopts struct {
	contracts.LedgerOptions
	cfg metering.Config
	// dedicated starts a Redis of its own: a test that bumps a shard's generation, cuts a replica
	// or truncates an AOF must not do it to a server other tests are using.
	dedicated bool
	// redisOpts tune that Redis (fsync, a fixed port…). srv, when set, is a server the caller already
	// started and owns; proxied puts a proxy in front of it so the caller can Retarget it.
	redisOpts redistest.Options
	srv       *redistest.Redis
	proxied   bool
	watches   ports.WatchSource
	// jetstream reads intents through the optional bus: the hot functions still write the Redis
	// outbox, a Relay forwards it to JetStream, and the writer reads JetStream.
	jetstream bool
}

// wstack is the whole durable side over real stores: Postgres for the books, Redis for the hot tier
// and its outbox streams, the real Writer between them.
type wstack struct {
	t       *testing.T
	rawPool *pgxpool.Pool
	books   *pgadapter.Books
	srv     *redistest.Redis
	proxy   *chaos.Proxy
	rdb     goredis.UniversalClient
	store   *hotredis.Store
	archive ports.UsageArchive
	bus     *recBus
	met     *metrics
	clock   *contracts.FakeClock
	cfg     metering.Config
	writer  *app.Writer
	expirer *app.Expirer
	fault   *faultBalance
	group   string
	stream  *scopedStream
	relay   *natsjetstream.Relay // nil unless jetstream
	jsOpts  natsjetstream.IntentsOptions

	mu     sync.Mutex
	scopes map[domain.Scope]bool
	extra  map[domain.Shard]bool
}

// client dials the hot tier as this stack's components do: through the proxy when there is one, so
// that a failover is a Retarget and every client follows it.
func (s *wstack) client() goredis.UniversalClient {
	if s.proxy != nil {
		return goredis.NewClient(&goredis.Options{Addr: s.proxy.Addr()})
	}
	return s.srv.Client()
}

func newWriterStack(t *testing.T, o wopts) *wstack {
	t.Helper()
	ctx := context.Background()
	s := &wstack{t: t, met: newMetrics(), bus: &recBus{}, scopes: map[domain.Scope]bool{}, extra: map[domain.Shard]bool{}}

	now := o.Now
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Second)
	}
	s.clock = contracts.NewFakeClock(now)

	dsn := pg.NewDB(t)
	pool, err := pgadapter.Connect(ctx, dsn)
	must(t, err)
	t.Cleanup(pool.Close)
	s.books = pgadapter.NewBooks(pool)
	s.archive = pgarchive.New(pool)
	s.rawPool = pool

	s.srv = redis
	if o.srv != nil {
		s.srv = o.srv
	} else if o.dedicated {
		ro := o.redisOpts
		ro.Mode = redistest.Standalone
		srv, err := redistest.RunWith(ctx, ro)
		must(t, err)
		t.Cleanup(func() { _ = srv.Terminate(context.Background()) })
		c := srv.Client()
		_, err = hotredis.EnsureInstalled(ctx, c)
		must(t, err)
		_ = c.Close()
		s.srv = srv
	}
	if o.proxied {
		s.proxy, err = chaos.NewProxy(s.srv.Addr)
		must(t, err)
		t.Cleanup(s.proxy.Close)
	}
	s.rdb = s.client()
	t.Cleanup(func() { _ = s.rdb.Close() })

	s.cfg = o.cfg
	s.cfg.Realm, s.cfg.BalanceRedisURL, s.cfg.LedgerDSN = "t1", "redis://x", "postgres://x"
	s.cfg.Shards = writerShards
	s.cfg.AckWait = time.Millisecond // a failed delivery is retried at once; there is no second consumer to wait for
	s.cfg.NegativeBalance = o.Policy
	if s.cfg.NegativeBalance == "" {
		s.cfg.NegativeBalance = metering.ClampToZero
	}
	s.cfg = s.cfg.WithDefaults()
	must(t, s.books.EnsureHotConfig(ctx, s.cfg.Shards))

	s.store, err = hotredis.New(s.rdb, hotredis.Options{
		Config: s.cfg, Pools: contracts.StaticPools(o.Pools), Limits: contracts.StaticLimits(o.Limits), Clock: s.clock,
		Watches: o.watches,
	})
	must(t, err)

	streamRdb := s.client()
	t.Cleanup(func() { _ = streamRdb.Close() })
	s.group = fmt.Sprintf("test-%d", seq.Add(1))
	var inner ports.IntentStream
	if o.jetstream {
		inner = s.newJetStreamIntents(streamRdb)
	} else {
		in, err := redisstreams.NewStream(streamRdb, redisstreams.IntentOptions{Consumer: "w1", Group: s.group, Clock: s.clock})
		must(t, err)
		inner = in
	}
	s.stream = &scopedStream{inner: inner, mine: s.owns, mu: &sync.Mutex{}, injected: map[string]bool{}}
	s.fault = &faultBalance{BalanceStore: s.store}

	deps := s.deps(s.stream)
	s.writer, err = app.NewWriter(s.cfg, deps)
	must(t, err)
	s.expirer, err = app.NewExpirer(s.cfg, deps)
	must(t, err)
	return s
}

func (s *wstack) deps(stream ports.IntentStream) app.Deps {
	return app.Deps{
		Balance: s.fault, Books: s.books, Stream: stream, Archive: s.archive, Bus: s.bus,
		Clock: s.clock, IDs: &system.IDs{}, Metrics: s.met,
	}
}

// ------------------------------------------------------------- ownership --

func (s *wstack) track(scopes ...domain.Scope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sc := range scopes {
		s.scopes[sc.Normalized()] = true
	}
}

func (s *wstack) owns(sc domain.Scope) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scopes[sc.Normalized()]
}

func (s *wstack) shards() []domain.Shard {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[domain.Shard]bool{}
	for sc := range s.scopes {
		seen[sc.Shard(s.cfg.Shards)] = true
	}
	for sh := range s.extra {
		seen[sh] = true
	}
	out := make([]domain.Shard, 0, len(seen))
	for sh := range seen {
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// scopedStream is the writer's view of the outbox restricted to the scopes THIS test created. The
// stream is per shard and shared with every other test on the server; each test reads it through a
// consumer group of its own, and acknowledges — in that group only — entries that are not its own.
type scopedStream struct {
	inner    ports.IntentStream
	mine     func(domain.Scope) bool
	mu       *sync.Mutex
	injected map[string]bool
}

func (s *scopedStream) Read(ctx context.Context, sh domain.Shard, count int, minIdle time.Duration) ([]ports.Delivery, error) {
	ds, err := s.inner.Read(ctx, sh, count, minIdle)
	if err != nil {
		return nil, err
	}
	var keep []ports.Delivery
	var foreign []string
	for _, d := range ds {
		if s.isMine(d) {
			keep = append(keep, d)
		} else {
			foreign = append(foreign, d.ID)
		}
	}
	if len(foreign) > 0 {
		if err := s.inner.Ack(ctx, sh, foreign...); err != nil {
			return nil, err
		}
	}
	return keep, nil
}

func (s *scopedStream) isMine(d ports.Delivery) bool {
	if d.ParseErr == nil {
		return s.mine(d.Intent.Scope)
	}
	// An entry that does not parse is this test's if it names one of its scopes, or was injected by it.
	if s.mine(domain.Scope{Realm: domain.Realm(d.Fields["realm"]), Kind: d.Fields["kind"], ID: d.Fields["id"]}) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.injected[d.ID]
}

func (s *scopedStream) Ack(ctx context.Context, sh domain.Shard, ids ...string) error {
	return s.inner.Ack(ctx, sh, ids...)
}

func (s *scopedStream) Trim(ctx context.Context, sh domain.Shard, keep ports.RetentionPolicy) (int64, error) {
	return s.inner.Trim(ctx, sh, keep)
}

func (s *scopedStream) Stats(ctx context.Context, sh domain.Shard) (ports.StreamStats, error) {
	return s.inner.Stats(ctx, sh)
}

// ------------------------------------------------------------------ ledger --

// ledger is the hot store as the production wiring uses it: a shard is opened at the generation
// Postgres holds, and an account that is cold is rebuilt from the books by the writer's Rehydrate
// before the operation is retried.
type ledger struct {
	*wstack
}

func (l ledger) ensure(ctx context.Context, scopes ...domain.Scope) error {
	l.track(scopes...)
	for _, sc := range scopes {
		sh := sc.Normalized().Shard(l.cfg.Shards)
		rec, err := l.books.EnsureShard(ctx, sh)
		if err != nil {
			return err
		}
		st, err := l.store.ShardState(ctx, sh)
		if err != nil {
			return err
		}
		switch {
		case !st.Open:
			if err := l.store.OpenShard(ctx, sh, rec.Gen); err != nil {
				return err
			}
		case st.Gen < rec.Gen:
			if err := l.store.BumpShard(ctx, sh, rec.Gen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l ledger) retry(ctx context.Context, sc domain.Scope, op func() error) error {
	if err := l.ensure(ctx, sc); err != nil {
		return err
	}
	err := op()
	if errors.Is(err, domain.ErrColdScope) {
		if rerr := l.writer.Rehydrate(ctx, sc); rerr != nil {
			return rerr
		}
		err = op()
	}
	return err
}

func (l ledger) Balance(ctx context.Context, sc domain.Scope) (m map[domain.Pool]domain.Credits, err error) {
	err = l.retry(ctx, sc, func() (e error) { m, e = l.store.Balance(ctx, sc); return })
	return
}

func (l ledger) Admit(ctx context.Context, sc domain.Scope, req domain.AdmitRequest) (a domain.Admission, err error) {
	err = l.retry(ctx, sc, func() (e error) { a, e = l.store.Admit(ctx, sc, req); return })
	return
}

func (l ledger) Debit(ctx context.Context, c domain.Charge) (r domain.Receipt, err error) {
	err = l.retry(ctx, c.Scope, func() (e error) { r, e = l.store.Debit(ctx, c); return })
	return
}

func (l ledger) Grant(ctx context.Context, g domain.Grant) (r domain.Receipt, err error) {
	err = l.retry(ctx, g.Scope, func() (e error) { r, e = l.store.Grant(ctx, g); return })
	return
}

func (l ledger) Transfer(ctx context.Context, t domain.Transfer) (r domain.TransferReceipt, err error) {
	if err = l.ensure(ctx, t.To); err != nil {
		return
	}
	err = l.retry(ctx, t.From, func() (e error) { r, e = l.store.Transfer(ctx, t); return })
	return
}

// ------------------------------------------------------------------ drain --

// drain runs the writer over every shard the test has touched until nothing more books. A transfer
// puts an intent on a second shard while the first is being booked, so it takes more than one pass.
func (s *wstack) drain() {
	s.t.Helper()
	ctx := context.Background()
	quiet := 0
	for pass := 0; pass < 400; pass++ {
		total := 0
		for _, sh := range s.shards() {
			if s.relay != nil {
				if err := s.relay.Drain(ctx, sh); err != nil {
					s.t.Fatalf("relay shard %d: %v", sh, err)
				}
			}
			n, err := s.writer.Drain(ctx, sh)
			if err != nil {
				s.t.Fatalf("drain shard %d: %v", sh, err)
			}
			total += n
		}
		if total > 0 {
			quiet = 0
			continue
		}
		// Two empty passes, a few milliseconds apart: an entry left un-acked behind a gap only becomes
		// claimable once it has been idle for AckWait.
		if quiet++; quiet >= 2 && (s.relay == nil || s.settled()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.t.Fatal("the writer did not reach quiescence")
}

// settled reports whether the stream holds nothing un-acknowledged or undelivered on any shard the test
// has touched. It is only asked of JetStream, whose streams are the test's alone; on the shared Redis
// outbox a neighbour's traffic would keep it from ever being true.
func (s *wstack) settled() bool {
	for _, sh := range s.shards() {
		st, err := s.stream.Stats(context.Background(), sh)
		if err != nil || st.Pending != 0 || st.Undelivered != 0 {
			return false
		}
	}
	return true
}

// newJetStreamIntents wires the optional bus: a Relay reads the Redis outbox through a group of its
// own and forwards it, and the writer reads what the relay forwarded.
func (s *wstack) newJetStreamIntents(rdb goredis.UniversalClient) ports.IntentStream {
	s.t.Helper()
	ctx := context.Background()
	nc := s.natsConn()
	io := natsjetstream.IntentsOptions{Clock: s.clock, Prefix: fmt.Sprintf("wjs%d", seq.Add(1)), AckWait: 100 * time.Millisecond, FetchWait: 50 * time.Millisecond}
	s.jsOpts = io
	src, err := redisstreams.NewStream(rdb, redisstreams.IntentOptions{Consumer: "relay", Group: fmt.Sprintf("relay-%d", seq.Add(1)), Clock: s.clock})
	must(s.t, err)
	s.relay, err = natsjetstream.NewRelay(ctx, nc, src, natsjetstream.RelayOptions{Intents: io, AckWait: 100 * time.Millisecond})
	must(s.t, err)
	in, err := natsjetstream.NewIntents(ctx, nc, io)
	must(s.t, err)
	return in.WithRelay(s.relay)
}

func (s *wstack) natsConn() *nats.Conn {
	s.t.Helper()
	nc, err := natsSrv.Connect()
	must(s.t, err)
	s.t.Cleanup(func() { nc.Close() })
	return nc
}

// ----------------------------------------------------------------- probes --

func (s *wstack) booked(sc domain.Scope, p domain.Pool) domain.Credits {
	s.t.Helper()
	st, err := s.books.Booked(context.Background(), sc.Normalized())
	must(s.t, err)
	return st.Booked[p]
}

func (s *wstack) scalar(q string, args ...any) int64 {
	s.t.Helper()
	var n int64
	if err := s.rawPool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		s.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (s *wstack) hasRow(sc domain.Scope, opType string, delta domain.Credits) bool {
	sc = sc.Normalized()
	return s.scalar(`SELECT count(*) FROM credit_ledger WHERE realm=$1 AND scope_kind=$2 AND scope_id=$3 AND operation_type=$4 AND delta=$5`,
		string(sc.Realm), sc.Kind, sc.ID, opType, int64(delta)) > 0
}

func (s *wstack) ledgerSum(realm domain.Realm) domain.Credits {
	return domain.Credits(s.scalar(`SELECT COALESCE(SUM(delta),0)::bigint FROM credit_ledger WHERE realm=$1`, string(realm)))
}

func (s *wstack) inTransit(realm domain.Realm) domain.Credits {
	return domain.Credits(s.scalar(`SELECT COALESCE(SUM(amount),0)::bigint FROM credit_transfers WHERE realm=$1 AND status='in_transit'`, string(realm)))
}

// ---------------------------------------------------------------- the bus --

type recBus struct {
	ports.Bus
	mu   sync.Mutex
	subj []string
}

func (b *recBus) Publish(_ context.Context, subject string, _ []byte) error {
	b.mu.Lock()
	b.subj = append(b.subj, subject)
	b.mu.Unlock()
	return nil
}

func (b *recBus) count(prefix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.subj {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}
