# Feature Specification: Metering, Credits & Payments Core

**Feature Branch**: `001-metering-billing-core` | **Status**: Design complete, implementation pending
**Input**: Extracted from [aisat-intel](https://github.com/truongpx396/aisat-intel) — see [PROVENANCE.md](../../PROVENANCE.md)

## Overview

`intel-payment` is a **self-contained metering, credits and payments service**. A host product
delegates four things to it and keeps everything else:

1. **Metering** — count what was used, price it deterministically, debit a balance in sub-millisecond time.
2. **Enforcement** — refuse work *before* it is performed when a ceiling is reached, with a reason the user can act on.
3. **Payments** — convert fiat to credits through a provider, with fulfilment driven by verified webhooks.
4. **Entitlements** — answer what a plan permits beyond credits.

It runs **embedded as a Go library** or as a **standalone container service** over gRPC + REST.
The choice is a wiring decision in the host's `cmd/`, reversible, and invisible to business code.

## What makes it reusable rather than merely extracted

Three parameters, and nothing else, are product-specific:

| Parameter | Supplied by | Example |
|---|---|---|
| `Pricer` | the host, as an injected port | tokens · GB-days · seats · API calls |
| `Scope` | the host, per request | `{workspace, w_123}` · `{organization, o_9}` · `{api_key, k_7}` |
| `Realm` | deployment config | which host product this is, inside a shared deployment |

Everything else — the ledger, the outbox, reconciliation, idempotency, admission, payments,
webhooks, entitlements — is generic, and the conformance suites run unchanged across pricers
over disjoint unit spaces to keep that claim honest.

## User Scenarios & Testing

### User Story 1 — A host meters and enforces spend (Priority: P1)

A host product's service checks admission before expensive work, performs the work, and reports
what was actually produced. The balance decrements in real time; an exhausted ceiling refuses
the next attempt with a reason and a remedy rather than a silent failure or a generic error.

**Independent test**: drive `Admit` → work → `Record` against a seeded balance; confirm the
balance decrements by the documented amount, that a near-limit warning fires at the configured
threshold, that an exhausted balance refuses with `402` + an upgrade path, and that a spent
windowed limit refuses with `409` + a reset time.

**Acceptance**
1. Given a positive balance, when a metered event is recorded, the balance decrements by the priced amount and a ledger row is written exactly once.
2. Given a retried or double-submitted event with the same `idem_key`, when it is processed, the balance changes **once** and the second call reports a replay.
3. Given an exhausted balance, when admission is requested, it is refused with `payment_required` and an upgrade path — never a silent success and never an unclassified `500`.
4. Given a known upper bound on cost, when admission is requested against an insufficient balance, it is refused **before** the work starts, and the response carries the remaining headroom so the caller can resize the work.
5. Given a unit of work cancelled midway, when it settles, it is billed for what it actually produced — not zero, not the full ceiling.
6. Given an unknown `rate_key`, when pricing runs, it **fails closed** with `unpriceable` rather than pricing at zero.

### User Story 2 — A host sells credits (Priority: P1)

A member buys a credit pack or subscribes. They are redirected to a hosted checkout. Credits
appear once the provider confirms payment — never on the redirect.

**Independent test**: complete a provider sandbox purchase; confirm credits arrive only after
the verified webhook, that replaying the webhook grants nothing further, that a refund appends
a negative row without mutating the original, and that a tampered webhook body is rejected.

**Acceptance**
1. Given a completed checkout, when the verified `payment_succeeded` webhook arrives, the plan's allotment is granted exactly once and the payment is recorded.
2. Given the same webhook delivered again — or eight times concurrently — when processed, the grant applies exactly once and every response is `200`.
3. Given a tampered, unsigned, or stale-but-signed body, when received, it is rejected `400` as a **security** event, with no side effect and no partial processing.
4. Given a refund, when processed, a negative ledger row is appended and the original payment and grant rows are unchanged.
5. Given a refund exceeding credits still held, when processed, the configured `NegativeBalancePolicy` applies — and under `clamp_to_zero` a compensating `writeoff` row keeps `balance == SUM(ledger)`.
6. Given a failed renewal, when processed, the subscription enters `past_due` with a grace window and is **not** revoked instantly.
7. Given a webhook naming another scope in its metadata, when processed, the grant lands on the scope resolved from the **verified customer mapping**, never the metadata.

### User Story 3 — A member understands their position (Priority: P2)

A member opens a credits screen: balance, how long it will last, each ceiling with its own
window, spend over time, spend by dimension, and a ledger where a replayed charge is visibly
deduped.

**Independent test**: render the panel from a fixture snapshot; confirm the ceiling count comes
from the data, that no component reads the string `credit`, that parts reconcile to totals, and
that an exhausted limit renders a blocking notice rather than nothing.

**Acceptance**
1. Given a snapshot with N limits, when the panel renders, it shows N meters — no empty cards, no missing ceilings.
2. Given a unit other than credits, when labels are supplied, every figure and header reads in that unit.
3. Given a limit past its cap, when rendered, a blocking notice carries the deny code and a remedy. Rendering nothing is not permitted.
4. Given a return from checkout, when rendered, the state is **processing** until the server reports fulfilment — never "credits added".
5. Given a total shown beside its parts, when rendered, the parts sum to the total or an explicit residual row is shown.

### User Story 4 — A host gates features by plan (Priority: P2)

A host asks whether a scope may use a capability or add a seat, and shows why not.

**Independent test**: resolve entitlements across default / subscription / override; confirm
precedence, that unknown keys deny, that a store outage denies, that a refusal carries a reason
and an upgrade path, and that an upgrade takes effect without waiting out a cache TTL.

**Acceptance**
1. Given a plan without a capability, when checked, it is denied with a human-readable reason and, where one exists, an upgrade path.
2. Given an override, when checked, it wins over the plan in both directions.
3. Given an undeclared key or an unreachable store, when checked, the result is **denied**.
4. Given a just-completed upgrade, when checked, the new capability is available immediately.

### User Story 5 — An operator runs it (Priority: P2)

An operator deploys the container, watches drift and outbox depth, recovers from a hot-store
loss, and can answer where any charge came from.

**Independent test**: kill the hot store mid-traffic; confirm the configured fail policy holds,
that rehydration rebuilds the balance from the ledger before serving resumes, that drift beyond
tolerance pages rather than silently healing, and that `/readyz` fails on a behind-head schema.

**Acceptance**
1. Given a lost hot balance, when serving restarts, the balance is rebuilt from the ledger under a per-scope lock **before** the tier serves.
2. Given drift beyond tolerance, when reconcile runs, it heals *and* raises an alarm — never silently absorbs it.
3. Given a behind-head schema, when `/readyz` is probed, it fails, so a rollout cannot serve money against a half-migrated database.
4. Given any ledger row, when audited, it names its `rate_card_version`, so its price can be re-derived exactly.

### Edge cases

- **Hot-store loss between the atomic Redis step and the drain** — the intent is gone, the ledger never receives the row, and reconcile heals the balance *upward*. This is **under-billing, never double-billing**, it is the accepted RPO of the default `outbox` mode, and the `journal` mode removes it at the cost of one synchronous write.
- **Concurrent admits against a near-empty balance** — bounded overshoot of `concurrency × MaxCost`, healed at settlement and reconcile. Without `MaxCost` the bound is unknown, which is why omissions are counted and can be made a hard error.
- **A provider replaying a webhook after `payment_events` retention** — the `credit_idem (realm, idem_key)` backstop still prevents a double grant. Retention must nonetheless exceed the provider's replay window.
- **A provider whose verification API is down** (PayPal) — deliveries are **parked for replay**, never processed optimistically and never dropped.
- **A missed `subscription.deleted`** — the drift sweep repairs it. Without that sweep, a cancelled customer stays entitled indefinitely.
- **Two host products colliding on an idempotency key** — realm-scoped uniqueness keeps them independent.
- **A cancelled stream / aborted job** — settles on actual production; zero would make "start then abort" free.

## Requirements

### Functional — Metering & enforcement
- **FR-001**: The system MUST price a metered event as a pure, deterministic function of its rate key, its integer quantities and a versioned rate card, failing closed on an unknown key or unit.
- **FR-002**: The system MUST record the rate-card version on every ledger row so a price can be re-derived exactly.
- **FR-003**: The system MUST charge a retried or duplicated operation **at most once**, guarded by a durable `credit_idem (realm, idem_key)` index, and MUST report a replay distinguishably from a first application.
- **FR-004**: The system MUST gate work before it is performed, evaluating every configured ceiling, and MUST refuse with a machine-readable deny code plus a human-readable reason. It MUST NOT pre-debit.
- **FR-005**: The system MUST accept an optional upper bound on a call's cost at the gate and refuse when that bound exceeds available allowance, so overshoot is bounded rather than asserted.
- **FR-006**: The system MUST warn at an operator-configurable near-limit fraction, coalesced per scope/limit/window.
- **FR-007**: The system MUST have exactly one durable writer of the ledger and the durable balance. Every other producer publishes intents.
- **FR-008**: The system MUST write the usage record and enqueue the debit in one atomic step, so a crash cannot log without billing or bill without logging.
- **FR-009**: The system MUST reconcile the hot balance against the ledger periodically, heal drift with an explicit compensating row, and **alarm** when drift exceeds a configured tolerance.
- **FR-010**: The system MUST rebuild a hot balance from the ledger under a per-scope lock before serving resumes after a loss.
- **FR-011**: The system MUST bill a cancelled unit of work for what it actually produced.
- **FR-012**: The system MUST represent all internal amounts as signed 64-bit integers and all fiat as integer minor units plus an ISO-4217 currency. No floating point may touch a balance, price or ledger row.
- **FR-013**: The system MUST treat a scope as opaque and MUST NOT parse, rank or special-case it.
- **FR-014**: The system MUST isolate realms completely — keys, subjects, rows, plans, rate cards, limits and idempotency guards.
- **FR-015**: The system MUST apply an explicit, configured policy when a credit change would drive a balance negative, and MUST record a compensating row when it floors one.
- **FR-016**: The system MUST support an optional credit-expiry mode that consumes and expires grants oldest-first.
- **FR-017**: The system MUST load ceilings and rate cards as per-realm configuration data, so launching a price or a limit requires no redeploy.
- **FR-017a**: The system MUST support a cumulative ceiling scoped to a single unit of work, so a multi-step job's total spend is bounded independently of every calendar ceiling. It MUST be a counter rather than a granted balance, so no ledger rows accrue per job and abandoned work leaks nothing.
- **FR-017b**: The system MUST provide an atomic transfer of credits between two scopes in one realm under a single idempotency key, writing a paired out/in record that sums to zero, refusing rather than overdrawing the source, and optionally capping what the destination may hold. It MUST NOT create or destroy credits — a realm's total is invariant across a transfer.

### Functional — Payments
- **FR-018**: The system MUST verify every provider webhook's signature on the raw body, in constant time, within a timestamp tolerance, **before any parsing or side effect**, and MUST reject failures with `400` as a security event.
- **FR-019**: The system MUST grant credits only on a verified provider event, never on a checkout return or client callback.
- **FR-020**: The system MUST dedup provider events by an atomic insert whose conflict short-circuits processing, and MUST answer `200` to a replay.
- **FR-021**: The system MUST resolve scope, amount, currency and credit quantity from the verified provider object plus its own customer and catalogue records — never from client input or provider metadata.
- **FR-022**: The system MUST create credits only through the metering grant path, never by writing the ledger or a balance directly.
- **FR-023**: The system MUST append refunds and chargebacks as new negative rows and MUST NOT mutate the original payment or grant.
- **FR-024**: The system MUST support interchangeable providers behind one port, with each provider SDK confined to its own adapter.
- **FR-025**: The system MUST hold entitlements through a configurable grace window on a failed payment rather than revoking instantly.
- **FR-026**: The system MUST reconcile local subscription state against the provider periodically, so a missed webhook is repaired.
- **FR-027**: The system MUST retain provider event records longer than the provider's replay window.
- **FR-028**: The system MUST name an explicit tax strategy and MUST NOT compute tax itself.
- **FR-029**: The system MUST apply a configured policy for mid-period plan changes.
- **FR-030**: The system MUST never receive, store or log card data, and MUST keep provider secrets server-side only.

### Functional — Entitlements
- **FR-031**: The system MUST resolve plan capabilities as data with the precedence override > subscription > default, and MUST report the provenance of every effective grant.
- **FR-032**: The system MUST deny an undeclared key and MUST deny on a resolution error — entitlements fail closed with no fail-open option.
- **FR-033**: The system MUST accompany every refusal with a human-readable reason and, where one exists, an upgrade path.
- **FR-034**: The system MUST require a reason and write an audit row for every override.
- **FR-035**: The system MUST invalidate cached entitlements immediately on a subscription change, an override write, or a grace expiry.

### Functional — Surfaces & operations
- **FR-036**: The system MUST expose the whole credits snapshot — balance, every limit, breakdown, series, ledger page, anchor, notices — in one round trip.
- **FR-037**: The system MUST report fulfilment state server-side, so no client infers a grant from a redirect.
- **FR-038**: The system MUST render the credits surface over an opaque unit and an opaque scope, taking the ceiling count, unit labels, ledger columns and dimension members as data.
- **FR-039**: The system MUST scope a member's spend breakdown to their own usage unless they hold an administrative entitlement.
- **FR-040**: The system MUST authenticate callers, bind each credential to a realm set, and separate the credit-minting privilege from the spend-recording privilege.
- **FR-041**: The system MUST export named metrics for admission latency and outcome, drift, outbox depth and age, fail-open events, uncapped admissions, pricing failures and webhook verification failures.
- **FR-042**: The system MUST fail its readiness probe when the schema is behind head.
- **FR-043**: The system MUST park, never drop, an intent that exhausts its retry budget, and MUST alert when it does.
- **FR-044**: The system MUST run both as an embedded library and as a standalone service over the same ports, switchable by wiring alone.
- **FR-045**: The system MUST ship as a container with its migrations, requiring only a Redis URL, a Postgres DSN and a bus URL to start.

### Key entities
- **Realm** — one host product on a deployment; the outermost isolation key.
- **Scope** — the opaque billing subject within a realm.
- **Credits** — the signed internal accounting unit. Negative is consumption, positive is a grant.
- **Money** — a fiat amount in integer minor units plus a currency, used only at the payments boundary.
- **Rate card** — an immutable, versioned price list; a change publishes a new version.
- **Limit** — one ceiling: a unit, a maximum, a window (account · calendar · rolling · **per-job**), a warn fraction and a deny code.
- **Allocation** — credits moved from a pool scope to a drawing scope, recorded as a paired out/in that conserves the realm.
- **Ledger** — the append-only account of record; the authoritative balance.
- **Plan** — a purchasable thing: prices per currency, a credit allotment, entitlements.
- **Payment / Payment event** — a fiat transaction and the verified provider event that caused it.
- **Entitlement** — a capability a plan confers: flag, quota or enum.

## Success Criteria

- **SC-001**: Admission adds no more than **5 ms at p99** to a host request when co-located, and no more than **1 ms** embedded.
- **SC-002**: Credit accounting is exact — no operation is charged twice under retry, duplicate submission, concurrent delivery or webhook replay — and the hot balance reconciles to the ledger within tolerance on every cycle.
- **SC-003**: An exhausted ceiling is always refused with a classified code and an actionable message. Silent failure and unclassified errors are zero.
- **SC-004**: The same conformance suites pass unchanged against pricers over **at least three disjoint unit spaces**, and against every provider adapter.
- **SC-005**: A new host product is onboarded by supplying a `Pricer`, a `Scope` binding and a realm's configuration rows — **no change to any file under `metering/`**.
- **SC-006**: `metering/` builds standalone with no dependency on the fiat or entitlement layers, asserted in CI.
- **SC-007**: Every webhook is verified before any side effect; tampered, unsigned and stale bodies are rejected in 100% of seeded cases.
- **SC-008**: After a hot-store loss, the balance is rebuilt from the ledger before serving resumes, and the measured rebuild time is inside the host's stated RTO.
- **SC-009**: Drift beyond tolerance pages a human in 100% of injected cases and is never silently absorbed.
- **SC-010**: Every ledger row can be re-priced exactly from its recorded event and rate-card version.
- **SC-011**: A capability check denies on an undeclared key and on a store outage in 100% of cases.
- **SC-012**: The credits surface renders correctly for one, three and five ceilings, and for a non-credit unit, with no component edit. A per-job and a per-call ceiling render distinguishably — one as a draining budget, the other as a wall.
- **SC-012a**: A multi-step job halts at its own cap without reaching any calendar ceiling, and a transfer interrupted at any point either applies both sides or neither.
- **SC-013**: A single `docker compose up` yields a service passing `/readyz` with migrations at head.

## Out of scope

- Tax computation, filing, and merchant-of-record duties — provider or specialist service (FR-028).
- Invoice rendering and dunning email templates — the provider's hosted invoices and the host's notification system.
- End-user authentication and authorization — the host's. This system authenticates *callers*.
- Usage counting for quota entitlements — the host owns its own counts.
- Double-entry general-ledger accounting and statutory reporting — this is a credit ledger, not an ERP.
- Fraud scoring and chargeback representment — the provider's tooling.
- Multi-currency FX conversion — prices are set per currency; no rate is applied here.
- Release-engineering feature flags — distinct from commercial entitlements (see entitlement non-goals).
