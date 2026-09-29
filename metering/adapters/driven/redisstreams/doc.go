// Package redisstreams is the default bus adapter: Redis Streams, in the very Redis the balances
// already require, so nothing else needs to run (FR-045, FR-046).
//
// The intent stream is written by the hot functions — atomically with the balance change, in the
// same slot — and is only READ here, per shard, in order, at least once: consumer-group delivery,
// XAUTOCLAIM for a dead consumer's entries, XACK once the writer has booked (or parked) them.
// Nothing here can add to an outbox, which is what keeps a balance change and its intent one thing.
package redisstreams
