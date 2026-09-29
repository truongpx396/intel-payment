package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Rehydrate installs booked + open suspense into a stale or new account, and ONLY while it is still
// stale, so concurrent callers converge on one rebuild. It is safe only once the shard's stream is
// drained (hot-path-consistency.md §5): an undrained intent is in neither booked nor the rebuilt
// balance. That ordering is the caller's — the writer's Recover and the lazy rehydrate both drain first.
func (s *Store) Rehydrate(ctx context.Context, sc domain.Scope, st ports.RehydrateState) (ports.RehydrateOutcome, error) {
	if err := sc.Validate(); err != nil {
		return 0, err
	}
	sc = sc.Normalized()
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()
	sh := s.shard(sc)
	blocked := "0"
	if st.Blocked {
		blocked = "1"
	}
	args := []any{st.ShardGen, st.AppliedSeq, blocked}
	for _, p := range sortedPools(st.Pools) {
		args = append(args, string(p), int64(st.Pools[p]))
	}
	r, err := s.call(ctx, "ip_rehydrate", false, []string{metaKey(sh), acctKey(sh, sc)}, args...)
	if err != nil {
		return 0, err
	}
	switch r.status() {
	case "OK":
		return ports.RehydrateInstalled, nil
	case "ALREADY":
		return ports.RehydrateAlready, nil
	case "STALE_GEN":
		return ports.RehydrateStaleGen, nil
	}
	return 0, statusError(r.status())
}

// Heal is reconcile's correction of a defect on the HOT side only: a compare-and-set on seq, so a
// charge that landed since the drift was measured is never overwritten. false means it raced.
func (s *Store) Heal(ctx context.Context, sc domain.Scope, observedSeq int64, targets map[domain.Pool]domain.Credits) (bool, error) {
	if err := sc.Validate(); err != nil {
		return false, err
	}
	sc = sc.Normalized()
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()
	sh := s.shard(sc)
	args := []any{strconv.FormatInt(observedSeq, 10)}
	for _, p := range sortedPools(targets) {
		args = append(args, string(p), int64(targets[p]))
	}
	r, err := s.call(ctx, "ip_heal", false, []string{metaKey(sh), acctKey(sh, sc)}, args...)
	if err != nil {
		return false, err
	}
	switch r.status() {
	case "OK":
		return true, nil
	case "RACE":
		return false, nil
	}
	return false, statusError(r.status())
}

func sortedPools(m map[domain.Pool]domain.Credits) []domain.Pool {
	out := make([]domain.Pool, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	// insertion sort: pool sets are tiny, and a stable order keeps ARGV deterministic
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ShardState reads the hot mirror of hot_shards.
func (s *Store) ShardState(ctx context.Context, sh domain.Shard) (ports.ShardState, error) {
	ctx, cancel := s.withTimeout(ctx, s.cfg.AdmitTimeout)
	defer cancel()
	h, err := s.rdb.HGetAll(ctx, metaKey(sh)).Result()
	if err != nil {
		return ports.ShardState{}, wrapErr("HGETALL meta", err)
	}
	if h["gen"] == "" {
		return ports.ShardState{}, nil
	}
	gen, err := strconv.ParseInt(h["gen"], 10, 64)
	if err != nil {
		return ports.ShardState{}, fmt.Errorf("redis: meta gen %q: %w", h["gen"], err)
	}
	return ports.ShardState{Open: true, Gen: gen, Frozen: h["frozen"] == "1", Reason: h["frozen_reason"]}, nil
}

func (s *Store) shardOp(ctx context.Context, sh domain.Shard, args ...any) error {
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()
	r, err := s.call(ctx, "ip_shard", false, []string{metaKey(sh)}, args...)
	if err != nil {
		return err
	}
	if r.status() != "OK" {
		return statusError(r.status())
	}
	return nil
}

// OpenShard installs the shard's meta only if absent (first boot, or total loss).
func (s *Store) OpenShard(ctx context.Context, sh domain.Shard, gen int64) error {
	return s.shardOp(ctx, sh, "open", gen)
}

// BumpShard raises the generation: every account in the shard becomes stale at once, without a scan.
func (s *Store) BumpShard(ctx context.Context, sh domain.Shard, gen int64) error {
	return s.shardOp(ctx, sh, "bump", gen)
}

// FreezeShard makes every mutation on the shard return FROZEN.
func (s *Store) FreezeShard(ctx context.Context, sh domain.Shard, reason string) error {
	return s.shardOp(ctx, sh, "freeze", reason)
}

// UnfreezeShard lifts a freeze.
func (s *Store) UnfreezeShard(ctx context.Context, sh domain.Shard) error {
	return s.shardOp(ctx, sh, "unfreeze")
}

// NodeID is the replication id of the primary serving the shard's slot. A change means a failover
// (or a restore): the hot tier may have lost a suffix of its history, and the shard must be frozen,
// drained and rebuilt (hot-path-consistency.md §5).
func (s *Store) NodeID(ctx context.Context, sh domain.Shard) (string, error) {
	ctx, cancel := s.withTimeout(ctx, s.cfg.AdmitTimeout)
	defer cancel()
	var c goredis.Cmdable = s.rdb
	if cc, ok := s.rdb.(*goredis.ClusterClient); ok {
		node, err := cc.MasterForKey(ctx, metaKey(sh))
		if err != nil {
			return "", wrapErr("cluster route", err)
		}
		c = node
	}
	info, err := c.Info(ctx, "replication").Result()
	if err != nil {
		return "", wrapErr("INFO replication", err)
	}
	for _, line := range strings.Split(info, "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "master_replid:"); ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("redis: INFO replication has no master_replid")
}
