//go:build integration

package redis_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Every case gets a shard of its own, above the range the other tests hash into (a case that freezes a shard or moves its generation on
// would otherwise make a neighbour's account stale), and no two cases share one.
var (
	unblockShardsMu   sync.Mutex
	unblockShardsUsed = map[domain.Shard]bool{}
)

const unblockShards = 16384

func hotUnblockWorld(mode redistest.Mode) contracts.HotUnblockFactory {
	return func(tb testing.TB) contracts.HotUnblockWorld {
		tb.Helper()
		ctx := context.Background()
		rdb := servers[mode].Client()
		tb.Cleanup(func() { _ = rdb.Close() })
		cfg := metering.Config{Shards: unblockShards}.WithDefaults()
		st, err := hotredis.New(rdb, hotredis.Options{Config: cfg, Pools: contracts.StaticPools(nil), Limits: contracts.StaticLimits(nil), Clock: contracts.NewFakeClock(epoch)})
		if err != nil {
			tb.Fatal(err)
		}
		must := func(err error) {
			tb.Helper()
			if err != nil {
				tb.Fatal(err)
			}
		}
		return contracts.HotUnblockWorld{
			Hot: st,
			NewScope: func(tb testing.TB) domain.Scope {
				tb.Helper()
				for i := 0; ; i++ {
					s := domain.Scope{Realm: "unblock", Kind: "workspace", ID: fmt.Sprintf("%d-%d", time.Now().UnixNano(), i)}
					sh := s.Shard(unblockShards)
					unblockShardsMu.Lock()
					fresh := sh >= 256 && !unblockShardsUsed[sh]
					unblockShardsUsed[sh] = unblockShardsUsed[sh] || fresh
					unblockShardsMu.Unlock()
					if fresh {
						must(st.OpenShard(ctx, sh, 1))
						return s
					}
				}
			},
			Install: func(tb testing.TB, s domain.Scope, blocked bool) {
				tb.Helper()
				out, err := st.Rehydrate(ctx, s, ports.RehydrateState{ShardGen: 1, AppliedSeq: 41, Blocked: blocked, Pools: map[domain.Pool]domain.Credits{"general": 500, "promo": 25}})
				if err != nil || out != ports.RehydrateInstalled {
					tb.Fatalf("install the account of %s: %v, %v", s.Tag(), out, err)
				}
			},
			Account: func(tb testing.TB, s domain.Scope) ports.HotAccount {
				tb.Helper()
				a, err := st.Snapshot(ctx, s)
				must(err)
				return a
			},
			Stale: func(tb testing.TB, s domain.Scope) { tb.Helper(); must(st.BumpShard(ctx, s.Shard(unblockShards), 2)) },
			Freeze: func(tb testing.TB, s domain.Scope, frozen bool) {
				tb.Helper()
				if frozen {
					must(st.FreezeShard(ctx, s.Shard(unblockShards), "test"))
					return
				}
				must(st.UnfreezeShard(ctx, s.Shard(unblockShards)))
			},
		}
	}
}

// R112a against Redis 7: the same contract the in-memory clearer honours, on a standalone node and on a cluster, where a function that
// touched a key outside the scope's slot would be refused.
func TestHotUnblockHonoursTheContract_Standalone(t *testing.T) {
	t.Parallel()
	contracts.HotUnblockContract(t, hotUnblockWorld(redistest.Standalone))
}

func TestHotUnblockHonoursTheContract_Cluster(t *testing.T) {
	t.Parallel()
	contracts.HotUnblockContract(t, hotUnblockWorld(redistest.Cluster))
}
