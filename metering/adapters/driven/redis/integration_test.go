//go:build integration

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/metering"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Two real Redis instances for the whole package: a standalone primary and a cluster-mode node.
// The hot path must behave identically on both, and only the cluster refuses a function that
// touches two hash slots — that is the constraint the key layout exists to satisfy (FR-017f).
var servers = map[redistest.Mode]*redistest.Redis{}

func TestMain(m *testing.M) {
	ctx := context.Background()
	for _, mode := range []redistest.Mode{redistest.Standalone, redistest.Cluster} {
		r, err := redistest.Run(ctx, mode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "redistest:", err)
			os.Exit(1)
		}
		servers[mode] = r
		rdb := r.Client()
		if _, err := hotredis.EnsureInstalled(ctx, rdb); err != nil {
			fmt.Fprintln(os.Stderr, "install hot path:", err)
			os.Exit(1)
		}
		_ = rdb.Close()
	}
	code := m.Run()
	for _, r := range servers {
		_ = r.Terminate(ctx)
	}
	if code == 0 {
		if err := goleak.Find(
			// testcontainers' Ryuk reaper and the docker client keep connections alive by design.
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

var epoch = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// healing wraps the raw store with what the Meter does in production: a shard that has never been
// opened is opened (the worker does this at start), and an account that is new or stale is
// rehydrated from the books — here, an empty book at generation 1. It lets the SAME suite that the
// full system will run exercise the adapter without a database.
type healing struct {
	*hotredis.Store
	cfg metering.Config
}

func (h healing) ensure(ctx context.Context, scopes ...domain.Scope) error {
	for _, s := range scopes {
		sh := s.Normalized().Shard(h.cfg.Shards)
		st, err := h.Store.ShardState(ctx, sh)
		if err != nil {
			return err
		}
		if !st.Open {
			if err := h.Store.OpenShard(ctx, sh, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h healing) retry(ctx context.Context, s domain.Scope, op func() error) error {
	if err := h.ensure(ctx, s); err != nil {
		return err
	}
	err := op()
	if errors.Is(err, domain.ErrColdScope) {
		if _, rerr := h.Store.Rehydrate(ctx, s, ports.RehydrateState{ShardGen: 1}); rerr != nil {
			return rerr
		}
		err = op()
	}
	return err
}

func (h healing) Balance(ctx context.Context, s domain.Scope) (m map[domain.Pool]domain.Credits, err error) {
	err = h.retry(ctx, s, func() (e error) { m, e = h.Store.Balance(ctx, s); return })
	return
}

func (h healing) Admit(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (a domain.Admission, err error) {
	err = h.retry(ctx, s, func() (e error) { a, e = h.Store.Admit(ctx, s, req); return })
	return
}

func (h healing) Debit(ctx context.Context, c domain.Charge) (r domain.Receipt, err error) {
	err = h.retry(ctx, c.Scope, func() (e error) { r, e = h.Store.Debit(ctx, c); return })
	return
}

func (h healing) Grant(ctx context.Context, g domain.Grant) (r domain.Receipt, err error) {
	err = h.retry(ctx, g.Scope, func() (e error) { r, e = h.Store.Grant(ctx, g); return })
	return
}

func (h healing) Transfer(ctx context.Context, t domain.Transfer) (r domain.TransferReceipt, err error) {
	if err = h.ensure(ctx, t.To); err != nil {
		return
	}
	err = h.retry(ctx, t.From, func() (e error) { r, e = h.Store.Transfer(ctx, t); return })
	return
}

func newStore(t *testing.T, rdb goredis.UniversalClient, clock ports.Clock, cfg metering.Config, o contracts.LedgerOptions) *hotredis.Store {
	t.Helper()
	cfg.NegativeBalance = o.Policy
	if cfg.NegativeBalance == "" {
		cfg.NegativeBalance = metering.ClampToZero
	}
	s, err := hotredis.New(rdb, hotredis.Options{
		Config: cfg, Pools: contracts.StaticPools(o.Pools), Limits: contracts.StaticLimits(o.Limits), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func harness(mode redistest.Mode) func(t *testing.T, o contracts.LedgerOptions) contracts.LedgerHarness {
	return func(t *testing.T, o contracts.LedgerOptions) contracts.LedgerHarness {
		t.Helper()
		rdb := servers[mode].Client()
		t.Cleanup(func() { _ = rdb.Close() })
		now := o.Now
		if now.IsZero() {
			now = epoch
		}
		clock := contracts.NewFakeClock(now)
		cfg := metering.Config{}.WithDefaults()
		return contracts.LedgerHarness{
			Ledger: healing{Store: newStore(t, rdb, clock, cfg, o), cfg: cfg},
			Clock:  clock,
		}
	}
}

func TestLedgerContract_Standalone(t *testing.T) {
	t.Parallel()
	contracts.LedgerContract(t, harness(redistest.Standalone))
}

// The same suite on Redis Cluster: a script that touched two slots would be refused here.
func TestLedgerContract_Cluster(t *testing.T) {
	t.Parallel()
	contracts.LedgerContract(t, harness(redistest.Cluster))
}

func TestLibraryInstallation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for mode, srv := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			rdb := srv.Client()
			defer func() { _ = rdb.Close() }()
			if err := hotredis.CheckInstalled(ctx, rdb); err != nil {
				t.Fatal(err)
			}
			if changed, err := hotredis.EnsureInstalled(ctx, rdb); err != nil || changed {
				t.Fatalf("installing the same library twice must change nothing: %v %v", changed, err)
			}
		})
	}
}

func TestShardLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			rdb := servers[mode].Client()
			defer func() { _ = rdb.Close() }()
			clock := contracts.NewFakeClock(epoch)
			// This test bumps and freezes a shard, which would make every other test's account in
			// that shard stale. Every other test hashes into shards 0-255, so this one uses 16384
			// shards and a scope that lands above them: it owns its shard outright.
			cfg := metering.Config{Shards: 16384}.WithDefaults()
			st := newStore(t, rdb, clock, cfg, contracts.LedgerOptions{})
			var s domain.Scope
			for i := 0; ; i++ {
				s = domain.Scope{Realm: "life", Kind: "workspace", ID: fmt.Sprintf("%d-%d", time.Now().UnixNano(), i)}
				if s.Shard(cfg.Shards) >= 256 {
					break
				}
			}
			sh := s.Shard(cfg.Shards)

			// A never-opened shard is COLD_SHARD: the hot tier refuses rather than guessing a balance.
			if state, err := st.ShardState(ctx, sh); err != nil || state.Open {
				t.Fatalf("a fresh shard is not open: %+v %v", state, err)
			}
			if _, err := st.Grant(ctx, domain.Grant{Scope: s, Amount: 1, IdemKey: "g", Reason: "promo"}); !errors.Is(err, domain.ErrColdShard) {
				t.Fatalf("want ErrColdShard, got %v", err)
			}
			if err := st.OpenShard(ctx, sh, 1); err != nil {
				t.Fatal(err)
			}
			// Open is idempotent and never lowers or replaces a live generation.
			if err := st.OpenShard(ctx, sh, 7); err != nil {
				t.Fatal(err)
			}
			if state, _ := st.ShardState(ctx, sh); !state.Open || state.Gen != 1 || state.Frozen {
				t.Fatalf("%+v", state)
			}

			// A new account in an open shard is COLD_SCOPE until it is rehydrated from the books.
			if _, err := st.Grant(ctx, domain.Grant{Scope: s, Amount: 1, IdemKey: "g", Reason: "promo"}); !errors.Is(err, domain.ErrColdScope) {
				t.Fatalf("want ErrColdScope, got %v", err)
			}
			outcome, err := st.Rehydrate(ctx, s, ports.RehydrateState{ShardGen: 1, AppliedSeq: 41, Pools: map[domain.Pool]domain.Credits{"general": 500, "promo": 25}})
			if err != nil || outcome != ports.RehydrateInstalled {
				t.Fatalf("%v %v", outcome, err)
			}
			// Concurrent callers converge: the second finds it already installed and changes nothing.
			if outcome, _ = st.Rehydrate(ctx, s, ports.RehydrateState{ShardGen: 1, AppliedSeq: 0, Pools: map[domain.Pool]domain.Credits{"general": 999}}); outcome != ports.RehydrateAlready {
				t.Fatalf("second rehydrate: %v", outcome)
			}
			acct, err := st.Snapshot(ctx, s)
			if err != nil || acct.Cold || acct.Seq != 41 || acct.Pools["general"] != 500 || acct.Pools["promo"] != 25 {
				t.Fatalf("the hot seq must RESUME at applied_seq so no number is reissued: %+v %v", acct, err)
			}
			// The next mutation takes seq 42.
			r, err := st.Grant(ctx, domain.Grant{Scope: s, Amount: 5, IdemKey: "g2", Reason: "promo"})
			if err != nil || r.Seq != 42 {
				t.Fatalf("%+v %v", r, err)
			}

			// Freeze: every mutation is refused with FROZEN and changes nothing.
			if err := st.FreezeShard(ctx, sh, "test"); err != nil {
				t.Fatal(err)
			}
			if state, _ := st.ShardState(ctx, sh); !state.Frozen || state.Reason != "test" {
				t.Fatalf("%+v", state)
			}
			if _, err := st.Debit(ctx, domain.Charge{Scope: s, Amount: 1, IdemKey: "d"}); !errors.Is(err, domain.ErrShardFrozen) {
				t.Fatalf("want ErrShardFrozen, got %v", err)
			}
			if _, err := st.Admit(ctx, s, domain.AdmitRequest{}); !errors.Is(err, domain.ErrShardFrozen) {
				t.Fatalf("a frozen shard refuses admission too: %v", err)
			}
			if err := st.UnfreezeShard(ctx, sh); err != nil {
				t.Fatal(err)
			}

			// Bump: every account in the shard is stale at once, without a scan. Bumping to a
			// generation the shard already has is a no-op, so a retried recovery is safe.
			if err := st.BumpShard(ctx, sh, 2); err != nil {
				t.Fatal(err)
			}
			if err := st.BumpShard(ctx, sh, 2); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Debit(ctx, domain.Charge{Scope: s, Amount: 1, IdemKey: "after-bump"}); !errors.Is(err, domain.ErrColdScope) {
				t.Fatalf("a bump makes every account stale: %v", err)
			}
			// A rehydrate built against the OLD generation is refused — the books were read too early.
			if outcome, _ := st.Rehydrate(ctx, s, ports.RehydrateState{ShardGen: 1}); outcome != ports.RehydrateStaleGen {
				t.Fatalf("want STALE_GEN, got %v", outcome)
			}
			if outcome, _ := st.Rehydrate(ctx, s, ports.RehydrateState{ShardGen: 2, AppliedSeq: 42, Pools: map[domain.Pool]domain.Credits{"general": 505}}); outcome != ports.RehydrateInstalled {
				t.Fatalf("rehydrate at the new generation: %v", outcome)
			}
			if err := st.Ping(ctx); err != nil {
				t.Fatal(err)
			}
			if id, err := st.NodeID(ctx, sh); err != nil || len(id) != 40 {
				t.Fatalf("a replication id is 40 hex chars: %q %v", id, err)
			}
		})
	}
}

func TestHealIsACompareAndSetOnSeq(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := servers[redistest.Standalone].Client()
	defer func() { _ = rdb.Close() }()
	cfg := metering.Config{}.WithDefaults()
	raw := newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{})
	l := healing{Store: raw, cfg: cfg}
	s := domain.Scope{Realm: "heal", Kind: "workspace", ID: fmt.Sprint(time.Now().UnixNano())}
	r, err := l.Grant(ctx, domain.Grant{Scope: s, Amount: 100, IdemKey: "g", Reason: "promo"})
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent charge lands after drift was measured: the heal must not overwrite it.
	if _, err := l.Debit(ctx, domain.Charge{Scope: s, Amount: 30, IdemKey: "racer"}); err != nil {
		t.Fatal(err)
	}
	ok, err := raw.Heal(ctx, s, r.Seq, map[domain.Pool]domain.Credits{"general": 999})
	if err != nil || ok {
		t.Fatalf("a heal against a stale seq must lose the race, not overwrite: %v %v", ok, err)
	}
	if b, _ := l.Balance(ctx, s); b["general"] != 70 {
		t.Fatalf("the racing charge was clobbered: %v", b)
	}
	acct, _ := raw.Snapshot(ctx, s)
	ok, err = raw.Heal(ctx, s, acct.Seq, map[domain.Pool]domain.Credits{"general": 65})
	if err != nil || !ok {
		t.Fatalf("a heal at the current seq applies: %v %v", ok, err)
	}
	if b, _ := l.Balance(ctx, s); b["general"] != 65 {
		t.Fatalf("%v", b)
	}
}

// Backpressure is refused loudly. An intent is never dropped and the stream never trimmed to make
// room; the Meter journals the charge instead (invariant 16).
func TestBackpressureRefusesAndChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := servers[redistest.Standalone].Client()
	defer func() { _ = rdb.Close() }()
	cfg := metering.Config{BusMaxLen: 3}.WithDefaults()
	raw := newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{})
	l := healing{Store: raw, cfg: cfg}
	s := domain.Scope{Realm: "bp", Kind: "workspace", ID: fmt.Sprint(time.Now().UnixNano())}
	sh := s.Shard(cfg.Shards)

	before, _ := rdb.XLen(ctx, "billing:outbox:{s"+fmt.Sprint(int(sh))+"}").Result()
	var refused error
	for i := 0; i < 10 && refused == nil; i++ {
		_, refused = l.Grant(ctx, domain.Grant{Scope: s, Amount: 10, IdemKey: fmt.Sprintf("g%d", i), Reason: "promo"})
	}
	if !errors.Is(refused, domain.ErrBackpressure) {
		t.Fatalf("a full outbox must refuse with BACKPRESSURE, got %v", refused)
	}
	if !domain.IsHotUnavailable(refused) {
		t.Fatal("…and the Meter must treat that as 'journal it', not as a failure")
	}
	// No intent beyond the cap, and nothing was trimmed.
	if n, _ := rdb.XLen(ctx, "billing:outbox:{s"+fmt.Sprint(int(sh))+"}").Result(); n-before > 3 {
		t.Fatalf("the stream grew past its cap: %d", n-before)
	}
}

// HotAckWait: with aof_local the call returns only after the write is fsynced on the primary.
func TestHotAckWaitAOFLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			rdb := servers[mode].Client()
			defer func() { _ = rdb.Close() }()
			// With appendfsync everysec a WAITAOF waits for the next fsync tick — up to ~1 s (measured) — so the
			// record timeout must allow for it; with appendfsync always it returns at once.
			cfg := metering.Config{HotAckWait: metering.HotAckAOFLocal, RecordTimeout: 3 * time.Second}.WithDefaults()
			l := healing{Store: newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{}), cfg: cfg}
			s := domain.Scope{Realm: "ack", Kind: "workspace", ID: fmt.Sprint(time.Now().UnixNano())}
			r, err := l.Grant(ctx, domain.Grant{Scope: s, Amount: 10, IdemKey: "g", Reason: "promo"})
			if err != nil || !r.Applied {
				t.Fatalf("WAITAOF 1 0 is satisfiable on a primary with AOF on: %+v %v", r, err)
			}
		})
	}
}

// aof_replica cannot be satisfied with no replica: the operation HAS applied, but the caller is
// not told it is durable. It gets ErrHotStoreUnavailable, so the Meter journals it — and the replay
// through the hot guard is a REPLAY, so nothing is charged twice.
func TestHotAckWaitAOFReplicaWithoutAReplicaIsNotAcknowledged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := servers[redistest.Standalone].Client()
	defer func() { _ = rdb.Close() }()
	cfg := metering.Config{HotAckWait: metering.HotAckAOFReplica, RecordTimeout: 300 * time.Millisecond}.WithDefaults()
	strict := healing{Store: newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{}), cfg: cfg}
	s := domain.Scope{Realm: "ack", Kind: "workspace", ID: fmt.Sprint(time.Now().UnixNano())}
	g := domain.Grant{Scope: s, Amount: 10, IdemKey: "g", Reason: "promo"}
	_, err := strict.Grant(ctx, g)
	if !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("an unconfirmed durable wait must not be reported as success: %v", err)
	}
	// The write went through; a lenient ledger sees the guard and replays it.
	lenient := healing{Store: newStore(t, rdb, contracts.NewFakeClock(epoch), metering.Config{}.WithDefaults(), contracts.LedgerOptions{}), cfg: metering.Config{}.WithDefaults()}
	r, err := lenient.Grant(ctx, g)
	if err != nil || r.Applied {
		t.Fatalf("the replay must be REPLAY, not a second mint: %+v %v", r, err)
	}
	if b, _ := lenient.Balance(ctx, s); b["general"] != 10 {
		t.Fatalf("minted more than once: %v", b)
	}
}

// The adapter must work as the SERVICE ROLE of deploy/redis/users.acl — the default user off, no
// FUNCTION LOAD, no CONFIG, only the commands the hot functions call on the caller's behalf. An ACL
// applies inside a function, so a command the adapter (or a function) uses that the ACL forgot fails
// here rather than in production: this test is the ACL's specification.
func TestAdapterWorksAsTheServiceRoleOfTheDeployACL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, err := redistest.RunWith(ctx, redistest.Options{Mode: redistest.Standalone, ACLFile: "../../../../deploy/redis/users.acl"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Terminate(context.Background()) })

	deploy := goredis.NewClient(&goredis.Options{Addr: srv.Addr, Username: "deploy", Password: "dev-deploy"})
	defer func() { _ = deploy.Close() }()
	svc := goredis.NewClient(&goredis.Options{Addr: srv.Addr, Username: "payment", Password: "dev-payment"})
	defer func() { _ = svc.Close() }()
	anon := goredis.NewClient(&goredis.Options{Addr: srv.Addr})
	defer func() { _ = anon.Close() }()

	if err := anon.Ping(ctx).Err(); err == nil {
		t.Fatal("a host has no credential: the default user must be off")
	}
	if _, err := hotredis.EnsureInstalled(ctx, svc); err == nil {
		t.Fatal("the service role must NOT be able to install code into the money store")
	}
	if changed, err := hotredis.EnsureInstalled(ctx, deploy); err != nil || !changed {
		t.Fatalf("the deploy role installs the library: %v %v", changed, err)
	}
	if err := hotredis.CheckInstalled(ctx, svc); err != nil {
		t.Fatalf("the service role can verify it: %v", err)
	}

	cfg := metering.Config{HotAckWait: metering.HotAckAOFLocal, RecordTimeout: 3 * time.Second}.WithDefaults()
	o := contracts.LedgerOptions{Pools: []domain.PoolDef{{Name: "promo", Priority: 1}},
		Limits: []domain.Limit{{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 1000, Window: domain.Daily, DenyCode: domain.DenyLimitReached}}}
	raw := newStore(t, svc, contracts.NewFakeClock(epoch), cfg, o)
	l := healing{Store: raw, cfg: cfg}
	from := domain.Scope{Realm: "acl", Kind: "organization", ID: fmt.Sprint(time.Now().UnixNano())}
	to := domain.Scope{Realm: "acl", Kind: "workspace", ID: from.ID}

	if _, err := l.Grant(ctx, domain.Grant{Scope: from, Pool: "promo", Amount: 100, IdemKey: "g1", Reason: "promo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Grant(ctx, domain.Grant{Scope: from, Amount: 900, IdemKey: "g2", Reason: "purchase"}); err != nil {
		t.Fatal(err)
	}
	sj := domain.Subjects{"user": "u1", "job": "j1"}
	if _, err := l.Debit(ctx, domain.Charge{Scope: from, Amount: 150, IdemKey: "u1", Subjects: sj, Quantities: []domain.Quantity{{Unit: "tok", Amount: 3}}}); err != nil {
		t.Fatalf("debit (draws, counters, job counters, stream, WAITAOF) as the service role: %v", err)
	}
	if _, err := l.Admit(ctx, from, domain.AdmitRequest{Subjects: sj, Limits: o.Limits}); err != nil {
		t.Fatalf("admit (FCALL_RO + counter reads): %v", err)
	}
	if _, err := l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 100, IdemKey: "t1"}); err != nil {
		t.Fatalf("transfer phase 1: %v", err)
	}
	// The writer rehydrates a cold destination from the books before it credits it.
	if _, err := raw.Rehydrate(ctx, to, ports.RehydrateState{ShardGen: 1}); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if _, err := raw.ApplyDelta(ctx, ports.HotDelta{Op: domain.OpTransferIn, Scope: to, Delta: 100, IdemKey: "t1"}); err != nil {
		t.Fatalf("the writer's credit leg: %v", err)
	}
	acct, err := raw.Snapshot(ctx, from)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := raw.Heal(ctx, from, acct.Seq, acct.Pools); err != nil || !ok {
		t.Fatalf("heal: %v %v", ok, err)
	}
	sh := from.Shard(cfg.Shards)
	if _, err := raw.NodeID(ctx, sh); err != nil {
		t.Fatalf("INFO replication: %v", err)
	}
	if err := raw.FreezeShard(ctx, sh, "acl"); err != nil {
		t.Fatal(err)
	}
	if err := raw.UnfreezeShard(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if err := raw.BumpShard(ctx, sh, 2); err != nil {
		t.Fatal(err)
	}

	// …and the service role still cannot do the things the ACL exists to forbid.
	for name, cmd := range map[string][]any{
		"FLUSHALL": {"FLUSHALL"}, "CONFIG SET": {"CONFIG", "SET", "maxmemory-policy", "allkeys-lru"},
		"KEYS": {"KEYS", "*"}, "EVAL": {"EVAL", "return 1", 0}, "ACL LIST": {"ACL", "LIST"},
	} {
		if err := svc.Do(ctx, cmd...).Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
			t.Errorf("the service role ran %s: %v", name, err)
		}
	}
}

func TestUnreachableRedisIsHotStoreUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = rdb.Close() }()
	cfg := metering.Config{}.WithDefaults()
	st := newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, contracts.LedgerOptions{})
	s := domain.Scope{Realm: "down", Kind: "workspace", ID: "w"}
	if _, err := st.Debit(ctx, domain.Charge{Scope: s, Amount: 1, IdemKey: "k"}); !domain.IsHotUnavailable(err) {
		t.Fatalf("an outage must be journalable, got %v", err)
	}
	if _, err := st.Admit(ctx, s, domain.AdmitRequest{}); !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("Admit under an outage: %v", err)
	}
	if err := st.Ping(ctx); !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("readiness must fail: %v", err)
	}
}

// Intents are written by the hot function atomically with the balance: exactly one per mutation,
// carrying the scope's sequence number, the draws actually applied and the usage record.
func TestEveryMutationEmitsExactlyOneCompleteIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rdb := servers[redistest.Standalone].Client()
	defer func() { _ = rdb.Close() }()
	cfg := metering.Config{}.WithDefaults()
	o := contracts.LedgerOptions{Pools: []domain.PoolDef{{Name: "promo", Priority: 1}}}
	l := healing{Store: newStore(t, rdb, contracts.NewFakeClock(epoch), cfg, o), cfg: cfg}
	s := domain.Scope{Realm: "intent", Kind: "workspace", ID: fmt.Sprint(time.Now().UnixNano())}
	sh := s.Shard(cfg.Shards)
	stream := "billing:outbox:{s" + fmt.Sprint(int(sh)) + "}"
	entries := func() []goredis.XMessage {
		msgs, err := rdb.XRange(ctx, stream, "-", "+").Result()
		if err != nil {
			t.Fatal(err)
		}
		var mine []goredis.XMessage
		for _, m := range msgs {
			if m.Values["id"] == s.ID {
				mine = append(mine, m)
			}
		}
		return mine
	}

	if _, err := l.Grant(ctx, domain.Grant{Scope: s, Pool: "promo", Amount: 60, IdemKey: "g1", Reason: "signup_grant"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Grant(ctx, domain.Grant{Scope: s, Amount: 500, IdemKey: "g2", Reason: "purchase"}); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 29, 11, 59, 0, 5, time.UTC)
	c := domain.Charge{Scope: s, Amount: 100, IdemKey: "u1", Reason: "query", Resource: "llm.chat", RateKey: "gpt", RateCardVersion: "v3", CostMicros: 77,
		OccurredAt: when, Quantities: []domain.Quantity{{Unit: "in", Amount: 10}}, Subjects: domain.Subjects{"user": "u1"}, Ref: map[string]string{"trace_id": "t"}}
	if _, err := l.Debit(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Debit(ctx, c); err != nil { // a replay emits NOTHING
		t.Fatal(err)
	}

	got := entries()
	if len(got) != 3 {
		t.Fatalf("2 grants + 1 debit (+ a silent replay) = 3 intents, got %d", len(got))
	}
	fields := func(m goredis.XMessage) map[string]string {
		f := map[string]string{}
		for k, v := range m.Values {
			f[k] = fmt.Sprint(v)
		}
		return f
	}
	var seqs []int64
	for _, m := range got {
		in, err := domain.IntentFromFields(fields(m))
		if err != nil {
			t.Fatalf("the writer must be able to parse what the hot function wrote: %v (%v)", err, m.Values)
		}
		seqs = append(seqs, in.Seq)
	}
	if seqs[1] != seqs[0]+1 || seqs[2] != seqs[1]+1 {
		t.Fatalf("intents carry consecutive per-scope sequence numbers: %v", seqs)
	}
	use, _ := domain.IntentFromFields(fields(got[2]))
	if use.Op != domain.OpUsage || use.IdemKey != "u1" || use.Fingerprint != c.Fingerprint() {
		t.Fatalf("%+v", use)
	}
	if len(use.Draws) != 2 || use.Draws[0] != (domain.PoolDelta{Pool: "promo", Delta: -60}) || use.Draws[1] != (domain.PoolDelta{Pool: "general", Delta: -40}) {
		t.Fatalf("the intent carries the pool draws the hot tier ACTUALLY applied, in order: %+v", use.Draws)
	}
	p := use.Payload
	occurred, _ := p.Occurred()
	if p.Resource != "llm.chat" || p.RateKey != "gpt" || p.RateCardVersion != "v3" || p.CostMicros != 77 || !occurred.Equal(when) ||
		p.Subjects["user"] != "u1" || p.Ref["trace_id"] != "t" || len(p.Quantities) != 1 {
		t.Fatalf("invariant 8: the usage record travels with the debit: %+v", p)
	}
}

var _ = atomic.Int64{}
