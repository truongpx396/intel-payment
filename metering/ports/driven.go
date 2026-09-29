package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// ---------------------------------------------------------------- pricing --

// Pricer converts a metered Event into an internal debit under a card. It is the ONE
// product-specific port — and for anything linear it needs no code at all, only a card for
// the built-in `table` pricer.
type Pricer interface {
	// Price MUST be a PURE function of (card, e.RateKey, e.Quantities): deterministic and
	// side-effect-free, so the SAME computation runs at the call site, in a dispute replay and in
	// an audit and yields the SAME number. It MUST NOT read e.Attributes or e.Subjects for cost,
	// MUST NOT perform I/O, and MUST fail CLOSED (ErrUnpriceable) on an unknown rate key or unit.
	// Arithmetic is exact; rounding is UP, once, at the end.
	Price(ctx context.Context, card domain.RateCard, e domain.Event) (domain.Price, error)
}

// PricerRegistry resolves a card's Pricer by name. Every deployment has `table`; the reference
// pricers (`llmtoken`, `seat`, `storagebyte`) and a host's own are registered at wire time in
// cmd/. A card naming an unregistered pricer fails closed — this is how a container deployment
// serving several realms prices each with its own strategy.
type PricerRegistry map[string]Pricer

// ------------------------------------------------------- configuration data --

// Configuration is DATA (constitution VIII): prices, ceilings and pools are rows a host changes
// without a deploy. Adapters cache and invalidate on the database's own change notification, so a
// change reaches every process at once (invariant 18).

// RateCardStore serves immutable, versioned cards.
type RateCardStore interface {
	// Active returns the realm's one active card. ErrUnpriceable-class failure is the caller's to
	// map: no active card means the realm cannot price, and that fails closed.
	Active(ctx context.Context, realm domain.Realm) (domain.RateCard, error)
	// Get returns a specific version, active or retired — replay and audit re-price under it.
	Get(ctx context.Context, realm domain.Realm, version string) (domain.RateCard, error)
}

// LimitStore serves the realm's configured ceilings, ordered as configured (sort_order).
type LimitStore interface {
	Limits(ctx context.Context, realm domain.Realm) ([]domain.Limit, error)
}

// PoolStore serves the realm's configured credit pools. `general` is implicit and not returned.
type PoolStore interface {
	Pools(ctx context.Context, realm domain.Realm) ([]domain.PoolDef, error)
}

// Unlimited is a quota of -1: the entitlement grants no ceiling.
const Unlimited int64 = -1

// QuotaSource sizes a limit whose row names a max_entitlement. The entitlement module provides it
// (wired in cmd/); metering never imports that module. An error means the quota could not be
// resolved, and the Meter applies AdmitFailPolicy — it never treats the failure as unlimited.
type QuotaSource interface {
	Quota(ctx context.Context, s domain.Scope, key string) (int64, error)
}

// ---------------------------------------------------------------- hot tier --

// Ledger is the hot tier the Meter uses. It runs INSIDE request-serving tiers and NEVER waits on
// the durable store: every mutation is ONE Redis Function in the scope's shard slot that applies
// the change, advances the scope's sequence number and emits exactly one intent
// (hot-path-consistency.md §2). It never writes the ledger — the LedgerWriter books every intent.
//
// A hot-tier refusal reaches the caller as a typed domain error (ErrColdScope, ErrColdShard,
// ErrShardFrozen, ErrBackpressure, ErrHotStoreUnavailable); the Meter acts on each.
type Ledger interface {
	// Balance returns the HOT balance per pool — a fast copy. The ledger is the account of record.
	Balance(ctx context.Context, s domain.Scope) (map[domain.Pool]domain.Credits, error)

	// Admit is the admission GATE: one read of the account and the relevant counters, then every
	// Limit evaluated, MaxCost included, against the eligible pools plus the overdraft.
	// req.Limits is the FINAL, merged set. It does NOT reserve or pre-debit (see invariants).
	Admit(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (domain.Admission, error)

	// Debit settles usage: draws the eligible pools in priority order (the last absorbs
	// overshoot), increments the window counters, advances seq, writes the idempotency guard and
	// XADDs the intent — atomically, in one slot. Same key + same fingerprint = replay
	// (Applied=false); same key + different fingerprint = ErrIdemConflict, never a silent no-op.
	Debit(ctx context.Context, c domain.Charge) (domain.Receipt, error)

	// Grant adds or removes credits in one pool. A negative grant applies NegativeBalancePolicy
	// inside the same function; a floor is reported as a writeoff that the writer books as its own
	// row, so balance == SUM(ledger) still holds (invariant 12).
	Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error)

	// Transfer is phase 1: an atomic check-and-debit of the source's HOT pool that emits a
	// transfer_out intent, or ErrInsufficient. The writer completes it.
	Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error)
}

// HotDelta is a writer-issued hot mutation: a transfer's credit leg, a correction, an expiry.
type HotDelta struct {
	Op      domain.IntentOp // OpTransferIn | OpCorrection | OpExpiry
	Scope   domain.Scope
	Pool    domain.Pool
	Delta   domain.Credits // signed
	IdemKey string         // idempotent on its own key
	// CapAtAvailable limits a negative Delta to the pool's available (non-negative) balance. It is
	// how an expiry is pre-applied as an upper bound without ever driving a pool below zero.
	CapAtAvailable bool
	Payload        domain.Payload
}

// HotAccount is one consistent snapshot of a hot account (one key, so one atomic read).
type HotAccount struct {
	Cold    bool // the account is absent or was built in an older shard generation
	Seq     int64
	Blocked bool
	Pools   map[domain.Pool]domain.Credits
}

// RehydrateState is what the books say an account should hold, read from Postgres after the
// shard's stream is drained.
type RehydrateState struct {
	ShardGen   int64 // the generation read from hot_shards; the install is refused if Redis moved on
	AppliedSeq int64
	Blocked    bool
	Pools      map[domain.Pool]domain.Credits // booked + open suspense
}

// RehydrateOutcome reports ip_rehydrate.
type RehydrateOutcome int

const (
	RehydrateInstalled RehydrateOutcome = iota // the account was stale and is now current
	RehydrateAlready                           // another caller got there first — converge, do nothing
	RehydrateStaleGen                          // the shard's generation moved; re-read the books
)

// ShardState is the hot mirror of hot_shards.
type ShardState struct {
	Open   bool // meta exists; false is a fresh or emptied Redis (COLD_SHARD)
	Gen    int64
	Frozen bool
	Reason string
}

// BalanceStore is the hot-tier adapter (Redis Functions on the shard-tagged key layout). Beyond the
// Ledger the Meter uses, it carries the operations only the writer, reconcile and recovery need.
type BalanceStore interface {
	Ledger

	// ApplyDelta issues a writer hot mutation (transfer_in, correction, expiry) in the scope's slot.
	ApplyDelta(ctx context.Context, d HotDelta) (domain.Receipt, error)
	// Snapshot reads an account for reconcile.
	Snapshot(ctx context.Context, s domain.Scope) (HotAccount, error)
	// Rehydrate installs booked + suspense into a stale or new account, only while it is still stale.
	Rehydrate(ctx context.Context, s domain.Scope, st RehydrateState) (RehydrateOutcome, error)
	// Heal compare-and-sets pool balances if the account's seq is still observedSeq (ip_heal).
	// Reconcile's correction of a defect, on the HOT side only; false means a charge raced it.
	Heal(ctx context.Context, s domain.Scope, observedSeq int64, targets map[domain.Pool]domain.Credits) (bool, error)

	// Shard lifecycle, driven from hot_shards in Postgres (ip_shard).
	ShardState(ctx context.Context, sh domain.Shard) (ShardState, error)
	OpenShard(ctx context.Context, sh domain.Shard, gen int64) error
	BumpShard(ctx context.Context, sh domain.Shard, gen int64) error
	FreezeShard(ctx context.Context, sh domain.Shard, reason string) error
	UnfreezeShard(ctx context.Context, sh domain.Shard) error
	// NodeID is the replication id of the node currently serving the shard, so the writer can
	// notice a failover (hot_shards.node_replid) — the second way, besides a sequence regression,
	// of detecting that the hot tier lost history.
	NodeID(ctx context.Context, sh domain.Shard) (string, error)
	// Ping reports whether the hot tier is reachable (for /readyz).
	Ping(ctx context.Context) error
}

// ---------------------------------------------------------------- journal --

// JournalEntry is a usage charge that has not reached the hot tier yet.
type JournalEntry struct {
	ID            string
	Charge        domain.Charge // priced once, at Record; replayed unchanged
	Fingerprint   string
	Attempts      int
	NextAttemptAt time.Time
	CreatedAt     time.Time
}

// JournalStore is the durable home of a usage intent before the hot tier has it: on every call
// under Settlement=journal, and during a hot-store outage in any mode. The writer marks a row
// settled when it books the resulting intent, inside its own transaction.
type JournalStore interface {
	// Put records the charge. created=false with a nil error means an identical entry already
	// exists (an idempotent retry). The same key with a different fingerprint is ErrIdemConflict.
	Put(ctx context.Context, e JournalEntry) (created bool, err error)
	// Due returns pending entries whose next attempt is at or before now, oldest first.
	Due(ctx context.Context, now time.Time, limit int) ([]JournalEntry, error)
	// Retry records a failed replay and schedules the next attempt.
	Retry(ctx context.Context, id string, attempts int, next time.Time, lastErr string) error
	// Pending counts unsettled entries for a realm (metering_journal_pending).
	Pending(ctx context.Context, realm domain.Realm) (int64, error)
}

// ------------------------------------------------------------------- bus --

// Bus is the durable, at-least-once async seam for events and ticks. Both adapters satisfy it
// identically. Intents do not go through Publish: the hot functions write them (bus-subjects.md).
type Bus interface {
	// Publish appends to a subject's stream. Durable on return.
	Publish(ctx context.Context, subject string, payload []byte) error
	// Subscribe joins a named consumer group and delivers at-least-once. The handler returning
	// nil acks; an error redelivers after backoff, up to MaxAttempts, then the DLQ.
	Subscribe(ctx context.Context, subject, group string, h Handler) (Subscription, error)
	// Pending reports un-acked work for a group: the count and the age of the oldest.
	Pending(ctx context.Context, subject, group string) (count int64, oldest time.Duration, err error)
	// Trim drops acked history below a retention floor. A no-op on JetStream; REQUIRED on Redis
	// Streams, where nothing else does it. It never removes an un-acked entry.
	Trim(ctx context.Context, subject string, keep RetentionPolicy) error
}

// Handler processes one message; returning nil acknowledges it.
type Handler func(ctx context.Context, m Message) error

// Message is one delivery.
type Message struct {
	Subject  string
	ID       string // stream entry id / sequence — the adapter's, not ours
	Payload  []byte
	Attempts int // delivery count; drives DLQ escalation
}

// Subscription is a live consumer.
type Subscription interface{ Close() error }

// RetentionPolicy is the floor below which acked history may be trimmed.
type RetentionPolicy struct {
	MinAge time.Duration // keep at least this much history
	MaxLen int64         // trim toward this length once past MinAge (0 = no length target)
}

// ------------------------------------------------------------- collaborators --

// Clock is the only source of time. A core that reads the wall clock is not replayable, so window
// math, staleness and TTLs all take their "now" from here.
type Clock interface{ Now() time.Time }

// IDSource issues identifiers (UUID v7: time-ordered, so index inserts stay local).
type IDSource interface{ NewID() string }

// Labels are a metric's dimensions.
type Labels map[string]string

// Metrics exports the named metrics of the observability contract (metrics.go). Money never goes
// through a float: counts and gauges are int64; only durations are observed as float seconds.
type Metrics interface {
	Count(name string, n int64, l Labels)
	Gauge(name string, v int64, l Labels)
	Observe(name string, seconds float64, l Labels)
}
