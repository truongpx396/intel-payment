# Contract: Metering & Credit Ledger (core ports)

**Spec**: [../spec.md](../spec.md) | **Plan**: [../plan.md](../plan.md) | **Status**: Normative — this is the core of `intel-payment`.

This contract defines the metering engine: a Redis-hot-balance + outbox + durable-ledger + reconcile machine that any host product drops in **without touching its tenancy model, its spend source, or its bus vocabulary**. Ports are given in Go (the implementation language). LLM-token metering is presented at the end as **one implementation** of these ports, not as the core.

> **Provenance.** These ports were designed and hardened inside [aisat-intel](https://github.com/truongpx396/aisat-intel) as the reusability seam for its credit backbone, then extracted here with their history intact. See [PROVENANCE.md](../../../PROVENANCE.md). Everything the seam promised — swap one `Pricer`, bind one `Scope`, change nothing on the money path — is now the product rather than a promise about one.

---

## Why these three seams exist

A credit engine welded to its first host acquires three couplings. Each one turns a config change into a schema migration; each port below removes one.

| # | The coupling | What it costs when it bites | The port that removes it |
|---|---|---|---|
| 1 | Billing subject is a hard-coded tenant column (`workspace_id`) | Re-anchoring the billing subject (workspace → organization → account) becomes a **schema migration across every money table**, not a config change | `Scope` — an opaque billing subject the engine never interprets |
| 2 | Spend is welded to one domain's units (LLM tokens) | Cost math lives inline at call sites (`cost_usd_micros` computed from returned tokens); metering anything else means writing a second engine | `Pricer` + `Event` — a deterministic, domain-agnostic cost function over metered quantities |
| 3 | Bus subjects and cache keys are tenant-shaped (`billing.deduct.<ws>`, `credit:{ws}:balance`) | The key space encodes one host's tenancy, so two products cannot share a deployment | `Realm` + `Scope.Tag()` — canonical key/subject derivation; the host decides what a scope *is* |

The rule: **the metering kernel is generic; only the `Pricer` implementation and the `Scope` binding are product-specific.** Everything that is release-blocking — single-writer, idempotency, admission gate, reconcile — holds without assuming any particular tenant or unit.

## Ports at a glance

```text
CALLER (any runtime, any domain)
   │  Admit(scope, limits…, maxCost)   ── admission gate (no reservation)
   │  Record(event)                    ── price + settle, idempotent
   ▼
┌── Meter (orchestration) ─────────────────────────────────────────────────┐
│   Pricer.Price(event)  → Credits (+ informational CostMicros)      PURE   │
│   Ledger.Debit(charge) → fast path: ONE atomic Redis script:              │
│                          DECRBY balance + XADD outbox + SET NX applied    │
└──────────────────────────────────────────────────┬───────────────────────┘
        returns now (no durable-store wait)         │  outbox:{shard}  (a STREAM,
                                                    │  not a list — so the intent is
                                                    ▼  already on the durable bus)
┌── LedgerWriter (SOLE durable writer · single-owner worker role) ─────────┐
│   Drain      XREADGROUP outbox:{shard} → INSERT credit_ledger → XACK     │
│              (XAUTOCLAIM recovers a dead consumer's un-acked entries)    │
│   Reconcile  expected = SUM(ledger); heal drift; ALARM if > tolerance    │
│   Rehydrate  rebuild hot balance from ledger (cold-start / Redis loss)   │
└──────────────────────────────────────────────────────────────────────────┘
```

Three seams a host can swap independently: the **`Pricer`** (its domain), the **`Ledger`/`LedgerWriter`** backend (its infra), and the **`Scope`** binding (its tenancy). The reference implementation uses Redis + Postgres + NATS, but nothing in the port signatures requires them — the driven ports name capabilities, not products.

---

## Domain types

```go
package metering

// Realm is the top-level isolation axis: one HOST PRODUCT drawing on this deployment.
// Plans, rate cards, limits, provider credentials, ledger rows and idempotency keys are
// all realm-partitioned, so one intel-payment deployment can serve several products
// without their key spaces, catalogues or idem keys colliding. A single-product
// deployment leaves it at DefaultRealm and pays nothing for the axis.
//
// This is the difference between "reusable if you deploy another copy" and "reusable".
type Realm string

const DefaultRealm Realm = "default"

// Scope is the opaque metering/billing subject WITHIN a realm. The HOST decides what it
// means (workspace, organization, user, project, api_key); the kernel treats it only as
// an identity + a key/subject source. Parameterizing this is what makes a re-anchor
// (workspace→organization→account) a config change rather than a migration.
type Scope struct {
	Realm Realm  // defaults to DefaultRealm when empty
	Kind  string // host-defined: "workspace" | "organization" | "user" | "project" | …
	ID    string // opaque, stable
}

// Tag is the Redis hash-tag / NATS token for this scope. Keys `credit:{acme/ws:abc}:balance`
// colocate on one slot so a scope's ops stay atomically scriptable under Redis Cluster.
// Subjects derive as `billing.deduct.<tag>`. The realm is INSIDE the tag so two products
// can never share a balance key, an outbox entry or an idempotency guard by accident.
func (s Scope) Tag() string {
	realm := s.Realm
	if realm == "" {
		realm = DefaultRealm
	}
	return string(realm) + "/" + s.Kind + ":" + s.ID
}

// Credits is the single INTERNAL accounting unit — a signed delta.
// Negative = debit (consumption); positive = credit (grant). Never a float.
// RESOLVED: signed delta, not a debit/credit column pair — see ../design-decisions.md D1.
type Credits int64

// Money is a FIAT amount, used only at the payments boundary. Integer minor
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
// that makes Record exactly-once (SC-002).
type Event struct {
	Scope      Scope
	Resource   string            // host taxonomy: "llm.chat" | "storage" | "connector.sync" | …
	RateKey    string            // pricing selector (model / storage-tier / sku) — a LEGITIMATE cost input
	Quantities []Quantity        // integer counts; priced by the Pricer over RateKey + Quantities
	IdemKey    string            // REQUIRED
	OccurredAt time.Time
	Attributes map[string]string // provider, feature, user_id, trace_id — usage-log/audit ONLY, never a cost input
}

// Price is the output of pricing an Event: the internal debit, an informational external
// cost for dashboards, and the rate-card version that produced them.
type Price struct {
	Credits    Credits // amount to debit (as a positive magnitude; Debit applies it as negative)
	CostMicros int64   // upstream cost in USD micros — informational, for usage analytics only
	// RateCardVersion is persisted on the ledger row. Without it, "pricing is replayable"
	// is not actually true: a dispute months later cannot know WHICH card priced the row,
	// so a re-derivation silently uses today's rates and disagrees with the charge.
	RateCardVersion string
}

// Window classifies a limit counter's reset behavior.
type Window int

const (
	Balance Window = iota // no reset — the durable credit balance (→ 402 when exhausted)
	Daily                 // calendar-day counter (→ 409)
	Hourly                // calendar-hour counter (→ 409)
	Rolling               // sliding window of Limit.Dur (→ 409)

	// Job is a cumulative budget for ONE unit of work, keyed by Limit.Scope (an ephemeral
	// scope such as {Kind:"run", ID:run_id}) and expiring with it after Limit.Dur.
	//
	// It exists because the other windows cannot bound a single multi-step job. A daily
	// ceiling still permits one runaway agent loop to burn the whole day's budget in an hour,
	// and AdmitRequest.MaxCost bounds one call rather than a run's accumulated spend. So a
	// long-horizon job carries its own cap, checked after each step, and stops when it is
	// reached — independently of every calendar ceiling.
	//
	// Unlike Balance this is a COUNTER, not a balance: no credits are granted to the job and
	// no ledger rows are written for it. Leftover budget needs no return, and a job that dies
	// mid-run leaks nothing but a TTL'd Redis key.
	Job
)

// Limit is one ceiling evaluated at admission. The generic form of the three ceilings
// (workspace balance · per-role daily token budget · per-user cap). DenyCode maps a breach
// to the wire error the host already speaks.
type Limit struct {
	Name     string        // stable, surfaced in the deny reason: "workspace_balance" | "role_daily" | "user_daily"
	Scope    Scope         // whose counter (MAY differ from the charge scope — e.g. per-user within a
	                       // workspace, or an ephemeral {Kind:"run", ID:…} for a Job window)
	Unit     Unit          // what the ceiling is measured in
	Max      int64         // the ceiling
	Window   Window
	Dur      time.Duration // used only when Window == Rolling
	WarnAt   float64       // 0..1 near-limit fraction for a soft warning (default 0.8, operator-configurable)
	DenyCode string        // "payment_required" (402) | "limit_reached" (429)
}

// AdmitRequest is the admission-gate input. Splitting it out of a variadic parameter is
// what lets MaxCost exist at all.
type AdmitRequest struct {
	Limits []Limit
	// MaxCost is the caller's UPPER BOUND on what the unit of work it is about to start
	// can cost (a token ceiling priced through the same Pricer, a per-call cap, a quoted
	// figure). Optional — zero means "unknown".
	//
	// It exists because a gate that only asks "are you already at the ceiling?" does not
	// bound overshoot: a scope with 1 credit left admits a 10,000-credit call and settles
	// 9,999 negative. With MaxCost the gate refuses when `balance - MaxCost < 0`, so the
	// overshoot bound stated in invariant 3 is ENFORCED rather than asserted. This is still
	// a gate, not a reservation: nothing is pre-debited and concurrent admits can still
	// overshoot by (in-flight concurrency × MaxCost) — a bound the operator can now compute.
	MaxCost Credits
}

// Admission is the gate decision. Allowed=false blocks BEFORE any expensive work.
type Admission struct {
	Allowed  bool
	Exceeded *Limit // which ceiling blocked (nil when Allowed)
	Warning  *Limit // a ceiling crossed WarnAt but not Max — drives the near-limit notification
	// Headroom is the remaining allowance on the binding limit, so a caller can size the
	// work it is about to start (e.g. cap max_tokens) instead of starting work it cannot
	// afford to finish.
	Headroom Credits
}

// Charge settles a completed unit of work. Grant adds/removes credits (payments, refunds, admin).
// Both are idempotent on IdemKey and carry a Reason (→ credit_ledger.operation_type).
type Charge struct {
	Scope   Scope
	Amount  Credits           // positive magnitude to debit
	IdemKey string            // REQUIRED
	Reason  string            // operation_type: "query" | "ingest" | "enrich" | "caption" | …
	// Job is the unit-of-work scope this charge also counts against, when the caller is a step
	// of a multi-step job carrying a `Window: Job` ceiling. Zero value = not part of a job.
	// The charge still debits Scope; Job only increments that job's counter.
	Job     Scope
	Ref     map[string]string // trace_id, call_id — audit metadata
}

type Grant struct {
	Scope   Scope
	Amount  Credits           // positive to add; NEGATIVE for refund/chargeback/expiry
	IdemKey string            // REQUIRED (e.g. provider_payment_id / invoice_id)
	Reason  string            // "purchase" | "subscription_grant" | "signup" | "refund" | "chargeback" | "admin_adjustment"
	Ref     map[string]string
}

// Transfer moves credits between two scopes in ONE realm, atomically, under one IdemKey.
// It is NOT a hot-path operation: it settles durably first (see Ledger.Transfer).
//
// This is the pool→allocation pattern, and it is common enough to belong in the engine: an
// enterprise buys once at the parent scope and its teams draw allocations from that pool. The
// alternative — two independent Grants — is not equivalent, because a crash between them leaves
// credits destroyed at the source or minted at the destination, and no reconcile can tell which.
//
// It is NOT a Grant: no credits enter or leave the realm, so the realm's total is invariant.
// That makes it auditable as a pair (`allocation_out` at From, `allocation_in` at To) sharing one
// IdemKey, and a reconcile can assert the pair sums to zero.
type Transfer struct {
	From    Scope             // the pool. MUST have sufficient balance — a transfer never overdraws
	To      Scope             // the allocation. MUST share From.Realm
	Amount  Credits           // positive magnitude
	IdemKey string            // REQUIRED
	Reason  string            // "allocation" | "reallocation" | "reclaim"
	// MaxDestBalance caps what the destination may HOLD after the transfer (0 = uncapped).
	// A ceiling on spend is a Limit; this is a ceiling on the allocation itself, which is what
	// stops one team being handed the whole pool by an errant admin action.
	MaxDestBalance Credits
	Ref            map[string]string
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

**Why pure matters:** reconciliation (`LedgerWriter.Reconcile`) and dispute audits both re-derive cost from the immutable event/quantities. A pricer that reaches for the network, a clock, or mutable global rates cannot be replayed, and the ledger stops being auditable. Rate cards are **versioned and immutable**; a price change publishes a new version, never an in-place edit — which is also why "changing a plan's price never affects already-granted credits" holds by construction rather than by care.

---

## Port: `Ledger` — the hot path (request-serving tier)

```go
// Ledger is the fast, non-durable balance tier. It runs INSIDE request-serving tiers.
// It NEVER blocks on the durable store: Debit/Grant do the atomic Redis op + enqueue the
// durable-write intent and return. The durable write is LedgerWriter's job (below).
type Ledger interface {
	// Balance returns the authoritative hot balance for a scope.
	Balance(ctx context.Context, s Scope) (Credits, error)

	// Admit is the admission GATE — it reads the balance, evaluates each Limit, and refuses
	// when one is already at/over its ceiling OR when req.MaxCost would take it past one.
	// It does NOT reserve or pre-debit (see invariants).
	Admit(ctx context.Context, s Scope, req AdmitRequest) (Admission, error)

	// Debit settles a completed unit of work on the fast path. Idempotent on c.IdemKey:
	//   1. atomically in Redis, ONE Lua script: DECRBY balance:{tag}
	//      + XADD outbox:{shard} {intent} + SET NX billing:applied:{tag}:{idem}
	//   2. return immediately (no durable-store wait)
	// A replay (SET NX miss) returns Receipt{Applied:false} without a second decrement.
	//
	// The outbox is a STREAM with a consumer group, not a list. That is what collapses the
	// old two-step "drain the outbox, then publish to a broker" into one: the atomic step has
	// already put the intent on the durable bus, so there is no window in which an intent
	// exists in the outbox but not on the bus, and no translation layer between them.
	Debit(ctx context.Context, c Charge) (Receipt, error)

	// Grant mirrors Debit for positive/negative credit changes (payments, refunds, admin).
	// Same atomic Redis step + outbox intent + idempotency guard.
	Grant(ctx context.Context, g Grant) (Receipt, error)

	// Transfer moves credits between two scopes in one realm. BOTH sides apply or NEITHER does.
	//
	// Unlike Debit/Grant this settles in POSTGRES FIRST, not on the Redis fast path — one
	// transaction writing the paired allocation_out/allocation_in rows and the idempotency
	// guard, then the two hot balances are updated (and reconcile heals them if that second
	// step is lost, because the ledger is authoritative).
	//
	// The reason is a hard constraint rather than a preference: under Redis Cluster a Lua script
	// may not touch keys in different hash slots, and two scopes' keys are in different slots by
	// construction. Forcing them together with a shared `{realm}` tag would put every scope in a
	// realm on ONE slot — destroying the sharding the rest of the design depends on. Since a
	// transfer is an administrative operation measured in per-day, not per-request, paying a
	// Postgres round trip for real atomicity is the right trade. See design-decisions.md D24.
	//
	// It refuses rather than overdraws: an insufficient pool, a cross-realm destination, or a
	// destination that would exceed MaxDestBalance all return an error and change nothing.
	Transfer(ctx context.Context, t Transfer) (TransferReceipt, error)
}

// TransferReceipt reports both sides, because an allocation that moved only one is the bug this
// operation exists to prevent.
type TransferReceipt struct {
	IdemKey     string
	Applied     bool    // false = idempotent replay
	Amount      Credits
	FromBalance Credits
	ToBalance   Credits
}
```

## Port: `Meter` — orchestration callers actually use

```go
// Meter is the single entry point business code depends on. It composes Pricer (cost) +
// Ledger (account) so callers never touch pricing math or balance keys directly.
type Meter interface {
	// Admit gates before expensive work. Resolves the realm's configured Limits for this
	// scope/resource from the LimitStore, merges any the caller passes explicitly, and
	// delegates to Ledger.Admit.
	Admit(ctx context.Context, s Scope, req AdmitRequest) (Admission, error)

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
// Exactly one owner: `cmd/payment-worker` — never a request-serving tier.
// Correctness rests on idempotent atomic claims, NOT on "only one replica runs."
type LedgerWriter interface {
	// Drain pops queued intents for a shard and writes credit_ledger rows.
	// At-least-once (a crash after LPOP redelivers); credit_idem PRIMARY KEY (realm, idem_key)
	// collapses any retry to exactly one row (SC-002).
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

// Shard is the scope partition key. A small deployment runs one drainer + one reconciler;
// the shard lets N run in parallel later with no key-space redesign and no double-apply.
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

1. **Single durable writer.** Exactly one component (`LedgerWriter`, the worker role) writes `credit_ledger` and mutates the durable balance. Every other producer *publishes intents* and never touches the ledger (SC-002). No second money-writer — not a host service, not a payment-provider callback, not a request handler.
2. **Dual idempotency guard, realm-scoped.** Every credit-affecting apply carries an `IdemKey`. A Redis `SET NX billing:applied:{tag}:{idem}` is the fast-path no-op; a dedicated `credit_idem PRIMARY KEY (realm, idem_key)` is the durable backstop. It is a **separate, non-partitioned table**, because a partitioned ledger cannot carry a unique constraint that omits its partition key — see [../data-model.md](../data-model.md) and [../design-decisions.md](../design-decisions.md) D21. The uniqueness is **per realm**, never global: two host products both minting `"invoice-1"` must not silently collapse into one another's no-op. The Redis TTL is a cache-lifetime choice with no correctness role; the Postgres index is the guard. The Redis guard is **not** a correctness primitive under async-replication failover (the Redlock critique) — correctness always rests on the Postgres unique index. `Receipt.Applied=false` reports a replay.
3. **Admission is a gate, not a reservation.** `Admit` reads current balance/counters and refuses an over-limit call; it does not pre-debit. Concurrent admits against a near-empty balance may settle it slightly negative — **bounded overshoot ≤ in-flight concurrency × `AdmitRequest.MaxCost`**. That bound is only real when callers supply `MaxCost`: with it omitted, a single admitted call can overshoot by its entire (unknown) cost, so an implementation SHOULD emit a `metering_admit_uncapped_total` counter and a deployment that cares about the bound SHOULD require it. Overshoot is healed at settlement plus the periodic reconcile. A hard per-call reservation is opt-in, not the default (it either serializes a scope or doubles ledger writes).
4. **Partial settlement on cancel.** A unit of work aborted midway (e.g. a cancelled stream) is billed for what it **actually produced** — not zero (which makes "start then abort" a free-usage exploit) and not the full ceiling (which over-bills). Callers pass the real produced `Quantities` to `Record`. The engine cannot enforce this — it never sees the work — so it is a contract obligation on the caller, and a host whose callers report ceilings instead of actuals is over-billing its own users.
5. **Deterministic, replayable pricing — and the card version is recorded.** `Pricer.Price` is pure over `RateKey` + `Quantities` + a versioned rate card (see the `Pricer` port); it fails **closed** on an unknown `RateKey`/`Unit` (never prices at zero). Reconciliation and audits re-derive cost from the immutable event. Every ledger row persists `rate_card_version`, because replay is only *possible* if the row says which card priced it — otherwise a re-derivation months later quietly applies today's rates and disagrees with the charge it is meant to verify.
6. **Integer money only.** `Credits int64` (signed delta) internally; `Money{MinorUnits, Currency}` at the fiat boundary. Never floats, anywhere.
7. **Documented RPO direction on hot-store loss.** The hot path does `DECRBY + LPUSH` atomically in Redis and returns before the durable write. If Redis loses that atomic pair before `Drain` runs (AOF `everysec` window, or a failover), the ledger row is never written, and `Reconcile` — which trusts the ledger as source of truth — heals the balance **upward**, i.e. the charge is silently forgotten. **This is under-billing, not double-billing, and the ledger is authoritative.** Implementations MUST state this direction and pick a `SettlementDurability` (below) accordingly; they MUST NOT imply reconcile makes every loss whole. *(Closes the "reconcile heals in the under-bill direction" gap.)*
8. **Atomic usage-log + debit enqueue.** `Record` MUST write the usage/analytics record and enqueue the debit intent in **one** atomic step (one Lua script over the balance key, the outbox stream and the idempotency key, or one Postgres tx in `journal` mode) — never as two independent best-effort writes, which could log a call with no debit or debit with no log. With a stream outbox this is strictly easier than with a list plus a separate broker publish: there is no second hop that can fail after the first succeeded. *(Closes the "per-call chain lacks the outbox's atomicity" gap.)*
9. **Reconcile alarms on drift beyond tolerance.** Out-of-tolerance drift pages (`ReconcileReport.Alarmed`); it is never silently absorbed by a `reconcile` row. `Tolerance` is configured, not implicit.
10. **Scope opacity.** The kernel never parses, ranks, or special-cases a `Scope`. Re-anchoring the billing subject (workspace→organization→…) changes only the `Scope` the host constructs and the RLS predicate — never a kernel signature.
11. **Realm isolation.** No read or write crosses a realm. Every key, subject, ledger row, plan, rate card, limit and idempotency guard is realm-partitioned, and a query that omits the realm predicate is a defect. A caller authenticated for realm A can never name a scope in realm B (enforced at the transport, see [security.md](../../../docs/security.md)).
12. **A negative balance is a policy, never an accident.** A refund or chargeback for credits already consumed drives the balance below zero, and there is no arithmetic that avoids it. The engine therefore makes it an explicit `NegativeBalancePolicy` — `allow_debt` (record it, keep serving; the host collects), `clamp_to_zero` (floor the balance and write a `writeoff` ledger row so the books still close), or `block_and_flag` (floor it and refuse further spend pending an operator decision). Silently flooring with no compensating row is forbidden: it breaks `balance == SUM(ledger)` and makes the next reconcile page for a drift the system created itself.
13. **Grants are privileged.** Minting credits is separated from spending them at the API boundary and the authz boundary both. A caller able to `Record` spend must not be able to `Grant`. Grants originate only from a verified payment webhook, an administrative action with an audit row, or a host's own signup/promo path — never from an open endpoint.
14. **A transfer conserves the realm.** `Transfer` moves credits between scopes and never creates or destroys them, so the realm's total is invariant across it. Both sides apply atomically under one `IdemKey`, and the paired `allocation_out`/`allocation_in` rows MUST sum to zero — a reconcile asserts this, because a half-applied allocation is indistinguishable from theft. A transfer never overdraws its source; it refuses.
15. **Every terminal state is observable.** Out-of-tolerance drift, a poison outbox entry, a failed webhook verification, an uncapped admit, an exhausted retry budget and a refused transfer each emit a named metric and land in durable storage. "Logged and moved on" is not a permitted outcome for anything that touched money.

---

## Settlement durability (customizable knob)

One config selects the durability/latency trade for `Record`/`Debit`, without changing any port signature:

| `SettlementDurability` | Hot-path cost | On hot-store loss | Use when |
|---|---|---|---|
| `outbox` (default) | 1 atomic Redis step, no durable-store wait | Accepts bounded silent **under-bill** (invariant 7); reconcile heals balance | Metering commodity spend (LLM tokens) where a sub-cent RPO gap is acceptable for sub-ms enforcement |
| `journal` | +1 synchronous durable write (Postgres spend-journal / transactional outbox) before returning | No under-bill; the durable intent survives a Redis loss | High-value units, regulated billing, or when revenue-loss RPO must be ~zero |

Both satisfy invariants 1–2 and 8; they differ only in *when* the durable intent becomes crash-safe. A host picks per deployment (or per resource) — this is the main "how strict is my money" dial.

---

## Reference wiring: LLM-token metering as ONE implementation

The binding that first exercised these ports was an AI product metering LLM tokens. Nothing in the kernel knows that:

| Generic port / type | LLM-metering binding |
|---|---|
| `Realm` | `"aisat-intel"` — one of N products on the deployment |
| `Scope` | `{Kind: "workspace", ID: workspace_id}`, later `{Kind: "organization", …}` — a binding change, no kernel edit |
| `Pricer` | `LLMTokenPricer`: `Credits` from `input_tokens·rate_in + cached·rate_cached + output·rate_out`; `CostMicros` feeds the usage dashboard |
| `Event.Quantities` | `[{llm_input_token, n}, {llm_cached_token, c}, {llm_output_token, m}]` from the gateway's returned usage |
| `AdmitRequest.MaxCost` | `max_tokens` priced through the same `Pricer` — the request's own ceiling, so overshoot is bounded |
| `Window: Job` | a long-horizon agent run's per-run cap, checked after each step — bounds a runaway loop independently of the daily ceiling |
| `Transfer` | an organization buys the pool; each workspace draws an allocation with an optional per-workspace cap |
| `Limit[]` | `workspace_balance` (Balance→402) · `role_daily` on a per-role token budget (Daily→429) · `user_daily` (Daily→429), `WarnAt=0.8` |
| `Charge.Reason` / `Grant.Reason` | `credit_ledger.operation_type`: `query`/`ingest`/`enrich`/`caption` · `purchase`/`subscription_grant`/`refund`/`reconcile` |
| `Ledger` / `LedgerWriter` | Redis `DECRBY` + `outbox:{shard}` → single-owner worker → `credit_ledger` + periodic reconcile |
| Payments (`Grant` producers) | [`payment-provider-ports.md`](./payment-provider-ports.md) adapters publish `billing.grant.<tag>` on a verified webhook |

Three other bindings the same ports serve without a kernel edit, to show the shape is not accidental:

| Product | `Scope.Kind` | `Unit`s | `Pricer` | Notable `Limit` |
|---|---|---|---|---|
| Object storage | `account` | `storage_byte_day`, `egress_byte` | tiered per-GB with a free allowance | `Balance` + `Rolling` egress burst |
| Seat-based SaaS | `organization` | `seat` | flat per-seat per period, prorated on change | `Balance` only; `Entitler` carries the feature gates |
| Public API | `api_key` | `api_call`, `compute_ms` | per-endpoint class from a versioned card | `Hourly` + `Rolling` per key |

Swapping products is: write a `Pricer`, set `Scope.Kind`, declare the `Unit`s, configure the `Limit` rows. The ledger, outbox, reconcile, idempotency, entitlement and payment code are untouched.

---

## Reference `Pricer`: `LLMTokenPricer`

The concrete implementation behind the wiring table — pure, versioned, fails closed. It ships as a **reference adapter** (`metering/adapters/driven/pricing/llmtoken`), not as core: a host either uses it, copies it, or writes its own.

```go
package llmtoken // metering/adapters/driven/pricing/llmtoken

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Units this pricer understands. Convention: input = NON-cached input tokens,
// cached = cache-read input tokens (billed at a discount), output = generated tokens —
// decomposed by the caller so the pricer is a plain sum (no double count).
const (
	UnitInputToken  domain.Unit = "llm_input_token"
	UnitCachedToken domain.Unit = "llm_cached_token"
	UnitOutputToken domain.Unit = "llm_output_token"
)

// ModelRate is provider cost in USD micros per token.
type ModelRate struct {
	InMicros     int64 // per input token
	CachedMicros int64 // per cache-read token (usually << InMicros)
	OutMicros    int64 // per output token
}

// RateCard is IMMUTABLE + versioned. A price change publishes a new Version; rows are
// never edited in place, so past ledger + reconcile replays stay auditable.
//
// Cards are DATA, loaded per realm from `rate_cards`/`rate_card_entries` (see
// ../data-model.md) and cached in memory — not Go literals. Repricing is then an
// operator action against a config table, not a redeploy, which is what makes the
// engine configurable for hosts who do not control its release cycle.
type RateCard struct {
	Version         string               // e.g. "2026-07-01"
	MicrosPerCredit int64                // USD micros that 1 internal credit represents (margin baked in). MUST be > 0.
	Models          map[string]ModelRate // keyed by RateKey (model id), e.g. "gpt-4o"
}

// LLMTokenPricer implements ports.Pricer.
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
func (p *LLMTokenPricer) Price(_ context.Context, e domain.Event) (domain.Price, error) {
	rate, ok := p.card.Models[e.RateKey]
	if !ok {
		return domain.Price{}, fmt.Errorf("pricing: no rate for %q in card %q", e.RateKey, p.card.Version)
	}
	var costMicros int64
	for _, q := range e.Quantities {
		if q.Amount < 0 {
			return domain.Price{}, fmt.Errorf("pricing: negative quantity for unit %q", q.Unit)
		}
		switch q.Unit {
		case UnitInputToken:
			costMicros += q.Amount * rate.InMicros
		case UnitCachedToken:
			costMicros += q.Amount * rate.CachedMicros
		case UnitOutputToken:
			costMicros += q.Amount * rate.OutMicros
		default:
			return domain.Price{}, fmt.Errorf("pricing: unpriceable unit %q", q.Unit)
		}
	}
	// credits = ceil(costMicros / MicrosPerCredit) — round UP so rounding never under-bills.
	credits := (costMicros + p.card.MicrosPerCredit - 1) / p.card.MicrosPerCredit
	return domain.Price{
		Credits:         domain.Credits(credits),
		CostMicros:      costMicros,
		RateCardVersion: p.card.Version, // travels onto the ledger row — see invariant 5
	}, nil
}
```

Notes that make it production-standard: **integer-only** math (no float touches money); **round-up** on the credit conversion so rounding is always platform-favorable (aligns with the never-under-bill posture, invariant 7); **fail-closed** on unknown model/unit; and rate cards are **swapped by value, never mutated**, so a price change is a new version and old ledger rows re-price identically.

---

## Contract-test skeleton

These validate *any* implementation of the ports against the invariants, so a swapped `Pricer` or a service-mode `Ledger` is held to the same guarantees. The `Pricer` suite is pure (no infra); the `Ledger` suite runs against a real impl via Testcontainers (`//go:build integration`, per the repo test convention).

> **The seam is only *proven* by a second implementation.** A conformance suite exercised by a single impl demonstrates that impl is correct, not that the port is domain-agnostic. `PricerContract` MUST therefore run against **at least two pricers over disjoint `Unit`/`RateKey` spaces** — this repo ships `llmtoken`, `seat` and `storagebyte` for exactly that reason, and CI runs the same suite unchanged against all three. If it passes, "swap one `Pricer`, no money-path change" holds by construction rather than by assertion.

```go
package domain_test

// PricerContract runs against ANY metering.Pricer — pure, no infra.
func PricerContract(t *testing.T, p ports.Pricer) {
	ctx := context.Background()
	base := domain.Event{
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
		one := base; one.Quantities = []domain.Quantity{{Unit: llmtoken.UnitInputToken, Amount: 1}}
		got, err := p.Price(ctx, one); mustNoErr(t, err)
		if got.CostMicros > 0 && got.Credits < 1 { t.Fatalf("rounding under-billed: %+v", got) }
	})
	t.Run("monotonic in quantity", func(t *testing.T) {
		more := base; more.Quantities = []domain.Quantity{{Unit: llmtoken.UnitInputToken, Amount: 2000}, {Unit: llmtoken.UnitOutputToken, Amount: 500}}
		a, _ := p.Price(ctx, base); b, _ := p.Price(ctx, more)
		if b.Credits < a.Credits { t.Fatalf("more tokens cost fewer credits: %d < %d", b.Credits, a.Credits) }
	})
}

// LedgerContract runs against a REAL ports.Ledger (Testcontainers Redis).  //go:build integration
func LedgerContract(t *testing.T, newLedger func(t *testing.T) ports.Ledger) {
	ctx := context.Background()
	ws := domain.Scope{Realm: "t1", Kind: "workspace", ID: uuidv7()}
	balanceOnly := domain.Limit{Name: "workspace_balance", Scope: ws, Unit: "credit", Max: 0, Window: domain.Balance, DenyCode: "payment_required"}

	t.Run("debit is idempotent on idem_key", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 1000)
		c := domain.Charge{Scope: ws, Amount: 100, IdemKey: "chg-1", Reason: "query"}
		r1, err := l.Debit(ctx, c); mustNoErr(t, err)
		if !r1.Applied { t.Fatal("first debit should apply") }
		r2, err := l.Debit(ctx, c); mustNoErr(t, err) // replay
		if r2.Applied { t.Fatal("replay must be a no-op (Applied=false)") }
		if bal, _ := l.Balance(ctx, ws); bal != 900 { t.Fatalf("double-charged: balance=%d want 900", bal) }
	})
	t.Run("admit gates without reserving (no pre-debit)", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 50)
		adm, err := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}}); mustNoErr(t, err)
		if !adm.Allowed { t.Fatal("positive balance should admit") }
		if bal, _ := l.Balance(ctx, ws); bal != 50 { t.Fatalf("admit pre-debited: balance=%d want 50", bal) }
	})
	t.Run("admit refuses an exhausted balance with the right code", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 0)
		adm, _ := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}})
		if adm.Allowed { t.Fatal("empty balance must be refused") }
		if adm.Exceeded == nil || adm.Exceeded.DenyCode != "payment_required" { t.Fatal("must report the 402 limit") }
	})
	t.Run("grant is idempotent on idem_key (replayed webhook)", func(t *testing.T) {
		l := newLedger(t)
		g := domain.Grant{Scope: ws, Amount: 500, IdemKey: "pay-1", Reason: "purchase"}
		_, _ = l.Grant(ctx, g); _, _ = l.Grant(ctx, g)
		if bal, _ := l.Balance(ctx, ws); bal != 500 { t.Fatalf("replayed grant double-credited: %d want 500", bal) }
	})

	// ---- the refinements, each pinned by a test -------------------------------

	t.Run("MaxCost bounds overshoot at the gate", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 50)
		// Already-at-ceiling is false (50 > 0), but the work cannot be afforded.
		adm, err := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}, MaxCost: 10_000})
		mustNoErr(t, err)
		if adm.Allowed { t.Fatal("a 10k-credit call against a 50-credit balance must be refused") }
		if adm.Headroom != 50 { t.Fatalf("headroom should let the caller resize the work: got %d", adm.Headroom) }
	})

	t.Run("idem keys do not collide across realms", func(t *testing.T) {
		l := newLedger(t)
		a := domain.Scope{Realm: "prod-a", Kind: "org", ID: "1"}
		b := domain.Scope{Realm: "prod-b", Kind: "org", ID: "1"}
		_, _ = l.Grant(ctx, domain.Grant{Scope: a, Amount: 100, IdemKey: "invoice-1", Reason: "purchase"})
		r, err := l.Grant(ctx, domain.Grant{Scope: b, Amount: 100, IdemKey: "invoice-1", Reason: "purchase"})
		mustNoErr(t, err)
		if !r.Applied { t.Fatal("realm B's grant was swallowed by realm A's idem key") }
		if bal, _ := l.Balance(ctx, b); bal != 100 { t.Fatalf("realm B balance=%d want 100", bal) }
	})

	t.Run("clamp_to_zero writes a compensating row, never a silent floor", func(t *testing.T) {
		l := newLedger(t, withPolicy(domain.ClampToZero)); seed(t, l, ws, 10)
		_, _ = l.Grant(ctx, domain.Grant{Scope: ws, Amount: -100, IdemKey: "cb-1", Reason: "chargeback"})
		if bal, _ := l.Balance(ctx, ws); bal != 0 { t.Fatalf("balance=%d want 0 (clamped)", bal) }
		// The 90 credits that cannot be taken back must be BOOKED, or balance != SUM(ledger)
		// and the next reconcile pages for a drift the engine created itself (invariant 12).
		if !hasLedgerRow(t, ws, "writeoff", -90) { t.Fatal("clamp must append a writeoff row") }
	})

	t.Run("a job budget halts work without touching any calendar ceiling", func(t *testing.T) {
		l := newLedger(t); seed(t, l, ws, 1_000_000)          // account is nowhere near empty
		run := domain.Scope{Realm: "t1", Kind: "run", ID: "r1"}
		cap := domain.Limit{Name: "run_cap", Scope: run, Unit: "credit", Max: 500,
			Window: domain.Job, Dur: time.Hour, DenyCode: "limit_reached"}
		for i := 0; i < 5; i++ {                              // five steps × 100 = the cap
			_, _ = l.Debit(ctx, domain.Charge{Scope: ws, Amount: 100,
				IdemKey: fmt.Sprintf("step-%d", i), Reason: "agent_step", Job: run})
		}
		adm, err := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{cap}, MaxCost: 100})
		mustNoErr(t, err)
		if adm.Allowed { t.Fatal("a runaway loop must halt at its own cap, not at the daily ceiling") }
		if bal, _ := l.Balance(ctx, ws); bal != 999_500 {
			t.Fatalf("job budget must be a counter, not a granted balance: %d", bal)
		}
	})

	t.Run("transfer applies both sides or neither", func(t *testing.T) {
		l := newLedger(t)
		pool := domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}
		alloc := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
		seed(t, l, pool, 5000)
		r, err := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 2000,
			IdemKey: "alloc-1", Reason: "allocation"})
		mustNoErr(t, err)
		if r.FromBalance != 3000 || r.ToBalance != 2000 { t.Fatalf("one-sided transfer: %+v", r) }

		// Replay: neither side moves again.
		r2, _ := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 2000,
			IdemKey: "alloc-1", Reason: "allocation"})
		if r2.Applied { t.Fatal("replayed allocation must be a no-op") }
		if b, _ := l.Balance(ctx, pool); b != 3000 { t.Fatalf("pool drained twice: %d", b) }
	})
	t.Run("transfer refuses rather than overdrawing, and honours the destination cap", func(t *testing.T) {
		l := newLedger(t)
		pool := domain.Scope{Realm: "t1", Kind: "organization", ID: "o2"}
		alloc := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w2"}
		seed(t, l, pool, 100)
		if _, err := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 500,
			IdemKey: "over-1", Reason: "allocation"}); err == nil {
			t.Fatal("a transfer must refuse an insufficient pool, never overdraw it")
		}
		if _, err := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 100,
			IdemKey: "cap-1", Reason: "allocation", MaxDestBalance: 50}); err == nil {
			t.Fatal("a transfer must refuse to push the destination past MaxDestBalance")
		}
		// Cross-realm is never a transfer, it is a mint and a burn.
		other := domain.Scope{Realm: "t2", Kind: "workspace", ID: "w9"}
		if _, err := l.Transfer(ctx, domain.Transfer{From: pool, To: other, Amount: 10,
			IdemKey: "xr-1", Reason: "allocation"}); err == nil {
			t.Fatal("a cross-realm transfer must be refused (invariant 11)")
		}
	})
}

// Wiring — every impl plugs into the SAME suites. Three pricers over disjoint unit
// spaces is what turns "domain-agnostic" into a test result:
//   func TestLLMTokenPricer_Contract(t *testing.T)  { p, _ := llmtoken.New(testCard());    PricerContract(t, p) }
//   func TestSeatPricer_Contract(t *testing.T)      { p, _ := seat.New(testSeatCard());    PricerContract(t, p) }
//   func TestStorageBytePricer_Contract(t *testing.T){ p, _ := storagebyte.New(testTiers()); PricerContract(t, p) }
//   func TestRedisLedger_Contract(t *testing.T)     { LedgerContract(t, func(t *testing.T) ports.Ledger { return newRedisLedger(t, startRedis(t)) }) }
```

---

## Deployment topology: embedded library **or** standalone container service

Both shapes ship from this repo, over one set of interfaces. Which one a host picks is a wiring decision in its `cmd/`, and it is reversible:

1. **Embedded library.** The host imports `github.com/truongpx396/intel-payment/metering`; `Admit`/`Debit` are in-process Redis ops — sub-ms, no extra network hop. Best when the host is Go and owns the Redis/Postgres anyway. The durable half is *already* its own runtime either way: `LedgerWriter` runs in the single-owner worker, event-driven over the bus.
2. **Standalone container service** (`deploy/docker-compose.yml`). The same ports behind a gRPC + REST facade; the service owns the credit Redis, the outbox and the payment tables. Callers get a client stub — or, for non-Go hosts, speak gRPC/REST directly. Only the `Meter` binding changes; caller logic does not. **This is the default for a polyglot host, and the only option for a non-Go one.**

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
- **Redis ownership moves with the service** — it owns `credit:{tag}:balance`, `outbox:{shard}` and `billing:applied`. That store MUST be `noeviction` + AOF and MUST NOT be shared with a host's LRU cache: one eviction policy fits a cache, the other fits money, and a shared instance silently applies the wrong one.

**Recommended posture:** a Go host with one product starts **embedded**. Everyone else — a non-Go host, or the moment a *second* product must draw on the same pool — runs the **co-located container service**, one per availability zone, with the realm axis keeping the products' books apart. The swap in either direction is a `cmd/` wiring change plus a deploy, never a data migration.

---

## Service surface: `MeteringService` (gRPC, contract-locked)

The service-mode facade is the SAME `Meter`/`Ledger` ports over the wire. gRPC (not REST) because this is an internal, latency-sensitive, strongly-typed hot path. Money crosses the wire as `int64` credits (never floats); `idem_key` is required on every mutating RPC; `Scope` stays opaque `{kind,id}`.

```proto
syntax = "proto3";
package metering.v1;
option go_package = "github.com/truongpx396/intel-payment/api/meteringv1";
import "google/protobuf/timestamp.proto";

message Scope    { string realm = 1; string kind = 2; string id = 3; } // opaque to the service
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

enum Window { BALANCE = 0; DAILY = 1; HOURLY = 2; ROLLING = 3; JOB = 4; }
message Limit {
  string name = 1; Scope scope = 2; string unit = 3; int64 max = 4;
  Window window = 5; int64 dur_seconds = 6;  // ROLLING only
  double warn_at = 7; string deny_code = 8;  // "payment_required" | "limit_reached"
}
message Admission { bool allowed = 1; Limit exceeded = 2; Limit warning = 3;
                    int64 headroom = 4; }
message Receipt   { string idem_key = 1; bool applied = 2; int64 delta = 3; int64 balance = 4;
                    string rate_card_version = 5; }  // which card priced it (invariant 5)

message AdmitRequest   { Scope scope = 1; repeated Limit limits = 2;
                         int64 max_cost = 3; }     // 0 = unknown; see AdmitRequest.MaxCost
message RecordRequest  { Event event = 1; }
message GrantRequest   { Scope scope = 1; int64 amount = 2; string idem_key = 3;
                         string reason = 4; map<string,string> ref = 5; }
message BalanceRequest { Scope scope = 1; }
message BalanceReply   { int64 credits = 1; }

message TransferRequest { Scope from = 1; Scope to = 2; int64 amount = 3; string idem_key = 4;
                          string reason = 5; int64 max_dest_balance = 6;
                          map<string,string> ref = 7; }
message TransferReply   { string idem_key = 1; bool applied = 2; int64 amount = 3;
                          int64 from_balance = 4; int64 to_balance = 5; }

service Metering {
  rpc Admit      (AdmitRequest)   returns (Admission);   // gate, no reservation — hot path, keep co-located
  rpc Record     (RecordRequest)  returns (Receipt);     // price + settle; idempotent on event.idem_key
  rpc Grant      (GrantRequest)   returns (Receipt);     // PRIVILEGED: add/remove credits; idempotent
  rpc GetBalance (BalanceRequest) returns (BalanceReply);
  rpc Transfer   (TransferRequest) returns (TransferReply);  // PRIVILEGED: pool → allocation
}
```

Contract rules baked into the surface:

- **`Record` prices *inside* the service.** The whole `Meter` (Pricer + Ledger) moves server-side, so the client stub is just a *remote `Meter`* — pricing has one home and rate cards never ship to callers. In a multi-product deployment the service holds each product's rate card keyed by `rate_key`; the `Pricer` strategy is selected by `Unit`s. (Library mode injects the `Pricer` in-process instead — identical semantics.)
- **No streaming RPC for partial billing.** A cancelled `chat_stream` still settles with ONE terminal `Record` carrying the quantities actually produced (invariant 4). Settlement is a single call, not a stream.
- **`Grant` is privileged**, separated from `Record` at the RPC level: only the payments/webhook path and admin tools may call it (mTLS + authz), so a compromised spend producer can debit-with-idempotency but never mint credits.
- **Deadlines + fail policy are the caller's.** `Admit` is on the critical path; callers set a tight deadline and a documented fail-open/closed policy (see Deployment topology). The service is stateless per-RPC; all state is in its Redis + Postgres.
- **The realm is authenticated, never asserted.** A caller's credential binds it to one or more realms, and the service rejects any `Scope.realm` outside that set — a caller cannot read, debit or grant in another product's realm by naming it (invariant 11). See [security.md](../../../docs/security.md).

The generated client is wrapped so it satisfies the `Meter` interface — business code depends on `metering.Meter`, not on gRPC:

```go
// adapters/driving/grpcclient — a remote Meter. Callers can't tell it from the in-process one.
type Client struct{ c meteringv1.MeteringClient }

func New(conn *grpc.ClientConn) *Client { return &Client{meteringv1.NewMeteringClient(conn)} }

func (m *Client) Admit(ctx context.Context, s metering.Scope, limits ...metering.Limit) (metering.Admission, error) { /* map → RPC → map */ }
func (m *Client) Record(ctx context.Context, e domain.Event) (metering.Receipt, error)                            { /* map → RPC → map */ }

var _ metering.Meter = (*Client)(nil) // ← the swap is invisible to business code
```

---

## Code organization

**Ports & adapters (hexagonal).** The layout is what keeps the engine host-independent — and this repo *is* the extraction, so the litmus test that justified the layout now runs in the other direction:

> **Can a host vendor `metering/` into its own tree, `go build ./...`, and have it compile with zero edits?**
> It compiles iff nothing under `metering/` imports a host, its schema and config travel with it, and the only product-specific things are *injected* (a `Pricer` and a `Scope` constructor). CI asserts this on every push.

```text
github.com/truongpx396/intel-payment
  go.mod
  metering/                        # THE ENGINE — self-contained, zero inbound host deps
    domain/                        #   pure types + invariants — imports NO infra, NO host
      realm.go  scope.go  credits.go  money.go  event.go  limit.go  receipt.go  policy.go
    ports/                         #   the interfaces = the hexagon's edges
      driving.go                   #     Meter, LedgerWriter      (how the world calls metering)
      driven.go                    #     Pricer, BalanceStore, LedgerStore, LimitStore,
                                   #     RateCardStore, Bus, Clock, IDs, Metrics
    app/                           #   use-cases: compose Pricer + stores; own the invariants
      meter.go   admit.go  record.go  grant.go   # implement ports.Meter
      writer.go  reconcile.go  rehydrate.go      # implement ports.LedgerWriter
    adapters/
      driven/                      #   swappable INFRA impls of the driven ports
        redis/      balance + outbox (atomic DECRBY+LPUSH+SETNX)
        postgres/   ledger, limits, rate cards, reconcile queries
        nats/       Bus impl
        pricing/    REFERENCE pricers — llmtoken/ seat/ storagebyte/ (pick one, or write yours)
        otel/       Metrics impl
      driving/                     #   TRANSPORTS that expose app over a boundary
        inprocess/  returns ports.Meter directly              (library mode)
        grpcserver/ meteringv1 server over app                (service mode)
        grpcclient/ meteringv1 client, satisfies ports.Meter  (service mode caller)
        resthandler/ REST + webhook ingress
    config.go                      #   metering.Config — no os.Getenv in the core

  billing/                         # THE FIAT BOUNDARY — see payment-provider-ports.md
    domain/  ports/  app/
    adapters/driven/{stripe,polar,paypal,postgres}/
    config.go

  entitlement/                     # PLAN FEATURES beyond credits — see entitlement-ports.md

  api/meteringv1/                  # generated protobuf (the wire contract)
  cmd/
    paymentd/                      # gRPC + REST server (service mode)
    payment-worker/                # SOLE durable writer: drain · reconcile · rehydrate · dunning
    payment-migrate/               # migration runner
  migrations/                      # OWNS its schema — travels with the module
  deploy/                          # Dockerfile · docker-compose.yml · Caddyfile
```

**Dependency rule (one direction only):** `adapters → app → ports → domain`. `domain` and `ports` import nothing outside the module; `app` imports only `ports`+`domain`; `adapters` may import infra SDKs (redis/pgx/nats/grpc) but **never** the product. The two product-specific bindings — the `Pricer` impl and how a `Scope` is built from a request — are supplied by the **host at wire time**, in `cmd/`, never reached into by the module:

```go
// cmd/paymentd/main.go — or the HOST's own main.go in library mode. Identical either way.
pricer, _ := llmtoken.New(rateCard)                      // the ONE product-specific port
meter, _ := app.New(cfg, app.Deps{                       // generic core
    Pricer:    pricer,
    Balance:   redisstore.New(creditRedis),              // driven adapter
    Ledger:    pgstore.New(db),                          // driven adapter
    Limits:    pgstore.NewLimitStore(db),                // limits are CONFIG DATA, per realm
    RateCards: pgstore.NewRateCardStore(db),             // cards are CONFIG DATA, versioned
    Bus:       natsbus.New(nc),                          // driven adapter
    Metrics:   otelmetrics.New(mp),
})
// business code depends on ports.Meter — an in-process value here, grpcclient.New(conn) there.
// Neither the host's handlers nor its tests can tell the difference.
```

Enforcement + hygiene that keep the boundary honest over time:

- **`depguard`/`go-arch-lint`** rule: `metering/**` may not import `billing/**`, `entitlement/**`, `cmd/**`, or any host package. CI fails the moment someone reaches across.
- **Own the schema.** Credit/ledger/outbox/payment tables live in `migrations/`, keyed on `(realm, scope_kind, scope_id)` — the opaque scope triple — never on a host's tenant column. See [../data-model.md](../data-model.md).
- **Own the config.** A `metering.Config` struct (Redis DSN, PG DSN, bus, shard count, reconcile tolerance, settlement durability) passed in — the core never reads global app config or env directly.
- **Abstract the bus.** `app` depends on a `Bus` *port*, not a concrete broker. The **default adapter is Redis Streams**, which keeps the required infrastructure to Redis + Postgres and lets the hot path's atomic script publish straight onto the durable stream. **NATS JetStream is a drop-in alternative** for deployments that need file-backed storage with replication, cross-region mirroring, or an existing JetStream estate. See [bus-subjects.md](./bus-subjects.md) for both dialects and the trade-off.
- **One module, clean internal graph.** `metering/` is import-clean with respect to `billing/` and `entitlement/`, so a host that wants only metering can vendor that subtree alone.

Net: `metering/` (engine) + `metering/billing/` (fiat adapter) is a cohesive, self-contained unit whose *only* seams to the outside world are the injected `Pricer`, the injected `Scope`, and the driven infra adapters — exactly the three things a different host would provide anyway.

---

## Machine-enforced boundary (lint + config)

Two complementary linters make the extraction rules a CI gate, not a convention. **go-arch-lint** enforces the *internal* component graph (one-direction deps); **depguard** bans *specific* imports (product tier + infra SDKs in the core). Both drop in on day one; they fail the build the moment the boundary is crossed.

### `.go-arch-lint.yml` — the hexagon's dependency graph

```yaml
version: 3
workdir: .
allow:
  depOnAnyVendor: true          # infra SDKs are gated by depguard below, not here

components:
  metering-domain:   { in: metering/domain }
  metering-ports:    { in: metering/ports }
  metering-app:      { in: metering/app }
  metering-driven:   { in: metering/adapters/driven/** }
  metering-driving:  { in: metering/adapters/driving/** }
  billing:           { in: billing/** }
  entitlement:       { in: entitlement/** }
  api:               { in: api/** }
  cmd:               { in: cmd/** }

deps:
  metering-domain:   { mayDependOn: [] }                                   # pure — nothing
  metering-ports:    { mayDependOn: [ metering-domain ] }
  metering-app:      { mayDependOn: [ metering-domain, metering-ports ] }
  metering-driven:   { mayDependOn: [ metering-domain, metering-ports ] }  # implements ports; NOT app
  metering-driving:  { mayDependOn: [ metering-domain, metering-ports, api ] }
  billing:           { mayDependOn: [ metering-domain, metering-ports ] }  # emits Grants THROUGH the port
  entitlement:       { mayDependOn: [ metering-domain, metering-ports ] }
  cmd:               { mayDependOn: [ metering-domain, metering-ports, metering-app,
                                      metering-driven, metering-driving,
                                      billing, entitlement, api ] }
```

Four load-bearing rows. `metering-domain` depends on nothing, so it is portable by construction. `billing` and `entitlement` may depend on metering's **ports** but never its `app` or adapters — so the fiat layer mints credits by calling `Grant` through the interface, exactly as any other host would, and cannot reach around it. `metering-*` may never depend on `billing`/`entitlement`, so a host that wants metering alone can take that subtree. And `cmd` is the only place allowed to wire concrete impls together.

### `.golangci.yml` — banned imports (depguard v2)

```yaml
linters:
  enable: [ depguard ]
linters-settings:
  depguard:
    rules:
      # 1. The CORE (domain + ports + app) is pure: no infra, no transport.
      metering-core-pure:
        list-mode: lax
        files:
          - "**/metering/domain/**"
          - "**/metering/ports/**"
          - "**/metering/app/**"
        deny:
          - { pkg: "github.com/redis/go-redis",  desc: "no infra in the core; use the BalanceStore driven port" }
          - { pkg: "github.com/jackc/pgx",       desc: "no infra in the core; use the LedgerStore driven port" }
          - { pkg: "github.com/nats-io",         desc: "no broker in the core; use the Bus driven port" }
          - { pkg: "google.golang.org/grpc",     desc: "no transport in the core; grpc lives in adapters/driving" }
          - { pkg: "github.com/stripe/stripe-go", desc: "no provider SDK anywhere near the core; see billing/adapters" }
          - { pkg: "time.Now",                   desc: "inject the Clock port — a core that reads the wall clock is not replayable" }
      # 2. The ENGINE never depends on the fiat or entitlement layers (the portability guarantee).
      metering-is-standalone:
        list-mode: lax
        files: [ "**/metering/**" ]
        deny:
          - { pkg: "github.com/truongpx396/intel-payment/billing",     desc: "metering/ must stand alone — fiat depends on it, not the reverse" }
          - { pkg: "github.com/truongpx396/intel-payment/entitlement", desc: "metering/ must stand alone" }
          - { pkg: "github.com/truongpx396/intel-payment/cmd",         desc: "the core never imports its own wiring" }
      # 3. The FIAT layer touches metering only through ports, never its guts.
      billing-uses-ports-only:
        list-mode: lax
        files: [ "**/billing/**", "**/entitlement/**" ]
        deny:
          - { pkg: "github.com/truongpx396/intel-payment/metering/app",              desc: "mint credits via the Meter/Ledger port, not app internals" }
          - { pkg: "github.com/truongpx396/intel-payment/metering/adapters/driven",  desc: "do not reach into metering's infra adapters" }
      # 4. Provider SDKs stay inside their own adapter.
      provider-sdk-containment:
        list-mode: lax
        files: [ "**", "!**/billing/adapters/driven/stripe/**", "!**/billing/adapters/driven/polar/**", "!**/billing/adapters/driven/paypal/**" ]
        deny:
          - { pkg: "github.com/stripe/stripe-go", desc: "a provider SDK may only be imported by its own adapter (design principle 1)" }
```

Rule 2 *is* the portability litmus as a lint; rule 4 is the "no product code imports a provider SDK" principle as a lint. Neither is a convention anyone has to remember.

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

// NegativeBalancePolicy decides what happens when a refund/chargeback exceeds the credits
// still held — which is not an edge case but the normal outcome of refunding consumed usage.
// There is no arithmetic that avoids it, so it is a stated policy (invariant 12).
type NegativeBalancePolicy string

const (
	AllowDebt     NegativeBalancePolicy = "allow_debt"     // record the negative; keep serving; the host collects
	ClampToZero   NegativeBalancePolicy = "clamp_to_zero"  // floor at 0 AND append a `writeoff` row (default)
	BlockAndFlag  NegativeBalancePolicy = "block_and_flag" // floor at 0, refuse spend, raise for an operator
)

// CreditExpiry decides whether granted credits have a lifetime. Off keeps the ledger a
// single running balance; lots_fifo tracks per-grant lots and expires the oldest first.
// Hosts differ on this for commercial, not technical, reasons — so it is config.
type CreditExpiry string

const (
	ExpiryOff      CreditExpiry = "off"       // default — a grant never expires
	ExpiryLotsFIFO CreditExpiry = "lots_fifo" // credit_lots table; oldest lot consumed and expired first
)

// PlanChangePolicy decides how a mid-period upgrade/downgrade settles.
type PlanChangePolicy string

const (
	ChangeAtPeriodEnd        PlanChangePolicy = "at_period_end"         // default — simplest, no proration math
	ChangeImmediateProrate   PlanChangePolicy = "immediate_prorate"     // switch now, credit the unused remainder
	ChangeImmediateNoProrate PlanChangePolicy = "immediate_no_prorate"  // switch now, forfeit the remainder
)

// AdmitFailPolicy is what Admit does when the hot store is unreachable. Naming it as a
// policy rather than a bool is the point: this is a revenue-vs-availability decision an
// operator makes deliberately, and `FailOpen: false` reads like a default nobody chose.
type AdmitFailPolicy string

const (
	FailClosed AdmitFailPolicy = "fail_closed" // refuse (default) — never serve unmetered work
	FailOpen   AdmitFailPolicy = "fail_open"   // serve; bounded overspend, healed by reconcile
)

// Config is the ENTIRE configuration surface of the metering module.
type Config struct {
	// Realm — the host product this configuration serves. Defaults to DefaultRealm.
	Realm Realm

	// Stores — driven adapters dial these; the core never does.
	BalanceRedisURL string // hot balance + outbox + idempotency guards (noeviction + AOF)
	LedgerDSN       string // durable Postgres (credit_ledger, account_credits)

	// Bus — subject convention; the Bus port owns the transport.
	SubjectPrefix string // default "billing" → billing.deduct.<tag> / billing.grant.<tag>

	// Sharding — fixed at init; N drainers/reconcilers scale under it.
	Shards int // default 16; MUST be >= 1 and stable for a deployment's life

	// Settlement + reconcile.
	Settlement         SettlementDurability // default SettlementOutbox
	ReconcileInterval  time.Duration        // default 1h
	ReconcileTolerance Credits              // |drift| above this PAGES instead of silently healing; default 0
	DefaultWarnAt      float64              // near-limit warn fraction when a Limit omits it; default 0.8

	// Hot-path safety.
	AdmitTimeout  time.Duration   // admission-gate deadline; default 250ms
	RecordTimeout time.Duration   // settle deadline; default 500ms
	AdmitFail     AdmitFailPolicy // hot store unreachable on Admit; default FailClosed

	// Money policies — the dials a host turns for its own commercial reality.
	NegativeBalance NegativeBalancePolicy // default ClampToZero
	CreditExpiry    CreditExpiry          // default ExpiryOff

	// Idempotency cache. The Redis guard is a fast path only; correctness rests on the
	// Postgres UNIQUE(realm, idem_key). Any TTL is safe — this only trades cache memory
	// for the odd extra DB round-trip on a very late replay.
	IdemCacheTTL time.Duration // default 24h

	// RequireMaxCost rejects an Admit that carries no MaxCost, turning the overshoot bound in
	// invariant 3 from a hope into a precondition. Off by default because it is a breaking
	// change for existing callers; deployments that must bound overspend turn it on.
	RequireMaxCost bool
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
	if c.AdmitFail == "" {
		c.AdmitFail = FailClosed
	}
	if c.NegativeBalance == "" {
		c.NegativeBalance = ClampToZero
	}
	if c.CreditExpiry == "" {
		c.CreditExpiry = ExpiryOff
	}
	if c.IdemCacheTTL == 0 {
		c.IdemCacheTTL = 24 * time.Hour
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
	switch c.AdmitFail {
	case FailClosed, FailOpen:
	default:
		errs = append(errs, fmt.Errorf("AdmitFail must be fail_closed|fail_open, got %q", c.AdmitFail))
	}
	switch c.NegativeBalance {
	case AllowDebt, ClampToZero, BlockAndFlag:
	default:
		errs = append(errs, fmt.Errorf("NegativeBalance must be allow_debt|clamp_to_zero|block_and_flag, got %q", c.NegativeBalance))
	}
	switch c.CreditExpiry {
	case ExpiryOff, ExpiryLotsFIFO:
	default:
		errs = append(errs, fmt.Errorf("CreditExpiry must be off|lots_fifo, got %q", c.CreditExpiry))
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
	Pricer    ports.Pricer        // ← the ONE product-specific port (llmtoken, seat, storagebyte, yours)
	Balance   ports.BalanceStore  // redis adapter
	Ledger    ports.LedgerStore   // postgres adapter
	Limits    ports.LimitStore    // per-realm Limit rows — CONFIG DATA, not Go literals
	RateCards ports.RateCardStore // versioned, immutable cards — CONFIG DATA
	Bus       ports.Bus           // nats adapter
	Clock     ports.Clock         // default: system clock. Injected so period math is testable
	IDs       ports.IDSource      // uuid v7
	Metrics   ports.Metrics       // default: no-op. Invariant 14 needs a real one in production
}

// New validates config, assembles the core, and returns it as the ports.Meter interface.
func New(cfg metering.Config, d Deps) (ports.Meter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metering config: %w", err)
	}
	if d.Pricer == nil || d.Balance == nil || d.Ledger == nil || d.Bus == nil {
		return nil, errors.New("metering: Pricer, Balance, Ledger and Bus are required")
	}
	if d.Limits == nil || d.RateCards == nil {
		return nil, errors.New("metering: Limits and RateCards are required — ceilings and prices are config data, not code")
	}
	// … construct the Meter over the driven ports …
	return &meter{cfg: cfg, deps: d}, nil
}
```

So the wiring is: build a `Config`, build the driven adapters, inject a `Pricer`, call `app.New` → get a `ports.Meter`. Swapping to service mode replaces that one `app.New(...)` with `grpcclient.New(conn)` — both return `ports.Meter`, and the two linters guarantee nothing downstream ever depended on more than that interface.

---

## Observability contract

Invariant 15 requires that every terminal state be observable, which means the metric names are part of the contract, not an implementation detail — a host writes alerts against them before it ever reads the code.

| Metric | Type | Why it pages |
|---|---|---|
| `metering_admit_latency_seconds` | histogram (`realm`, `outcome`) | `Admit` is on every request's critical path; a p99 above ~5 ms means metering became the bottleneck it exists to avoid |
| `metering_admit_denied_total` | counter (`realm`, `limit`, `deny_code`) | a spike is either an abuse event or a limit misconfigured to zero |
| `metering_admit_uncapped_total` | counter (`realm`) | callers omitting `MaxCost` — the overshoot bound is not being enforced for them (invariant 3) |
| `metering_admit_failopen_total` | counter (`realm`) | work served **unmetered** during a hot-store outage. Never expected to be non-zero on a `fail_closed` deployment |
| `metering_ledger_drift_credits` | gauge (`realm`, `shard`) | the SC of the whole system: hot balance vs `SUM(ledger)`. Beyond `ReconcileTolerance` it pages (invariant 9) |
| `metering_outbox_depth` | gauge (`shard`) | a rising depth means the sole durable writer is behind; the RPO window in invariant 7 is widening |
| `metering_outbox_age_seconds` | gauge (`shard`) | depth alone hides a single stuck entry. Age is what catches a poisoned head-of-line |
| `metering_reconcile_healed_credits` | counter (`realm`, `direction`) | steady healing in the *up* direction is silent under-billing (invariant 7), not a healthy system |
| `metering_rehydrate_total` | counter (`realm`) | each one is a hot-store loss; the count is the incident rate |
| `metering_price_error_total` | counter (`realm`, `rate_key`, `reason`) | fail-closed pricing rejecting real traffic — usually a rate card missing a newly launched SKU |
| `billing_webhook_verify_failed_total` | counter (`provider`) | a signature failure is a **security** event, not a parse error (see [payment-provider-ports.md](./payment-provider-ports.md)) |
| `billing_grant_applied_total` | counter (`realm`, `reason`) | reconciles against the provider's own payout report |
| `metering_transfer_refused_total` | counter (`realm`, `reason`) | an allocation refused for an empty pool or a destination cap — usually a budget that needs raising, occasionally an admin script in a loop |

Health endpoints: `/healthz` (process up) and `/readyz` (hot store reachable, durable store reachable, migrations at head). `/readyz` MUST fail when migrations are behind — a service that serves money against a half-migrated schema is worse than one that is down.

---

## Adoption checklist (per host product)

Copy-paste and tick once per realm. [docs/integration-guide.md](../../../docs/integration-guide.md) walks each line.

- [ ] **Scope defined** — pick the billing subject (`workspace`/`org`/`user`/`account`); construct `Scope` from request context; set the RLS/tenant predicate to match. No kernel signature changes.
- [ ] **Pricer implemented** — a pure `Price(Event)` over the host's `Unit`s + a versioned, injected rate card. No I/O, no clock, no `Attributes` in cost math.
- [ ] **Units enumerated** — the host's metered dimensions as `Unit` constants; `Event`s carry integer `Quantity`s only.
- [ ] **Limits configured** — the ceilings as `[]Limit` with `DenyCode`/`Window`/`WarnAt`; map each `DenyCode` to a wire status the host already returns (402/429/…).
- [ ] **Settlement durability chosen** — `outbox` vs `journal` per invariant 7 + the durability table; document the RPO direction in the host's runbook.
- [ ] **Sole durable writer wired** — exactly one worker owns `LedgerWriter`; no request tier writes the ledger; scheduling is external/event-driven (a `CronJob`/timer publishes the tick, a queue group delivers it once).
- [ ] **Idempotency backstop present** — the durable store has a `PRIMARY KEY (realm, idem_key)` guard table; every producer supplies an `IdemKey`.
- [ ] **Reconcile alarm wired** — `Tolerance` set; `ReconcileReport.Alarmed` pages an on-call, not just writes a row.
- [ ] **Rehydrate-before-serve** — cold-start/after-loss rebuilds the hot balance from the ledger under a per-scope lock before the tier serves.
- [ ] **Realm chosen** — a stable slug for this product; every `Scope` it constructs carries it. One realm per product, never per tenant.
- [ ] **Rate card published** — a versioned row set in `rate_cards`/`rate_card_entries`, not a Go literal, so repricing is an operator action.
- [ ] **`MaxCost` supplied at the gate** — every caller passes an upper bound, so the overshoot bound is enforced. Turn on `RequireMaxCost` once they all do.
- [ ] **Multi-step work capped** — any job that spends across several steps carries a `Window: Job` limit on an ephemeral scope. A daily ceiling alone still lets one runaway loop burn the day.
- [ ] **Allocation model decided (if a pool is shared)** — `Transfer` for parent-buys/child-draws, with `MaxDestBalance` set so one destination cannot absorb the pool.
- [ ] **Negative-balance policy chosen** — `allow_debt` / `clamp_to_zero` / `block_and_flag`, matching how the host actually handles a refund past spend.
- [ ] **Credit expiry decided** — `off` or `lots_fifo`; if on, the expiry sweep is scheduled.
- [ ] **Grant path (if monetized)** — use the [`PaymentProvider`](./payment-provider-ports.md) adapters, or publish `Grant`s from the host's own top-up flow; grants are just positive ledger rows through the same idempotent path.
- [ ] **Entitlements mapped (if the plan sells more than credits)** — feature gates via [`entitlement-ports.md`](./entitlement-ports.md), not ad-hoc plan-code branches in the host.
- [ ] **Partial-settlement honored** — any cancellable unit of work reports real produced `Quantities` to `Record`.
- [ ] **Money is integer** — `Credits`/`Money`; no float touches a balance, a price, or a ledger row.

Operational readiness (before the first real charge):

- [ ] **Alerts wired** — `metering_ledger_drift_credits`, `metering_outbox_age_seconds`, `metering_admit_failopen_total` and `billing_webhook_verify_failed_total` all page a human. See [docs/operations.md](../../../docs/operations.md).
- [ ] **`/readyz` gating deploys** — it fails on a behind-head schema, so a rollout cannot serve money against a half-migrated database.
- [ ] **Redis is `noeviction` + AOF** — a cache eviction policy on the balance store turns a cache miss into a billing incident.
- [ ] **Caller↔service auth chosen** — mTLS or bearer, with `Grant` restricted to the payment/admin callers only (invariant 13). `PAYMENT_SERVICE_AUTH=none` is permitted **only** on loopback/UDS.
- [ ] **Realm binding enforced** — each caller credential names its realms; a cross-realm scope is rejected at the transport (invariant 11).
- [ ] **Restore rehearsed** — `Rehydrate` has been run against a real ledger copy, and the measured time is inside the host's RTO.

---

## Non-goals (stays in the host, by design)

- **Pricing/rate cards** — a `Pricer` implementation, never kernel code. The kernel debits credits; it does not know what a token or a gigabyte costs.
- **What produces spend** — call sites (LLM gateway client, ingestion, connectors) build `Event`s. The kernel does not instrument callers.
- **Fiat, tax, invoicing** — the payments boundary ([`payment-provider-ports.md`](./payment-provider-ports.md)) and the provider/merchant-of-record. The metering core is fiat-agnostic; it only ever sees `Credits`. Tax is explicitly the provider's or an external hook's job (`TaxStrategy`), never computed here.
- **Feature entitlements** — what a plan unlocks *besides* credits is [`entitlement-ports.md`](./entitlement-ports.md). The metering core counts; it does not gate features.
- **Authentication of end users** — the host authenticates its people and hands the engine a `Scope`. The engine authenticates *callers* (services), never end users.
- **Tenancy semantics** — what a `Scope` *means*, and its RLS predicate, are the host's. The kernel treats it as an opaque identity.
