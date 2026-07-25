# Contract: Metering & Credit Ledger (reusable ports)

**Plan**: [../plan.md](../plan.md) | **Status**: Design addition — the reusability seam for the credit-metering backbone (research §3, §21; SC-006). **Changes no Phase 1 behavior.** It factors the existing Redis-hot-balance + outbox + durable-ledger + reconcile machinery into three ports so the *same* engine drops into other systems without touching their tenancy model, their spend source, or their bus vocabulary.

The Phase 1 credit machinery is already production-grade, but it is packaged as an app-internal kernel module welded to three of *this* app's assumptions. This contract names the seam that removes that welding. Ports are given in Go (the kernel language); the LLM-token metering used by ContextEngine is presented at the end as **one implementation** of these ports, not as the core.

---

## Why: the three couplings this removes

| # | Today's coupling | Evidence it is a coupling | The port that removes it |
|---|---|---|---|
| 1 | Billing subject is a hard-coded `workspace_id` column | Phase 2 had to **re-anchor `billing_customers`/`subscriptions`/`payments`/`workspace_credits` from `workspace_id` → `organization_id`** ([draft-plan.md](../../draft-plan.md#phase-2--tenancy--delegated-administration)) — a schema migration, not a config change | `Scope` — an opaque billing subject the engine never interprets |
| 2 | Spend is welded to LLM tokens | `cost_usd_micros`, `llm_call_log`, `token_budget_day`, cost computed inline from returned `input/output_tokens` ([llm-gateway.md](./llm-gateway.md#L77)) | `Pricer` + `Event` — a deterministic, domain-agnostic cost function over metered quantities |
| 3 | Bus subjects + Redis keys are workspace-shaped | `billing.deduct.<ws>`, `credit:{ws}:balance`, RLS on `app.workspace_id` | `Scope.Tag()` — canonical key/subject derivation; the host decides what a scope *is* |

The rule: **the metering kernel is generic; only the `Pricer` implementation and the `Scope` binding are product-specific.** Everything that is release-blocking today (single-writer, idempotency, admission-gate, reconcile) is preserved verbatim — it just stops assuming "workspace" and "LLM token."

---

## Ports at a glance

```text
CALLER (any runtime, any domain)
   │  Admit(scope, limits…)            ── admission gate (no reservation)
   │  Record(event)                    ── price + settle, idempotent
   ▼
┌── Meter (orchestration) ─────────────────────────────────────────────┐
│   Pricer.Price(event)  → Credits (+ informational CostMicros)   PURE  │
│   Ledger.Debit(charge) → fast path: atomic Redis DECRBY + outbox LPUSH │
└───────────────────────────────────────────────┬───────────────────────┘
        returns now (no durable-store wait)      │  outbox:{shard}
                                                 ▼
┌── LedgerWriter (SOLE durable writer · single-owner worker role) ──────┐
│   Drain      LPOP outbox:{shard} → INSERT credit_ledger (idem UNIQUE) │
│   Reconcile  expected = SUM(ledger); heal drift; ALARM if > tolerance │
│   Rehydrate  rebuild hot balance from ledger (cold-start / Redis loss)│
└───────────────────────────────────────────────────────────────────────┘
```

Three seams a host can swap independently: the **`Pricer`** (its domain), the **`Ledger`/`LedgerWriter`** backend (its infra), and the **`Scope`** binding (its tenancy). The reference impl uses Redis+Postgres+NATS, but nothing in the port signatures requires them.

---

## Domain types

```go
package metering

// Scope is the opaque metering/billing subject. The HOST decides what it means
// (workspace, organization, user, project, api_key); the kernel treats it only as
// an identity + a key/subject source. Parameterizing this is what makes the
// workspace→organization move (and any future re-anchor) a config change, not a migration.
type Scope struct {
	Kind string // host-defined: "workspace" | "organization" | "user" | "project" | …
	ID   string // opaque, stable
}

// Tag is the Redis hash-tag / NATS token for this scope: keys `credit:{ws:abc}:balance`
// colocate on one slot so a scope's ops stay atomically scriptable under Redis Cluster
// (research §12). Subjects derive as `billing.deduct.<tag>`.
func (s Scope) Tag() string { return s.Kind + ":" + s.ID }

// Credits is the single INTERNAL accounting unit — a signed delta.
// Negative = debit (consumption); positive = credit (grant). Never a float. (draft-plan §Ledger sign convention)
type Credits int64

// Money is a FIAT amount, used only at the payments boundary (Phase 2). Integer minor
// units + ISO-4217 currency; never floats. Kept distinct from Credits: pricing (fiat→credits)
// is decoupled from consumption (credits), so changing a price never alters granted credits.
type Money struct {
	MinorUnits int64  // e.g. cents
	Currency   string // ISO-4217, e.g. "USD"
}

// Unit is a host-extensible metered dimension. The kernel ships none as special.
type Unit string // "llm_input_token" | "llm_output_token" | "storage_byte_day" | "api_call" | "seat" | "compute_ms" | …

type Quantity struct {
	Unit   Unit
	Amount int64 // integer count of Unit
}

// Event is one metered occurrence. A single event may carry several quantities
// (e.g. input + output tokens). IdemKey is REQUIRED — it is the retry/dedup identity
// that makes Record exactly-once (SC-006, FR-019).
type Event struct {
	Scope      Scope
	Resource   string            // host taxonomy: "llm.chat" | "storage" | "connector.sync" | …
	RateKey    string            // pricing selector (model / storage-tier / sku) — a LEGITIMATE cost input
	Quantities []Quantity        // integer counts; priced by the Pricer over RateKey + Quantities
	IdemKey    string            // REQUIRED
	OccurredAt time.Time
	Attributes map[string]string // provider, feature, user_id, trace_id — usage-log/audit ONLY, never a cost input
}

// Price is the output of pricing an Event: the internal debit plus an informational
// external cost for dashboards (maps to today's cost_usd_micros / llm_call_log).
type Price struct {
	Credits    Credits // amount to debit (as a positive magnitude; Debit applies it as negative)
	CostMicros int64   // provider cost in USD micros — informational, for usage analytics only
}

// Window classifies a limit counter's reset behavior.
type Window int

const (
	Balance Window = iota // no reset — the durable credit balance (→ 402 when exhausted)
	Daily                 // calendar-day counter (→ 429)
	Hourly                // calendar-hour counter (→ 429)
	Rolling               // sliding window of Limit.Dur (→ 429)
)

// Limit is one ceiling evaluated at admission. The generic form of the three ceilings
// (workspace balance · per-role daily token budget · per-user cap). DenyCode maps a breach
// to the wire error the host already speaks.
type Limit struct {
	Name     string        // stable, surfaced in the deny reason: "workspace_balance" | "role_daily" | "user_daily"
	Scope    Scope         // whose counter (MAY differ from the charge scope — e.g. per-user within a workspace)
	Unit     Unit          // what the ceiling is measured in
	Max      int64         // the ceiling
	Window   Window
	Dur      time.Duration // used only when Window == Rolling
	WarnAt   float64       // 0..1 near-limit fraction for a soft warning (default 0.8, admin-configurable)
	DenyCode string        // "payment_required" (402) | "limit_reached" (429)
}

// Admission is the gate decision. Allowed=false blocks BEFORE any expensive work.
type Admission struct {
	Allowed  bool
	Exceeded *Limit // which ceiling blocked (nil when Allowed)
	Warning  *Limit // a ceiling crossed WarnAt but not Max — drives the credit_warning notification (FR-035)
}

// Charge settles a completed unit of work. Grant adds/removes credits (payments, refunds, admin).
// Both are idempotent on IdemKey and carry a Reason (→ credit_ledger.operation_type).
type Charge struct {
	Scope   Scope
	Amount  Credits           // positive magnitude to debit
	IdemKey string            // REQUIRED
	Reason  string            // operation_type: "query" | "ingest" | "enrich" | "caption" | …
	Ref     map[string]string // trace_id, call_id — audit metadata
}

type Grant struct {
	Scope   Scope
	Amount  Credits           // positive to add; NEGATIVE for refund/chargeback/expiry
	IdemKey string            // REQUIRED (e.g. provider_payment_id / invoice_id)
	Reason  string            // "purchase" | "subscription_grant" | "signup" | "refund" | "chargeback" | "admin_adjustment"
	Ref     map[string]string
}

// Receipt is the outcome of an idempotent apply. Applied=false ⇒ this IdemKey was already
// settled and this call was a no-op (a replay) — the caller can rely on that to stay exactly-once.
type Receipt struct {
	IdemKey string
	Applied bool    // false = idempotent replay, already applied
	Delta   Credits // the signed delta this call applied (0 on replay)
	Balance Credits // resulting hot balance (eventual; informational)
}
```

---

## Port: `Pricer` — domain-specific, everything else is not

```go
// Pricer converts a metered Event into an internal debit. It is the ONE product-specific
// port: the LLM-token pricer is one implementation; a storage, seat, or api-call pricer is another.
type Pricer interface {
	// Price MUST be a PURE function of e.RateKey + e.Quantities and a versioned, injected rate
	// card: deterministic and side-effect-free, so the SAME computation runs at the call site,
	// during reconciliation, and in an audit replay and yields the SAME number.
	// It MUST NOT read e.Attributes for cost math (those are for the usage log only) and
	// MUST NOT perform I/O on the hot path.
	Price(ctx context.Context, e Event) (Price, error)
}
```

**Why pure matters:** reconciliation (`LedgerWriter.Reconcile`) and dispute audits both re-derive cost from the immutable event/quantities. A pricer that reaches for the network, a clock, or mutable global rates cannot be replayed, and the ledger stops being auditable. Rate cards are **versioned and injected**; a price change is a new rate-card version, never an in-place edit — which is also why "changing a plan's price never affects already-granted credits" (draft-plan §Design principles) holds by construction.

---

## Port: `Ledger` — the hot path (request-serving tier)

```go
// Ledger is the fast, non-durable balance tier. It runs INSIDE request-serving tiers.
// It NEVER blocks on the durable store: Debit/Grant do the atomic Redis op + enqueue the
// durable-write intent and return. The durable write is LedgerWriter's job (below).
type Ledger interface {
	// Balance returns the authoritative hot balance for a scope.
	Balance(ctx context.Context, s Scope) (Credits, error)

	// Admit is the admission GATE — it reads the balance + evaluates each Limit and refuses
	// when one is already at/over its ceiling. It does NOT reserve or pre-debit (see invariants).
	Admit(ctx context.Context, s Scope, limits ...Limit) (Admission, error)

	// Debit settles a completed unit of work on the fast path. Idempotent on c.IdemKey:
	//   1. atomically in Redis: DECRBY balance:{tag} + LPUSH outbox:{shard} {intent}
	//      + SET NX billing:applied:{tag}:{idem}   (all-or-nothing; one Lua script / MULTI)
	//   2. return immediately (no durable-store wait)
	// A replay (SET NX miss) returns Receipt{Applied:false} without a second decrement.
	Debit(ctx context.Context, c Charge) (Receipt, error)

	// Grant mirrors Debit for positive/negative credit changes (payments, refunds, admin).
	// Same atomic Redis step + outbox intent + idempotency guard.
	Grant(ctx context.Context, g Grant) (Receipt, error)
}
```

## Port: `Meter` — orchestration callers actually use

```go
// Meter is the single entry point business code depends on. It composes Pricer (cost) +
// Ledger (account) so callers never touch pricing math or balance keys directly.
type Meter interface {
	// Admit gates before expensive work. Thin pass-through to Ledger.Admit with the
	// host's configured Limits for this scope/resource.
	Admit(ctx context.Context, s Scope, limits ...Limit) (Admission, error)

	// Record prices e and settles the debit idempotently. For a stream, pass the quantities
	// ACTUALLY produced (partial on cancel) — see the partial-billing invariant.
	// Record writes the usage/analytics record and enqueues the debit in ONE atomic step
	// (see the atomicity invariant) so a crash cannot log a call without billing it, or vice versa.
	Record(ctx context.Context, e Event) (Receipt, error)
}
```

## Port: `LedgerWriter` — the sole durable writer (single-owner worker)

```go
// LedgerWriter is the ONLY component that writes the durable account of record.
// Exactly one owner: the dedicated worker role (research §15) — never a request tier.
// Correctness rests on idempotent atomic claims, NOT on "only one replica runs."
type LedgerWriter interface {
	// Drain pops queued intents for a shard and writes credit_ledger rows.
	// At-least-once (a crash after LPOP redelivers); credit_ledger UNIQUE(idem_key)
	// collapses any retry to exactly one row (SC-006).
	Drain(ctx context.Context, shard Shard) (drained int, err error)

	// Reconcile recomputes expected = SUM(credit_ledger delta) per scope for a (shard, bucket),
	// compares to the live hot balance, writes an operation_type='reconcile' row to heal drift,
	// and RAISES AN ALARM when |drift| exceeds Tolerance (drift is a signal, not just a heal).
	// Guarded by SET NX reconcile:lock:{shard}:{bucket} so a duplicate tick is a no-op.
	Reconcile(ctx context.Context, shard Shard, bucket time.Time) (ReconcileReport, error)

	// Rehydrate rebuilds a scope's hot balance from the durable ledger (cold-start, or after a
	// Redis loss) under a per-scope lock, so serving never resumes on an empty/short balance.
	Rehydrate(ctx context.Context, s Scope) (Credits, error)
}

// Shard is the workspace/scope partition key. Phase 1 runs one drainer + one reconciler;
// the shard lets Phase 4 run N in parallel with no key-space redesign and no double-apply (research §3).
type Shard string

type ReconcileReport struct {
	Scope     Scope
	Expected  Credits // SUM(ledger)
	Observed  Credits // live hot balance
	Drift     Credits // Observed - Expected
	Tolerance Credits
	Healed    bool
	Alarmed   bool // true when |Drift| > Tolerance — pages, does not silently heal
}
```

---

## Invariants every implementation MUST uphold

1. **Single durable writer.** Exactly one component (`LedgerWriter`, the worker role) writes `credit_ledger` and mutates the durable balance. Every other producer *publishes intents* and never touches the ledger (research §3, SC-006). No second money-writer — not the LLM gateway, not a payment provider callback, not a request handler.
2. **Dual idempotency guard.** Every credit-affecting apply carries an `IdemKey`. A Redis `SET NX billing:applied:{tag}:{idem}` is the fast-path no-op; `credit_ledger UNIQUE(idem_key)` is the durable backstop. The Redis guard is **not** a correctness primitive under async-replication failover (the Redlock critique) — correctness always rests on the Postgres unique index (research §12). `Receipt.Applied=false` reports a replay.
3. **Admission is a gate, not a reservation.** `Admit` reads current balance/counters and refuses an over-limit call; it does not pre-debit. Concurrent admits against a near-empty balance may settle it slightly negative — **bounded overshoot** ≈ *in-flight concurrency × per-call ceiling* — accepted in Phase 1 and healed at settlement + hourly reconcile ([llm-gateway.md](./llm-gateway.md#L77-L78)). A hard per-call reservation is an opt-in, not the default (it either serializes a scope or doubles ledger writes).
4. **Partial settlement on cancel.** A unit of work aborted midway (e.g. a cancelled stream) is billed for what it **actually produced** — not zero (which makes "start then abort" a free-usage exploit) and not the full ceiling (which over-bills). Callers pass the real produced `Quantities` to `Record` ([llm-gateway.md](./llm-gateway.md#L79)).
5. **Deterministic, replayable pricing.** `Pricer.Price` is pure over `RateKey` + `Quantities` + a versioned rate card (see the Pricer port); it fails **closed** on an unknown `RateKey`/`Unit` (never prices at zero). Reconciliation and audits re-derive cost from the immutable event.
6. **Integer money only.** `Credits int64` (signed delta) internally; `Money{MinorUnits, Currency}` at the fiat boundary. Never floats, anywhere.
7. **Documented RPO direction on hot-store loss.** The hot path does `DECRBY + LPUSH` atomically in Redis and returns before the durable write. If Redis loses that atomic pair before `Drain` runs (AOF `everysec` window, or a failover), the ledger row is never written, and `Reconcile` — which trusts the ledger as source of truth — heals the balance **upward**, i.e. the charge is silently forgotten. **This is under-billing, not double-billing, and the ledger is authoritative.** Implementations MUST state this direction and pick a `SettlementDurability` (below) accordingly; they MUST NOT imply reconcile makes every loss whole. *(Closes the "reconcile heals in the under-bill direction" gap.)*
8. **Atomic usage-log + debit enqueue.** `Record` MUST write the usage/analytics record and enqueue the debit intent in **one** atomic step (one Lua script over both Redis structures, or one Postgres tx in `journal` mode) — never as two independent best-effort writes, which could log a call with no debit or debit with no log. *(Closes the "per-call chain lacks the outbox's atomicity" gap.)*
9. **Reconcile alarms on drift beyond tolerance.** Out-of-tolerance drift pages (`ReconcileReport.Alarmed`); it is never silently absorbed by a `reconcile` row. `Tolerance` is configured, not implicit.
10. **Scope opacity.** The kernel never parses, ranks, or special-cases a `Scope`. Re-anchoring the billing subject (workspace→organization→…) changes only the `Scope` the host constructs and the RLS predicate — never a kernel signature.

---

## Settlement durability (customizable knob)

One config selects the durability/latency trade for `Record`/`Debit`, without changing any port signature:

| `SettlementDurability` | Hot-path cost | On hot-store loss | Use when |
|---|---|---|---|
| `outbox` (default) | 1 atomic Redis step, no durable-store wait | Accepts bounded silent **under-bill** (invariant 7); reconcile heals balance | Metering commodity spend (LLM tokens) where a sub-cent RPO gap is acceptable for sub-ms enforcement |
| `journal` | +1 synchronous durable write (Postgres spend-journal / transactional outbox) before returning | No under-bill; the durable intent survives a Redis loss | High-value units, regulated billing, or when revenue-loss RPO must be ~zero |

Both satisfy invariants 1–2 and 8; they differ only in *when* the durable intent becomes crash-safe. A host picks per deployment (or per resource) — this is the main "how strict is my money" dial.

---

## Reference wiring: ContextEngine LLM metering as ONE implementation

The Phase 1 system is these ports bound to the LLM domain — nothing in the kernel knows that:

| Generic port / type | ContextEngine (Phase 1) binding |
|---|---|
| `Scope` | `{Kind: "workspace", ID: workspace_id}` → Phase 2 `{Kind: "organization", …}` — a binding change, no kernel edit |
| `Pricer` | `LLMTokenPricer` (product tier): `Credits` from `input_tokens·rate_in + output_tokens·rate_out`, `CostMicros` → `llm_call_log.cost_usd_micros` |
| `Event.Quantities` | `[{llm_input_token, n}, {llm_output_token, m}, {cached_token, c}]` from the gateway's returned usage |
| `Limit[]` (three ceilings) | `workspace_balance` (Balance→402) · `role_daily` on `agent_policies.token_budget_day` (Daily→429) · `user_daily` (Daily→429), `WarnAt=0.8` |
| `Charge.Reason` / `Grant.Reason` | `credit_ledger.operation_type`: `query`/`ingest`/`enrich`/`caption` · `purchase`/`subscription_grant`/`refund`/`reconcile` |
| `Ledger` / `LedgerWriter` | Redis `DECRBY` + `outbox:{shard}` → Go kernel billing worker → `credit_ledger` + hourly reconcile (research §3) |
| Payments (`Grant` producers) | Phase 2 `PaymentProvider` adapters publish `billing.grant.<tag>` on a verified webhook ([draft-plan.md](../../draft-plan.md#phase-2-billing-and-payments)) |

Swapping ContextEngine for, say, a storage-metering product = write a `StorageBytePricer`, set `Scope.Kind="account"`, define its `Limit`s. The ledger, outbox, reconcile, idempotency, and payments code are untouched.

---

## Reference `Pricer`: `LLMTokenPricer`

The concrete implementation behind the wiring table — pure, versioned, fails closed. Lives in the product tier (`internal/pricing/`), depends only on `kernel/metering`.

```go
package pricing // backend-go/internal/pricing

import (
	"context"
	"fmt"

	"github.com/aisat/backend-go/kernel/metering"
)

// Units this pricer understands. Convention: input = NON-cached input tokens,
// cached = cache-read input tokens (billed at a discount), output = generated tokens —
// decomposed by the caller so the pricer is a plain sum (no double count).
const (
	UnitInputToken  metering.Unit = "llm_input_token"
	UnitCachedToken metering.Unit = "llm_cached_token"
	UnitOutputToken metering.Unit = "llm_output_token"
)

// ModelRate is provider cost in USD micros per token.
type ModelRate struct {
	InMicros     int64 // per input token
	CachedMicros int64 // per cache-read token (usually << InMicros)
	OutMicros    int64 // per output token
}

// RateCard is IMMUTABLE + versioned. A price change publishes a new Version; rows are
// never edited in place, so past ledger + reconcile replays stay auditable.
type RateCard struct {
	Version         string               // e.g. "2026-07-01"
	MicrosPerCredit int64                // USD micros that 1 internal credit represents (margin baked in). MUST be > 0.
	Models          map[string]ModelRate // keyed by RateKey (model id), e.g. "gpt-4o"
}

// LLMTokenPricer implements metering.Pricer.
type LLMTokenPricer struct{ card RateCard }

func NewLLMTokenPricer(card RateCard) (*LLMTokenPricer, error) {
	if card.MicrosPerCredit <= 0 {
		return nil, fmt.Errorf("pricing: MicrosPerCredit must be > 0, got %d", card.MicrosPerCredit)
	}
	if len(card.Models) == 0 {
		return nil, fmt.Errorf("pricing: rate card %q has no models", card.Version)
	}
	return &LLMTokenPricer{card: card}, nil
}

// Price is PURE: reads only e.RateKey + e.Quantities + the injected card. No I/O, no clock,
// no e.Attributes. Fails CLOSED on an unknown model or unit — never prices unpriceable usage at zero.
func (p *LLMTokenPricer) Price(_ context.Context, e metering.Event) (metering.Price, error) {
	rate, ok := p.card.Models[e.RateKey]
	if !ok {
		return metering.Price{}, fmt.Errorf("pricing: no rate for %q in card %q", e.RateKey, p.card.Version)
	}
	var costMicros int64
	for _, q := range e.Quantities {
		if q.Amount < 0 {
			return metering.Price{}, fmt.Errorf("pricing: negative quantity for unit %q", q.Unit)
		}
		switch q.Unit {
		case UnitInputToken:
			costMicros += q.Amount * rate.InMicros
		case UnitCachedToken:
			costMicros += q.Amount * rate.CachedMicros
		case UnitOutputToken:
			costMicros += q.Amount * rate.OutMicros
		default:
			return metering.Price{}, fmt.Errorf("pricing: unpriceable unit %q", q.Unit)
		}
	}
	// credits = ceil(costMicros / MicrosPerCredit) — round UP so rounding never under-bills.
	credits := (costMicros + p.card.MicrosPerCredit - 1) / p.card.MicrosPerCredit
	return metering.Price{Credits: metering.Credits(credits), CostMicros: costMicros}, nil
}
```

Notes that make it production-standard: **integer-only** math (no float touches money); **round-up** on the credit conversion so rounding is always platform-favorable (aligns with the never-under-bill posture, invariant 7); **fail-closed** on unknown model/unit; and rate cards are **swapped by value, never mutated**, so a price change is a new version and old ledger rows re-price identically.

---

## Contract-test skeleton

These validate *any* implementation of the ports against the invariants, so a swapped `Pricer` or a service-mode `Ledger` is held to the same guarantees. The `Pricer` suite is pure (no infra); the `Ledger` suite runs against a real impl via Testcontainers (`//go:build integration`, per the repo test convention).

```go
package metering_test

// PricerContract runs against ANY metering.Pricer — pure, no infra.
func PricerContract(t *testing.T, p metering.Pricer) {
	ctx := context.Background()
	base := metering.Event{
		Scope: metering.Scope{Kind: "workspace", ID: "w1"}, Resource: "llm.chat", RateKey: "gpt-4o",
		Quantities: []metering.Quantity{
			{Unit: pricing.UnitInputToken, Amount: 1000},
			{Unit: pricing.UnitOutputToken, Amount: 500},
		}, IdemKey: "e1",
	}

	t.Run("deterministic", func(t *testing.T) {
		a, err := p.Price(ctx, base); mustNoErr(t, err)
		b, _ := p.Price(ctx, base)
		if a != b { t.Fatalf("non-deterministic: %+v != %+v", a, b) }
	})
	t.Run("audit attributes never affect price", func(t *testing.T) {
		withAttrs := base; withAttrs.Attributes = map[string]string{"user_id": "u9", "trace_id": "t9"}
		a, _ := p.Price(ctx, base); b, _ := p.Price(ctx, withAttrs)
		if a != b { t.Fatalf("attributes changed price: %+v != %+v", a, b) }
	})
	t.Run("empty event is free", func(t *testing.T) {
		z := base; z.Quantities = nil
		got, err := p.Price(ctx, z); mustNoErr(t, err)
		if got.Credits != 0 || got.CostMicros != 0 { t.Fatalf("empty event not free: %+v", got) }
	})
	t.Run("fails closed on unknown rate key", func(t *testing.T) {
		u := base; u.RateKey = "no-such-model"
		if _, err := p.Price(ctx, u); err == nil { t.Fatal("unknown model must error, never price at zero") }
	})
	t.Run("credits round up (never under-bill)", func(t *testing.T) {
		one := base; one.Quantities = []metering.Quantity{{Unit: pricing.UnitInputToken, Amount: 1}}
		got, err := p.Price(ctx, one); mustNoErr(t, err)
		if got.CostMicros > 0 && got.Credits < 1 { t.Fatalf("rounding under-billed: %+v", got) }
	})
	t.Run("monotonic in quantity", func(t *testing.T) {
		more := base; more.Quantities = []metering.Quantity{{Unit: pricing.UnitInputToken, Amount: 2000}, {Unit: pricing.UnitOutputToken, Amount: 500}}
		a, _ := p.Price(ctx, base); b, _ := p.Price(ctx, more)
		if b.Credits < a.Credits { t.Fatalf("more tokens cost fewer credits: %d < %d", b.Credits, a.Credits) }
	})
}

// LedgerContract runs against a REAL metering.Ledger (Testcontainers Redis).  //go:build integration
func LedgerContract(t *testing.T, newLedger func(t *testing.T) metering.Ledger) {
	ctx := context.Background()
	ws := metering.Scope{Kind: "workspace", ID: uuidv7()}
	balanceOnly := metering.Limit{Name: "workspace_balance", Scope: ws, Unit: "credit", Max: 0, Window: metering.Balance, DenyCode: "payment_required"}

	t.Run("debit is idempotent on idem_key", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 1000)
		c := metering.Charge{Scope: ws, Amount: 100, IdemKey: "chg-1", Reason: "query"}
		r1, err := l.Debit(ctx, c); mustNoErr(t, err)
		if !r1.Applied { t.Fatal("first debit should apply") }
		r2, err := l.Debit(ctx, c); mustNoErr(t, err) // replay
		if r2.Applied { t.Fatal("replay must be a no-op (Applied=false)") }
		if bal, _ := l.Balance(ctx, ws); bal != 900 { t.Fatalf("double-charged: balance=%d want 900", bal) }
	})
	t.Run("admit gates without reserving (no pre-debit)", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 50)
		adm, err := l.Admit(ctx, ws, balanceOnly); mustNoErr(t, err)
		if !adm.Allowed { t.Fatal("positive balance should admit") }
		if bal, _ := l.Balance(ctx, ws); bal != 50 { t.Fatalf("admit pre-debited: balance=%d want 50", bal) }
	})
	t.Run("admit refuses an exhausted balance with the right code", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 0)
		adm, _ := l.Admit(ctx, ws, balanceOnly)
		if adm.Allowed { t.Fatal("empty balance must be refused") }
		if adm.Exceeded == nil || adm.Exceeded.DenyCode != "payment_required" { t.Fatal("must report the 402 limit") }
	})
	t.Run("grant is idempotent on idem_key (replayed webhook)", func(t *testing.T) {
		l := newLedger(t)
		g := metering.Grant{Scope: ws, Amount: 500, IdemKey: "pay-1", Reason: "purchase"}
		_, _ = l.Grant(ctx, g); _, _ = l.Grant(ctx, g)
		if bal, _ := l.Balance(ctx, ws); bal != 500 { t.Fatalf("replayed grant double-credited: %d want 500", bal) }
	})
}

// Wiring — the concrete impls plug into the shared suites:
//   func TestLLMTokenPricer_Contract(t *testing.T) { p, _ := pricing.NewLLMTokenPricer(testCard()); PricerContract(t, p) }
//   func TestRedisLedger_Contract(t *testing.T)   { LedgerContract(t, func(t *testing.T) metering.Ledger { return newRedisLedger(t, startRedis(t)) }) }
```

---

## Deployment topology: embedded library **or** standalone runtime service

**Yes — the metering/credit/billing engine can run as its own runtime service, and the ports are what make it a config+wiring change rather than a rewrite** (the same library↔service move the LLM gateway makes, research §21). Two shapes, one set of interfaces:

1. **Embedded library (Phase 1 default).** Callers import `kernel/metering`; `Admit`/`Debit` are in-process Redis ops — sub-ms, no extra network hop. Note the durable half is *already* its own runtime: `LedgerWriter` runs in the single-owner `cmd/worker` role, event-driven over NATS.
2. **Standalone metering service.** Wrap the *same* ports behind a thin gRPC/HTTP facade (`MeteringService`), hand callers a client stub, and the service owns the credit Redis + outbox. Only the `Meter` binding changes (in-process impl → client stub); caller logic does not.

| Concern | Extract as a service? | Note |
|---|---|---|
| Durable writer (`Drain`/`Reconcile`/`Rehydrate`) | ✅ **already separate** | it is the `cmd/worker` role — nothing to do |
| Producer → ledger decoupling | ✅ already | `billing.deduct.<tag>` NATS subject already crosses the boundary |
| Hot-path `Admit`/`Debit` | ✅ but adds one network hop | co-locate (same node/AZ, or a sidecar over a Unix socket); keep the atomic `DECRBY + LPUSH` server-side = one round trip |
| `Scope` / tenancy context | ✅ | already passed explicitly — no ambient RLS needed at the port |
| Pricing | ✅ | `Pricer` runs at the call site *or* inside the service; pure + versioned either way |
| Single-writer invariant | ✅ preserved | the service simply *becomes* the one writer |

What makes it **not free** (decide these before extracting):
- **Latency** — `Admit` is on every request's critical path. A hop is fine co-located; a cross-region metering service taxes every call. Keep the API coarse (one atomic op), never chatty.
- **Availability** — the service becomes **Tier-0** (like the gateway). Pick fail-open (serve, risk bounded overspend healed by reconcile) vs fail-closed (block) explicitly; the admission-gate + bounded-overshoot posture already tolerates brief fail-open windows.
- **Auth between caller ↔ service** — it mutates money, so callers authenticate (mTLS / signed service token); a compromised caller must not forge `Grant`s. Grants stay behind the payments/webhook path, never an open `Debit` API.
- **Redis ownership moves with the service** — it owns `credit:{tag}:balance`, `outbox:{shard}`, `billing:applied` (the role-partitioned DBs in research §11 already isolate these cleanly).

**Recommended posture:** start **embedded**; extract to a **co-located service** the moment a *second* product needs to draw on the same credit pool — at which point it is a deploy/config change plus a client stub, exactly the guidance the LLM gateway already follows ("MAY start library-mode and swap to the service later with no caller changes").

---

## Service surface: `MeteringService` (gRPC, contract-locked)

The service-mode facade is the SAME `Meter`/`Ledger` ports over the wire. gRPC (not REST) because this is an internal, latency-sensitive, strongly-typed hot path. Money crosses the wire as `int64` credits (never floats); `idem_key` is required on every mutating RPC; `Scope` stays opaque `{kind,id}`.

```proto
syntax = "proto3";
package metering.v1;
option go_package = "github.com/acme/metering/api/meteringv1";
import "google/protobuf/timestamp.proto";

message Scope    { string kind = 1; string id = 2; }              // opaque to the service
message Quantity { string unit = 1; int64 amount = 2; }           // integer counts only

message Event {
  Scope scope = 1;
  string resource = 2;
  string rate_key = 3;                       // pricing selector (model / tier / sku)
  repeated Quantity quantities = 4;
  string idem_key = 5;                       // REQUIRED — exactly-once identity
  google.protobuf.Timestamp occurred_at = 6;
  map<string,string> attributes = 7;         // audit only — never a cost input
}

enum Window { BALANCE = 0; DAILY = 1; HOURLY = 2; ROLLING = 3; }
message Limit {
  string name = 1; Scope scope = 2; string unit = 3; int64 max = 4;
  Window window = 5; int64 dur_seconds = 6;  // ROLLING only
  double warn_at = 7; string deny_code = 8;  // "payment_required" | "limit_reached"
}
message Admission { bool allowed = 1; Limit exceeded = 2; Limit warning = 3; }
message Receipt   { string idem_key = 1; bool applied = 2; int64 delta = 3; int64 balance = 4; }

message AdmitRequest   { Scope scope = 1; repeated Limit limits = 2; }
message RecordRequest  { Event event = 1; }
message GrantRequest   { Scope scope = 1; int64 amount = 2; string idem_key = 3;
                         string reason = 4; map<string,string> ref = 5; }
message BalanceRequest { Scope scope = 1; }
message BalanceReply   { int64 credits = 1; }

service Metering {
  rpc Admit      (AdmitRequest)   returns (Admission);   // gate, no reservation — hot path, keep co-located
  rpc Record     (RecordRequest)  returns (Receipt);     // price + settle; idempotent on event.idem_key
  rpc Grant      (GrantRequest)   returns (Receipt);     // PRIVILEGED: add/remove credits; idempotent
  rpc GetBalance (BalanceRequest) returns (BalanceReply);
}
```

Contract rules baked into the surface:

- **`Record` prices *inside* the service.** The whole `Meter` (Pricer + Ledger) moves server-side, so the client stub is just a *remote `Meter`* — pricing has one home and rate cards never ship to callers. In a multi-product deployment the service holds each product's rate card keyed by `rate_key`; the `Pricer` strategy is selected by `Unit`s. (Library mode injects the `Pricer` in-process instead — identical semantics.)
- **No streaming RPC for partial billing.** A cancelled `chat_stream` still settles with ONE terminal `Record` carrying the quantities actually produced (invariant 4). Settlement is a single call, not a stream.
- **`Grant` is privileged**, separated from `Record` at the RPC level: only the payments/webhook path and admin tools may call it (mTLS + authz), so a compromised spend producer can debit-with-idempotency but never mint credits.
- **Deadlines + fail policy are the caller's.** `Admit` is on the critical path; callers set a tight deadline and a documented fail-open/closed policy (see Deployment topology). The service is stateless per-RPC; all state is in its Redis + Postgres.

The generated client is wrapped so it satisfies the `Meter` interface — business code depends on `metering.Meter`, not on gRPC:

```go
// adapters/driving/grpcclient — a remote Meter. Callers can't tell it from the in-process one.
type Client struct{ c meteringv1.MeteringClient }

func New(conn *grpc.ClientConn) *Client { return &Client{meteringv1.NewMeteringClient(conn)} }

func (m *Client) Admit(ctx context.Context, s metering.Scope, limits ...metering.Limit) (metering.Admission, error) { /* map → RPC → map */ }
func (m *Client) Record(ctx context.Context, e metering.Event) (metering.Receipt, error)                            { /* map → RPC → map */ }

var _ metering.Meter = (*Client)(nil) // ← the swap is invisible to business code
```

---

## Extraction-ready code organization

Organize the module as **ports & adapters (hexagonal)** now, so lifting it into its own repo/service later is a `git mv` + `go mod init` — never a refactor. In this repo the unit lives at `backend-go/kernel/metering/` (with `backend-go/kernel/metering/billing/` for the fiat layer); the tree below is shown module-relative so it reads the same in-repo and after extraction. The litmus test for the whole layout:

> **Could I `git mv metering/ ../metering-service/ && cd ../metering-service && go mod init && go build ./...` and have it compile with zero edits?**
> It compiles iff nothing under `metering/` imports the product, its schema and config travel with it, and the only product-specific things are *injected* (a `Pricer` and a `Scope` constructor).

```text
metering/                          # THE EXTRACTION UNIT — self-contained, zero inbound product deps
  go.mod                           #   (optional today; the dir is already `go mod init`-ready)
  domain/                          #   pure types + invariants — imports NO infra, NO product
    scope.go  credits.go  money.go  event.go  limit.go  receipt.go
  ports/                           #   the interfaces = the hexagon's edges
    driving.go                     #     Meter, LedgerWriter   (how the world calls metering)
    driven.go                      #     Pricer, BalanceStore, LedgerStore, Bus, Clock, IDs
                                   #                            (what metering needs from the world)
  app/                             #   use-cases: compose Pricer + stores; own the invariants
    meter.go   admit.go  record.go  grant.go   # implement ports.Meter over the driven ports
    writer.go  reconcile.go  rehydrate.go      # implement ports.LedgerWriter
  adapters/
    driven/                        #   swappable INFRA impls of the driven ports
      redis/     balance + outbox (atomic DECRBY+LPUSH+SETNX)
      postgres/  ledger store + reconcile queries (+ owns migrations/)
      nats/      Bus impl
    driving/                       #   TRANSPORTS that expose app over a boundary
      inprocess/ returns ports.Meter directly              (library mode)
      grpcserver/ meteringv1 server over app               (service mode)
      grpcclient/ meteringv1 client, satisfies ports.Meter (service mode caller)
  billing/                         #   FIAT boundary (Phase 2): PaymentProvider port + stripe/polar/paypal
                                   #     adapters, webhook verify→dedup→grant. Emits Grants into app. Fiat-only.
  migrations/                      #   OWNS its schema — travels with the module
    NNNN_account_credits.sql  NNNN_credit_ledger.sql  NNNN_outbox.sql  NNNN_payments.sql
  config.go                        #   metering.Config struct — no os.Getenv scattered in the core
```

**Dependency rule (one direction only):** `adapters → app → ports → domain`. `domain` and `ports` import nothing outside the module; `app` imports only `ports`+`domain`; `adapters` may import infra SDKs (redis/pgx/nats/grpc) but **never** the product. The two product-specific bindings — the `Pricer` impl and how a `Scope` is built from a request — are supplied by the **host at wire time**, in `cmd/`, never reached into by the module:

```go
// backend-go/cmd/api/main.go  — the host wires product specifics INTO the generic module
pricer, _ := pricing.NewLLMTokenPricer(rateCard)          // product-specific Pricer (injected driven port)
meter := meteringapp.New(meteringapp.Deps{                // generic core
    Pricer:  pricer,
    Balance: redisstore.New(creditRedis),                 // driven adapter
    Ledger:  pgstore.New(db),                             // driven adapter
    Bus:     natsbus.New(nc),                             // driven adapter
})
// business code depends on ports.Meter — today an in-process value, tomorrow grpcclient.New(conn). No caller change.
```

Enforcement + hygiene that keep the boundary honest over time:

- **`depguard`/`go-arch-lint`** rule: `metering/**` may not import `backend-go/internal/**` (product). CI fails the moment someone reaches across.
- **Own the schema.** Credit/ledger/outbox/payment tables live in `metering/migrations/`, so extraction takes the data model with it. The one rename that generalizes it: `workspace_credits` → `account_credits` keyed by `scope_tag` (the opaque `Scope.Tag()`), not `workspace_id`.
- **Own the config.** A `metering.Config` struct (Redis DSN, PG DSN, bus, shard count, reconcile tolerance, settlement durability) passed in — the core never reads global app config or env directly.
- **Abstract the bus.** `app` depends on a `Bus` *port*, not NATS, so the extracted service can keep NATS or swap it (`billing.deduct.<tag>` becomes the port's subject convention).
- **Nested module, or module-ready dir.** Either make `metering/` its own Go module today, or keep it import-clean so `go mod init` is the only extraction step. Same choice the LLM gateway offers (library now, service later).

Net: `metering/` (engine) + `metering/billing/` (fiat adapter) is a cohesive, self-contained unit whose *only* seams to the outside world are the injected `Pricer`, the injected `Scope`, and the driven infra adapters — exactly the three things a different host would provide anyway.

---

## Machine-enforced boundary (lint + config)

Two complementary linters make the extraction rules a CI gate, not a convention. **go-arch-lint** enforces the *internal* component graph (one-direction deps); **depguard** bans *specific* imports (product tier + infra SDKs in the core). Both drop in on day one; they fail the build the moment the boundary is crossed.

### `.go-arch-lint.yml` — the hexagon's dependency graph

```yaml
version: 3
workdir: backend-go
allow:
  depOnAnyVendor: true          # infra SDKs are gated by depguard below, not here

components:
  metering-domain:   { in: kernel/metering/domain }
  metering-ports:    { in: kernel/metering/ports }
  metering-app:      { in: kernel/metering/app }
  metering-driven:   { in: kernel/metering/adapters/driven/** }
  metering-driving:  { in: kernel/metering/adapters/driving/** }
  metering-billing:  { in: kernel/metering/billing/** }
  product:           { in: internal/** }
  cmd:               { in: cmd/** }

deps:
  metering-domain:   { mayDependOn: [] }                                   # pure — nothing
  metering-ports:    { mayDependOn: [ metering-domain ] }
  metering-app:      { mayDependOn: [ metering-domain, metering-ports ] }
  metering-driven:   { mayDependOn: [ metering-domain, metering-ports ] }  # implements ports; NOT app
  metering-driving:  { mayDependOn: [ metering-domain, metering-ports ] }  # drives via the Meter port
  metering-billing:  { mayDependOn: [ metering-domain, metering-ports ] }
  product:           { mayDependOn: [ metering-ports ] }                   # ← product sees ONLY the interfaces
  cmd:               { mayDependOn: [ metering-domain, metering-ports, metering-app,
                                      metering-driven, metering-driving, metering-billing, product ] }
```

The two load-bearing rows: `product → metering-ports` only (the product can't reach into `app`/adapters — it depends on interfaces), and `cmd` is the *only* place allowed to wire concrete impls together. `metering-domain` depends on nothing, so it lifts out untouched.

### `.golangci.yml` — banned imports (depguard v2)

```yaml
linters:
  enable: [ depguard ]
linters-settings:
  depguard:
    rules:
      # 1. The CORE (domain + ports + app) is pure: no infra, no product.
      metering-core-pure:
        list-mode: lax
        files:
          - "**/kernel/metering/domain/**"
          - "**/kernel/metering/ports/**"
          - "**/kernel/metering/app/**"
        deny:
          - { pkg: "github.com/aisat/backend-go/internal", desc: "core must not import the product tier — keep it extractable" }
          - { pkg: "github.com/redis/go-redis",            desc: "no infra in the core; use the BalanceStore driven port" }
          - { pkg: "github.com/jackc/pgx",                 desc: "no infra in the core; use the LedgerStore driven port" }
          - { pkg: "github.com/nats-io",                   desc: "no broker in the core; use the Bus driven port" }
          - { pkg: "google.golang.org/grpc",              desc: "no transport in the core; grpc lives in adapters/driving" }
      # 2. The WHOLE module never depends on the product (the extraction guarantee).
      metering-no-product:
        list-mode: lax
        files: [ "**/kernel/metering/**" ]
        deny:
          - { pkg: "github.com/aisat/backend-go/internal", desc: "metering/** is the extraction unit — it may not import internal/** (product)" }
      # 3. The PRODUCT touches metering only through ports (+ cmd wiring), never its guts.
      product-uses-ports-only:
        list-mode: lax
        files: [ "**/internal/**" ]
        deny:
          - { pkg: "github.com/aisat/backend-go/kernel/metering/app",              desc: "wire via cmd/ + the Meter port, not app internals" }
          - { pkg: "github.com/aisat/backend-go/kernel/metering/adapters/driven",  desc: "product must not reach into metering's infra adapters" }
```

Rule 2 *is* the litmus test as a lint: if nothing under `metering/**` imports `internal/**`, the `git mv … && go mod init` extraction compiles.

### `config.go` — the module's only configuration surface

The host passes **one** `Config` value in at wire time; the core reads no env / global config directly, so the config travels with the module on extraction.

```go
package metering

import (
	"errors"
	"fmt"
	"time"
)

type SettlementDurability string

const (
	SettlementOutbox  SettlementDurability = "outbox"  // fast; accepts bounded under-bill RPO (default)
	SettlementJournal SettlementDurability = "journal" // durable-first: +1 sync write, no under-bill
)

// Config is the ENTIRE configuration surface of the metering module.
type Config struct {
	// Stores — driven adapters dial these; the core never does.
	BalanceRedisURL string // hot balance + outbox + idempotency guards (noeviction + AOF)
	LedgerDSN       string // durable Postgres (credit_ledger, account_credits)

	// Bus — subject convention; the Bus port owns the transport.
	SubjectPrefix string // default "billing" → billing.deduct.<tag> / billing.grant.<tag>

	// Sharding — fixed at init; N drainers/reconcilers scale under it (research §3).
	Shards int // default 16; MUST be >= 1 and stable for a deployment's life

	// Settlement + reconcile.
	Settlement         SettlementDurability // default SettlementOutbox
	ReconcileInterval  time.Duration        // default 1h
	ReconcileTolerance Credits              // |drift| above this PAGES instead of silently healing; default 0
	DefaultWarnAt      float64              // near-limit warn fraction when a Limit omits it; default 0.8

	// Hot-path safety.
	AdmitTimeout  time.Duration // admission-gate deadline; default 250ms
	RecordTimeout time.Duration // settle deadline; default 500ms
	FailOpen      bool          // hot store unreachable on Admit: true = serve (bounded overspend, healed by reconcile); false = block. Default false
}

func (c *Config) withDefaults() {
	if c.SubjectPrefix == "" {
		c.SubjectPrefix = "billing"
	}
	if c.Shards == 0 {
		c.Shards = 16
	}
	if c.Settlement == "" {
		c.Settlement = SettlementOutbox
	}
	if c.ReconcileInterval == 0 {
		c.ReconcileInterval = time.Hour
	}
	if c.DefaultWarnAt == 0 {
		c.DefaultWarnAt = 0.8
	}
	if c.AdmitTimeout == 0 {
		c.AdmitTimeout = 250 * time.Millisecond
	}
	if c.RecordTimeout == 0 {
		c.RecordTimeout = 500 * time.Millisecond
	}
}

// Validate checks the EFFECTIVE config (defaults applied to a copy; caller not mutated).
func (c Config) Validate() error {
	c.withDefaults()
	var errs []error
	if c.BalanceRedisURL == "" {
		errs = append(errs, errors.New("BalanceRedisURL is required"))
	}
	if c.LedgerDSN == "" {
		errs = append(errs, errors.New("LedgerDSN is required"))
	}
	if c.Shards < 1 {
		errs = append(errs, fmt.Errorf("Shards must be >= 1, got %d", c.Shards))
	}
	switch c.Settlement {
	case SettlementOutbox, SettlementJournal:
	default:
		errs = append(errs, fmt.Errorf("Settlement must be outbox|journal, got %q", c.Settlement))
	}
	if c.ReconcileTolerance < 0 {
		errs = append(errs, errors.New("ReconcileTolerance must be >= 0"))
	}
	if c.DefaultWarnAt < 0 || c.DefaultWarnAt > 1 {
		errs = append(errs, fmt.Errorf("DefaultWarnAt must be in [0,1], got %v", c.DefaultWarnAt))
	}
	return errors.Join(errs...)
}
```

### `Deps` + `New` — product specifics injected, nothing reached into

```go
package app // kernel/metering/app

// Deps are the driven ports the core needs. The host supplies them in cmd/ — the ONLY
// product-specific one is Pricer; the rest are generic infra adapters.
type Deps struct {
	Pricer  ports.Pricer       // ← product-specific (LLMTokenPricer, StorageBytePricer, …)
	Balance ports.BalanceStore // redis adapter
	Ledger  ports.LedgerStore  // postgres adapter
	Bus     ports.Bus          // nats adapter
	Clock   ports.Clock        // default: system clock
	IDs     ports.IDSource     // uuid v7
}

// New validates config, assembles the core, and returns it as the ports.Meter interface.
func New(cfg metering.Config, d Deps) (ports.Meter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metering config: %w", err)
	}
	if d.Pricer == nil || d.Balance == nil || d.Ledger == nil || d.Bus == nil {
		return nil, errors.New("metering: Pricer, Balance, Ledger and Bus are required")
	}
	// … construct the Meter over the driven ports …
	return &meter{cfg: cfg, deps: d}, nil
}
```

So the day-one wiring in `cmd/api/main.go` is: build a `Config`, build the driven adapters, inject a product `Pricer`, call `app.New` → get a `ports.Meter`. Swapping to service mode later replaces that one `app.New(...)` with `grpcclient.New(conn)` — both return `ports.Meter`, and the two linters guarantee nothing downstream ever depended on more than that interface.

---

## Generalization checklist (before reusing this in another system)

Copy-paste and tick per new host system:

- [ ] **Scope defined** — pick the billing subject (`workspace`/`org`/`user`/`account`); construct `Scope` from request context; set the RLS/tenant predicate to match. No kernel signature changes.
- [ ] **Pricer implemented** — a pure `Price(Event)` over the host's `Unit`s + a versioned, injected rate card. No I/O, no clock, no `Attributes` in cost math.
- [ ] **Units enumerated** — the host's metered dimensions as `Unit` constants; `Event`s carry integer `Quantity`s only.
- [ ] **Limits configured** — the ceilings as `[]Limit` with `DenyCode`/`Window`/`WarnAt`; map each `DenyCode` to a wire status the host already returns (402/429/…).
- [ ] **Settlement durability chosen** — `outbox` vs `journal` per invariant 7 + the durability table; document the RPO direction in the host's runbook.
- [ ] **Sole durable writer wired** — exactly one worker owns `LedgerWriter`; no request tier writes the ledger; scheduling is external/event-driven (research §15).
- [ ] **Idempotency backstop present** — the durable store has a `UNIQUE(idem_key)` on the ledger; every producer supplies an `IdemKey`.
- [ ] **Reconcile alarm wired** — `Tolerance` set; `ReconcileReport.Alarmed` pages an on-call, not just writes a row.
- [ ] **Rehydrate-before-serve** — cold-start/after-loss rebuilds the hot balance from the ledger under a per-scope lock before the tier serves.
- [ ] **Grant path (if monetized)** — reuse `kernel/billing` `PaymentProvider` adapters, or publish `Grant`s from the host's own top-up flow; grants are just positive ledger rows through the same idempotent path.
- [ ] **Partial-settlement honored** — any cancellable unit of work reports real produced `Quantities` to `Record`.
- [ ] **Money is integer** — `Credits`/`Money`; no float touches a balance, a price, or a ledger row.

Extraction-readiness (so it lifts into its own service later without a refactor):

- [ ] **Import-clean module** — nothing under `metering/` imports the product (`internal/**`); a `depguard`/`go-arch-lint` rule enforces it in CI.
- [ ] **Litmus passes** — `git mv metering/ …/ && go mod init && go build ./...` compiles with zero edits.
- [ ] **Schema travels** — credit/ledger/outbox/payment tables live in `metering/migrations/`; balance table keyed by opaque `scope_tag`, not `workspace_id`.
- [ ] **Config self-contained** — a `metering.Config` struct is passed in; the core reads no global app config or env directly.
- [ ] **Bus abstracted** — `app` depends on a `Bus` port, not a concrete broker.
- [ ] **Both transports present** — `driving/inprocess` (library) and `driving/grpcserver`+`grpcclient` (service), both satisfying `Meter`; the swap is a wiring change in `cmd/`.

---

## Non-goals (stays in the host, by design)

- **Pricing/rate cards** — a `Pricer` implementation, never kernel code. The kernel debits credits; it does not know what a token or a gigabyte costs.
- **What produces spend** — call sites (LLM gateway client, ingestion, connectors) build `Event`s. The kernel does not instrument callers.
- **Fiat, tax, invoicing** — the payments boundary (`kernel/billing`, Phase 2) and the provider/MoR. The metering core is fiat-agnostic; it only ever sees `Credits`.
- **Tenancy semantics** — what a `Scope` *means*, and its RLS predicate, are the host's. The kernel treats it as an opaque identity.
