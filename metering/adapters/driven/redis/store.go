package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Options are what the store needs besides a client. The configuration is DATA the store reads
// through its stores (pools, limits) — it never dials Postgres itself.
type Options struct {
	Config metering.Config
	Pools  ports.PoolStore
	Limits ports.LimitStore
	Clock  ports.Clock
	// Watches is optional: nil means no balance watches.
	Watches ports.WatchSource
}

// Store implements ports.BalanceStore.
type Store struct {
	rdb   goredis.UniversalClient
	cfg   metering.Config
	pools ports.PoolStore
	lims  ports.LimitStore
	clock ports.Clock
	watch ports.WatchSource
}

var _ ports.BalanceStore = (*Store)(nil)

// New builds a Store. It validates its inputs and does no I/O.
func New(rdb goredis.UniversalClient, o Options) (*Store, error) {
	if rdb == nil {
		return nil, errors.New("redis: a client is required")
	}
	if o.Pools == nil || o.Limits == nil || o.Clock == nil {
		return nil, errors.New("redis: Pools, Limits and Clock are required")
	}
	cfg := o.Config.WithDefaults()
	// The store reads only these; a full Validate needs URLs the store does not use.
	for _, v := range []interface{ Validate() error }{cfg.Settlement, cfg.HotAckWait, cfg.NegativeBalance} {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	if cfg.Shards < 1 {
		return nil, fmt.Errorf("redis: Shards must be >= 1, got %d", cfg.Shards)
	}
	if cfg.MaxOperationAmount <= 0 || cfg.MaxOperationAmount > domain.MaxHotAmount {
		return nil, errors.New("redis: MaxOperationAmount must be in (0, 2^52]")
	}
	return &Store{rdb: rdb, cfg: cfg, pools: o.Pools, lims: o.Limits, clock: o.Clock, watch: o.Watches}, nil
}

// shard places a scope. Placement is deterministic and configuration-only: no I/O.
func (s *Store) shard(sc domain.Scope) domain.Shard { return sc.Shard(s.cfg.Shards) }

func (s *Store) eligible(ctx context.Context, realm domain.Realm, resource string) ([]domain.Pool, error) {
	defs, err := s.pools.Pools(ctx, realm)
	if err != nil {
		return nil, fmt.Errorf("redis: pools: %w", err)
	}
	return domain.EligiblePools(defs, resource), nil
}

func poolNames(ps []domain.Pool) string {
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = string(p)
	}
	return strings.Join(names, ",")
}

func (s *Store) checkAmount(a domain.Credits) error {
	if a > s.cfg.MaxOperationAmount || a < -s.cfg.MaxOperationAmount {
		return fmt.Errorf("%w: %d exceeds MaxOperationAmount %d", domain.ErrAmountOutOfRange, a, s.cfg.MaxOperationAmount)
	}
	return nil
}

func (s *Store) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// call runs one hot function. ro selects FCALL_RO (ip_admit is registered no-writes, so it may also
// run on a replica).
func (s *Store) call(ctx context.Context, fn string, ro bool, keys []string, args ...any) (reply, error) {
	var (
		v   any
		err error
	)
	if ro {
		v, err = s.rdb.FCallRO(ctx, fn, keys, args...).Result()
	} else {
		v, err = s.rdb.FCall(ctx, fn, keys, args...).Result()
	}
	if err != nil {
		return nil, wrapErr(fn, err)
	}
	return asReply(v)
}

// mutate runs a state-changing hot function and, when HotAckWait asks for it, waits for the write
// to be durable BEFORE acknowledging (hot-path-consistency.md §5). WAITAOF blocks the calling
// connection until that connection's prior writes are fsynced, so the function and the wait run on
// one pinned connection to the node that owns the slot.
//
// If the wait is not confirmed the operation HAS applied: the error is ErrHotStoreUnavailable, so
// the Meter journals the charge and the replay through the hot guard is a REPLAY. That is the
// honest outcome — the caller was never told the write is durable when it is not.
func (s *Store) mutate(ctx context.Context, fn string, keys []string, args ...any) (reply, error) {
	if s.cfg.HotAckWait == metering.HotAckNone {
		return s.call(ctx, fn, false, keys, args...)
	}
	conn, err := s.pin(ctx, keys[0])
	if err != nil {
		return nil, wrapErr(fn, err)
	}
	defer func() { _ = conn.Close() }()
	v, err := conn.FCall(ctx, fn, keys, args...).Result()
	if err != nil {
		return nil, wrapErr(fn, err)
	}
	r, err := asReply(v)
	if err != nil || r.status() != "APPLIED" {
		return r, err // a refusal changed nothing: there is nothing to make durable
	}
	local, replicas := 1, 0
	if s.cfg.HotAckWait == metering.HotAckAOFReplica {
		local, replicas = 0, 1
	}
	got, err := conn.WaitAOF(ctx, local, replicas, s.cfg.RecordTimeout).Result()
	if err != nil {
		return nil, wrapErr("WAITAOF", err)
	}
	if len(got) != 2 || int(got[0]) < local || int(got[1]) < replicas {
		return nil, fmt.Errorf("%w: %s applied but WAITAOF %d/%d not confirmed (got %v)", domain.ErrHotStoreUnavailable, fn, local, replicas, got)
	}
	return r, nil
}

// pin returns a connection to the primary that owns key's slot.
func (s *Store) pin(ctx context.Context, key string) (*goredis.Conn, error) {
	switch c := s.rdb.(type) {
	case *goredis.ClusterClient:
		node, err := c.MasterForKey(ctx, key)
		if err != nil {
			return nil, err
		}
		return node.Conn(), nil
	case *goredis.Client:
		return c.Conn(), nil
	}
	return nil, fmt.Errorf("HotAckWait=%s needs a *redis.Client or *redis.ClusterClient, got %T", s.cfg.HotAckWait, s.rdb)
}

// Ping reports whether the hot tier is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		return wrapErr("PING", err)
	}
	return nil
}

// Check verifies the loaded function library is exactly this build's (read-only; for /readyz).
func (s *Store) Check(ctx context.Context) error { return CheckInstalled(ctx, s.rdb) }

// lowWater picks the balance watch this charge could cross: the first watch on a pool the charge
// may draw. The hot function supports one watch per mutation; with several, the writer's own
// reconcile of watches catches the rest.
func (s *Store) lowWater(ctx context.Context, sc domain.Scope, eligible []domain.Pool) string {
	if s.watch == nil {
		return ""
	}
	ws, err := s.watch.Watches(ctx, sc)
	if err != nil {
		return "" // a missing watch is a missed notification, never a failed charge
	}
	for _, w := range ws {
		for _, p := range eligible {
			if w.Pool.Or() == p {
				return fmt.Sprintf("%s:%d", w.Pool.Or(), w.Threshold)
			}
		}
	}
	return ""
}
