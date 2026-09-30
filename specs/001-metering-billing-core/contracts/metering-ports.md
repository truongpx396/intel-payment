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
| 3 | Bus subjects and cache keys are tenant-shaped (`billing.deduct.<workspace>`, a balance key named after the workspace) | The key space encodes one host's tenancy, so two products cannot share a deployment | `Realm` + `Scope.Tag()` — canonical key/subject derivation; the host decides what a scope *is* |

The rule: **the metering kernel is generic; only the pricing — a rate card, or for non-linear prices a registered `Pricer` — and the `Scope` binding are product-specific.** Everything that is release-blocking — single-writer, idempotency, admission gate, reconcile — holds without assuming any particular tenant or unit.

## Ports at a glance

```text
CALLER (any runtime, any domain)
   │  Admit(scope, {resource, subjects, maxCost})  ── admission gate (no reservation)
   │  Record(event)                                ── price + settle, idempotent
   ▼
┌── Meter (orchestration) ───────────────────────────────────────────────────┐
│   Pricer.Price(card, event) → Credits (+ informational CostMicros)   PURE  │
│   Ledger.Debit(charge) → ONE Redis Function in the scope's SHARD slot:     │
│      pool draws · seq+1 · idempotency guard · counters · XADD intent       │
└──────────────────────────────────────────────────┬─────────────────────────┘
        returns now (no durable-store wait)         │  billing:outbox:{s<n>} — a STREAM in the
                                                    │  same slot as the account, so the intent is
                                                    ▼  on the bus the instant the balance moves
┌── LedgerWriter (SOLE durable writer · one owner per shard) ─────────────────┐
│   Drain      book intents in per-scope SEQUENCE order, batched → XACK        │
│   Reconcile  hot vs booked + open suspense AT THE SAME SEQ; heal hot; alarm  │
│   Rehydrate  rebuild a stale or new account from the books, lazily          │
│   Recover    freeze → drain → bump generation → unfreeze, after any loss    │
│   Audit      booked == checkpoint + Σ ledger; advance checkpoints           │
└──────────────────────────────────────────────────────────────────────────────┘
```

Three seams a host can swap independently: the **`Pricer`** (its domain — or, for anything
linear, just a rate card for the built-in `table` pricer), the **`Ledger`/`LedgerWriter`** backend
(its infra), and the **`Scope`** binding (its tenancy). The reference implementation uses Redis +
Postgres, with Redis Streams as the bus; nothing in the port signatures requires them.

**How the hot tier and the books stay consistent** — the cluster-safe key layout, the per-scope
sequence watermark, suspense, window-bounded dedup, two-phase transfers, recovery — is its own
normative contract: [hot-path-consistency.md](./hot-path-consistency.md). This file defines the
ports and their invariants; that one defines the mechanism that makes them true.

---

## Domain types

```go
package metering

// Realm is the top-level isolation axis: one HOST PRODUCT drawing on this deployment.
// Plans, rate cards, limits, pools, provider accounts, ledger rows and idempotency keys are
// all realm-partitioned. A single-product deployment leaves it at DefaultRealm.
type Realm string

const DefaultRealm Realm = "default"

// Scope is the opaque metering/billing subject WITHIN a realm. The HOST decides what it
// means (workspace, organization, user, project, api_key); the kernel treats it only as
// an identity. Parameterizing this is what makes a re-anchor a config change, not a migration.
type Scope struct {
	Realm Realm  // defaults to DefaultRealm when empty
	Kind  string // host-defined: "workspace" | "organization" | "user" | "project" | …
	ID    string // opaque, stable
}

// Tag is the scope's IDENTITY — the stable name used in keys, intents and subjects. It is not
// its PLACEMENT: every Redis key of a scope is hash-tagged by its shard,
// fnv1a64(Tag()) mod Shards → `{s<n>}`, because a Redis Cluster script may touch only one slot
// and each operation touches the account, its guard, its counters AND the outbox stream together
// (hot-path-consistency.md §1). The realm is inside the tag, so two products never share a
// balance, an intent or an idempotency guard.
func (s Scope) Tag() string {
	realm := s.Realm
	if realm == "" {
		realm = DefaultRealm
	}
	return string(realm) + "/" + s.Kind + ":" + s.ID
}

// Subjects are the attribution dimensions of one call WITHIN its charged scope: the member who
// made it, the API key, the project, the job. They key subject-level ceilings ("user u1's daily
// budget inside org o1"), land on the ledger row for breakdowns, and are never a cost input.
// Kinds are host-defined, except "job", which keys a Window: Job budget.
type Subjects map[string]string // kind → id: {"user": "u1", "api_key": "k7", "job": "run_42"}

// Pool names a credit pool within a scope — see § Credit pools. GeneralPool always exists,
// applies to every resource and is drawn last.
type Pool string

const GeneralPool Pool = "general"

// Credits is the single INTERNAL accounting unit — a signed delta. Never a float.
// RESOLVED: signed delta, not a debit/credit column pair — see ../design-decisions.md D1.
//
// The unit should be FINE. The reference configuration is 1 credit = 1 µ$ of list price: rounding
// up once per event then costs under a millionth of a dollar, and int64 still holds $9.2 trillion.
// How credits are DISPLAYED ("$12.40", "12,400 credits") is UnitLabels.scale in the UI — never a
// reason to coarsen the unit the books are kept in (D31).
type Credits int64

// Money is a FIAT amount, used only at the payments boundary. Integer minor units + ISO-4217.
// Kept distinct from Credits: changing a price never alters granted credits.
type Money struct {
	MinorUnits int64  // e.g. cents
	Currency   string // ISO-4217, e.g. "USD"
}

// Unit is a host-extensible metered dimension. The kernel ships none as special.
type Unit string // "llm_input_token" | "storage_byte_day" | "api_call" | "seat_day" | "compute_ms" | …

type Quantity struct {
	Unit   Unit
	Amount int64 // integer count of Unit, >= 0
}

// Event is one metered occurrence. A single event may carry several quantities.
type Event struct {
	Scope      Scope
	Resource   string     // host taxonomy: "llm.chat" | "storage" | … — selects limits and eligible pools
	RateKey    string     // pricing selector (model / tier / sku) — a LEGITIMATE cost input
	Quantities []Quantity // integer counts, priced by the Pricer over RateKey + Quantities
	Subjects   Subjects   // who/what within the scope; keys subject ceilings; never a cost input
	IdemKey    string     // REQUIRED, stable across retries of one logical operation (SC-002)
	// OccurredAt is REQUIRED and MUST be identical on every retry. An event older than the
	// usage dedup window is refused (ErrStaleEvent, 422) rather than risk a second charge once
	// its durable guard has expired (hot-path-consistency.md §6).
	OccurredAt time.Time
	// Attributes are usage-log/audit ONLY, never a cost input and never part of the request
	// fingerprint (a retry legitimately carries a new trace id). One key is read: "operation_type"
	// becomes the ledger row's operation_type ("query", "ingest", "agent_step"…); without it the
	// Resource is used. It is not copied into the row's ref.
	Attributes map[string]string // trace_id, feature, provider, operation_type
}

// Price is the output of pricing an Event.
type Price struct {
	Credits         Credits // positive magnitude to debit, rounded UP once per event
	CostMicros      int64   // upstream cost in USD micros — informational, for analytics only
	RateCardVersion string  // persisted on the ledger row: replay is only possible if the row names its card
}

// Window classifies a limit counter's reset behavior.
type Window int

const (
	Balance Window = iota // the scope's eligible pools; Limit.Max is the permitted OVERDRAFT (→ 402)
	Daily                 // calendar day in Limit.TZ (→ 409)
	Hourly                // calendar hour in Limit.TZ (→ 409)
	Rolling               // sliding window of Limit.Dur, bucketed; error ≤ one bucket (→ 409)

	// Job is a cumulative budget for ONE unit of work, keyed by Subjects["job"] and expiring
	// after Limit.Dur. It exists because no other window bounds a multi-step job: a daily ceiling
	// still permits one runaway agent loop to burn the day in an hour, and MaxCost bounds one
	// call. It is a COUNTER, not a balance: nothing is granted to the job and no ledger row is
	// written for it, so an abandoned job leaks only a TTL'd key.
	Job
)

// Limit is one ceiling evaluated at admission and counted at settlement.
type Limit struct {
	Name string // stable, surfaced in the deny reason: "scope_balance" | "user_daily" | "run_cap"
	// Subject is the subject kind whose counter this is ("user", "api_key", "job"), or "" for
	// the charged scope itself. A call that does not carry that subject in Subjects is refused
	// (ErrMissingSubject, 400): a ceiling that silently does not apply is the worst kind.
	Subject  string
	Resource string // "" = every resource; otherwise only this resource counts and is gated
	Unit     Unit   // "credit", or a metered Unit whose quantities the counter sums
	// Max is the ceiling. For Window=Balance it is the permitted overdraft (0 = none). When the
	// realm's limit row names a max_entitlement, Max is resolved per scope from that entitlement
	// quota — a plan, a per-customer override or the free tier sizes it, with provenance (D30).
	Max      int64
	Window   Window
	Dur      time.Duration // Rolling and Job
	TZ       string        // IANA zone for Daily/Hourly resets; default "UTC"
	WarnAt   float64       // 0..1 near-limit fraction; default 0.8, operator-configurable
	DenyCode string        // "payment_required" (402) | "limit_reached" (409)

	// MaxEntitlement names the entitlement key whose quota sizes Max for a scope (-1 there means
	// unlimited). It is configuration: the Meter resolves it through the QuotaSource BEFORE the
	// ledger sees the limit, so the ledger only ever evaluates a concrete Max.
	MaxEntitlement string
}

// AdmitRequest is the admission-gate input.
type AdmitRequest struct {
	Resource string   // selects resource-scoped limits and the eligible pools
	Subjects Subjects // MUST cover every configured subject limit for this resource
	// Limits are caller-supplied ceilings, merged with the realm's configured ones. A caller can
	// only TIGHTEN: for the same Name the smaller Max wins. A per-run Job budget is passed here.
	Limits []Limit
	// MaxCost is the caller's UPPER BOUND on what the work it is about to start can cost. With it
	// the gate refuses when `eligible balance + overdraft − MaxCost < 0`, so the overshoot bound in
	// invariant 3 is ENFORCED rather than asserted. Still a gate, not a reservation: concurrent
	// admits can overshoot by (in-flight concurrency × MaxCost) — a bound the operator can compute.
	MaxCost Credits
}

// Admission is the gate decision. Allowed=false blocks BEFORE any expensive work.
type Admission struct {
	Allowed  bool
	Exceeded *Limit // which ceiling blocked (nil when Allowed)
	Warning  *Limit // a ceiling crossed WarnAt but not Max — drives the near-limit notification
	// Headroom is the remaining allowance on the binding credit-denominated limit, so a caller
	// can size the work (cap max_tokens) instead of starting work it cannot afford to finish.
	Headroom Credits
	Blocked  bool // held by block_and_flag pending an operator (deny code "account_blocked")
}

// Charge settles a completed unit of work on the hot tier. Built by the Meter from an Event
// and its Price; the Ledger derives the request fingerprint from the canonical charge.
type Charge struct {
	Scope      Scope
	Amount     Credits    // positive magnitude to debit
	IdemKey    string     // REQUIRED
	Reason     string     // operation_type: "query" | "ingest" | "agent_step" | …
	Resource   string     // selects eligible pools and the counters it increments
	Subjects   Subjects   // increments subject counters, including a job budget
	Quantities []Quantity // for counters measured in a metered Unit
	Ref        map[string]string

	// The event, carried so the intent is a complete usage record (invariant 8) and so the request
	// fingerprint covers what identifies the operation. None is a cost input to the ledger.
	RateKey         string
	RateCardVersion string
	CostMicros      int64
	OccurredAt      time.Time
}

// Grant adds or removes credits in one pool (payments, refunds, chargebacks, promos, admin).
type Grant struct {
	Scope     Scope
	Pool      Pool       // "" = GeneralPool; a plan's allotment names its pool
	Amount    Credits    // positive to add; NEGATIVE for refund/chargeback — NegativeBalancePolicy applies
	IdemKey   string     // REQUIRED; unique per realm: one provider payment mints once, for one scope
	Reason    string     // "purchase" | "subscription_grant" | "signup_grant" | "promo" | "refund" | "chargeback" | "chargeback_reversal" | "admin_adjustment"
	ExpiresAt *time.Time // the lot's expiry under CreditExpiry=lots_fifo
	Ref       map[string]string
}

// Transfer moves credits between two scopes of ONE realm under one IdemKey — the pool →
// allocation pattern: a parent buys once and its children draw allocations. It is NOT a Grant:
// nothing enters or leaves the realm.
//
// It is two-phase (hot-path-consistency.md §4) because the two scopes live in different shards
// and no script may touch both: phase 1 debits the source's HOT pool — refusing rather than
// overdrawing — and the writer then books allocation_out, credits the destination, and books
// allocation_in. Σ ledger + Σ in-transit is invariant at every step, and once phase 1 succeeds
// both sides apply; there is no outcome in which one side moved and the other never will.
type Transfer struct {
	From, To         Scope // MUST share a realm
	FromPool, ToPool Pool  // "" = GeneralPool
	Amount           Credits
	IdemKey          string // REQUIRED
	Reason           string // "allocation" | "reallocation" | "reclaim"
	// MaxDestBalance caps what the destination may HOLD (0 = uncapped), checked against its hot
	// balance at request time. It guards against an errant admin action handing one team the pool.
	MaxDestBalance Credits
	Ref            map[string]string
}

// Receipt is the outcome of an idempotent apply.
type Receipt struct {
	IdemKey  string
	Applied  bool    // false = idempotent replay, already applied (same key, same fingerprint)
	Deferred bool    // true = accepted into the spend journal while the hot tier was unavailable
	Delta    Credits // the signed delta this call applied (0 on replay)
	Balance  Credits // resulting hot balance across pools (eventual; informational)
	Seq      int64   // the scope's sequence number for this operation (0 when deferred)

	RateCardVersion string  // the card that priced a Record (the wire receipt's rate_card_version)
	Writeoff        Credits // a negative grant's floored shortfall, booked by the writer as its own row
}
```

**Errors are typed**, because a caller acts differently on each: `ErrIdemConflict` (409 — a key
reused with a different request), `ErrStaleEvent` (422 — older than the dedup window),
`ErrMissingSubject` (400), `ErrUnpriceable` (422, fail closed), `ErrAmountOutOfRange` (422 —
above `MaxOperationAmount`), `ErrInsufficient` (409 — a transfer that would overdraw),
`ErrCrossRealm` (403), `ErrHotStoreUnavailable` (503 — `Admit` under `fail_closed`),
`ErrQuotaUnavailable` (503 — a limit sized by an entitlement that could not be resolved, under
`fail_closed`), `ErrUncountableLimit` (400 — see [Window counters](#window-counters)) and `ErrInvalid`
(400 — a malformed request). The hot tier's refusals — `ErrColdScope`, `ErrColdShard`,
`ErrShardFrozen`, `ErrBackpressure` — are typed too: the Meter rehydrates on the first and defers
usage to the journal on the others, so none of them reaches a `Record` caller.

---

## Credit pools

One balance per scope is not enough for a credit product. The first two requests are nearly
always *"promotional credits are spent before paid ones"* and *"these credits only work on GPU
jobs"* — and neither can be expressed by lot expiry order. A scope therefore holds one balance per
**pool** (`credit_pools`, realm configuration):

| Property | Meaning |
|---|---|
| `priority` | lower is drawn first. `general` is implicit and always drawn **last** |
| `applies_to` | the resources a pool may pay for; `NULL` = all. `general` pays for everything |

- **Draw order.** A debit draws its *eligible* pools (those whose `applies_to` covers the event's
  resource) in priority order, each down to zero; `general`, last, absorbs any overshoot. The pool
  deltas travel on the intent and become one ledger row per pool drawn.
- **Admission** measures the `Balance` window over the eligible pools only: GPU-only credits do not
  admit an LLM call.
- **Grants name a pool** (`plans.pool`, `Grant.Pool`). A refund or chargeback claws back from the
  pool the payment granted into, and `NegativeBalancePolicy` applies per pool.
- **Expiry** (`lots_fifo`) is per pool; **transfers** name a source and a destination pool.
- A realm that defines no pools has one balance per scope, exactly as before: `general`.

---

## Port: `Pricer` — a strategy over a versioned card

```go
// RateCard is IMMUTABLE, versioned configuration data (rate_cards / rate_card_entries; the
// database refuses edits). It names the Pricer that interprets it.
type RateCard struct {
	Realm   Realm
	Version string
	Pricer  string            // registry name: "table" (default) | a compiled pricer
	Entries []RateEntry
	Params  map[string]string // read only by a named pricer
}

// RateEntry prices one (rate key, unit) as a RATIONAL: CreditsPerBlock credits for every
// BlockSize units. $0.15 per million tokens at 1 credit = 1 µ$ is {150000, 1000000} — exact,
// where an integer "credits per unit" cannot express any price below one credit per unit (D31).
type RateEntry struct {
	RateKey            string
	Unit               Unit
	CreditsPerBlock    int64 // >= 0
	BlockSize          int64 // > 0
	CostMicrosPerBlock int64 // informational upstream cost; 0 = unknown
}

// Pricer converts a metered Event into an internal debit under a card. It is the ONE
// product-specific port — and for anything linear it needs no code at all, only a card for
// the built-in `table` pricer.
type Pricer interface {
	// Price MUST be a PURE function of (card, e.RateKey, e.Quantities): deterministic and
	// side-effect-free, so the SAME computation runs at the call site, in a dispute replay and in
	// an audit and yields the SAME number. It MUST NOT read e.Attributes or e.Subjects for cost,
	// MUST NOT perform I/O, and MUST fail CLOSED (ErrUnpriceable) on an unknown rate key or unit.
	// Arithmetic is exact; rounding is UP, once, at the end.
	Price(ctx context.Context, card RateCard, e Event) (Price, error)
}

// PricerRegistry resolves a card's Pricer by name. Every deployment has `table`; the reference
// pricers (`llmtoken`, `seat`, `storagebyte`) and a host's own are registered at wire time in
// cmd/. A card naming an unregistered pricer fails closed — this is how a container deployment
// serving several realms prices each with its own strategy.
type PricerRegistry map[string]Pricer
```

**Why pure matters:** dispute audits re-derive cost from the immutable event and the recorded card
version. A pricer that reaches for the network, a clock, or mutable rates cannot be replayed, and
the ledger stops being auditable. **Which card:** `Record` prices under the realm's active card at
the moment it is called and records that version on the ledger row; an event deferred to the
journal keeps the price computed at `Record`. A price change publishes a new version — which is also
why "changing a plan's price never affects already-granted credits" holds by construction.

**Where the Pricer runs** is a deployment choice with identical results: in-process in library
mode, inside the service in container mode (so rate cards never ship to callers, and a non-Go host
prices through the `table` pricer or a pricer compiled into its build of `paymentd`).

---

## Port: `Ledger` — the hot tier (request-serving)

> **In code** the hot-tier adapter is `ports.BalanceStore`: it embeds this `Ledger` and adds what only
> the writer, reconcile and recovery need — `ApplyDelta` (transfer_in, correction, expiry),
> `Snapshot`, `Rehydrate`, `Heal`, and the shard lifecycle (`OpenShard`/`BumpShard`/`FreezeShard`/
> `UnfreezeShard`/`NodeID`). The Meter depends only on `Ledger`. `Ledger.Admit` receives the FINAL,
> merged limit set; the adapter needs the realm's pools to resolve a resource's eligible pools.

```go
// Ledger is the hot tier. It runs INSIDE request-serving tiers and NEVER waits on the durable
// store: every mutation is ONE Redis Function in the scope's shard slot that applies the change,
// advances the scope's sequence number and emits exactly one intent (hot-path-consistency.md §2).
// It never writes the ledger — the LedgerWriter books every intent.
type Ledger interface {
	// Balance returns the HOT balance per pool — a fast copy. The ledger is the account of record.
	Balance(ctx context.Context, s Scope) (map[Pool]Credits, error)

	// Admit is the admission GATE: one read of the account and the relevant counters, then every
	// Limit evaluated, MaxCost included, against the eligible pools plus the overdraft.
	// It does NOT reserve or pre-debit (see invariants).
	Admit(ctx context.Context, s Scope, req AdmitRequest) (Admission, error)

	// Debit settles usage: draws the eligible pools in priority order (the last absorbs
	// overshoot), increments the window counters, advances seq, writes the idempotency guard and
	// XADDs the intent — atomically, in one slot. Same key + same fingerprint = replay
	// (Applied=false); same key + different fingerprint = ErrIdemConflict, never a silent no-op.
	Debit(ctx context.Context, c Charge) (Receipt, error)

	// Grant adds or removes credits in one pool. A negative grant applies NegativeBalancePolicy
	// inside the same function; a floor is reported as a writeoff that the writer books as its own
	// row, so balance == SUM(ledger) still holds (invariant 12).
	Grant(ctx context.Context, g Grant) (Receipt, error)

	// Transfer is phase 1: an atomic check-and-debit of the source's HOT pool that emits a
	// transfer_out intent, or ErrInsufficient. The writer completes it (see Transfer).
	Transfer(ctx context.Context, t Transfer) (TransferReceipt, error)
}

// TransferReceipt reports phase 1. The destination is credited by the writer within
// milliseconds; GET /v1/transfers/{idem_key} reports the transfer "settled".
type TransferReceipt struct {
	IdemKey     string
	Applied     bool    // false = idempotent replay
	Amount      Credits
	FromBalance Credits // the source's hot balance after the debit
	Status      string  // "in_transit" | "settled"
}
```

### Window counters

A window limit is a counter, not a balance, and the hot function that settles a charge must know
which counters to increment without being told the limits (a `Charge` carries none):

- **Configured limits** (`daily`, `hourly`, `rolling`, and `job` limits in the realm's rows) are
  counted for every charge they govern — by resource and by subject — keyed
  `ctr:{s<n>}:<tag>:<limit name>:<subject id>:<bucket>`. Daily and hourly buckets are the calendar
  day/hour in `Limit.TZ` (the hourly key carries the zone offset, so the repeated hour when clocks
  fall back is two counters); rolling is 12 sub-buckets, so its error is at most `Dur/12`. A bucket's
  lifetime is the time to the end of its window plus a margin, and is always positive — a zone that
  skips a calendar date still gets the next midnight that lies ahead — so a counter never expires
  while it is still the current bucket.
- **A job budget is counted whenever the call carries `Subjects["job"]`**, in credits and in each
  metered unit of the charge, keyed `…:job.<unit>:<job id>:-`. Its counter lives 24 h from the first
  charge (`DefaultJobTTL`): the limit that names its `Dur` is passed at `Admit`, which is not seen at
  settlement, so an abandoned job leaks one TTL'd key.
- **A caller can therefore tighten a configured limit and pass a job budget, but nothing else.** A
  caller-supplied `daily`/`hourly`/`rolling` limit whose `Name` is not configured could never be
  counted, so it would silently never bind — `ErrUncountableLimit`, not a no-op (invariant 17).

### When the hot tier cannot answer `Admit`

`AdmitFail` decides. Under `fail_closed` the caller gets `ErrHotStoreUnavailable` (503). Under
`fail_open` it is admitted with `Headroom = MaxOperationAmount` — *unknown, not zero*, because a
caller sizing its work by headroom must not be told it can afford nothing during an outage it is
being served through — and `metering_admit_failopen_total` counts it. Two things are NOT governed by
`AdmitFail`: a realm whose limits cannot be read fails closed regardless (serving without the
ceilings is serving unbounded), and a limit sized by an entitlement that cannot be resolved follows
`AdmitFail` only for that limit (`ErrQuotaUnavailable` under `fail_closed`; `domain.ErrQuotaNotGranted`
means nothing grants it, and the limit keeps its row's own `max`).

### Retries across a price change

The fingerprint of a usage charge includes its amount. A retry of one event after a new rate card
was published in between therefore prices differently and is `ErrIdemConflict` — loud, never a
double charge, and the first price stands. Producers retry with the same `OccurredAt`; a card
change inside one retry horizon is rare, and refusing it is the alternative to silently choosing
which price the customer was charged.

The canonical form is length-prefixed, so no split of the same characters across two fields can
encode alike, and a charge's `Quantities` are a **multiset**: they are sorted by unit, then amount,
before encoding, because the pricer sums them and a retry that lists them in another order — even
one that names a unit twice — is the same request, not a conflict.

## Port: `Meter` — orchestration callers actually use

```go
// Meter is the single entry point business code — and the billing module — depends on. It
// composes the PricerRegistry, the stores and the Ledger so callers never touch pricing math,
// keys, limits or the journal.
type Meter interface {
	// Admit gates before expensive work. It resolves the realm's configured limits for the
	// resource, sizes any with a max_entitlement from the QuotaSource, merges the caller's (which
	// can only tighten), and delegates to Ledger.Admit. A configured subject limit whose subject
	// the request does not carry is ErrMissingSubject.
	Admit(ctx context.Context, s Scope, req AdmitRequest) (Admission, error)

	// Record prices e under the realm's active card and settles it idempotently. Pass the
	// quantities ACTUALLY produced — partial on cancel (invariant 4). It refuses a stale
	// OccurredAt, and when the hot tier is unavailable — or Settlement=journal — it writes the
	// event to the spend journal first and returns Receipt.Deferred, so no usage is ever dropped
	// by an outage (invariant 16).
	Record(ctx context.Context, e Event) (Receipt, error)

	// Grant mints or removes credits. PRIVILEGED: separated at the RPC, the route, the authz
	// check and the audit trail (invariant 13).
	Grant(ctx context.Context, g Grant) (Receipt, error)

	// Transfer moves credits pool → allocation. PRIVILEGED.
	Transfer(ctx context.Context, t Transfer) (TransferReceipt, error)
}
```

## Port: `LedgerWriter` — the sole durable writer

```go
// LedgerWriter is the ONLY component that writes the account of record: credit_ledger,
// account_credits, the watermarks, suspense and the idempotency guards. It runs in
// `cmd/payment-worker`, never a request-serving tier, with ONE owner per shard at a time
// (a Postgres advisory lock). Correctness rests on idempotency rows and per-scope transaction
// locks, NOT on "only one replica runs"; single ownership exists for ordering and throughput.
type LedgerWriter interface {
	// Drain books a shard's intents IN PER-SCOPE SEQUENCE ORDER, batched, then XACKs them.
	// A redelivery, a gap and a sequence regression are told apart by the watermark plus the
	// idempotency row — never by timing (hot-path-consistency.md §3).
	Drain(ctx context.Context, shard Shard) (booked int, err error)

	// Reconcile compares each recently booked scope's hot account with booked + open suspense AT
	// THE SAME SEQUENCE NUMBER. Intents in flight are `deferred`, not drift; a hot sequence behind
	// the watermark is a `regression` that freezes the shard. Real drift is healed on the HOT side
	// by compare-and-set and pages beyond Tolerance. It NEVER books a ledger row.
	Reconcile(ctx context.Context, shard Shard, kind ReconcileKind) (ReconcileRun, error)

	// Rehydrate rebuilds a stale or brand-new account from booked + open suspense at
	// applied_seq, and only while it is still stale, so concurrent callers converge.
	Rehydrate(ctx context.Context, s Scope) error

	// Recover runs freeze → drain → bump generation → unfreeze for a shard after a total loss,
	// a rollback or a reshard (hot-path-consistency.md §5).
	Recover(ctx context.Context, shard Shard, reason string) error

	// Audit verifies booked == checkpoint + Σ ledger since the checkpoint, per scope and pool,
	// and advances the checkpoints that make archiving old ledger partitions safe.
	Audit(ctx context.Context, shard Shard) (AuditReport, error)
}

// Three idempotent sweeps complete work an effect could not, and run on their own ticks rather than
// through the port: RedriveTransfers (billing.transfer.tick — the credit leg of a transfer in transit
// longer than TransferRedrive), ReissueCorrections (the hot-side reversal of a suspense entry whose
// first attempt did not land; never past HotIdemTTL) and the Expirer (billing.expiry.tick — see FR-016).
// Every hot mutation they issue is keyed, so a repeat is a REPLAY, never a second effect.

// The writer reads the outbox through the narrow IntentStream port (redisstreams), not a Bus:
//
//   Read(ctx, shard, count, minIdle) — entries a dead consumer left un-acked past minIdle first, oldest
//     first (XAUTOCLAIM), then new ones; each carries its delivery count, so an entry is parked after
//     MaxAttempts even across a restart, and an entry that cannot be parsed arrives with ParseErr
//     (it is parked whole, never dropped, FR-043). A consumer group lost with a rollback is created
//     again from the start.
//   Ack(ctx, shard, ids...)          — never deletes; retention is explicit (Trim, once acked AND old).
//   Stats(ctx, shard)                — length, pending, undelivered and the age of the oldest of each.
//   Trim(ctx, shard, keep)           — drops history every group has acknowledged and that is past the retention
//     floor; never an entry any group has not acknowledged or been delivered (that is deleting a money intent).

// Shard is fnv1a64(Scope.Tag()) mod Shards — the unit of Redis Cluster placement and of writer
// ownership. The count is recorded in hot_config and changed only by a reshard.
type Shard int

type ReconcileKind string // "incremental" (scopes booked since the last run) | "full" (every scope)

type ReconcileFinding struct {
	Scope              Scope
	Pool               Pool
	Outcome            string // "drift" | "regression"
	HotSeq, AppliedSeq int64
	Hot, Expected      Credits // Expected = booked + open suspense
	Drift              Credits
	Healed, Alarmed    bool
}

type ReconcileRun struct {
	Shard                                            Shard
	Checked, InSync, Deferred, Cold, Regressions, Drifted int
	AbsDrift Credits // Σ |drift| — never a net figure, so offsetting errors cannot cancel out
	Findings []ReconcileFinding
}
```

---

## Invariants every implementation MUST uphold

1. **Single durable writer.** Exactly one component — the `LedgerWriter`, one owner per shard — writes `credit_ledger`, `account_credits`, the watermarks, suspense and the idempotency guards. Request-serving tiers write only the hot tier (through the hot functions) and the `spend_journal` (intents, not the books). No second money-writer — not a host service, not a provider callback, not a request handler, not a transfer (SC-002).
2. **Dual idempotency, fingerprinted.** Every credit-affecting operation carries an `IdemKey` and is guarded twice: a hot guard in the scope's slot for speed, and a durable guard for correctness — `usage_idem` per scope for consumption (window-bounded, with stale events refused), `credit_idem` per `(realm, op)` for anything that mints or moves money (permanent). Both store a request fingerprint: the same key with a different request is `ErrIdemConflict`, never a silent `Applied:false`. The hot guard is never the guarantee — not across its TTL and not across an asynchronous failover. See [hot-path-consistency.md §6](./hot-path-consistency.md#6-idempotency-two-guards-two-lifetimes).
3. **Admission is a gate, not a reservation.** `Admit` reads current balances and counters and refuses an over-limit call; it does not pre-debit. Concurrent admits against a near-empty balance may settle it slightly negative — **bounded overshoot ≤ in-flight concurrency × `AdmitRequest.MaxCost`**. That bound is only real when callers supply `MaxCost`; an implementation MUST count omissions (`metering_admit_uncapped_total`), and `RequireMaxCost` makes it a precondition. A hard per-call reservation is opt-in, not the default (it either serializes a scope or doubles writes).
4. **Partial settlement on cancel.** A unit of work aborted midway is billed for what it **actually produced** — not zero (a free-usage exploit) and not the full ceiling (over-billing). The engine never sees the work, so this is a contract obligation on the caller.
5. **Deterministic, replayable, exact pricing.** `Pricer.Price` is pure over the card, the rate key and the quantities; prices are rationals, arithmetic is exact, rounding is up and happens **once per event**; unknown rate keys and units fail **closed**. Every ledger row persists `rate_card_version`, because replay is only possible if the row names its card. Rate cards are insert-only, enforced by the database.
6. **Integer money only.** `Credits int64` internally, in a fine accounting unit; `Money{MinorUnits, Currency}` at the fiat boundary; prices as integer rationals. Never floats — not in a balance, a price, a ledger row or a log line. A single operation above `MaxOperationAmount` (default 2^50) is refused, which keeps every hot balance inside the range Redis Lua represents exactly.
7. **The under-bill window is stated exactly.** In `outbox` settlement, a hot-store crash or asynchronous failover can lose the most recent **acknowledged** intents — at most the AOF fsync interval or the replication lag — *together with their balance change*, because Redis persists and replicates a function's effects as one unit. Both halves vanish, so **no reconcile can see the loss**: it is silent under-billing, never double-billing. `HotAckWait` narrows the window and `journal` settlement removes it. Implementations MUST state this and MUST NOT claim reconcile recovers it. The opposite case — the books holding intents a rolled-back Redis forgot — loses nothing: it is detected as a sequence regression and the shard is rebuilt from the books.
8. **Atomic usage-log + debit.** `Record`'s intent carries the usage record (quantities, subjects, resource, card version) and is emitted by the same function, in the same slot, as the balance change — never as two best-effort writes that could log without billing or bill without logging.
9. **Hot equals booked plus open suspense, at every sequence number.** Reconcile compares the two only at equal sequence numbers, so in-flight intents are never mistaken for drift; any disagreement there is a defect, is healed on the **hot** side by compare-and-set, and pages beyond `Tolerance` (default 0). Reconcile never books a ledger row; the books change only through intents and audited adjustments.
10. **Scope opacity.** The kernel never parses, ranks, or special-cases a `Scope`. Re-anchoring the billing subject changes only the `Scope` the host constructs.
11. **Realm isolation.** No read or write crosses a realm. Every key, intent, ledger row, plan, rate card, limit, pool and idempotency guard is realm-partitioned. A caller authenticated for realm A can never name a scope in realm B (enforced at the transport, see [security.md](../../../docs/security.md)).
12. **A negative balance is a policy, never an accident.** A refund or chargeback for credits already consumed drives a pool below zero. `NegativeBalancePolicy` decides, per pool, inside the hot function: `allow_debt` (record it; the host collects), `clamp_to_zero` (floor it and book a `writeoff` row for the shortfall) or `block_and_flag` (floor it, book the writeoff, and refuse spend until an operator clears it). A floor never forgives debt that already existed — it is `min(balance_before, 0)` — and silently flooring with no compensating row is forbidden.
13. **Grants are privileged.** Minting is separated from spending at the RPC, the route, the authz check and the audit trail. A caller able to `Record` must not be able to `Grant` or `Transfer`. Grants originate only from a verified payment, an audited administrative action, or a host's own signup/promo path.
14. **A transfer conserves the realm.** `Σ credit_ledger + Σ in-transit` is unchanged by every step of a transfer. Phase 1 refuses rather than overdraws, against the source's hot balance; once it succeeds, both sides apply, and the paired `allocation_out`/`allocation_in` rows share one key and sum to zero. A transfer in transit longer than `TransferTransitAlarm` pages.
15. **Every terminal state is observable.** Drift, a regression, a poison intent, a late replay, an idempotency conflict, a failed webhook verification, an uncapped admit, a deferred record, an exhausted retry budget, a stuck transfer and a frozen shard each emit a named metric and land in durable storage. "Logged and moved on" is not a permitted outcome for anything that touched money.
16. **No usage is dropped by an outage.** When the hot tier is unavailable, `Record` writes the event to the spend journal and returns `Deferred`; the journal is replayed through the hot path on recovery. `fail_open` therefore means *served before it is metered*, never *served unmetered*.
17. **A ceiling is never silently skipped.** A configured subject limit whose subject the call omits is refused, and a limit whose size comes from an entitlement fails with `AdmitFailPolicy` — not open — when that entitlement cannot be resolved.
18. **Configuration changes are audited by the database.** Every write to rate cards, limits, pools, plans, prices, provider accounts, entitlements and endpoints writes an `audit_log` row and a cache-invalidation notification in the same transaction, whether it came through the admin API or through `psql`.

---

## Settlement durability (customizable knobs)

Two settings select the durability/latency trade for `Record`, without changing any port signature:

| `Settlement` | Hot-path cost | On a hot-store loss | Use when |
|---|---|---|---|
| `outbox` (default) | one Redis Function, no durable wait | can lose the last acknowledged intents within the `HotAckWait` window — silently, under-billing (invariant 7) | commodity spend where a sub-second RPO beats sub-ms enforcement |
| `journal` | + one synchronous Postgres insert before the Redis Function | nothing: the intent's first durable home is Postgres | high-value units, regulated billing, revenue-loss RPO ≈ 0 |

| `HotAckWait` (outbox only) | Mechanism | Added latency | Window |
|---|---|---|---|
| `none` (default) | — | — | ≤ 1 s on a crash (AOF `everysec`), plus replication lag on a failover |
| `aof_local` | `WAITAOF 1 0` after the function | one local fsync — group-committed under `appendfsync always`; **up to ~1 s under `everysec`** (the wait ends at the next fsync tick) | a crash loses nothing acknowledged; a failover still loses the replication lag |
| `aof_replica` | `WAITAOF 0 1` | a replica round trip + its fsync | an acknowledged intent survives losing the primary |

Every mode satisfies invariants 1, 2 and 8; they differ only in *when* an intent becomes crash-safe.
The outage fallback (invariant 16) uses the journal in every mode.

---

## Reference wiring: LLM-token metering as ONE implementation

The binding that first exercised these ports was an AI product metering LLM tokens. Nothing in the kernel knows that:

| Generic port / type | LLM-metering binding |
|---|---|
| `Realm` | `"aisat-intel"` — one of N products on the deployment |
| `Scope` | `{Kind: "workspace", ID: workspace_id}`, later `{Kind: "organization", …}` — a binding change, no kernel edit |
| `Subjects` | `{"user": member_id, "job": run_id}` from the request context |
| `Pricer` | the `table` pricer over a card of rationals: gpt-4o input `{2500000, 1000000}`, output `{10000000, 1000000}`, cached `{1250000, 1000000}` at 1 credit = 1 µ$ |
| `Event.Quantities` | `[{llm_input_token, n}, {llm_cached_token, c}, {llm_output_token, m}]` from the gateway's returned usage |
| `AdmitRequest.MaxCost` | `max_tokens` priced through the same card — the request's own ceiling, so overshoot is bounded |
| `Window: Job` | `Limit{Name: "run_cap", Subject: "job", Window: Job, Max: …}` passed per run — bounds a runaway agent loop independently of the daily ceiling |
| `Transfer` | an organization buys the pool; each workspace draws an allocation with an optional cap |
| `Limit[]` | `scope_balance` (Balance, overdraft 0 → 402) · `user_daily` (Subject `user`, Daily, sized by the plan's `daily_user_credits` entitlement → 409) · `burst_hourly` (Subject `user`, Rolling 1 h → 409) |
| Pools | `promo` (priority 10, drawn first) for signup credits; `general` for purchases |
| `Charge.Reason` / `Grant.Reason` | `credit_ledger.operation_type`: `query`/`ingest`/`agent_step` · `purchase`/`subscription_grant`/`signup_grant`/`refund` |
| `Ledger` / `LedgerWriter` | Redis Functions + `billing:outbox:{s<n>}` in the scope's shard slot → one writer per shard → `credit_ledger` + reconcile at the watermark |
| Payments (`Grant` producers) | [`payment-provider-ports.md`](./payment-provider-ports.md): the webhook inbox processor calls `Meter.Grant` for a verified payment |

Three other bindings the same ports serve without a kernel edit, to show the shape is not accidental:

| Product | `Scope.Kind` | `Unit`s | Pricing | Notable `Limit` |
|---|---|---|---|---|
| Object storage | `account` | `storage_byte_day`, `egress_byte` | `table`: flat per GB-day and per GB. Tiers and free allowances depend on period-to-date volume, so they are a `Rater` concern ([feature 002](../../002-postpaid-invoicing/)) | `Balance` + `Rolling` egress burst |
| Seat-based SaaS | `organization` | `seat_day` | `table`: flat per seat-day. Mid-period proration of a subscription is the provider's, under `PlanChangePolicy` | `Balance` only; the `Entitler` carries the feature gates |
| Public API | `api_key` | `api_call`, `compute_ms` | `table`: per-endpoint class as the rate key | `Hourly` + `Rolling`, Subject `api_key` |

Swapping products is: publish a card (or register a `Pricer`), set `Scope.Kind`, declare the `Unit`s, configure the `Limit` rows. The ledger, outbox, reconcile, idempotency, entitlement and payment code are untouched.

---

## Reference `Pricer`: `table`

The data-driven default, behind every deployment's `PricerRegistry`. It ships in
`metering/adapters/driven/pricing/table`; the `llmtoken`, `seat` and `storagebyte` reference
pricers validate their own unit vocabulary (e.g. that cached tokens are not also counted as input)
and then delegate to it.

```go
package table // metering/adapters/driven/pricing/table

import (
	"context"
	"fmt"
	"math/big"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Pricer prices Σ quantity × CreditsPerBlock / BlockSize over an event's quantities, EXACTLY,
// and rounds UP once. It covers every per-unit linear price — tokens, calls, GB-days, seconds,
// requests — without code. Pure: reads only the card, the rate key and the quantities.
type Pricer struct{ MaxAmount int64 } // MaxOperationAmount; 0 = 2^50

func (p Pricer) Price(_ context.Context, card domain.RateCard, e domain.Event) (domain.Price, error) {
	rates := make(map[domain.Unit]domain.RateEntry)
	for _, r := range card.Entries { // in practice indexed once per card version and cached
		if r.RateKey == e.RateKey {
			rates[r.Unit] = r
		}
	}
	if len(rates) == 0 { // an unknown rate key fails closed even for an empty event
		return domain.Price{}, fmt.Errorf("%w: no rate key %q in card %q", domain.ErrUnpriceable, e.RateKey, card.Version)
	}
	credits, cost := new(big.Rat), new(big.Rat)
	for _, q := range e.Quantities {
		if q.Amount < 0 {
			return domain.Price{}, fmt.Errorf("%w: negative quantity for %q", domain.ErrUnpriceable, q.Unit)
		}
		r, ok := rates[q.Unit]
		if !ok {
			return domain.Price{}, fmt.Errorf("%w: no rate for %q/%q in card %q", domain.ErrUnpriceable, e.RateKey, q.Unit, card.Version)
		}
		qty := big.NewInt(q.Amount)
		credits.Add(credits, new(big.Rat).SetFrac(new(big.Int).Mul(qty, big.NewInt(r.CreditsPerBlock)), big.NewInt(r.BlockSize)))
		cost.Add(cost, new(big.Rat).SetFrac(new(big.Int).Mul(qty, big.NewInt(r.CostMicrosPerBlock)), big.NewInt(r.BlockSize)))
	}
	c, m := ceil(credits), ceil(cost)
	limit := p.MaxAmount
	if limit == 0 {
		limit = 1 << 50
	}
	if !c.IsInt64() || c.Int64() > limit || !m.IsInt64() {
		return domain.Price{}, fmt.Errorf("%w: %s credits", domain.ErrAmountOutOfRange, c)
	}
	return domain.Price{
		Credits:         domain.Credits(c.Int64()),
		CostMicros:      m.Int64(),
		RateCardVersion: card.Version, // travels onto the ledger row — invariant 5
	}, nil
}

// ceil rounds a non-negative rational UP: rounding never under-bills, and with a fine
// accounting unit it never over-bills by more than one unit per event.
func ceil(r *big.Rat) *big.Int {
	n := new(big.Int).Add(r.Num(), new(big.Int).Sub(r.Denom(), big.NewInt(1)))
	return n.Quo(n, r.Denom())
}
```

Worked examples at 1 credit = 1 µ$: 1,200 gpt-4o input + 800 output tokens = 3,000 + 8,000 =
**11,000** credits, exactly. A 100-token gpt-4o-mini call = 100 × 150,000 / 1,000,000 = **15**
credits — exactly its list cost, where the previous design (integer µ$ per token, then a whole
1,000-µ$ credit per event) billed it at 1,000. `big.Rat` costs a few hundred nanoseconds per event;
an implementation may use checked 128-bit arithmetic instead, provided the conformance suite passes.

---

## Contract-test skeleton

These validate *any* implementation of the ports against the invariants. The `Pricer` suite is pure (no infra); the `Ledger` and consistency suites run against real Redis and Postgres via Testcontainers (`//go:build integration`). The Redis half of the hot path is also exercised directly, on a cluster-mode Redis, by [`scripts/verify-hot-path.sh`](../../../scripts/verify-hot-path.sh).

> **The seam is only *proven* by a second implementation.** `PricerContract` MUST run against the `table` pricer over **three cards in disjoint `Unit`/`RateKey` spaces** (tokens, seat-days, byte-days) **and** against at least one compiled pricer resolved through the `PricerRegistry`. If it passes, "swap one card or one `Pricer`, no money-path change" is a test result rather than an assertion.

```go
package domain_test

// PricerContract runs against ANY Pricer and a card for it — pure, no infra.
// fx supplies a known rate key, a unit priced BELOW one credit per unit, and a second unit.
func PricerContract(t *testing.T, p ports.Pricer, card domain.RateCard, fx PricerFixture) {
	ctx := context.Background()
	base := domain.Event{RateKey: fx.RateKey, IdemKey: "e1",
		Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: 1000}, {Unit: fx.UnitB, Amount: 500}}}

	t.Run("deterministic", func(t *testing.T) {
		a, err := p.Price(ctx, card, base); mustNoErr(t, err)
		b, _ := p.Price(ctx, card, base)
		if a != b { t.Fatalf("non-deterministic: %+v != %+v", a, b) }
	})
	t.Run("attributes and subjects never affect price", func(t *testing.T) {
		w := base
		w.Attributes = map[string]string{"trace_id": "t9"}
		w.Subjects = domain.Subjects{"user": "u9"}
		a, _ := p.Price(ctx, card, base); b, _ := p.Price(ctx, card, w)
		if a != b { t.Fatalf("attributes/subjects changed price: %+v != %+v", a, b) }
	})
	t.Run("empty event under a known rate key is free", func(t *testing.T) {
		z := base; z.Quantities = nil
		got, err := p.Price(ctx, card, z); mustNoErr(t, err)
		if got.Credits != 0 { t.Fatalf("empty event not free: %+v", got) }
	})
	t.Run("fails closed on an unknown rate key or unit", func(t *testing.T) {
		u := base; u.RateKey = "no-such-key"
		if _, err := p.Price(ctx, card, u); !errors.Is(err, domain.ErrUnpriceable) { t.Fatal("unknown key must fail closed") }
		v := base; v.Quantities = []domain.Quantity{{Unit: "no-such-unit", Amount: 1}}
		if _, err := p.Price(ctx, card, v); !errors.Is(err, domain.ErrUnpriceable) { t.Fatal("unknown unit must fail closed") }
	})
	t.Run("a sub-credit unit price is exact, not rounded per unit", func(t *testing.T) {
		// fx.UnitA costs fx.Num/fx.Den credits per unit with fx.Num < fx.Den (e.g. 0.15).
		n := fx.Den * 7 // a quantity at which the exact price is a whole number
		got, err := p.Price(ctx, card, domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: n}}})
		mustNoErr(t, err)
		if int64(got.Credits) != fx.Num*7 { t.Fatalf("inexact: %d credits for %d units, want %d", got.Credits, n, fx.Num*7) }
	})
	t.Run("rounds up once per event, never per unit or per term", func(t *testing.T) {
		one := domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitA, Amount: 1}, {Unit: fx.UnitA, Amount: 1}}}
		got, _ := p.Price(ctx, card, one)
		if want := ceilDiv(2*fx.Num, fx.Den); int64(got.Credits) != want { t.Fatalf("rounded per term: %d want %d", got.Credits, want) }
	})
	t.Run("monotonic in quantity", func(t *testing.T) {
		more := base; more.Quantities = []domain.Quantity{{Unit: fx.UnitA, Amount: 2000}, {Unit: fx.UnitB, Amount: 500}}
		a, _ := p.Price(ctx, card, base); b, _ := p.Price(ctx, card, more)
		if b.Credits < a.Credits { t.Fatalf("more usage cost less: %d < %d", b.Credits, a.Credits) }
	})
	t.Run("reports the card version", func(t *testing.T) {
		got, _ := p.Price(ctx, card, base)
		if got.RateCardVersion != card.Version { t.Fatal("the card version must travel onto the ledger row") }
	})
	t.Run("refuses an amount above MaxOperationAmount", func(t *testing.T) {
		huge := domain.Event{RateKey: fx.RateKey, Quantities: []domain.Quantity{{Unit: fx.UnitB, Amount: math.MaxInt64 / 2}}}
		if _, err := p.Price(ctx, card, huge); !errors.Is(err, domain.ErrAmountOutOfRange) { t.Fatal("overflow must fail closed") }
	})
}

// LedgerContract runs against a REAL ports.Ledger + its writer (Testcontainers Redis + Postgres).
// drain(t) runs the writer to quiescence, so eventual effects can be asserted.  //go:build integration
func LedgerContract(t *testing.T, newLedger func(t *testing.T, opts ...Opt) (ports.Ledger, Drainer)) {
	ctx := context.Background()
	ws := domain.Scope{Realm: "t1", Kind: "workspace", ID: uuidv7()}
	balanceOnly := domain.Limit{Name: "scope_balance", Unit: "credit", Max: 0, Window: domain.Balance, DenyCode: "payment_required"}

	t.Run("debit is idempotent on idem_key", func(t *testing.T) {
		l, _ := newLedger(t); seed(t, l, ws, 1000)
		c := domain.Charge{Scope: ws, Amount: 100, IdemKey: "chg-1", Reason: "query", Resource: "llm.chat"}
		r1, err := l.Debit(ctx, c); mustNoErr(t, err)
		if !r1.Applied { t.Fatal("first debit should apply") }
		r2, err := l.Debit(ctx, c); mustNoErr(t, err)
		if r2.Applied || r2.Seq != r1.Seq { t.Fatal("replay must be a no-op reporting the original seq") }
		if bal := hot(t, l, ws); bal != 900 { t.Fatalf("double-charged: balance=%d want 900", bal) }
	})
	t.Run("a reused key with a different request is a conflict, not a silent no-op", func(t *testing.T) {
		l, _ := newLedger(t); seed(t, l, ws, 1000)
		_, _ = l.Debit(ctx, domain.Charge{Scope: ws, Amount: 100, IdemKey: "k", Reason: "query"})
		_, err := l.Debit(ctx, domain.Charge{Scope: ws, Amount: 700, IdemKey: "k", Reason: "query"})
		if !errors.Is(err, domain.ErrIdemConflict) { t.Fatalf("want ErrIdemConflict, got %v", err) }
	})
	t.Run("admit gates without reserving", func(t *testing.T) {
		l, _ := newLedger(t); seed(t, l, ws, 50)
		adm, err := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}}); mustNoErr(t, err)
		if !adm.Allowed { t.Fatal("positive balance should admit") }
		if bal := hot(t, l, ws); bal != 50 { t.Fatalf("admit pre-debited: %d", bal) }
	})
	t.Run("MaxCost bounds overshoot at the gate and reports headroom", func(t *testing.T) {
		l, _ := newLedger(t); seed(t, l, ws, 50)
		adm, _ := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}, MaxCost: 10_000})
		if adm.Allowed || adm.Headroom != 50 { t.Fatalf("want refused with headroom 50: %+v", adm) }
	})
	t.Run("promotional credits are drawn before paid ones; general absorbs overshoot", func(t *testing.T) {
		l, _ := newLedger(t, withPools(pool("promo", 10)))
		grant(t, l, ws, "general", 1000); grant(t, l, ws, "promo", 100)
		_, _ = l.Debit(ctx, domain.Charge{Scope: ws, Amount: 150, IdemKey: "p1", Resource: "llm.chat"})
		if b := pools(t, l, ws); b["promo"] != 0 || b["general"] != 950 { t.Fatalf("wrong draw order: %v", b) }
	})
	t.Run("idem keys do not collide across realms", func(t *testing.T) {
		l, d := newLedger(t)
		a := domain.Scope{Realm: "prod-a", Kind: "org", ID: "1"}
		b := domain.Scope{Realm: "prod-b", Kind: "org", ID: "1"}
		_, _ = l.Grant(ctx, domain.Grant{Scope: a, Amount: 100, IdemKey: "invoice-1", Reason: "purchase"})
		r, err := l.Grant(ctx, domain.Grant{Scope: b, Amount: 100, IdemKey: "invoice-1", Reason: "purchase"})
		mustNoErr(t, err); d.Drain(t)
		if !r.Applied || booked(t, b) != 100 { t.Fatal("realm B's grant was swallowed by realm A's key") }
	})
	t.Run("clamp_to_zero books a compensating writeoff and never forgives existing debt", func(t *testing.T) {
		l, d := newLedger(t, withPolicy(domain.ClampToZero)); seed(t, l, ws, 10)
		_, _ = l.Grant(ctx, domain.Grant{Scope: ws, Amount: -100, IdemKey: "cb-1", Reason: "chargeback"})
		d.Drain(t)
		if hot(t, l, ws) != 0 || !hasLedgerRow(t, ws, "writeoff", 90) { t.Fatal("clamp must floor and book +90 writeoff") }
		if booked(t, ws) != hot(t, l, ws) { t.Fatal("balance != SUM(ledger) after a clamp") }
	})
	t.Run("a job budget halts work without touching any calendar ceiling", func(t *testing.T) {
		l, _ := newLedger(t); seed(t, l, ws, 1_000_000)
		run := domain.Subjects{"job": "r1"}
		cap := domain.Limit{Name: "run_cap", Subject: "job", Unit: "credit", Max: 500, Window: domain.Job, Dur: time.Hour, DenyCode: "limit_reached"}
		for i := 0; i < 5; i++ {
			_, _ = l.Debit(ctx, domain.Charge{Scope: ws, Amount: 100, IdemKey: fmt.Sprintf("step-%d", i), Reason: "agent_step", Subjects: run})
		}
		adm, _ := l.Admit(ctx, ws, domain.AdmitRequest{Subjects: run, Limits: []domain.Limit{cap}, MaxCost: 100})
		if adm.Allowed { t.Fatal("a runaway loop must halt at its own cap") }
		if hot(t, l, ws) != 999_500 { t.Fatal("a job budget is a counter, not a granted balance") }
	})
	t.Run("a subject limit without its subject is refused, never skipped", func(t *testing.T) {
		l, _ := newLedger(t); seed(t, l, ws, 1000)
		daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: "credit", Max: 10, Window: domain.Daily, DenyCode: "limit_reached"}
		if _, err := l.Admit(ctx, ws, domain.AdmitRequest{Limits: []domain.Limit{daily}}); !errors.Is(err, domain.ErrMissingSubject) {
			t.Fatal("a ceiling that silently does not apply is the worst kind")
		}
	})
	t.Run("transfer: refuses to overdraw, then applies both sides exactly once", func(t *testing.T) {
		l, d := newLedger(t)
		pool := domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}
		alloc := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
		seed(t, l, pool, 5000)
		if _, err := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 9000, IdemKey: "over"}); !errors.Is(err, domain.ErrInsufficient) {
			t.Fatal("a transfer must refuse rather than overdraw")
		}
		r, err := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"})
		mustNoErr(t, err)
		if r.Status != "in_transit" || r.FromBalance != 3000 { t.Fatalf("phase 1: %+v", r) }
		assertRealmConserved(t, "t1") // Σ ledger + Σ in-transit, checked mid-flight
		d.Drain(t)
		if hot(t, l, alloc) != 2000 || booked(t, alloc) != 2000 || booked(t, pool) != 3000 { t.Fatal("both sides must apply") }
		r2, _ := l.Transfer(ctx, domain.Transfer{From: pool, To: alloc, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"})
		d.Drain(t)
		if r2.Applied || booked(t, pool) != 3000 { t.Fatal("a replayed allocation must be a no-op") }
		if _, err := l.Transfer(ctx, domain.Transfer{From: pool, To: domain.Scope{Realm: "t2", Kind: "w", ID: "x"}, Amount: 1, IdemKey: "xr"}); !errors.Is(err, domain.ErrCrossRealm) {
			t.Fatal("a cross-realm transfer is a mint and a burn, never a transfer")
		}
	})
}

// Wiring — every impl plugs into the SAME suites:
//   func TestTablePricer_Tokens(t *testing.T)    { PricerContract(t, table.Pricer{}, tokenCard(), tokenFx) }
//   func TestTablePricer_Seats(t *testing.T)     { PricerContract(t, table.Pricer{}, seatCard(), seatFx) }
//   func TestTablePricer_Bytes(t *testing.T)     { PricerContract(t, table.Pricer{}, byteCard(), byteFx) }
//   func TestLLMTokenPricer_Registry(t *testing.T){ PricerContract(t, registry()["llmtoken"], tokenCard(), tokenFx) }
//   func TestRedisLedger_Contract(t *testing.T)   { LedgerContract(t, newRedisLedgerWithWriter) }     // integration/writer_test.go
//   func TestWriter_Contract(t *testing.T)        { LedgerWriterContract(t, newWriterHarness) }        // in-order booking, gaps, suspense, reconcile, expiry
//   func TestConsistency_Contract(t *testing.T)   { ConsistencyContract(t, newRollbackHarness) }       // hot-path-consistency.md §9: truncated AOF; lagging replica
```

---

## Deployment topology: embedded library **or** standalone container service

Both shapes ship from this repo, over one set of interfaces. Which one a host picks is a wiring decision in its `cmd/`, and it is reversible:

1. **Embedded library.** The host imports `github.com/truongpx396/intel-payment/metering`; `Admit`/`Record` are in-process Redis calls — sub-ms, no extra network hop. Best when the host is Go and owns the Redis/Postgres anyway. The durable half is *already* its own runtime either way: the `LedgerWriter` runs in the worker.
2. **Standalone container service** (`deploy/docker-compose.yml`). The same ports behind a gRPC + REST facade; the service owns the credit Redis, the outbox and the payment tables. Callers get a client stub — or, for non-Go hosts, speak gRPC/REST directly and receive events as signed webhooks ([outbound-events.md](./outbound-events.md)). **This is the default for a polyglot host, and the only option for a non-Go one.** Pricing is the `table` pricer or a pricer compiled into the host's build of `paymentd`.

| Concern | Extract as a service? | Note |
|---|---|---|
| Durable writer (`Drain`/`Reconcile`/`Recover`/`Audit`) | ✅ **already separate** | the worker role — one owner per shard |
| Producer → ledger decoupling | ✅ already | the `billing:outbox:{s<n>}` streams cross the boundary |
| Hot-path `Admit`/`Record` | ✅ but adds one network hop | co-locate (same node/AZ, or a sidecar over a Unix socket); the Redis Function is one round trip server-side |
| `Scope` / tenancy context | ✅ | already passed explicitly — no ambient RLS needed at the port |
| Pricing | ✅ | the `PricerRegistry` runs inside the service; pure + versioned either way |
| Single-writer invariant | ✅ preserved | the service's worker *is* the one writer |

What makes it **not free** (decide these before extracting):
- **Latency** — `Admit` is on every request's critical path. A hop is fine co-located; a cross-region metering service taxes every call. Keep the API coarse, never chatty.
- **Availability** — the service becomes **Tier-0**. Pick `fail_open` (serve; `Record` defers to the journal, so nothing goes unmetered) vs `fail_closed` (refuse) explicitly.
- **Auth between caller ↔ service** — it mutates money, so callers authenticate (mTLS / signed service token); `Grant` and `Transfer` are separate privileges.
- **Redis ownership moves with the service** — it owns `acct:`, `idem:`, `ctr:`, `meta:` and `billing:outbox:` keys. That store MUST be `noeviction` + AOF, MUST NOT be shared with a host's cache, and MUST NOT be reachable by hosts at all: write access to it is the ability to mint credits ([security.md](../../../docs/security.md)).
- **Region** — one home region per deployment; disaster recovery is Postgres replication plus a cold Redis rebuilt from the books ([operations.md](../../../docs/operations.md#disaster-recovery-region-loss)).

**Recommended posture:** a Go host with one product starts **embedded**. Everyone else — a non-Go host, or the moment a *second* product must draw on the same pool — runs the **co-located container service**, one per availability zone, with the realm axis keeping the products' books apart. The swap in either direction is a `cmd/` wiring change plus a deploy, never a data migration.

---

## Service surface: `MeteringService` (gRPC, contract-locked)

The service-mode facade is the SAME `Meter` port over the wire. gRPC (not REST) because this is an internal, latency-sensitive, strongly-typed hot path. Money crosses the wire as `int64` credits (never floats); `idem_key` is required on every mutating RPC; `Scope` stays opaque.

```proto
syntax = "proto3";
package metering.v1;
option go_package = "github.com/truongpx396/intel-payment/api/meteringv1";
import "google/protobuf/timestamp.proto";

message Scope    { string realm = 1; string kind = 2; string id = 3; } // opaque to the service
message Quantity { string unit = 1; int64 amount = 2; }                // integer counts only

message Event {
  Scope scope = 1;
  string resource = 2;
  string rate_key = 3;                        // pricing selector (model / tier / sku)
  repeated Quantity quantities = 4;
  string idem_key = 5;                        // REQUIRED — exactly-once identity
  google.protobuf.Timestamp occurred_at = 6;  // REQUIRED, identical on every retry
  map<string,string> attributes = 7;          // audit only — never a cost input
  map<string,string> subjects = 8;            // kind → id: keys subject ceilings
}

enum Window { BALANCE = 0; DAILY = 1; HOURLY = 2; ROLLING = 3; JOB = 4; }
message Limit {
  string name = 1; string subject = 2; string resource = 3; string unit = 4; int64 max = 5;
  Window window = 6; int64 dur_seconds = 7; string tz = 8;
  double warn_at = 9;                          // a fraction, not money
  string deny_code = 10;                       // "payment_required" | "limit_reached"
}
message Admission { bool allowed = 1; Limit exceeded = 2; Limit warning = 3;
                    int64 headroom = 4; bool blocked = 5; }
message Receipt   { string idem_key = 1; bool applied = 2; bool deferred = 3; int64 delta = 4;
                    int64 balance = 5; int64 seq = 6; string rate_card_version = 7; }

message AdmitRequest    { Scope scope = 1; string resource = 2; map<string,string> subjects = 3;
                          repeated Limit limits = 4;   // can only tighten
                          int64 max_cost = 5; }        // 0 = unknown; see AdmitRequest.MaxCost
message RecordRequest   { Event event = 1; }
message GrantRequest    { Scope scope = 1; string pool = 2; int64 amount = 3; string idem_key = 4;
                          string reason = 5; google.protobuf.Timestamp expires_at = 6;
                          map<string,string> ref = 7; }
message BalanceRequest  { Scope scope = 1; }
message BalanceReply    { map<string,int64> pools = 1; int64 total = 2; }
message TransferRequest { Scope from = 1; Scope to = 2; string from_pool = 3; string to_pool = 4;
                          int64 amount = 5; string idem_key = 6; string reason = 7;
                          int64 max_dest_balance = 8; map<string,string> ref = 9; }
message TransferReply   { string idem_key = 1; bool applied = 2; int64 amount = 3;
                          int64 from_balance = 4; string status = 5; }  // "in_transit" | "settled"

service Metering {
  rpc Admit      (AdmitRequest)    returns (Admission);     // gate, no reservation — keep co-located
  rpc Record     (RecordRequest)   returns (Receipt);       // price + settle; idempotent on event.idem_key
  rpc GetBalance (BalanceRequest)  returns (BalanceReply);
  rpc Grant      (GrantRequest)    returns (Receipt);       // PRIVILEGED: mint/remove credits
  rpc Transfer   (TransferRequest) returns (TransferReply); // PRIVILEGED: pool → allocation
}
```

Contract rules baked into the surface:

- **`Record` prices *inside* the service.** The whole `Meter` moves server-side, so the client stub is just a *remote `Meter`* — pricing has one home and rate cards never ship to callers. Each realm's active card names its pricer in the `PricerRegistry`.
- **No streaming RPC for partial billing.** A cancelled stream settles with ONE terminal `Record` carrying the quantities actually produced (invariant 4).
- **`Grant` and `Transfer` are privileged**, separated from `Record` at the RPC level (mTLS + authz), so a compromised spend producer can debit-with-idempotency but never mint or move credits.
- **Deadlines + fail policy are the caller's** for its own call; the server's `AdmitFailPolicy` covers only its own hot-store outage. The service is stateless per-RPC.
- **The realm is authenticated, never asserted.** A caller's credential binds it to a realm set; a `Scope.realm` outside it is `PERMISSION_DENIED`.

The generated client is wrapped so it satisfies the `Meter` interface — business code depends on `metering.Meter`, not on gRPC:

```go
// adapters/driving/grpcclient — a remote Meter. Callers can't tell it from the in-process one.
type Client struct{ c meteringv1.MeteringClient }

func New(conn *grpc.ClientConn) *Client { return &Client{meteringv1.NewMeteringClient(conn)} }

func (m *Client) Admit(ctx context.Context, s metering.Scope, req metering.AdmitRequest) (metering.Admission, error) { /* map → RPC → map */ }
func (m *Client) Record(ctx context.Context, e metering.Event) (metering.Receipt, error)                            { /* map → RPC → map */ }
func (m *Client) Grant(ctx context.Context, g metering.Grant) (metering.Receipt, error)                             { /* map → RPC → map */ }
func (m *Client) Transfer(ctx context.Context, t metering.Transfer) (metering.TransferReceipt, error)               { /* map → RPC → map */ }

var _ metering.Meter = (*Client)(nil) // ← the swap is invisible to business code
```

---

## Code organization

**Ports & adapters (hexagonal).** The layout is what keeps the engine host-independent — and this repo *is* the extraction, so the litmus test that justified the layout now runs in the other direction:

> **Can a host vendor `metering/` into its own tree, `go build ./...`, and have it compile with zero edits?**
> It compiles iff nothing under `metering/` imports a host, its schema and config travel with it, and the only product-specific things are *injected* (a card or a `Pricer`, and a `Scope` constructor). CI asserts this on every push.

```text
github.com/truongpx396/intel-payment
  go.mod
  metering/                        # THE ENGINE — self-contained, zero inbound host deps
    domain/                        #   pure types + invariants — imports NO infra, NO host
      realm.go  scope.go  subjects.go  pool.go  credits.go  money.go  event.go  limit.go
      receipt.go  ratecard.go  policy.go  errors.go
    ports/                         #   the interfaces = the hexagon's edges
      driving.go                   #     Meter, LedgerWriter      (how the world calls metering)
      driven.go                    #     Pricer, BalanceStore, BookStore, LimitStore, PoolStore,
                                   #     RateCardStore, QuotaSource, JournalStore, UsageArchive,
                                   #     Bus, Clock, IDs, Metrics
    app/                           #   use-cases: compose the ports; own the invariants
      meter.go  admit.go  record.go  grant.go  transfer.go   # implement ports.Meter
      writer.go  reconcile.go  rehydrate.go  recover.go  audit.go  expiry.go  journal.go
    adapters/
      driven/                      #   swappable INFRA impls of the driven ports
        redis/         hot functions (reference/hot_path.lua) + key layout + rehydrate/heal
        postgres/      books, watermarks, suspense, guards, limits, pools, cards, journal
        redisstreams/  the DEFAULT Bus — the outbox stream is written by the hot functions
        natsjetstream/ the optional Bus (see bus-subjects.md for what it does and does not add)
        pricing/       table/ (the default) · llmtoken/ seat/ storagebyte/ (reference pricers)
        archive/       UsageArchive: postgres (usage_events) — a columnar adapter is the scale-out path
        otel/          Metrics
      driving/                     #   TRANSPORTS that expose app over a boundary
        inprocess/  returns ports.Meter directly              (library mode)
        grpcserver/ meteringv1 server over app                (service mode)
        grpcclient/ meteringv1 client, satisfies ports.Meter  (service mode caller)
        resthandler/ REST + admin + webhook ingress
    contracts/                     #   the conformance suites (PricerContract, LedgerContract, …): an
                                   #   ordinary package so an adapter's tests can import it and hand
                                   #   it a constructor; it imports no adapter
    config.go                      #   metering.Config — no os.Getenv in the core

  billing/                         # THE FIAT BOUNDARY — see payment-provider-ports.md
    domain/  ports/  app/
    adapters/driven/{stripe,polar,paypal,postgres}/
    config.go

  entitlement/                     # PLAN FEATURES beyond credits — see entitlement-ports.md
                                   #   (also provides metering's QuotaSource, wired in cmd/)
  events/                          # OUTBOUND EVENTS to hosts — see outbound-events.md

  api/meteringv1/                  # generated protobuf (the wire contract)
  api/openapi/v1.yaml              # the REST contract, machine-readable; SDKs are generated from both
  cmd/
    paymentd/                      # gRPC + REST server (service mode)
    payment-worker/                # SOLE durable writer + every tick
    payment-migrate/               # migration runner
  migrations/                      # OWNS its schema — travels with the module
  deploy/                          # Dockerfile · docker-compose.yml
```

**Dependency rule (one direction only):** `adapters → app → ports → domain`. `domain` and `ports` import nothing outside the module; `app` imports only `ports`+`domain`; `adapters` may import infra SDKs (redis/pgx/nats/grpc) but **never** the product. The product-specific bindings — a card or a `Pricer`, and how a `Scope` is built from a request — are supplied by the **host at wire time**, in `cmd/`:

```go
// cmd/paymentd/main.go — or the HOST's own main.go in library mode. Identical either way.
meter, _ := app.New(cfg, app.Deps{
    Pricers:   pricing.Registry{"table": table.Pricer{}, "llmtoken": llmtoken.Pricer{}}, // + your own
    Balance:   redisstore.New(creditRedis),              // hot functions, cluster-safe layout
    Books:     pgstore.New(db),                          // the writer's stores
    Limits:    pgstore.NewLimitStore(db),                // limits are CONFIG DATA, per realm
    Pools:     pgstore.NewPoolStore(db),
    RateCards: pgstore.NewRateCardStore(db),             // cards are CONFIG DATA, versioned
    Journal:   pgstore.NewJournal(db),                   // Settlement=journal + the outage fallback
    Quotas:    entitlementadapter.QuotaSource(entitler), // optional: plan-sized limits
    Bus:       redisstreams.New(creditRedis),            // the default bus
    Metrics:   otelmetrics.New(mp),
})
// business code depends on ports.Meter — an in-process value here, grpcclient.New(conn) there.
```

Enforcement + hygiene that keep the boundary honest over time:

- **`depguard`/`go-arch-lint`** rule: `metering/**` may not import `billing/**`, `entitlement/**`, `events/**`, `cmd/**`, or any host package. CI fails the moment someone reaches across. The entitlement module provides metering's `QuotaSource`; metering never imports it.
- **Own the schema.** Tables live in `migrations/`, keyed on `(realm, scope_kind, scope_id)` — the opaque scope triple — never on a host's tenant column. See [../data-model.md](../data-model.md).
- **Own the config.** A `metering.Config` struct passed in — the core never reads global app config or env directly.
- **Abstract the bus.** `app` depends on a `Bus` *port*. The **default adapter is Redis Streams**, whose outbox stream is written by the hot functions themselves, in the same slot as the account. **NATS JetStream** is an alternative for downstream subjects and cross-region mirroring; for money intents it sits *behind* the Redis outbox via a relay, so it adds durability downstream, not on the hot path. See [bus-subjects.md](./bus-subjects.md).
- **One module, clean internal graph.** `metering/` is import-clean with respect to `billing/`, `entitlement/` and `events/`, so a host that wants only metering can vendor that subtree alone.

Net: `metering/` is a cohesive, self-contained unit whose *only* seams to the outside world are the card or injected `Pricer`, the injected `Scope`, and the driven infra adapters — exactly the things a different host would provide anyway.

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
  events:            { in: events/** }
  api:               { in: api/** }
  cmd:               { in: cmd/** }

deps:
  metering-domain:   { mayDependOn: [] }                                   # pure — nothing
  metering-ports:    { mayDependOn: [ metering-domain ] }
  metering-app:      { mayDependOn: [ metering-domain, metering-ports ] }
  metering-driven:   { mayDependOn: [ metering-domain, metering-ports ] }  # implements ports; NOT app
  metering-driving:  { mayDependOn: [ metering-domain, metering-ports, api ] }
  billing:           { mayDependOn: [ metering-domain, metering-ports ] }  # emits Grants THROUGH the port
  entitlement:       { mayDependOn: [ metering-domain, metering-ports ] }  # also provides metering's QuotaSource
  events:            { mayDependOn: [ metering-domain ] }                  # outbound delivery to hosts
  cmd:               { mayDependOn: [ metering-domain, metering-ports, metering-app,
                                      metering-driven, metering-driving,
                                      billing, entitlement, events, api ] }
```

Four load-bearing rows. `metering-domain` depends on nothing, so it is portable by construction. `billing` and `entitlement` may depend on metering's **ports** but never its `app` or adapters — so the fiat layer mints credits by calling `Grant` through the interface, exactly as any other host would, and cannot reach around it. `metering-*` may never depend on `billing`/`entitlement`/`events`, so a host that wants metering alone can take that subtree. And `cmd` is the only place allowed to wire concrete impls together.

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
          - { pkg: "github.com/jackc/pgx",       desc: "no infra in the core; use the BookStore driven port" }
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
          - { pkg: "github.com/truongpx396/intel-payment/events",      desc: "metering/ must stand alone" }
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
	SettlementOutbox  SettlementDurability = "outbox"  // one Redis Function, no durable wait (default)
	SettlementJournal SettlementDurability = "journal" // Postgres first, then the hot function: no under-bill
)

// HotAckWait narrows outbox mode's loss window by waiting for the hot write to be durable
// before acknowledging (hot-path-consistency.md §5). Ignored under SettlementJournal.
type HotAckWait string

const (
	HotAckNone       HotAckWait = "none"        // default
	HotAckAOFLocal   HotAckWait = "aof_local"   // WAITAOF 1 0: survives a crash
	HotAckAOFReplica HotAckWait = "aof_replica" // WAITAOF 0 1: survives losing the primary
)

// LedgerGranularity decides how usage is booked. `event` writes one ledger row per event and pool;
// `rollup` writes one row per (scope, pool, resource, rate key, card version) per drain batch and
// archives per-event detail through the UsageArchive port — the path past a single Postgres's
// per-event write budget (plan.md § Scale envelope).
type LedgerGranularity string

const (
	GranularityEvent  LedgerGranularity = "event"  // default
	GranularityRollup LedgerGranularity = "rollup"
)

// NegativeBalancePolicy decides what happens when a refund/chargeback exceeds the credits still
// held in a pool — not an edge case but the normal outcome of refunding consumed usage.
type NegativeBalancePolicy string

const (
	AllowDebt    NegativeBalancePolicy = "allow_debt"     // record the negative; keep serving; the host collects
	ClampToZero  NegativeBalancePolicy = "clamp_to_zero"  // floor it AND book a `writeoff` row (default)
	BlockAndFlag NegativeBalancePolicy = "block_and_flag" // floor it, book the writeoff, refuse spend until cleared
)

// CreditExpiry decides whether granted credits have a lifetime.
type CreditExpiry string

const (
	ExpiryOff      CreditExpiry = "off"       // default — a grant never expires
	ExpiryLotsFIFO CreditExpiry = "lots_fifo" // credit_lots per pool; oldest lot consumed and expired first
)

// AdmitFailPolicy is what Admit does when the hot store is unreachable or the shard is frozen.
// Record never drops usage either way: it defers to the journal (invariant 16).
type AdmitFailPolicy string

const (
	FailClosed AdmitFailPolicy = "fail_closed" // refuse (default)
	FailOpen   AdmitFailPolicy = "fail_open"   // serve; usage is journaled and metered on recovery
)

// Config is the ENTIRE configuration surface of the metering module.
type Config struct {
	Realm Realm // the host product this configuration serves; default DefaultRealm

	// Stores — driven adapters dial these; the core never does.
	BalanceRedisURL string // hot accounts + outbox streams + guards (noeviction + AOF; cluster or single)
	LedgerDSN       string // durable Postgres

	SubjectPrefix string // default "billing" → billing.balance.low.<tag> …

	// Sharding: the Redis Cluster placement unit. Recorded in hot_config; a process whose count
	// disagrees refuses to start. Changed only by a reshard (hot-path-consistency.md §8).
	Shards int // default 256

	// Settlement.
	Settlement        SettlementDurability // default SettlementOutbox
	HotAckWait        HotAckWait           // default HotAckNone
	LedgerGranularity LedgerGranularity    // default GranularityEvent
	DrainBatch        int                  // intents booked per transaction; default 500

	// Idempotency (hot-path-consistency.md §6).
	HotIdemTTL       time.Duration // hot guard lifetime; default 24h; MUST be <= UsageIdemWindow
	UsageIdemWindow  time.Duration // durable usage dedup window; default 72h; > every producer's retry horizon
	IdemSafetyMargin time.Duration // Record refuses OccurredAt older than window − margin; default 1h
	MaxClockSkew     time.Duration // Record refuses OccurredAt this far in the future; default 5m

	// Reconcile and recovery.
	ReconcileInterval          time.Duration // incremental run; default 5m
	ReconcileFullSweepInterval time.Duration // every scope; default 24h
	ReconcileTolerance         Credits       // |drift| at equal seq above this PAGES; default 0
	GapTimeout                 time.Duration // a sequence gap older than this pages; default 2m
	TransferTransitAlarm       time.Duration // a transfer in transit longer than this pages; default 5m

	// Hot-path safety.
	AdmitTimeout       time.Duration   // default 250ms
	RecordTimeout      time.Duration   // default 500ms
	AdmitFail          AdmitFailPolicy // default FailClosed
	RequireMaxCost     bool            // reject an Admit with no MaxCost once every caller supplies one
	MaxOperationAmount Credits         // largest single operation; default 2^50 (hot-path-consistency.md §2)
	DefaultWarnAt      float64         // default 0.8

	// Money policies.
	NegativeBalance NegativeBalancePolicy // default ClampToZero
	CreditExpiry    CreditExpiry          // default ExpiryOff

	// The writer's stream (hot-path-consistency.md §2–3).
	BusMaxLen   int64         // outbox length at which the hot functions refuse with BACKPRESSURE; default 1,000,000
	AckWait     time.Duration // an un-acked entry idle this long is reclaimed; default 30s
	MaxAttempts int           // deliveries before an intent is parked; default 5
}

func (c *Config) withDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.Realm == "" {
		c.Realm = DefaultRealm
	}
	if c.SubjectPrefix == "" {
		c.SubjectPrefix = "billing"
	}
	if c.Shards == 0 {
		c.Shards = 256
	}
	if c.Settlement == "" {
		c.Settlement = SettlementOutbox
	}
	if c.HotAckWait == "" {
		c.HotAckWait = HotAckNone
	}
	if c.LedgerGranularity == "" {
		c.LedgerGranularity = GranularityEvent
	}
	if c.DrainBatch == 0 {
		c.DrainBatch = 500
	}
	def(&c.HotIdemTTL, 24*time.Hour)
	def(&c.UsageIdemWindow, 72*time.Hour)
	def(&c.IdemSafetyMargin, time.Hour)
	def(&c.MaxClockSkew, 5*time.Minute)
	def(&c.ReconcileInterval, 5*time.Minute)
	def(&c.ReconcileFullSweepInterval, 24*time.Hour)
	def(&c.GapTimeout, 2*time.Minute)
	def(&c.TransferTransitAlarm, 5*time.Minute)
	def(&c.AdmitTimeout, 250*time.Millisecond)
	def(&c.RecordTimeout, 500*time.Millisecond)
	if c.AdmitFail == "" {
		c.AdmitFail = FailClosed
	}
	if c.MaxOperationAmount == 0 {
		c.MaxOperationAmount = 1 << 50
	}
	if c.DefaultWarnAt == 0 {
		c.DefaultWarnAt = 0.8
	}
	if c.NegativeBalance == "" {
		c.NegativeBalance = ClampToZero
	}
	if c.CreditExpiry == "" {
		c.CreditExpiry = ExpiryOff
	}
	def(&c.AckWait, 30*time.Second)
	if c.BusMaxLen == 0 {
		c.BusMaxLen = 1_000_000
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
}

// Validate checks the EFFECTIVE config (defaults applied to a copy; caller not mutated).
func (c Config) Validate() error {
	c.withDefaults()
	var errs []error
	oneOf := func(name, v string, allowed ...string) {
		for _, a := range allowed {
			if v == a {
				return
			}
		}
		errs = append(errs, fmt.Errorf("%s must be one of %v, got %q", name, allowed, v))
	}
	if c.BalanceRedisURL == "" {
		errs = append(errs, errors.New("BalanceRedisURL is required"))
	}
	if c.LedgerDSN == "" {
		errs = append(errs, errors.New("LedgerDSN is required"))
	}
	if c.Shards < 1 || c.Shards > 16384 {
		errs = append(errs, fmt.Errorf("Shards must be in [1,16384], got %d", c.Shards))
	}
	oneOf("Settlement", string(c.Settlement), "outbox", "journal")
	oneOf("HotAckWait", string(c.HotAckWait), "none", "aof_local", "aof_replica")
	oneOf("LedgerGranularity", string(c.LedgerGranularity), "event", "rollup")
	oneOf("AdmitFail", string(c.AdmitFail), "fail_closed", "fail_open")
	oneOf("NegativeBalance", string(c.NegativeBalance), "allow_debt", "clamp_to_zero", "block_and_flag")
	oneOf("CreditExpiry", string(c.CreditExpiry), "off", "lots_fifo")
	if c.HotIdemTTL > c.UsageIdemWindow {
		errs = append(errs, errors.New("HotIdemTTL must not exceed UsageIdemWindow: a hot guard may never outlive the durable one"))
	}
	if c.IdemSafetyMargin >= c.UsageIdemWindow {
		errs = append(errs, errors.New("IdemSafetyMargin must be smaller than UsageIdemWindow"))
	}
	if c.ReconcileTolerance < 0 {
		errs = append(errs, errors.New("ReconcileTolerance must be >= 0"))
	}
	if c.MaxOperationAmount <= 0 || c.MaxOperationAmount > 1<<52 {
		errs = append(errs, errors.New("MaxOperationAmount must be in (0, 2^52]: hot-tier arithmetic is exact only within ±2^53"))
	}
	if c.DefaultWarnAt < 0 || c.DefaultWarnAt > 1 {
		errs = append(errs, fmt.Errorf("DefaultWarnAt must be in [0,1], got %v", c.DefaultWarnAt))
	}
	return errors.Join(errs...)
}
```

### `Deps` + `New` — product specifics injected, nothing reached into

```go
package app // metering/app

// Deps are the driven ports the core needs. The host supplies them in cmd/. The only
// product-specific ones are the Pricers it registers (and the cards it publishes).
type Deps struct {
	Pricers   ports.PricerRegistry // MUST contain "table"; add compiled pricers by name
	Balance   ports.BalanceStore   // redis adapter: the hot functions
	Books     ports.BookStore      // postgres: ledger, watermarks, suspense, guards (the writer's)
	Limits    ports.LimitStore     // per-realm Limit rows — CONFIG DATA
	Pools     ports.PoolStore      // per-realm pools — CONFIG DATA
	RateCards ports.RateCardStore  // versioned, immutable cards — CONFIG DATA
	Journal   ports.JournalStore   // Settlement=journal and the outage fallback
	Bus       ports.Bus            // redisstreams by default
	Quotas    ports.QuotaSource    // optional: sizes limits that name a max_entitlement
	Archive   ports.UsageArchive   // required when LedgerGranularity=rollup
	Clock     ports.Clock          // REQUIRED: the core reads no wall clock (a core that does is not replayable);
	                                //   `system.Clock{}` from metering/adapters/driven/system in the reference wiring
	IDs       ports.IDSource       // uuid v7 (`&system.IDs{}`); required by the writer, unused by the Meter
	Metrics   ports.Metrics        // default: no-op. Invariant 15 needs a real one in production
}

// New validates config, assembles the core, and returns it as the ports.Meter interface.
func New(cfg metering.Config, d Deps) (ports.Meter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metering config: %w", err)
	}
	if d.Clock == nil {
		return nil, errors.New("metering: a Clock is required — the core reads no wall clock")
	}
	if d.Balance == nil || d.Books == nil || d.Bus == nil || d.Journal == nil {
		return nil, errors.New("metering: Balance, Books, Bus and Journal are required")
	}
	if d.Pricers["table"] == nil {
		return nil, errors.New("metering: the `table` pricer must be registered")
	}
	if d.Limits == nil || d.RateCards == nil || d.Pools == nil {
		return nil, errors.New("metering: Limits, Pools and RateCards are required — they are config data, not code")
	}
	if cfg.LedgerGranularity == metering.GranularityRollup && d.Archive == nil {
		return nil, errors.New("metering: rollup granularity needs a UsageArchive, or per-event detail is lost")
	}
	// … construct the Meter over the driven ports …
	return &meter{cfg: cfg, deps: d}, nil
}
```

So the wiring is: build a `Config`, build the driven adapters, register pricers, call `app.New` → get a `ports.Meter`. Swapping to service mode replaces that one `app.New(...)` with `grpcclient.New(conn)` — both return `ports.Meter`, and the two linters guarantee nothing downstream ever depended on more than that interface.

---

## Observability contract

Invariant 15 requires that every terminal state be observable, which means the metric names are part of the contract, not an implementation detail — a host writes alerts against them before it ever reads the code. The `metering_*` rows below are `ports.Catalog`, and a test holds the two equal; `adapters/driven/otel` registers each with its declared kind, unit and description on the MeterProvider the host hands it, and creates the `billing_*` and `events_*` ones on first use.

| Metric | Type | Why it pages |
|---|---|---|
| `metering_admit_latency_seconds` | histogram (`realm`, `outcome`) | `Admit` is on every request's critical path; a p99 above ~5 ms means metering became the bottleneck it exists to avoid |
| `metering_admit_denied_total` | counter (`realm`, `limit`, `deny_code`) | a spike is either an abuse event or a limit misconfigured to zero |
| `metering_admit_uncapped_total` | counter (`realm`) | callers omitting `MaxCost` — the overshoot bound is not enforced for them (invariant 3) |
| `metering_admit_failopen_total` | counter (`realm`) | work served while the hot tier was unavailable. Never non-zero on a `fail_closed` deployment |
| `metering_record_deferred_total` | counter (`realm`) | usage journaled during an outage; `metering_journal_pending` is how much is still waiting |
| `metering_journal_pending` | gauge (`realm`) | deferred usage not yet applied to the hot tier. Rising after recovery means the replay is stuck |
| `metering_drift_findings_total` | counter (`realm`, `outcome`) | `drift` or `regression` at **equal** sequence numbers — a defect by construction (invariant 9) |
| `metering_drift_abs_credits` | gauge (`shard`) | Σ \|drift\| of the last run, never netted |
| `metering_reconcile_deferred_total` | counter (`shard`) | scopes with intents in flight at reconcile time: lag, not drift — see the outbox age |
| `metering_outbox_depth` | gauge (`shard`) | a rising depth means the writer is behind; every undrained intent is inside the RPO window |
| `metering_outbox_age_seconds` | gauge (`shard`) | depth alone hides a single stuck entry. Age catches a poisoned head-of-line |
| `metering_writer_seq_gap_seconds` | gauge (`shard`) | a sequence gap older than `GapTimeout`: an intent is missing from the stream |
| `metering_hot_regression_total` | counter (`shard`) | the hot tier lost history (restart, failover) — the shard froze and rebuilt from the books |
| `metering_shard_frozen_seconds` | gauge (`shard`) | how long a shard has been frozen. Should be seconds |
| `metering_late_replay_total` | counter (`realm`) | a replay after its hot guard expired, corrected by suspense. Rising means `HotIdemTTL` is shorter than producers' retries |
| `metering_idem_conflict_total` | counter (`realm`, `op`) | a key reused for a different request — a producer bug that would otherwise drop or double charges |
| `metering_suspense_open` | gauge (`realm`, `reason`) | open suspense entries. Anything older than a minute is a correction that is not landing |
| `metering_transfer_in_transit_age_seconds` | gauge (`realm`) | the oldest transfer in transit; beyond `TransferTransitAlarm` it pages |
| `metering_rehydrate_total` | counter (`realm`, `cause`) | `new_scope` is normal; `stale` follows a generation bump |
| `metering_price_error_total` | counter (`realm`, `rate_key`, `reason`) | fail-closed pricing refusing real traffic — usually a card missing a newly launched SKU |
| `metering_stale_event_total` | counter (`realm`) | events refused as older than the dedup window — a producer retrying far too late |
| `metering_default_partition_rows` | gauge (`table`) | rows in a DEFAULT partition: the partition tick is behind, and must be fixed before a partition can be created for that range |
| `metering_effect_failed_total` | counter (`kind`) | a follow-up to a booking (a transfer's second leg, a correction, a balance-low notification) failed after the commit. A sweep finishes it; a rising count means the hot tier is refusing the writer |
| `metering_correction_orphan_total` | counter (`realm`) | a correction was booked that closed no suspense entry: it was already closed, or never existed |
| `metering_correction_stale_total` | counter (`realm`) | an open suspense entry older than `HotIdemTTL`: re-issuing its correction could apply twice, so it is left for a human |
| `metering_recover_parked_total` | counter (`shard`) | intents a recovery parked because they sat behind a sequence gap that can no longer fill — each is a movement to investigate |
| `billing_webhook_verify_failed_total` | counter (`provider`) | a signature failure is a **security** event (see [payment-provider-ports.md](./payment-provider-ports.md)) |
| `billing_webhook_inbox_age_seconds` | gauge | the oldest unprocessed verified webhook — a payment someone made that has not been fulfilled yet |
| `billing_grant_applied_total` | counter (`realm`, `reason`) | reconciles against the provider's own payout report |
| `events_delivery_failed_total` | counter (`realm`, `endpoint`) | an outbound webhook to a host that is failing — see [outbound-events.md](./outbound-events.md) |

Health endpoints: `/healthz` (process up) and `/readyz` (hot store reachable, durable store reachable, migrations at head, `hot_config.shards` equal to the configured count). `/readyz` MUST fail when migrations are behind — a service that serves money against a half-migrated schema is worse than one that is down.

---

## Adoption checklist (per host product)

Copy-paste and tick once per realm. [docs/integration-guide.md](../../../docs/integration-guide.md) walks each line.

- [ ] **Scope defined** — pick the billing subject (`workspace`/`org`/`user`/`account`); construct `Scope` from request context. No kernel signature changes.
- [ ] **Subjects supplied** — every call carries the subjects its limits need (`user`, `api_key`, `job`…); a missing one is refused, not ignored.
- [ ] **Pricing published** — a versioned card of rationals for the `table` pricer, or a registered `Pricer` for anything non-linear. No I/O, no clock, no `Attributes` in cost math.
- [ ] **Accounting unit chosen** — fine (1 credit = 1 µ$ of list price is the reference); display scale set in `UnitLabels`, not in the books.
- [ ] **Units enumerated** — the host's metered dimensions as `Unit` constants; `Event`s carry integer `Quantity`s only.
- [ ] **Limits configured** — rows with `Subject`/`Window`/`WarnAt`/`DenyCode`; plan-dependent sizes via `max_entitlement`; each `DenyCode` mapped to the host's wire status.
- [ ] **Pools decided** — `general` only, or promo / resource-restricted pools with priorities.
- [ ] **Settlement durability chosen** — `outbox` (+ `HotAckWait`) vs `journal`; the RPO statement from invariant 7 copied into the host's runbook.
- [ ] **Sole durable writer wired** — the worker owns the shards; no request tier writes the books; ticks come from an external scheduler.
- [ ] **Idempotency windows set** — `UsageIdemWindow` longer than any producer's retry horizon; every producer sends a stable `IdemKey` **and** `OccurredAt`.
- [ ] **Reconcile alarm wired** — drift and regression findings page an on-call, not just write a row.
- [ ] **Realm chosen** — a stable slug for this product; every `Scope` it constructs carries it. One realm per product, never per tenant.
- [ ] **`MaxCost` supplied at the gate** — every caller passes an upper bound. Turn on `RequireMaxCost` once they all do.
- [ ] **Multi-step work capped** — any job that spends across several steps carries a `Job` limit and a `job` subject.
- [ ] **Allocation model decided (if a pool is shared)** — `Transfer` for parent-buys/child-draws, with `MaxDestBalance` set.
- [ ] **Negative-balance policy chosen** — matching how the host actually handles a refund past spend.
- [ ] **Credit expiry decided** — `off` or `lots_fifo`; if on, the expiry tick is scheduled.
- [ ] **Grant path (if monetized)** — the [`PaymentProvider`](./payment-provider-ports.md) adapters, or `Meter.Grant` from the host's own top-up flow.
- [ ] **Entitlements mapped (if the plan sells more than credits)** — via [`entitlement-ports.md`](./entitlement-ports.md), not plan-code branches.
- [ ] **Events consumed** — the host subscribes to signed webhooks or polls the event feed ([outbound-events.md](./outbound-events.md)); it never connects to this service's Redis.
- [ ] **Partial settlement honored** — any cancellable unit of work reports real produced `Quantities` to `Record`.
- [ ] **Money is integer** — `Credits`/`Money`; no float touches a balance, a price, or a ledger row.

Operational readiness (before the first real charge):

- [ ] **Alerts wired** — drift findings, regressions, outbox age, sequence gaps, frozen shards, fail-open, idempotency conflicts, transfer transit age, webhook inbox age and verification failures all page a human. See [docs/operations.md](../../../docs/operations.md).
- [ ] **`/readyz` gating deploys** — it fails on a behind-head schema or a shard-count mismatch.
- [ ] **Redis is `noeviction` + AOF** and unreachable from hosts — a cache eviction policy on the balance store turns a cache miss into a billing incident, and host access to it is mint access.
- [ ] **Caller↔service auth chosen** — mTLS or bearer, with `Grant`/`Transfer`/admin restricted. `PAYMENT_SERVICE_AUTH=none` is permitted **only** on loopback/UDS.
- [ ] **Realm binding enforced** — each caller credential names its realms; a cross-realm scope is rejected at the transport (invariant 11).
- [ ] **Recovery rehearsed** — a shard freeze → drain → bump → unfreeze run against real traffic, and a restore of the books with lazy rehydrate, both timed against the host's RTO.

---

## Non-goals (stays in the host, by design)

- **Prices** — rate cards are the host's data and a custom `Pricer` is the host's code. The kernel ships only the generic `table` arithmetic; it does not know what a token or a gigabyte costs.
- **What produces spend** — call sites (LLM gateway client, ingestion, connectors) build `Event`s. The kernel does not instrument callers.
- **Fiat, tax, invoicing** — the payments boundary ([`payment-provider-ports.md`](./payment-provider-ports.md)) and the provider/merchant-of-record. The metering core is fiat-agnostic; it only ever sees `Credits`. Tax is explicitly the provider's or an external hook's job (`TaxStrategy`), never computed here.
- **Feature entitlements** — what a plan unlocks *besides* credits is [`entitlement-ports.md`](./entitlement-ports.md). The metering core counts; it does not gate features.
- **Authentication of end users** — the host authenticates its people and hands the engine a `Scope`. The engine authenticates *callers* (services), never end users.
- **Tenancy semantics** — what a `Scope` *means*, and its RLS predicate, are the host's. The kernel treats it as an opaque identity.
