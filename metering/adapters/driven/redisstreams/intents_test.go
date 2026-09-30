//go:build integration

package redisstreams_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/metering"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var servers = map[redistest.Mode]*redistest.Redis{}

func TestMain(m *testing.M) {
	ctx := context.Background()
	for _, mode := range []redistest.Mode{redistest.Standalone, redistest.Cluster} {
		r, err := redistest.Run(ctx, mode)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		servers[mode] = r
		c := r.Client()
		if _, err := hotredis.EnsureInstalled(ctx, c); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = c.Close()
	}
	code := m.Run()
	for _, r := range servers {
		_ = r.Terminate(ctx)
	}
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

// producer writes real intents through the real hot functions.
type producer struct {
	t     *testing.T
	store *hotredis.Store
	scope domain.Scope
	shard domain.Shard
	rdb   goredis.UniversalClient
}

func newProducer(t *testing.T, mode redistest.Mode) *producer {
	t.Helper()
	rdb := servers[mode].Client()
	t.Cleanup(func() { _ = rdb.Close() })
	cfg := metering.Config{}.WithDefaults()
	st, err := hotredis.New(rdb, hotredis.Options{Config: cfg, Pools: contracts.StaticPools(nil), Limits: contracts.StaticLimits(nil), Clock: system.Clock{}})
	if err != nil {
		t.Fatal(err)
	}
	sc := domain.Scope{Realm: "rs", Kind: "workspace", ID: fmt.Sprintf("w-%d-%d", time.Now().UnixNano(), seq.Add(1))}
	p := &producer{t: t, store: st, scope: sc, shard: sc.Shard(cfg.Shards), rdb: rdb}
	ctx := context.Background()
	if err := st.OpenShard(ctx, p.shard, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Rehydrate(ctx, sc, ports.RehydrateState{ShardGen: 1}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *producer) grant(n int) {
	p.t.Helper()
	for i := 0; i < n; i++ {
		if _, err := p.store.Grant(context.Background(), domain.Grant{Scope: p.scope, Amount: 10, IdemKey: fmt.Sprintf("g%d", i), Reason: "promo"}); err != nil {
			p.t.Fatal(err)
		}
	}
}

func stream(t *testing.T, mode redistest.Mode, consumer string) *redisstreams.Stream {
	t.Helper()
	rdb := servers[mode].Client()
	t.Cleanup(func() { _ = rdb.Close() })
	s, err := redisstreams.NewStream(rdb, redisstreams.IntentOptions{Consumer: consumer, Clock: system.Clock{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mine keeps only this test's scope's deliveries: the stream is per SHARD, shared with other tests.
func mine(ds []ports.Delivery, s domain.Scope) []ports.Delivery {
	var out []ports.Delivery
	for _, d := range ds {
		if d.ParseErr == nil && d.Intent.Scope.Normalized() == s.Normalized() {
			out = append(out, d)
		}
	}
	return out
}

func TestReadIsInOrderAtLeastOnceAndAckNeverDeletes(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := newProducer(t, mode)
			s := stream(t, mode, "w1")
			p.grant(5)

			var got []ports.Delivery
			for len(mine(got, p.scope)) < 5 {
				ds, err := s.Read(ctx, p.shard, 100, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if len(ds) == 0 {
					t.Fatal("the writer must see every intent the hot functions wrote")
				}
				got = append(got, ds...)
			}
			seqs := mine(got, p.scope)
			for i, d := range seqs {
				if d.Intent.Seq != int64(i+1) || d.Attempts != 1 || d.Intent.Op != domain.OpGrant {
					t.Fatalf("per-scope sequence order, first delivery: %d %+v", i, d)
				}
			}
			stats, err := s.Stats(ctx, p.shard)
			if err != nil || stats.Pending < 5 {
				t.Fatalf("delivered and not acked is pending: %+v %v", stats, err)
			}
			ids := make([]string, len(got))
			for i, d := range got {
				ids[i] = d.ID
			}
			before := stats.Length
			if err := s.Ack(ctx, p.shard, ids...); err != nil {
				t.Fatal(err)
			}
			after, _ := s.Stats(ctx, p.shard)
			if after.Length != before {
				t.Fatalf("FR-047: an ack must never delete: the stream went %d → %d", before, after.Length)
			}
			if again, _ := s.Read(ctx, p.shard, 100, time.Minute); len(mine(again, p.scope)) != 0 {
				t.Fatal("an acked entry is not delivered again")
			}
		})
	}
}

// A consumer killed mid-handler has its in-flight entries redelivered, not lost, in order.
func TestADeadConsumersEntriesAreClaimedOldestFirst(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := newProducer(t, mode)
			dead := stream(t, mode, "dead")
			p.grant(3)
			var first []ports.Delivery
			for len(mine(first, p.scope)) < 3 {
				ds, err := dead.Read(ctx, p.shard, 100, time.Minute)
				if err != nil || len(ds) == 0 {
					t.Fatalf("%v", err)
				}
				first = append(first, ds...)
			}
			// "dead" never acks. A fresh entry arrives while it is down.
			p.grant(0)
			if _, err := p.store.Grant(ctx, domain.Grant{Scope: p.scope, Amount: 1, IdemKey: "late", Reason: "promo"}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)

			alive := stream(t, mode, "alive")
			ds, err := alive.Read(ctx, p.shard, 100, 100*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			re := mine(ds, p.scope)
			if len(re) != 4 {
				t.Fatalf("the dead consumer's 3 plus the new 1: %d", len(re))
			}
			for i, d := range re {
				if d.Intent.Seq != int64(i+1) {
					t.Fatalf("order must survive a takeover — older entries first: %d has seq %d", i, d.Intent.Seq)
				}
			}
			for _, d := range re[:3] {
				if d.Attempts < 2 || d.Idle == 0 {
					t.Fatalf("a redelivery says so (the writer parks a poison entry by this count): %+v", d)
				}
			}
			if re[3].Attempts != 1 {
				t.Fatalf("the new one is a first delivery: %+v", re[3])
			}
		})
	}
}

func TestAMalformedEntryIsSurfacedWholeNotDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newProducer(t, redistest.Standalone)
	s := stream(t, redistest.Standalone, "w1")
	key := hotredis.OutboxKey(p.shard)
	if err := p.rdb.XAdd(ctx, &goredis.XAddArgs{Stream: key, Values: map[string]any{"v": "1", "op": "usage", "realm": string(p.scope.Realm), "kind": "workspace", "id": p.scope.ID, "gen": "1", "seq": "NaN", "idem": "k", "fp": "f", "draws": "", "marker": "bad-" + p.scope.ID}}).Err(); err != nil {
		t.Fatal(err)
	}
	var bad *ports.Delivery
	for i := 0; i < 10 && bad == nil; i++ {
		ds, err := s.Read(ctx, p.shard, 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for j := range ds {
			if ds[j].Fields["marker"] == "bad-"+p.scope.ID {
				bad = &ds[j]
			}
		}
	}
	if bad == nil || bad.ParseErr == nil || bad.Fields["idem"] != "k" {
		t.Fatalf("FR-043: a malformed entry is parked, so it must arrive intact with its parse error: %+v", bad)
	}
}

func TestStatsReportsTheAgeOfTheOldestUnbookedEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newProducer(t, redistest.Standalone)
	s := stream(t, redistest.Standalone, "w1")
	p.grant(1)
	time.Sleep(1200 * time.Millisecond)
	st, err := s.Stats(ctx, p.shard)
	if err != nil {
		t.Fatal(err)
	}
	if st.OldestUndelivered < time.Second && st.OldestPendingAge < time.Second {
		t.Fatalf("depth alone hides a stuck head-of-line entry; age catches it: %+v", st)
	}
}

func TestConstructionRefusesWhatItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	rdb := servers[redistest.Standalone].Client()
	defer func() { _ = rdb.Close() }()
	if _, err := redisstreams.NewStream(rdb, redisstreams.IntentOptions{Clock: system.Clock{}}); err == nil {
		t.Fatal("a consumer name is required: two workers sharing one would steal each other's entries")
	}
	if _, err := redisstreams.NewStream(nil, redisstreams.IntentOptions{Consumer: "x", Clock: system.Clock{}}); err == nil {
		t.Fatal("a client is required")
	}
}

// A rollback can take the consumer group with it (it was created after the state Redis came back to).
// The writer must not wedge on NOGROUP: the group is made again, from the start, and every entry the
// books have not acknowledged is delivered again — which idempotent booking makes harmless.
func TestAGroupLostWithARollbackIsCreatedAgain(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := newProducer(t, mode)
			group := fmt.Sprintf("lost-%d", seq.Add(1)) // not the shared group: other tests read it
			rdb := servers[mode].Client()
			t.Cleanup(func() { _ = rdb.Close() })
			s, err := redisstreams.NewStream(rdb, redisstreams.IntentOptions{Consumer: "w1", Group: group, Clock: system.Clock{}})
			if err != nil {
				t.Fatal(err)
			}
			p.grant(3)
			if ds, err := s.Read(ctx, p.shard, 100, time.Minute); err != nil || len(mine(ds, p.scope)) != 3 {
				t.Fatalf("first read: %d %v", len(mine(ds, p.scope)), err)
			}

			if err := rdb.XGroupDestroy(ctx, hotredis.OutboxKey(p.shard), group).Err(); err != nil {
				t.Fatal(err)
			}
			ds, err := s.Read(ctx, p.shard, 100, time.Minute)
			if err != nil {
				t.Fatalf("a lost group must be created again, not surfaced as an error: %v", err)
			}
			if got := mine(ds, p.scope); len(got) != 3 || got[0].Intent.Seq != 1 {
				t.Fatalf("the group restarts from the beginning: %d entries", len(got))
			}

			if err := rdb.XGroupDestroy(ctx, hotredis.OutboxKey(p.shard), group).Err(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Stats(ctx, p.shard); err != nil {
				t.Fatalf("stats must survive a lost group too: %v", err)
			}
		})
	}
}
