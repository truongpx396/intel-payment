package ports

import (
	"context"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Meter is the single entry point business code — and the billing module — depends on. It
// composes the PricerRegistry, the stores and the Ledger so callers never touch pricing math,
// keys, limits or the journal. An in-process Meter and a gRPC client both satisfy it (FR-044).
type Meter interface {
	// Admit gates before expensive work. It resolves the realm's configured limits for the
	// resource, sizes any with a max_entitlement from the QuotaSource, merges the caller's (which
	// can only tighten), and delegates to Ledger.Admit. A configured subject limit whose subject
	// the request does not carry is ErrMissingSubject.
	Admit(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (domain.Admission, error)

	// Record prices e under the realm's active card and settles it idempotently. Pass the
	// quantities ACTUALLY produced — partial on cancel (invariant 4). It refuses a stale
	// OccurredAt, and when the hot tier is unavailable — or Settlement=journal — it writes the
	// event to the spend journal first and returns Receipt.Deferred, so no usage is ever dropped
	// by an outage (invariant 16).
	Record(ctx context.Context, e domain.Event) (domain.Receipt, error)

	// Grant mints or removes credits. PRIVILEGED: separated at the RPC, the route, the authz
	// check and the audit trail (invariant 13).
	Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error)

	// Transfer moves credits pool → allocation. PRIVILEGED.
	Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error)
}

// LedgerWriter is the ONLY component that writes the account of record: credit_ledger,
// account_credits, the watermarks, suspense and the idempotency guards. It runs in
// `cmd/payment-worker`, never a request-serving tier, with ONE owner per shard at a time
// (a Postgres advisory lock). Correctness rests on idempotency rows and per-scope transaction
// locks, NOT on "only one replica runs"; single ownership exists for ordering and throughput.
type LedgerWriter interface {
	// Drain books a shard's intents IN PER-SCOPE SEQUENCE ORDER, batched, then XACKs them.
	// A redelivery, a gap and a sequence regression are told apart by the watermark plus the
	// idempotency row — never by timing (hot-path-consistency.md §3).
	Drain(ctx context.Context, shard domain.Shard) (booked int, err error)

	// Reconcile compares each recently booked scope's hot account with booked + open suspense AT
	// THE SAME SEQUENCE NUMBER. Intents in flight are `deferred`, not drift; a hot sequence behind
	// the watermark is a `regression` that freezes the shard. Real drift is healed on the HOT side
	// by compare-and-set and pages beyond Tolerance. It NEVER books a ledger row.
	Reconcile(ctx context.Context, shard domain.Shard, kind domain.ReconcileKind) (domain.ReconcileRun, error)

	// Rehydrate rebuilds a stale or brand-new account from booked + open suspense at
	// applied_seq, and only while it is still stale, so concurrent callers converge.
	Rehydrate(ctx context.Context, s domain.Scope) error

	// Recover runs freeze → drain → bump generation → unfreeze for a shard after a total loss,
	// a rollback or a reshard (hot-path-consistency.md §5).
	Recover(ctx context.Context, shard domain.Shard, reason string) error

	// Audit verifies booked == checkpoint + Σ ledger since the checkpoint, per scope and pool,
	// and advances the checkpoints that make archiving old ledger partitions safe.
	Audit(ctx context.Context, shard domain.Shard) (domain.AuditReport, error)
}
