package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// The durable side. Only the LedgerWriter writes through these (invariant 1); the request tier
// reads booked state for a rehydrate and writes nothing here but the spend journal.

// Watermark is the per-scope record of the highest CONTIGUOUSLY applied hot sequence number.
// Intents are booked in sequence order; a redelivery, a gap and a regression are told apart by this
// plus the idempotency row, never by timing (hot-path-consistency.md §3).
type Watermark struct {
	Exists     bool
	Gen        int64
	AppliedSeq int64
	Blocked    bool // block_and_flag, until an operator clears it
}

// IdemRecord is what a durable guard remembers about the operation that claimed a key.
type IdemRecord struct {
	Fingerprint string
	Gen, Seq    int64 // identifies a redelivery exactly: the same intent has the same (gen, seq)
}

// LedgerRow is one row of the account of record. Delta is signed (D1): negative is consumption.
type LedgerRow struct {
	Scope         domain.Scope
	Pool          domain.Pool
	Delta         domain.Credits
	OperationType string // open vocabulary; engine-reserved: writeoff, expiry, allocation_out, allocation_in, admin_adjustment
	Counterparty  *domain.Scope
	IdemKey       string
	Gen           int64
	SeqFrom       int64
	SeqTo         int64
	EventCount    int // >= 1; more than 1 only in a rollup row

	RateCardVersion string // which card priced it (D6); empty for non-priced rows
	CostMicros      int64
	Resource        string
	RateKey         string
	Quantities      []domain.Quantity
	Subjects        domain.Subjects
	ActorID         string
	Ref             map[string]string
	OccurredAt      time.Time
}

// Suspense reasons and statuses.
const (
	SuspenseDuplicate    = "duplicate"     // a replay that outlived its hot guard
	SuspenseIdemConflict = "idem_conflict" // …with a different fingerprint: also pages
	SuspensePoison       = "poison"        // an intent that cannot be booked
	SuspenseExpiryTrueup = "expiry_trueup" // the unused part of an expiry's upper bound

	SuspenseStatusOpen   = "open"
	SuspenseStatusClosed = "closed"
)

// Durable-guard operations (credit_idem.op) and the usage guard.
const (
	IdemOpGrant      = "grant"
	IdemOpTransfer   = "transfer"
	IdemOpTransferIn = "transfer_in"
	IdemOpExpiry     = "expiry"
	IdemOpCorrection = "correction"
	IdemOpAdjustment = "adjustment"
)

// Engine-reserved ledger operation types. The vocabulary is otherwise open: a host's Reason
// becomes the operation_type of its rows.
const (
	OperationWriteoff   = "writeoff"
	OperationExpiry     = "expiry"
	OperationAllocOut   = "allocation_out"
	OperationAllocIn    = "allocation_in"
	OperationAdjustment = "admin_adjustment"
)

// Suspense is a hot-tier movement the books did not accept (a replay that outlived its hot guard, a
// poison intent, the unused part of an expiry's upper bound). Delta is the HOT movement that was
// not booked, so hot == booked + Σ open suspense holds at every sequence number.
type Suspense struct {
	ID        string
	Scope     domain.Scope
	Pool      domain.Pool
	Delta     domain.Credits
	Reason    string
	Gen, Seq  int64 // the intent that opened it
	IdemKey   string
	Status    string
	CreatedAt time.Time
}

// Lot is a grant's remainder under CreditExpiry=lots_fifo. Lots are maintained by the writer as it
// books in sequence order, so Remaining is exact at the watermark.
type Lot struct {
	ID        string
	Scope     domain.Scope
	Pool      domain.Pool
	Granted   domain.Credits
	Remaining domain.Credits
	ExpiresAt *time.Time
	SourceKey string
	CreatedAt time.Time
}

// TransferRecord is a transfer in flight or settled (credit_transfers).
type TransferRecord struct {
	Realm            domain.Realm
	IdemKey          string
	From, To         domain.Scope
	FromPool, ToPool domain.Pool
	Amount           domain.Credits
	Status           string // domain.TransferInTransit | domain.TransferSettled
	CreatedAt        time.Time
	SettledAt        *time.Time
}

// DeadIntent is an intent that exhausted its retry budget. Parked, never dropped (FR-043).
type DeadIntent struct {
	Scope    domain.Scope
	Op       domain.IntentOp
	Gen, Seq int64
	IdemKey  string
	Fields   map[string]string // the raw stream entry, for replay
	Attempts int
	LastErr  string
}

// BookTx is one transaction over ONE scope's books, holding that scope's advisory lock, so two
// writers overlapping during a shard takeover still book each intent once. Everything a booking
// needs is here; nothing else writes these tables.
type BookTx interface {
	Watermark(ctx context.Context) (Watermark, error)
	// SetWatermark upserts the watermark. applied_seq never moves backwards, whatever w says: a
	// regression is booked but must not lower it.
	SetWatermark(ctx context.Context, w Watermark, shard domain.Shard) error

	// UsageIdem probes the dedup window for key: every daily partition from `since` on. It is
	// correct only under this transaction's scope lock, which is why it lives here.
	UsageIdem(ctx context.Context, key string, since time.Time) (IdemRecord, bool, error)
	PutUsageIdem(ctx context.Context, key, fingerprint string, gen, seq int64) error
	// CreditIdem reads the PERMANENT guard for a minting or moving operation.
	CreditIdem(ctx context.Context, op, key string) (IdemRecord, bool, error)
	PutCreditIdem(ctx context.Context, op, key, fingerprint string, gen, seq int64, ledgerID string) error

	// Book appends the rows AND applies each pool's delta to account_credits in the same statement
	// batch, so `balance == SUM(ledger)` is a property of this one call rather than of two that
	// must agree. It returns the ids of the rows, in order.
	Book(ctx context.Context, rows []LedgerRow) ([]string, error)

	OpenSuspense(ctx context.Context, s Suspense) (string, error)
	CloseSuspense(ctx context.Context, id string) (bool, error)

	OpenLot(ctx context.Context, l Lot) error
	// ConsumeLots draws n credits from the pool's oldest unexpired lots (FIFO) and returns how many
	// it found; a pool with less in lots than n is not an error (grants before lots_fifo was on).
	ConsumeLots(ctx context.Context, pool domain.Pool, n domain.Credits) (domain.Credits, error)
	// ExpireLot zeroes a lot and returns the EXACT remainder it held at this point in the sequence.
	ExpireLot(ctx context.Context, lotID string) (remaining domain.Credits, found bool, err error)

	OpenTransfer(ctx context.Context, t TransferRecord) error
	// SettleTransfer marks a transfer settled and returns it; found=false if there is none.
	SettleTransfer(ctx context.Context, realm domain.Realm, idemKey string) (TransferRecord, bool, error)

	// SettleJournal marks the spend-journal row for key settled: the intent it carried is booked.
	SettleJournal(ctx context.Context, key string) error
}

// Lease is exclusive ownership of one shard's writer role (a Postgres session advisory lock). It
// releases when Release is called or the session dies, so a crashed worker's shards are taken over.
type Lease interface {
	// Alive reports whether the session that holds the lock is still connected.
	Alive(ctx context.Context) bool
	Release(ctx context.Context) error
}

// ShardRecord is the authoritative row of hot_shards; meta:{s<n>} in Redis mirrors it.
type ShardRecord struct {
	Shard      domain.Shard
	Gen        int64
	Frozen     bool
	Reason     string
	NodeReplID string
}

// BookedState is what the books say about one scope, read in one consistent snapshot.
type BookedState struct {
	Watermark Watermark
	Booked    map[domain.Pool]domain.Credits // account_credits
	Suspense  map[domain.Pool]domain.Credits // Σ open suspense deltas
}

// BookStore is the writer's Postgres port.
type BookStore interface {
	// InTx runs fn in one transaction holding scope's advisory lock. An error from fn rolls back.
	InTx(ctx context.Context, scope domain.Scope, fn func(ctx context.Context, tx BookTx) error) error

	// Acquire tries to take ownership of a shard's writer role. ok=false means another worker has it.
	Acquire(ctx context.Context, shard domain.Shard) (lease Lease, ok bool, err error)

	// HotConfig reads the recorded shard count; ok=false before the first start.
	HotConfig(ctx context.Context) (shards int, ok bool, err error)
	// EnsureHotConfig records the shard count on first start and refuses a different one after: two
	// processes hashing scopes differently keep two sets of books (FR-042).
	EnsureHotConfig(ctx context.Context, shards int) error

	// Shard state (hot_shards). EnsureShard creates the row at generation 1 if absent.
	EnsureShard(ctx context.Context, sh domain.Shard) (ShardRecord, error)
	SetFrozen(ctx context.Context, sh domain.Shard, frozen bool, reason string) error
	// BumpGen raises the generation by one and returns the new one.
	BumpGen(ctx context.Context, sh domain.Shard) (int64, error)
	SetNodeReplID(ctx context.Context, sh domain.Shard, id string) error

	// Transfer reads a transfer by key; found=false if no writer has booked its first leg yet.
	Transfer(ctx context.Context, realm domain.Realm, idemKey string) (TransferRecord, bool, error)

	// Booked reads a scope's watermark, booked balances and open suspense as one snapshot — what a
	// rehydrate installs and what reconcile compares against.
	Booked(ctx context.Context, scope domain.Scope) (BookedState, error)

	// ParkDead records an intent that exhausted its retry budget. It never drops it.
	ParkDead(ctx context.Context, d DeadIntent) error
}

// ArchivedEvent is one usage event's full detail, kept per event when the ledger books a rollup.
type ArchivedEvent struct {
	Scope           domain.Scope
	Gen, Seq        int64
	IdemKey         string
	PoolDraws       []domain.PoolDelta
	Credits         domain.Credits
	Resource        string
	RateKey         string
	RateCardVersion string
	Quantities      []domain.Quantity
	Subjects        domain.Subjects
	CostMicros      int64
	OccurredAt      time.Time
}

// UsageArchive is per-event detail behind a rollup ledger (LedgerGranularity=rollup). The default
// adapter is Postgres (usage_events); a columnar store is the scale-out path behind the same port.
type UsageArchive interface {
	// Append stores events idempotently on (scope, gen, seq): a retried batch adds nothing.
	Append(ctx context.Context, events []ArchivedEvent) error
	// Events reads a scope's events in [from, to), oldest first, for re-pricing and breakdowns.
	Events(ctx context.Context, scope domain.Scope, from, to time.Time, limit int) ([]ArchivedEvent, error)
}
