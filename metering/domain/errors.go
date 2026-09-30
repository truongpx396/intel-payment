package domain

import "errors"

// Errors are typed because a caller acts differently on each. Wrap with %w, test with errors.Is.
// The comment on each names the wire status a transport maps it to (grpc-surface.md, rest-api.md).
var (
	// ErrInvalid is a malformed request: 400 / INVALID_ARGUMENT.
	ErrInvalid = errors.New("invalid request")
	// ErrIdemConflict is a key reused with a DIFFERENT request: 409. Never a silent no-op.
	ErrIdemConflict = errors.New("idempotency key reused with a different request")
	// ErrStaleEvent is a usage event older than the dedup window: 422.
	ErrStaleEvent = errors.New("event is older than the usage dedup window")
	// ErrMissingSubject is a configured subject ceiling whose subject the call omitted: 400.
	// A ceiling that silently does not apply is the worst kind (invariant 17).
	ErrMissingSubject = errors.New("a configured limit needs a subject the call did not carry")
	// ErrUnpriceable is an unknown rate key or unit, or a bad quantity: 422. Pricing fails closed.
	ErrUnpriceable = errors.New("event cannot be priced")
	// ErrAmountOutOfRange is an operation above MaxOperationAmount, or an overflow: 422.
	ErrAmountOutOfRange = errors.New("amount out of range")
	// ErrInsufficient is a transfer that would overdraw its source: 409.
	ErrInsufficient = errors.New("insufficient balance")
	// ErrDestBalanceCap is a transfer that would leave its destination holding more than
	// Transfer.MaxDestBalance: 422. It guards against an errant admin action, not a race.
	ErrDestBalanceCap = errors.New("transfer would exceed the destination's balance cap")
	// ErrShardCountMismatch is a process configured with a different Shards than the one recorded in
	// hot_config: two processes hashing scopes differently keep two sets of books. It refuses to start
	// (and fails /readyz), FR-042.
	ErrShardCountMismatch = errors.New("configured shard count differs from the recorded one")
	// ErrCrossRealm is a transfer between two realms — a mint and a burn, never a transfer: 403.
	ErrCrossRealm = errors.New("transfer across realms")
	// ErrHotStoreUnavailable is the hot tier unreachable, frozen or cold: 503. Admit under
	// fail_closed returns it; Record never does, it defers to the journal instead.
	ErrHotStoreUnavailable = errors.New("hot store unavailable")
	// ErrQuotaUnavailable is a limit sized by an entitlement that could not be resolved: 503
	// under fail_closed. It is never treated as "unlimited" (invariant 17).
	ErrQuotaUnavailable = errors.New("entitlement quota unavailable")
	// ErrUncountableLimit is a caller-supplied non-job window limit that names no configured limit: 400.
	ErrUncountableLimit = errors.New("caller-supplied limit cannot be counted")

	// The following are the hot tier's refusals. The app acts on each: it rehydrates on
	// ErrColdScope and defers usage to the journal on the others; none reaches a caller as such.

	// ErrColdScope: the account was built in an older shard generation, or never built.
	ErrColdScope = errors.New("hot account is cold")
	// ErrColdShard: the shard's meta is missing (a fresh or emptied Redis).
	ErrColdShard = errors.New("hot shard is cold")
	// ErrShardFrozen: the shard is frozen for recovery.
	ErrShardFrozen = errors.New("hot shard is frozen")
	// ErrBackpressure: the outbox stream is at its cap. Nothing is trimmed or dropped to make room.
	ErrBackpressure = errors.New("outbox at capacity")
)

// IsHotUnavailable reports whether err means the hot tier cannot take a mutation right now, so a
// usage record must be journaled rather than lost (invariant 16). ErrColdScope is not one of
// them: it is repaired in place by rehydrating.
func IsHotUnavailable(err error) bool {
	return errors.Is(err, ErrHotStoreUnavailable) || errors.Is(err, ErrColdShard) ||
		errors.Is(err, ErrShardFrozen) || errors.Is(err, ErrBackpressure)
}
