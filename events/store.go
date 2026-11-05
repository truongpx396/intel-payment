package events

import (
	"context"
	"time"
)

// FeedQuery is one page of the feed: events of these realms after a cursor, oldest first.
type FeedQuery struct {
	Realms []string // the realms the credential covers; never empty
	After  string   // exclusive; "" starts at the oldest retained
	Types  []string // empty = every type
	Limit  int
}

// Ended is the deliveries of one endpoint whose retries ExpireStale ended.
type Ended struct {
	Realm, Endpoint string
	N               int64
}

// Failure is what a failed attempt records.
type Failure struct {
	Status int // the endpoint's HTTP status, 0 when none was received
	Err    string
	Next   time.Time // when to try again; ignored when Dead
	Dead   bool
}

// Store is the log, its deliveries, and the leases that let several workers share them. Everything an event needs to be sent is read
// from here, so a killed worker loses nothing: its lease lapses and the next one sends it.
type Store interface {
	// Append writes the event once per source (the bus message it came from, or a key the producer names) and, in the same
	// transaction, owes one delivery to each ACTIVE endpoint of the event's realm that subscribes to its type. The id and the time are
	// the store's: ids are issued under a lock so they sort in the order events became visible, which is what lets a cursor never skip
	// one. created is false when the source was already recorded; the stored event is returned either way.
	Append(ctx context.Context, e Event, source string) (stored Event, created bool, err error)
	// Feed is the log after a cursor, in id order. Every event is in exactly one page of a consumer that resumes from the last id it saw.
	Feed(ctx context.Context, q FeedQuery) ([]Event, error)

	// Claim takes up to limit deliveries due at now, leasing each for lease: until the lease lapses no other Claim returns it.
	Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Due, error)
	// Delivered records a 2xx.
	Delivered(ctx context.Context, deliveryID string, status int, at time.Time) error
	// Failed records a failed attempt: the next try, or the end of the retries.
	Failed(ctx context.Context, deliveryID string, f Failure) error
	// ExpireStale ends the retries of every pending delivery older than Window (an endpoint that was switched off keeps its owed
	// deliveries, and they must not stay pending forever). It returns how many it ended, per realm and endpoint.
	ExpireStale(ctx context.Context, now time.Time) ([]Ended, error)
	// Redeliver makes a dead delivery pending again, due now. ErrNotFound for no such delivery; ErrInvalid for one that is not dead.
	Redeliver(ctx context.Context, deliveryID string, at time.Time) error
	// DeadCounts is the deliveries that are dead, per realm: what the page is written against.
	DeadCounts(ctx context.Context) (map[string]int64, error)

	// Purge deletes events created before the cutoff that nothing is still owed for. It returns how many.
	Purge(ctx context.Context, before time.Time) (int, error)
}

// Sender makes the request. It owns the SSRF guard: a destination it refuses is an error before any byte is sent, a redirect is not
// followed, and the response is read only up to a cap. The status is the endpoint's, 0 when none was received.
type Sender interface {
	Send(ctx context.Context, url string, header map[string]string, body []byte) (status int, err error)
}

// SecretSource resolves a signing secret by reference (`env:NAME`, `file:/path`). A secret is never stored, logged or sent anywhere
// but into a signature.
type SecretSource interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
}
