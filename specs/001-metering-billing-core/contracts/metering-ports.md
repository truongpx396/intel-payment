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
	Quantities []Quantity        // priced by the Pricer; the ONLY input to cost math
	IdemKey    string            // REQUIRED
	OccurredAt time.Time
	Attributes map[string]string // model, provider, feature, user_id, trace_id — for the usage log/audit ONLY, never for pricing
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
	// Price MUST be a PURE function of e.Quantities and a versioned, injected rate card:
	// deterministic and side-effect-free, so the SAME computation runs at the call site,
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
5. **Deterministic, replayable pricing.** `Pricer.Price` is pure over `Quantities` + a versioned rate card (see the Pricer port). Reconciliation and audits re-derive cost from the immutable event.
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

## Package layout (the extraction)

```text
backend-go/
  kernel/
    metering/            # ← REUSABLE core (this contract). No product imports (depguard).
      scope.go           #   Scope, Tag()
      types.go           #   Credits, Money, Unit, Quantity, Event, Price, Limit, Receipt…
      meter.go           #   Meter, Ledger, Pricer, LedgerWriter interfaces
      ledger_redis.go    #   Ledger impl: atomic DECRBY + outbox LPUSH + SET NX
      writer.go          #   LedgerWriter impl: Drain / Reconcile / Rehydrate (worker role)
    billing/             #   PAYMENTS (Phase 2): PaymentProvider port + stripe/polar/paypal adapters,
                         #   webhook verify→dedup→grant. Produces Grants into kernel/metering. Fiat-only.
  internal/
    query/  ingest/  …   #   product tier: constructs Events, provides the LLMTokenPricer (a Pricer impl)
```

The split makes the reuse boundary a compiler-enforced fact: `kernel/metering` is the drop-in engine, `kernel/billing` is the fiat adapter layer, and the LLM-specific `Pricer` lives in the product tier where the domain knowledge belongs.

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

---

## Non-goals (stays in the host, by design)

- **Pricing/rate cards** — a `Pricer` implementation, never kernel code. The kernel debits credits; it does not know what a token or a gigabyte costs.
- **What produces spend** — call sites (LLM gateway client, ingestion, connectors) build `Event`s. The kernel does not instrument callers.
- **Fiat, tax, invoicing** — the payments boundary (`kernel/billing`, Phase 2) and the provider/MoR. The metering core is fiat-agnostic; it only ever sees `Credits`.
- **Tenancy semantics** — what a `Scope` *means*, and its RLS predicate, are the host's. The kernel treats it as an opaque identity.
```
