// Package redis is the hot-tier adapter: it implements ports.BalanceStore over the Redis Functions
// of the reference hot path, on the shard-tagged key layout of hot-path-consistency.md §1.
//
// Every mutation is ONE function call in the scope's shard slot (so it runs on Redis Cluster, where
// a script touching two slots is refused), and the hot functions are loaded UNCHANGED from
// lua/hot_path.lua — a test fails if that copy ever differs from the contract's reference file.
//
// The package speaks go-redis; the core never does (depguard). It returns only typed domain
// errors: a hot-tier refusal is ErrColdScope / ErrColdShard / ErrShardFrozen / ErrBackpressure, and
// anything that stops a call reaching Redis is ErrHotStoreUnavailable, which the Meter turns into
// a journaled, deferred Record (invariant 16).
package redis
