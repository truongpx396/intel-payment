package ports

import "context"

// Claimer is the idempotent claim every scheduled tick runs under (bus-subjects.md § Scheduled ticks). A tick may be published twice, by
// two schedulers or by one that retried, and delivered to two replicas at once; the claim is what makes the work happen once. It is a
// lock, not a record: it writes nothing, so a tick with nothing due leaves no trace, and it is released by whoever holds it dying — the
// run that was killed midway holds nothing, and the next one takes the work over. The work behind a tick is itself idempotent
// (every effect is keyed, every booking guarded), which is what makes that takeover safe; the claim only stops two runs wasting each
// other's time and contending for the same rows.
type Claimer interface {
	// TryClaim takes the claim on key if no one holds it. ok is false, with a nil error, when another run holds it. release gives the
	// claim back and is safe to call more than once; a holder that dies without calling it loses the claim all the same.
	TryClaim(ctx context.Context, key string) (release func(), ok bool, err error)
}
