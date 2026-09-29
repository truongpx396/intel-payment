package redis

import (
	"strconv"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// The key layout of hot-path-consistency.md §1. Every key of a scope carries its SHARD's hash tag
// {s<n>}, never the scope's: a Redis Cluster function may touch only one slot, and each operation
// touches the account, its guard, its counters and the outbox stream together. The scope's Tag() is
// its identity and rides inside the key after the hash tag.

// Idempotency-guard op classes. Guards are per scope and per class, so a usage key `x` and a grant
// key `x` never collide.
const (
	idemUsage      = "u"
	idemGrant      = "g"
	idemTransfer   = "t"
	idemTransferIn = "ti"
	idemExpiry     = "x"
	idemCorrection = "c"
)

func shardTag(sh domain.Shard) string { return "{s" + strconv.Itoa(int(sh)) + "}" }

func metaKey(sh domain.Shard) string   { return "meta:" + shardTag(sh) }
func outboxKey(sh domain.Shard) string { return "billing:outbox:" + shardTag(sh) }

func acctKey(sh domain.Shard, s domain.Scope) string {
	return "acct:" + shardTag(sh) + ":" + s.Tag()
}

func idemKey(sh domain.Shard, s domain.Scope, class, key string) string {
	return "idem:" + shardTag(sh) + ":" + s.Tag() + ":" + class + ":" + key
}

func ctrKey(sh domain.Shard, s domain.Scope, limit, subject, bucket string) string {
	return "ctr:" + shardTag(sh) + ":" + s.Tag() + ":" + limit + ":" + subject + ":" + bucket
}

// idemClass maps a writer-issued intent op to its guard class.
func idemClass(op domain.IntentOp) string {
	switch op {
	case domain.OpTransferIn:
		return idemTransferIn
	case domain.OpExpiry:
		return idemExpiry
	case domain.OpCorrection:
		return idemCorrection
	case domain.OpTransferOut:
		return idemTransfer
	case domain.OpGrant:
		return idemGrant
	default:
		return idemUsage
	}
}
