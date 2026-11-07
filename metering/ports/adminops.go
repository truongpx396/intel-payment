package ports

import (
	"context"
	"encoding/json"
	"time"
)

// AdminOperationKind is what an operator asked the engine to do (rest-api.md § Operations). Each is idempotent in its own right, and each
// is recorded before it is done, with the person who asked and why.
type AdminOperationKind string

// The operations.
const (
	AdminReconcile AdminOperationKind = "reconcile" // POST /v1/admin/reconcile
	AdminRecover   AdminOperationKind = "recover"   // POST /v1/admin/shards/{shard}/recover
	AdminRehydrate AdminOperationKind = "rehydrate" // POST /v1/admin/rehydrate
	AdminReplay    AdminOperationKind = "replay"    // POST /v1/admin/dead-letters/{id}/replay
	AdminSubdrift  AdminOperationKind = "subdrift"  // POST /v1/admin/subdrift
)

// AdminOperationStatus is where an operation stands. Pending and running are not final; succeeded and failed are, and neither is claimed again.
type AdminOperationStatus string

// The statuses.
const (
	AdminPending   AdminOperationStatus = "pending"
	AdminRunning   AdminOperationStatus = "running"
	AdminSucceeded AdminOperationStatus = "succeeded"
	AdminFailed    AdminOperationStatus = "failed"
)

// AdminOperation is one request from an operator, from the moment it is made to the moment it is done. Actor, Reason, Params and Kind are
// the request and never change; the rest is how it went.
type AdminOperation struct {
	ID     string
	Realm  string
	Kind   AdminOperationKind
	Params json.RawMessage // the request's arguments as JSON; their meaning is the kind's
	Actor  string          // the person: required
	Reason string          // why: required
	Key    string          // the Idempotency-Key the request was made under; unique per realm
	Via    string          // the credential that carried it

	Status     AdminOperationStatus
	Result     json.RawMessage // what a succeeded operation reports, as JSON
	Error      string          // why a failed one failed
	Attempts   int             // how many times a worker has claimed it
	CreatedAt  time.Time
	StartedAt  *time.Time // the latest claim
	FinishedAt *time.Time
}

// AdminBacklog is the operations nobody has in hand: pending, or running under a lease that has lapsed because the worker died.
type AdminBacklog struct {
	Waiting int64
	Oldest  time.Duration // how long the longest-waiting of them has been waiting; zero when none
}

// AdminOperationStore is the durable queue of operator requests. paymentd writes to it and payment-worker works it, so an operation
// outlives the process that asked for it and the worker that dies halfway through it.
type AdminOperationStore interface {
	// Submit records the request unless one is already recorded under the same (realm, key). It returns the operation and whether this call
	// made it, so a retry is the same operation and not a second one. A key reused for a different kind, params, actor or reason is
	// domain.ErrIdemConflict, and nothing changes; a different Via is not a different request.
	Submit(ctx context.Context, op AdminOperation) (AdminOperation, bool, error)
	// Get reads an operation of the realm: domain.ErrNotFound for one that does not exist or belongs to another realm.
	Get(ctx context.Context, realm, id string) (AdminOperation, error)
	// Claim takes the oldest operation that is pending, or running under a lease that has lapsed, for lease: it is running, its attempts
	// are one more, and no other Claim gets it until the lease lapses. A failed operation is never claimed again. lease must be positive.
	Claim(ctx context.Context, lease time.Duration) (AdminOperation, bool, error)
	// Finish records the outcome, succeeded or failed, of the attempt that holds the claim. false means it did not: the operation was
	// claimed again since, or already has an outcome.
	Finish(ctx context.Context, id string, attempt int, status AdminOperationStatus, result json.RawMessage, errText string) (bool, error)
	// Backlog is what waits, and how long the oldest has: the number the page for a worker that has stopped is written against.
	Backlog(ctx context.Context) (AdminBacklog, error)
}

// AdminRequest is what a caller asks for: the operation before it has an id, a status or a history.
type AdminRequest struct {
	Realm  string
	Kind   AdminOperationKind
	Params json.RawMessage
	Actor  string
	Reason string
	Key    string
	Via    string
}

// AdminOperations is the REST surface's view of the operations: ask for one, and look at how it went. Asking records the request and
// returns at once; the worker does the work, and what it came to is read back from the record.
type AdminOperations interface {
	// Submit validates and records the request. created is false when the same request was made before under the same key.
	// domain.ErrInvalid is a request that cannot be read; domain.ErrIdemConflict a key reused for a different request.
	Submit(ctx context.Context, rq AdminRequest) (op AdminOperation, created bool, err error)
	// Get reads an operation of the realm; domain.ErrNotFound for one that does not exist or belongs to another realm.
	Get(ctx context.Context, realm, id string) (AdminOperation, error)
}
