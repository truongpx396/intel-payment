package ports

import (
	"context"
	"time"
)

// A message that exhausts its delivery budget is parked, never dropped, and the operator is told (FR-043). The Bus redelivers a
// failing message up to MaxAttempts and then publishes it whole to `<prefix>.dlq.<subject>`; the sweeper (app.DeadLetters) moves
// each of those into this store, which is what an operator reads and what a replay is made from.

// DeadLetter is one parked message.
type DeadLetter struct {
	ID string // the store's
	// Realm is the realm the original payload named, or GlobalRealm when it named none (a tick, a malformed envelope).
	Realm string
	// Subject is the subject the message was published on: what a replay publishes it to again.
	Subject string
	// SourceID is the bus's id for the delivery that failed. (Subject, SourceID) is the letter's identity: an envelope the bus
	// delivers twice parks one letter.
	SourceID string
	Payload  []byte // the original bytes, whole
	Attempts int
	// LastError is why the last attempt failed.
	LastError string
	ParkedAt  time.Time
	// ReplayedAt is when an operator's replay re-published it; nil while it is parked. A replayed letter is kept.
	ReplayedAt *time.Time
}

// GlobalRealm is the realm of a letter whose payload names none.
const GlobalRealm = "(global)"

// DeadLetterStore is the durable home of parked messages. Nothing in it is ever deleted, and what a letter says about the
// failure never changes once written; only a replay moves it, and only forward.
type DeadLetterStore interface {
	// Park records l, once per (Subject, SourceID): created is true for the call that parked it and false for every later one,
	// which returns the letter as first written. This is what makes "the page fires once per parking" hold when the bus delivers
	// the same envelope twice, or two sweepers see it at once.
	Park(ctx context.Context, l DeadLetter) (stored DeadLetter, created bool, err error)
	// Get reads one letter; domain.ErrNotFound when there is none.
	Get(ctx context.Context, id string) (DeadLetter, error)
	// ClaimReplay takes the right to re-publish a letter for lease. claimed is false when it was already replayed or another
	// replay's claim is still live; a claim that lapsed (its holder was killed midway) is taken again, so the next replay
	// completes the work. domain.ErrNotFound when there is no such letter.
	ClaimReplay(ctx context.Context, id string, lease time.Duration) (l DeadLetter, claimed bool, err error)
	// FinishReplay records that the claimed letter was re-published. Repeating it changes nothing.
	FinishReplay(ctx context.Context, id string) error
	// ReleaseReplay gives a claim back after a failed publish, so the next replay need not wait for the lease.
	ReleaseReplay(ctx context.Context, id string) error
	// Open counts the letters nobody has replayed, per realm. A realm that has had letters and has none open is present with 0,
	// so a gauge made from it falls back instead of staying at its last value.
	Open(ctx context.Context) (map[string]int64, error)
}
