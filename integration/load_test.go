//go:build integration && hardening

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/truongpx396/intel-payment/metering"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	pricingtable "github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// T130: load test against the scale envelope (plan.md § Scale envelope) and SC-001.
//
// WHAT THIS MEASURES AND WHAT IT CANNOT. One machine runs the load generator, Redis, Postgres and (in the
// co-located tests) paymentd and payment-worker, with Redis and Postgres in Docker. A figure here is therefore a
// FLOOR for the design on this machine, taken with the generator competing for the same cores, and says nothing
// about a production primary on its own hardware. What it can show is where the design stops being the
// bottleneck and the box starts. The verdict is stated against the envelope either way; a miss is a design
// finding, recorded, not a number to tune until it passes.

// The envelope's targets, in one place so the verdicts quote them.
const (
	envelopeAdmitsPerSec   = 50_000
	envelopeMutationsPerS  = 20_000
	envelopeOneScopePerSec = 2_000
	envelopeWriterEventsPS = 5_000
	sc001Embedded          = time.Millisecond
	sc001CoLocated         = 5 * time.Millisecond
	envelopePrimaryP99     = time.Millisecond
)

// loadShards is the shard count of the load stacks: the envelope's "64 primaries before shards contend" and one
// Postgres connection per shard a worker owns (the fixture allows 400).
const loadShards = 64

type nopMetrics struct{}

func (nopMetrics) Count(string, int64, ports.Labels)     {}
func (nopMetrics) Gauge(string, int64, ports.Labels)     {}
func (nopMetrics) Observe(string, float64, ports.Labels) {}

// loadStack is one realm on a Redis and a Postgres of its own, reached by the Meter directly (embedded): no proxy,
// no transport, no telemetry exporter.
type loadStack struct {
	t      *testing.T
	realm  domain.Realm
	srv    *redistest.Redis
	dsn    string
	pool   *pgxpool.Pool
	rdb    goredis.UniversalClient
	store  *hotredis.Store
	books  *pgadapter.Books
	meter  ports.Meter
	cfg    metering.Config
	scopes []domain.Scope
	grant  int64
}

const loadGrant int64 = 1 << 40 // never overdrawn: an overdraw books a write-off, which is right and is not what these measure

// loadOpts shape a stack. The zero value is the load tests' own: a plain Redis and no telemetry.
type loadOpts struct {
	redis   redistest.Options // Mode is forced to Standalone
	metrics ports.Metrics     // nil: nopMetrics
	grant   int64             // 0: loadGrant
	short   bool              // keep the outbox at its default bound: a chaos test WANTS backpressure to be real
	shards  int               // 0: loadShards
}

func newLoadStack(t *testing.T, nScopes int) *loadStack {
	return newLoadStackWith(t, nScopes, loadOpts{})
}

func newLoadStackWith(t *testing.T, nScopes int, o loadOpts) *loadStack {
	t.Helper()
	ctx := context.Background()
	s := &loadStack{t: t, realm: domain.Realm(fmt.Sprintf("ld%d", seq.Add(1))), grant: loadGrant}
	if o.grant != 0 {
		s.grant = o.grant
	}
	if o.metrics == nil {
		o.metrics = nopMetrics{}
	}

	var err error
	o.redis.Mode = redistest.Standalone
	s.srv, err = redistest.RunWith(ctx, o.redis)
	must(t, err)
	t.Cleanup(func() { _ = s.srv.Terminate(context.Background()) })
	s.rdb = goredis.NewClient(&goredis.Options{Addr: s.srv.Addr, PoolSize: 512, MinIdleConns: 64, MaxRetries: -1})
	t.Cleanup(func() { _ = s.rdb.Close() })
	_, err = hotredis.EnsureInstalled(ctx, s.rdb)
	must(t, err)

	s.dsn = pg.NewDB(t)
	s.pool, err = pgadapter.Connect(ctx, s.dsn)
	must(t, err)
	t.Cleanup(s.pool.Close)
	s.books = pgadapter.NewBooks(s.pool)
	cfgStore, err := pgadapter.NewConfigStore(ctx, s.pool, pgadapter.ConfigOptions{})
	must(t, err)
	t.Cleanup(cfgStore.Close)
	seedRealmSQL(func(q string, a ...any) {
		t.Helper()
		_, err := s.pool.Exec(ctx, q, a...)
		must(t, err)
	}, string(s.realm))

	s.cfg = metering.Config{Realm: s.realm, BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x", Shards: loadShards,
		// The outbox is bounded (FR-047) and only the worker drains it. The hot-path measurements run WITHOUT a worker so
		// that they measure the primary and not the books; the bound is lifted far above what one run writes. The drain
		// has its own measurement.
		BusMaxLen: 100_000_000}
	if o.short {
		s.cfg.BusMaxLen = 0 // the default bound
	}
	if o.shards != 0 {
		s.cfg.Shards = o.shards
	}
	s.cfg = s.cfg.WithDefaults()
	must(t, s.books.EnsureHotConfig(ctx, s.cfg.Shards))
	s.store, err = hotredis.New(s.rdb, hotredis.Options{Config: s.cfg, Pools: cfgStore, Limits: cfgStore, Clock: system.Clock{}})
	must(t, err)
	deps := app.Deps{
		Pricers: ports.PricerRegistry{"table": pricingtable.New(0)}, Balance: s.store, Books: s.books,
		Limits: cfgStore, Pools: cfgStore, RateCards: cfgStore, Journal: pgadapter.NewJournal(s.pool), Bus: &nullBus{},
		Clock: system.Clock{}, IDs: &system.IDs{}, Metrics: o.metrics,
	}
	s.meter, err = app.New(s.cfg, deps)
	must(t, err)

	opened := map[domain.Shard]bool{}
	for i := 0; i < nScopes; i++ {
		sc := domain.Scope{Realm: s.realm, Kind: "workspace", ID: fmt.Sprintf("ld-%d", i)}
		s.scopes = append(s.scopes, sc)
		if sh := sc.Normalized().Shard(s.cfg.Shards); !opened[sh] {
			rec, err := s.books.EnsureShard(ctx, sh)
			must(t, err)
			must(t, s.store.OpenShard(ctx, sh, rec.Gen))
			opened[sh] = true
		}
		_, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: domain.Credits(s.grant), IdemKey: fmt.Sprintf("g-%d", i), Reason: "purchase"})
		must(t, err)
	}
	return s
}

// event is the same 1,200-in / 800-out call the rest of the suite prices at exactly 660 credits.
func (s *loadStack) event(sc domain.Scope, key string, at time.Time) domain.Event {
	return domain.Event{Scope: sc, Resource: "llm.chat", RateKey: "gpt", IdemKey: key, OccurredAt: at,
		Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}}}
}

// hotSum is the scope's hot balance straight from Redis, not through the Meter under test.
func (s *loadStack) hotSum(sc domain.Scope) domain.Credits {
	s.t.Helper()
	acct, err := s.store.Snapshot(context.Background(), sc)
	must(s.t, err)
	var sum domain.Credits
	for _, v := range acct.Pools {
		sum += v
	}
	return sum
}

// verdict states a measurement against the number the design set. A miss fails the test and is the finding.
func verdict(t *testing.T, id string, ok bool, format string, a ...any) {
	t.Helper()
	word := "MET"
	if !ok {
		word = "MISSED"
	}
	t.Logf("VERDICT %s %s: %s", id, word, fmt.Sprintf(format, a...))
	if !ok {
		t.Errorf("%s MISSED — a design finding, not a tuning task: %s", id, fmt.Sprintf(format, a...))
	}
}

// hotCmds are the two Redis commands the hot functions run under: Admit reads (FCALL_RO), the rest write (FCALL).
var hotCmds = []string{"fcall", "fcall_ro"}

// step is one rung of a ladder: a rate, held, with everything needed to say where the limit was.
type step struct {
	target     float64
	res        loadResult
	calls      int64
	sp50, sp99 time.Duration
	genCores   float64
	redisCores float64
	usecCall   float64
	stalls     string // what the primary itself stalled on during the step (Redis latency monitor, >= 20 ms)
}

// keptUp says whether the generator delivered the rate asked of it. An open-loop run that did not keep up has due
// times that fall further behind every second, so its "latency" is the length of a queue it built itself, not the
// latency of a call: it must never be read as one.
func (st step) keptUp() bool { return st.res.rate() >= 0.98*st.target }

func (st step) log(t *testing.T, id, what string) {
	t.Helper()
	measured(t, id, what, "target", int64(st.target), "rate", int64(st.res.rate()), "kept_up", st.keptUp(), "errs", st.res.errs,
		"client_p50", st.res.pct(0.5), "client_p99", st.res.pct(0.99), "client_p99.9", st.res.pct(0.999),
		"server_calls", st.calls, "server_p50", st.sp50, "server_p99", st.sp99,
		"server_usec_per_call", fmt.Sprintf("%.1f", st.usecCall), "server_stalls", st.stalls, "redis_cores", fmt.Sprintf("%.2f", st.redisCores), "generator_cores", fmt.Sprintf("%.2f", st.genCores))
}

// climb holds each rate for hold, from the closed-loop ceiling (target 0) up the ladder.
func climb(ctx context.Context, t *testing.T, s *loadStack, cmd string, workers int, hold time.Duration, rates []float64, fn func(int, int64) error) []step {
	t.Helper()
	var out []step
	for _, rate := range rates {
		resetServerStats(ctx, t, s.rdb)
		sp := beginSpan(ctx, t, s.rdb)
		res := runLoad(workers, rate, hold, fn)
		gen, rd := sp.end()
		st := step{target: rate, res: res, genCores: gen, redisCores: rd, usecCall: usecPerCall(ctx, t, s.rdb, cmd), stalls: serverStalls(ctx, t, s.rdb)}
		if cmd != "ping" { // the control runs no function, so the function histograms are legitimately empty
			st.calls, st.sp50, st.sp99, _ = serverLatency(ctx, t, s.rdb, hotCmds...)
		}
		out = append(out, st)
		if res.errs != 0 {
			t.Errorf("rate %.0f: %d operations failed", rate, res.errs)
		}
	}
	return out
}

// calibrate separates the design from the box, which a bare number cannot do. Two controls:
//
//   - a no-op (PING) driven through the SAME client, port and concurrency as the Meter: whatever this machine's
//     Docker networking and Go client can deliver for a call that does nothing. The hot path cannot beat it.
//   - redis-benchmark INSIDE the container, over loopback, running the exact command Admit sends: what the primary
//     itself can execute with no VM network in the way and no Go client.
//
// If the Meter reaches the first, the limit is the box. If the second is far above the first, the box is the limit
// and the primary is not. If both fall short of the target, the design is.
func (s *loadStack) calibrate(ctx context.Context, t *testing.T, workers int) (pingRate float64, loopbackRPS string) {
	t.Helper()
	sc := s.scopes[0].Normalized()
	tag := "{s" + strconv.Itoa(int(sc.Shard(s.cfg.Shards))) + "}"
	meta, acct := "meta:"+tag, "acct:"+tag+":"+sc.Tag()

	// The command must succeed before it is benchmarked: a function that refuses at once is fast, and says nothing.
	v, err := s.rdb.FCallRO(ctx, "ip_admit", []string{meta, acct}).Result()
	must(t, err)
	if r, ok := v.([]any); !ok || len(r) == 0 || r[0] != "OK" {
		t.Fatalf("the admit command used for calibration did not return OK: %#v", v)
	}

	ping := runLoad(workers, 0, 5*time.Second, func(int, int64) error { return s.rdb.Ping(ctx).Err() })
	measured(t, "T130", "calibration.ping-through-the-same-port", "workers", workers, "rate", int64(ping.rate()), "errs", ping.errs, "client_p99", ping.pct(0.99))

	run := func(args ...string) string {
		t.Helper()
		cmd := append([]string{"redis-benchmark", "-q", "-c", strconv.Itoa(workers), "-n", "2000000", "--threads", "4"}, args...)
		code, r, err := s.srv.Container.Exec(ctx, cmd, tcexec.Multiplexed())
		must(t, err)
		b, _ := io.ReadAll(r)
		if code != 0 {
			t.Fatalf("redis-benchmark %v exited %d: %s", args, code, b)
		}
		lines := strings.FieldsFunc(string(b), func(r rune) bool { return r == '\r' || r == '\n' })
		return strings.TrimSpace(lines[len(lines)-1])
	}
	pingLoop := run("-t", "ping_mbulk")
	admitLoop := run("FCALL_RO", "ip_admit", "2", meta, acct)
	measured(t, "T130", "calibration.in-container-loopback", "ping", strings.ReplaceAll(pingLoop, " ", "_"), "admit", strings.ReplaceAll(admitLoop, " ", "_"))
	return ping.rate(), admitLoop
}

// warm dials every connection the measured run will use and primes the configuration cache, so the first operations
// of a run are not the ones that pay for a dial. (A run warmed with fewer workers than it then uses reports the
// dials of the rest as its tail.)
func warm(workers int, fn func(int, int64) error) { runLoad(workers, 0, 3*time.Second, fn) }

// ladder holds each rate for hold. It runs the call under test and, at the same rate, a PING through the same
// client and port: what a call that does nothing costs on this machine. What the design adds is the difference.
func ladder(ctx context.Context, t *testing.T, s *loadStack, cmd string, workers int, hold time.Duration, rates []float64, fn func(int, int64) error) (steps, control []step) {
	t.Helper()
	ping := func(int, int64) error { return s.rdb.Ping(ctx).Err() }
	for _, r := range rates {
		steps = append(steps, climb(ctx, t, s, cmd, workers, hold, []float64{r}, fn)...)
		control = append(control, climb(ctx, t, s, "ping", workers, hold, []float64{r}, ping)...)
	}
	return steps, control
}

// echoControl is a second control, with no Docker and no Redis in it: workers exchange one byte with a TCP echo server
// inside this process over 127.0.0.1, at the same rate and worker count. If THIS shows the tail a Docker-fronted
// Redis shows, the tail belongs to the load generator or the operating system, not to the design or to Docker.
func echoControl(t *testing.T, workers int, rate float64, dur time.Duration) loadResult {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	conns := make([]net.Conn, workers)
	for i := range conns {
		conns[i], err = net.Dial("tcp", ln.Addr().String())
		must(t, err)
	}
	res := runLoad(workers, rate, dur, func(w int, _ int64) error {
		var b [1]byte
		if _, err := conns[w].Write(b[:]); err != nil {
			return err
		}
		_, err := io.ReadFull(conns[w], b[:])
		return err
	})
	for _, c := range conns {
		_ = c.Close()
	}
	_ = ln.Close()
	wg.Wait()
	return res
}

// judgeLatencyAtTarget decides a latency criterion measured through a path that is itself part of the machine under
// test. It FAILS only where the call missed a bound that a no-op over the same path, at the same rate, meets: then the
// extra latency is the design's. Where the no-op misses the bound too (or the target rate was not held), the machine
// cannot show the bound and no verdict about the design follows — the test is SKIPPED, loudly and with the numbers,
// because "missed" would blame the design for the machine and "met" would be false.
func judgeLatencyAtTarget(t *testing.T, id string, bound time.Duration, call, control step) {
	t.Helper()
	switch {
	case !call.keptUp() || !control.keptUp():
		t.Skipf("%s NOT ASSESSABLE ON THIS MACHINE: the target load was not held (call %.0f/s, a no-op over the same path %.0f/s of %.0f/s asked)", id, call.res.rate(), control.res.rate(), call.target)
	case control.res.pct(0.99) > bound:
		t.Skipf("%s INCONCLUSIVE ON THIS MACHINE: at %.0f/s the call's p99 was %v, but a no-op over the same path had p99 %v, itself over the %v bound — the path cannot show the bound, so no verdict about the design follows (the primary's own p99 was %v)",
			id, call.res.rate(), call.res.pct(0.99), control.res.pct(0.99), bound, call.sp99)
	default:
		verdict(t, id, call.res.pct(0.99) <= bound, "p99 %v at the target load of %.0f/s against the %v bound; a no-op over the same path was %v", call.res.pct(0.99), call.res.rate(), bound, control.res.pct(0.99))
	}
}

func TestLoadAdmitAgainstTheEnvelope(t *testing.T) { //nolint:paralleltest // a measurement: a parallel test would measure itself
	ctx := context.Background()
	s := newLoadStack(t, 1024)
	admit := func(w int, n int64) error {
		sc := s.scopes[(w*7919+int(n))%len(s.scopes)]
		adm, err := s.meter.Admit(ctx, sc, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1000})
		if err != nil {
			return err
		}
		if !adm.Allowed {
			return fmt.Errorf("a funded scope was refused: %+v", adm)
		}
		return nil
	}
	warm(256, admit)

	// What admission adds with no queue to hide behind: one caller at a time, the Meter's Admit beside a bare PING over
	// the same path. Whatever the PING costs the machine charges; the difference is the design's.
	one := climb(ctx, t, s, "fcall_ro", 1, 5*time.Second, []float64{0}, admit)[0]
	onePing := climb(ctx, t, s, "ping", 1, 5*time.Second, []float64{0}, func(int, int64) error { return s.rdb.Ping(ctx).Err() })[0]
	one.log(t, "T130", "admit.unloaded")
	onePing.log(t, "T130", "ping.unloaded")
	verdict(t, "T130/SC-001-unloaded", one.res.pct(0.99) <= sc001Embedded,
		"UNLOADED (one caller): the Meter's Admit p99 is %v (p50 %v) against SC-001's embedded %v; a bare PING over the same path is p99 %v. This is a lower bound: SC-001 is stated at the target load", one.res.pct(0.99), one.res.pct(0.5), sc001Embedded, onePing.res.pct(0.99))

	ceiling := climb(ctx, t, s, "fcall_ro", 256, 8*time.Second, []float64{0}, admit)[0]
	ceiling.log(t, "T130", "admit.ceiling")
	steps, control := ladder(ctx, t, s, "fcall_ro", 256, 6*time.Second, []float64{10_000, 25_000, 40_000, envelopeAdmitsPerSec}, admit)
	for i := range steps {
		steps[i].log(t, "T130", "admit.ladder")
		control[i].log(t, "T130", "ping.control")
	}
	for _, rate := range []float64{10_000, 25_000, 40_000} {
		e := echoControl(t, 256, rate, 6*time.Second)
		measured(t, "T130", "calibration.in-process-loopback-echo", "target", int64(rate), "rate", int64(e.rate()), "errs", e.errs, "client_p50", e.pct(0.5), "client_p99", e.pct(0.99), "client_p99.9", e.pct(0.999))
	}
	pingRate, loopback := s.calibrate(ctx, t, 256)
	t.Logf("the box: a bare PING through the same client and port tops out at %.0f/s; the Meter's Admit reached %.0f/s (%.0f%% of it)", pingRate, ceiling.res.rate(), 100*ceiling.res.rate()/pingRate)

	// The PRIMARY: executing the exact Admit command in the container over loopback, with no VM network and no Go
	// client in the way — and the time it spent on each call, from its own histogram, at the target rate.
	capacity := benchRate(t, loopback)
	atTarget := steps[len(steps)-1]
	verdict(t, "T130/admit-primary-capacity", capacity >= envelopeAdmitsPerSec, "one Redis primary executes the Admit command at %.0f/s over loopback against a target of %d", capacity, envelopeAdmitsPerSec)
	verdict(t, "T130/admit-primary-p99", atTarget.sp99 <= envelopePrimaryP99, "the primary's own p99 for the admit function is <= %v against a target of %v (mean %.1f µs per call)", atTarget.sp99, envelopePrimaryP99, atTarget.usecCall)

	// SC-001 is a latency at the TARGET load; judged only where the machine can show it.
	judgeLatencyAtTarget(t, "T130/SC-001-embedded", sc001Embedded, atTarget, control[len(control)-1])
}

// benchRate reads the requests-per-second out of a redis-benchmark -q line ("<name>: 114025.08 requests per second, p50=...").
func benchRate(t *testing.T, line string) float64 {
	t.Helper()
	_, rest, ok := strings.Cut(line, ": ")
	if !ok {
		t.Fatalf("not a redis-benchmark line: %q", line)
	}
	f, err := strconv.ParseFloat(strings.Fields(rest)[0], 64)
	if err != nil {
		t.Fatalf("redis-benchmark rate in %q: %v", line, err)
	}
	return f
}

// recordLoad is a Record workload with an oracle. A fraction of operations resend the previous request EXACTLY (same
// key, same occurred_at): each must come back as a replay that charges nothing. Every other operation is a fresh key
// whose receipt's Delta is added to its scope's running total, which the hot balance must equal at the end — the
// exactly-once property (SC-002) measured under load rather than argued.
type recordLoad struct {
	s        *loadStack
	scopes   []domain.Scope
	spent    []atomic.Int64 // per scope: the sum of Deltas the caller was told were applied
	applied  atomic.Int64
	replays  atomic.Int64
	doubled  atomic.Int64 // a replay that charged again: must stay 0
	deferred atomic.Int64
	prefix   string
	keys     atomic.Int64 // one counter for the whole load, so a later run never reuses an earlier run's key
	last     [][]recorded // per worker, the last request it sent
}

type recorded struct {
	ev    domain.Event
	scope int
}

func newRecordLoad(s *loadStack, scopes []domain.Scope, workers int, prefix string) *recordLoad {
	return &recordLoad{s: s, scopes: scopes, spent: make([]atomic.Int64, len(scopes)), prefix: prefix, last: make([][]recorded, workers)}
}

func (r *recordLoad) op(w int, n int64) error {
	ctx := context.Background()
	if len(r.last[w]) > 0 && n%50 == 49 { // 2% retries, as a flaky client or a timeout would produce
		prev := r.last[w][len(r.last[w])-1]
		rc, err := r.s.meter.Record(ctx, prev.ev)
		if err != nil {
			return err
		}
		if rc.Applied || rc.Delta != 0 {
			r.doubled.Add(1)
		}
		r.replays.Add(1)
		return nil
	}
	i := (w*7919 + int(n)) % len(r.scopes)
	ev := r.s.event(r.scopes[i], fmt.Sprintf("%s-%d", r.prefix, r.keys.Add(1)), time.Now())
	rc, err := r.s.meter.Record(ctx, ev)
	if err != nil {
		return err
	}
	if rc.Deferred {
		r.deferred.Add(1)
		return fmt.Errorf("a record was deferred to the journal: the hot tier refused it")
	}
	if !rc.Applied {
		return fmt.Errorf("a fresh key came back as a replay")
	}
	r.spent[i].Add(int64(rc.Delta))
	r.applied.Add(1)
	r.last[w] = append(r.last[w][:0], recorded{ev: ev, scope: i})
	return nil
}

// check asserts the oracle: no replay charged, nothing deferred, and each scope's hot balance is its grant plus
// exactly what it was told it spent.
func (r *recordLoad) check(t *testing.T) {
	t.Helper()
	if n := r.doubled.Load(); n != 0 {
		t.Errorf("SC-002 violated under load: %d replays charged a second time", n)
	}
	if n := r.deferred.Load(); n != 0 {
		t.Errorf("%d records were deferred to the journal during a run with a healthy hot tier", n)
	}
	var wrong int
	for i, sc := range r.scopes {
		if want, got := domain.Credits(r.s.grant+r.spent[i].Load()), r.s.hotSum(sc); got != want {
			wrong++
			if wrong <= 3 {
				t.Errorf("scope %s: hot balance %d, but its grant plus every applied receipt is %d (off by %d)", sc.ID, got, want, got-want)
			}
		}
	}
	if wrong > 0 {
		t.Errorf("%d of %d scopes disagree with what their callers were told", wrong, len(r.scopes))
	}
}

func TestLoadRecordAgainstTheEnvelope(t *testing.T) { //nolint:paralleltest // a measurement: a parallel test would measure itself
	ctx := context.Background()
	s := newLoadStack(t, 1024)

	many := newRecordLoad(s, s.scopes, 256, "many")
	warm(256, many.op)
	ceiling := climb(ctx, t, s, "fcall", 256, 8*time.Second, []float64{0}, many.op)[0]
	ceiling.log(t, "T130", "record.ceiling")
	steps, control := ladder(ctx, t, s, "fcall", 256, 6*time.Second, []float64{5_000, 10_000, 20_000}, many.op)
	for i := range steps {
		steps[i].log(t, "T130", "record.ladder")
		control[i].log(t, "T130", "ping.control")
	}
	many.check(t)
	measured(t, "T130", "record.exactly-once", "applied", many.applied.Load(), "replays_sent", many.replays.Load(), "replays_that_charged", many.doubled.Load())

	atTarget := steps[len(steps)-1]
	// The primary's capacity. If the closed-loop ceiling had Redis's thread (nearly) saturated, the ceiling IS what the
	// primary can do and the figure is measured. If something else limited the run first, the most that can be said is a
	// lower bound, and the verdict is worded as one. (The Admit command could be replayed inside the container; this
	// one, with a dozen arguments, a payload and a fingerprint, was not worth reconstructing.)
	const saturated = 0.85
	capacity, how := ceiling.res.rate(), "measured: Redis's thread was saturated at the closed-loop ceiling"
	if ceiling.redisCores < saturated {
		capacity, how = ceiling.res.rate()/ceiling.redisCores, "extrapolated from a run that did not saturate Redis's thread, so a LOWER bound only on the machine's limit"
	}
	measured(t, "T130", "record.primary-capacity", "ceiling_rate", int64(ceiling.res.rate()), "redis_cores_at_ceiling", fmt.Sprintf("%.2f", ceiling.redisCores),
		"capacity", int64(capacity), "basis", strings.ReplaceAll(how, " ", "_"), "calls_per_record", fmt.Sprintf("%.2f", float64(ceiling.calls)/float64(ceiling.res.ops)))
	verdict(t, "T130/record-primary-p99", atTarget.sp99 <= envelopePrimaryP99, "the primary's own p99 for the mutating FCALL is <= %v against a target of %v at %.0f mutations/s (mean %.1f µs per call; stalls: %s)", atTarget.sp99, envelopePrimaryP99, atTarget.res.rate(), atTarget.usecCall, atTarget.stalls)
	verdict(t, "T130/record-primary-capacity", capacity >= envelopeMutationsPerS, "one primary sustained %.0f mutations/s (%s) against a target of %d", capacity, how, envelopeMutationsPerS)

	// One scope: "a scope is never split across slots", so a hot tenant is bounded by one slot's single thread.
	one := newRecordLoad(s, s.scopes[:1], 64, "one")
	warm(64, one.op)
	oneCeil := climb(ctx, t, s, "fcall", 64, 8*time.Second, []float64{0}, one.op)[0]
	oneCeil.log(t, "T130", "record.one-scope.ceiling")
	oneAt := climb(ctx, t, s, "fcall", 64, 8*time.Second, []float64{envelopeOneScopePerSec}, one.op)[0]
	oneAt.log(t, "T130", "record.one-scope.at-target")
	one.spent[0].Add(many.spent[0].Load()) // the many-scope run charged scope 0 too: the oracle spans both
	one.check(t)
	verdict(t, "T130/one-scope", oneAt.keptUp() && oneCeil.res.rate() >= envelopeOneScopePerSec, "one scope held %.0f/s at a target of %d and peaked at %.0f/s", oneAt.res.rate(), envelopeOneScopePerSec, oneCeil.res.rate())

	if !atTarget.keptUp() || !control[len(control)-1].keptUp() {
		t.Skipf("the %d mutations/s target was not held through this machine's path (Record %.0f/s, a bare PING %.0f/s): the primary-side verdicts above stand, an end-to-end rate verdict would be about the machine",
			envelopeMutationsPerS, atTarget.res.rate(), control[len(control)-1].res.rate())
	}
	verdict(t, "T130/record-rate", atTarget.keptUp(), "sustained %.0f mutations/s through the Meter against a target of %d", atTarget.res.rate(), envelopeMutationsPerS)
}

// pagedOutbox is how much the outbox holds, summed over the stack's shards. (Redis counts stream length per key.)
func (s *loadStack) outboxLen(ctx context.Context) int64 {
	var total int64
	for sh := 0; sh < s.cfg.Shards; sh++ {
		n, err := s.rdb.XLen(ctx, hotredis.OutboxKey(domain.Shard(sh))).Result()
		if err == nil {
			total += n
		}
	}
	return total
}

// TestLoadTheWriterDrainsAgainstTheEnvelope measures the durable writer: the real payment-worker binary, booking
// into the real Postgres what the hot tier accepted. The outbox is filled first (the hot-path measurements above
// say how fast that can go), then the worker is started and timed until the books hold every event. The target is
// "≥ 5 000 events/s sustained, batched (DrainBatch 500)" at event granularity.
func TestLoadTheWriterDrainsAgainstTheEnvelope(t *testing.T) { //nolint:paralleltest // a measurement: a parallel test would measure itself
	ctx := context.Background()
	s := newLoadStack(t, 1024)
	load := newRecordLoad(s, s.scopes, 256, "drain")
	warm(256, load.op)
	runLoad(256, 0, 4*time.Second, load.op)
	load.check(t) // the hot tier is right before the writer is asked to agree with it

	want := int(1024 + load.applied.Load()) // one grant per scope, plus every record that was applied (replays book nothing)
	pending := s.outboxLen(ctx)
	if pending < int64(want) {
		t.Fatalf("the outbox holds %d intents but the books should receive %d", pending, want)
	}
	measured(t, "T130", "writer.backlog", "events", want, "outbox_entries", pending)

	env := workerEnv(string(s.realm), s.dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+s.srv.Addr+"/0", "PAYMENT_SHARDS", strconv.Itoa(loadShards),
		"PAYMENT_DRAIN_BATCH", "500", "PAYMENT_DRAIN_INTERVAL", "20ms", "PAYMENT_BUS_ACK_WAIT", "60s", "PAYMENT_LOG_LEVEL", "warn")
	started := time.Now()
	w := startWorker(t, env)

	rows := func() int {
		var n int
		must(t, s.pool.QueryRow(ctx, `SELECT count(*) FROM credit_ledger WHERE realm = $1`, string(s.realm)).Scan(&n))
		return n
	}
	type sample struct {
		at   time.Duration
		rows int
	}
	var samples []sample
	deadline := time.Now().Add(6 * time.Minute)
	for {
		n := rows()
		samples = append(samples, sample{time.Since(started), n})
		if n >= want {
			break
		}
		select {
		case <-w.exited:
			t.Fatalf("the worker exited (%v) with %d of %d events booked; logs:\n%s", w.err, n, want, w.logs.String())
		case <-time.After(250 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 6 minutes only %d of %d events were booked", n, want)
		}
	}
	total := time.Since(started)

	// The steady state: from 10% to 90% booked, so neither the worker's start-up nor its tail is in the rate.
	var lo, hi sample
	for _, sm := range samples {
		if lo.at == 0 && sm.rows >= want/10 {
			lo = sm
		}
		if sm.rows <= want*9/10 {
			hi = sm
		}
	}
	steady := float64(hi.rows-lo.rows) / (hi.at - lo.at).Seconds()
	overall := float64(want) / total.Seconds()
	var firstRow time.Duration // how long the worker took to book its first event: its start-up, which "sustained" does not include
	for _, sm := range samples {
		if sm.rows > 0 {
			firstRow = sm.at
			break
		}
	}
	measured(t, "T130", "writer.drain", "events", want, "seconds", fmt.Sprintf("%.1f", total.Seconds()), "overall_per_s", int64(overall), "steady_per_s", int64(steady),
		"first_row_after", firstRow.Round(time.Millisecond), "shards", loadShards, "batch", 500)

	// The plan's storage rule of thumb ("≈ 400 bytes per event row including indexes — to be measured"): the ledger's
	// partitions, indexes and TOAST included, over the rows the drain booked.
	var ledgerBytes int64
	must(t, s.pool.QueryRow(ctx, `SELECT coalesce(sum(pg_total_relation_size(inhrelid)), 0) FROM pg_inherits WHERE inhparent = 'credit_ledger'::regclass`).Scan(&ledgerBytes))
	measured(t, "T130", "storage.credit_ledger", "rows", want, "bytes_total", ledgerBytes, "bytes_per_row", ledgerBytes/int64(want))

	// Exactly once, and equal to the hot tier, scope by scope.
	var distinct, dupes int
	must(t, s.pool.QueryRow(ctx, `SELECT count(DISTINCT idem_key), count(*) - count(DISTINCT (scope_id, idem_key)) FROM credit_ledger WHERE realm = $1`, string(s.realm)).Scan(&distinct, &dupes))
	if dupes != 0 {
		t.Errorf("%d ledger rows share an idempotency key with another: something was booked twice", dupes)
	}
	books := map[string]int64{}
	rs, err := s.pool.Query(ctx, `SELECT scope_id, sum(delta) FROM credit_ledger WHERE realm = $1 GROUP BY scope_id`, string(s.realm))
	must(t, err)
	for rs.Next() {
		var id string
		var sum int64
		must(t, rs.Scan(&id, &sum))
		books[id] = sum
	}
	must(t, rs.Err())
	var wrong int
	for _, sc := range s.scopes {
		if hot := int64(s.hotSum(sc)); books[sc.ID] != hot {
			wrong++
			if wrong <= 3 {
				t.Errorf("scope %s: the books sum to %d and the hot tier holds %d", sc.ID, books[sc.ID], hot)
			}
		}
	}
	if wrong != 0 {
		t.Errorf("%d of %d scopes: the books disagree with the hot tier after the drain", wrong, len(s.scopes))
	}
	verdict(t, "T130/writer-rate", steady >= envelopeWriterEventsPS, "the writer booked %.0f events/s between 10%% and 90%% booked; the whole drain, including %v of start-up, averaged %.0f/s (%d events in %.1fs); target >= %d, event granularity, batch 500",
		steady, firstRow.Round(time.Millisecond), overall, want, total.Seconds(), envelopeWriterEventsPS)
}

// TestLoadAdmitCoLocatedThroughPaymentd is SC-001's co-located half: the host calls paymentd, on the same machine,
// over gRPC with a bearer credential — the real binary, with its telemetry, authentication and Redis pool — and the
// admission must add no more than 5 ms at p99 at the target load.
func TestLoadAdmitCoLocatedThroughPaymentd(t *testing.T) { //nolint:paralleltest // a measurement: a parallel test would measure itself
	ctx := context.Background()
	s := newLoadStack(t, 1024)
	d := startPaymentd(t, paymentdEnv(string(s.realm), s.dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+s.srv.Addr+"/0", "PAYMENT_SHARDS", strconv.Itoa(loadShards), "PAYMENT_LOG_LEVEL", "info"))
	awaitReady(t, d, &http.Client{Timeout: 10 * time.Second}, d.httpURL(t))

	var clients []*grpcclient.Client
	for i := 0; i < 4; i++ { // one HTTP/2 connection multiplexes, but a single one is its own ceiling
		g, err := grpc.NewClient(d.listening(t, "grpc"), grpc.WithTransportCredentials(insecure.NewCredentials()), grpcclient.WithBearer(serviceToken, grpcclient.OverInsecureTransport()))
		must(t, err)
		t.Cleanup(func() { _ = g.Close() })
		clients = append(clients, grpcclient.New(g))
	}
	admit := func(w int, n int64) error {
		sc := s.scopes[(w*7919+int(n))%len(s.scopes)]
		adm, err := clients[w%len(clients)].Admit(ctx, sc, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1000})
		if err != nil {
			return err
		}
		if !adm.Allowed {
			return fmt.Errorf("a funded scope was refused: %+v", adm)
		}
		return nil
	}
	warm(128, admit)

	one := climb(ctx, t, s, "fcall_ro", 1, 5*time.Second, []float64{0}, admit)[0]
	one.log(t, "T130", "paymentd.admit.unloaded")
	verdict(t, "T130/SC-001-co-located-unloaded", one.res.pct(0.99) <= sc001CoLocated,
		"UNLOADED (one caller): paymentd's gRPC Admit p99 is %v (p50 %v) against SC-001's co-located %v. This is a lower bound: SC-001 is stated at the target load", one.res.pct(0.99), one.res.pct(0.5), sc001CoLocated)

	ceiling := climb(ctx, t, s, "fcall_ro", 128, 8*time.Second, []float64{0}, admit)[0]
	ceiling.log(t, "T130", "paymentd.admit.ceiling")
	steps := climb(ctx, t, s, "fcall_ro", 128, 6*time.Second, []float64{5_000, 10_000, 20_000, 30_000, envelopeAdmitsPerSec}, admit)
	for _, st := range steps {
		st.log(t, "T130", "paymentd.admit.ladder")
	}
	atTarget := steps[len(steps)-1]
	ping := func(int, int64) error { return s.rdb.Ping(ctx).Err() }
	control := climb(ctx, t, s, "ping", 128, 6*time.Second, []float64{envelopeAdmitsPerSec}, ping)[0]
	control.log(t, "T130", "ping.control")
	judgeLatencyAtTarget(t, "T130/SC-001-co-located", sc001CoLocated, atTarget, control)
}
