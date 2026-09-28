# Feature Specification: Metering, Credits & Payments Core

**Feature Branch**: `001-metering-billing-core` | **Status**: Design complete, implementation pending
**Input**: Extracted from [aisat-intel](https://github.com/truongpx396/aisat-intel) — see [PROVENANCE.md](../../PROVENANCE.md)

## Overview

`intel-payment` is a **self-contained metering, credits and payments service**. A host product
delegates four things to it and keeps everything else:

1. **Metering** — count what was used, price it deterministically and exactly, debit a balance in sub-millisecond time.
2. **Enforcement** — refuse work *before* it is performed when a ceiling is reached, with a reason the user can act on.
3. **Payments** — convert fiat to credits through a provider, with fulfilment driven by verified webhooks.
4. **Entitlements** — answer what a plan permits beyond credits, including how large each ceiling is.

It runs **embedded as a Go library** or as a **standalone container service** over gRPC + REST.
The choice is a wiring decision in the host's `cmd/`, reversible, and invisible to business code.

## What makes it reusable rather than merely extracted

Three parameters, and nothing else, are product-specific:

| Parameter | Supplied by | Example |
|---|---|---|
| Pricing | the host, as **data** (a rate card for the built-in `table` pricer) or as an injected `Pricer` | tokens · GB-days · seat-days · API calls |
| `Scope` (+ `Subjects`) | the host, per request | `{organization, o_9}` with `{user: u_1, api_key: k_7}` |
| `Realm` | deployment config | which host product this is, inside a shared deployment |

Everything else — the ledger, the outbox, reconciliation, idempotency, admission, pools, payments,
webhooks, entitlements, events — is generic, and the conformance suites run unchanged across cards
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
1. Given a positive balance, when a metered event is recorded, the balance decrements by the exactly priced amount and a ledger row is written exactly once.
2. Given a retried event with the same `idem_key` and the same content, when it is processed, the balance changes **once** and the second call reports a replay.
3. Given an `idem_key` reused with **different** content, when it is recorded, it is refused as `idempotency_conflict` — never silently treated as a replay.
4. Given an exhausted balance, when admission is requested, it is refused with `payment_required` and an upgrade path — never a silent success and never an unclassified `500`.
5. Given a known upper bound on cost, when admission is requested against an insufficient balance, it is refused **before** the work starts, with the remaining headroom.
6. Given a unit of work cancelled midway, when it settles, it is billed for what it actually produced.
7. Given an unknown `rate_key`, when pricing runs, it **fails closed** with `unpriceable`.
8. Given a per-member daily ceiling, when a call omits the member, it is refused with `missing_subject` rather than skipping the ceiling; and when the member's plan is upgraded, the ceiling rises immediately.
9. Given promotional and paid credits, when usage is recorded, promotional credits are consumed first.
10. Given the hot store is unavailable, when usage is recorded, it is accepted as deferred and applied once the store recovers — never lost.

### User Story 2 — A host sells credits (Priority: P1)

A member buys a credit pack or subscribes. They are redirected to a hosted checkout. Credits
appear once the provider confirms payment — never on the redirect.

**Independent test**: complete a provider sandbox purchase; confirm credits arrive only after
the verified webhook, that replaying the webhook grants nothing further, that a refund claws back
exactly without mutating the original, and that a tampered webhook body is rejected.

**Acceptance**
1. Given a completed checkout, when the verified payment webhook is processed, the plan's allotment is granted exactly once, into the plan's pool, and the payment is recorded.
2. Given the same webhook delivered again — or eight times concurrently — every response is `200` and the grant applies exactly once.
3. Given the service crashes after acknowledging a verified webhook and before granting, when it recovers, the grant is applied — exactly once.
4. Given a tampered, unsigned, or stale-but-signed body, when received, it is rejected `400` as a **security** event, with no side effect.
5. Given partial refunds, when processed, the credits revoked equal `ceil(granted × refunded / paid)` cumulatively, never more than granted, and the original payment and grant rows are unchanged.
6. Given a refund exceeding credits still held, when processed, the configured `NegativeBalancePolicy` applies — and a floor always books a compensating `writeoff` row.
7. Given a dispute, when funds are withdrawn the credits are clawed back, and when funds are reinstated they are granted back.
8. Given a failed renewal, when processed, the subscription enters `past_due` with a grace window and is **not** revoked instantly.
9. Given subscription events delivered out of order, when processed, the newest provider state wins.
10. Given a webhook naming another scope in its metadata, when processed, the grant lands on the scope resolved from the **verified customer mapping**.

### User Story 3 — A member understands their position (Priority: P2)

A member opens a credits screen: balance per pool, how long it will last, each ceiling with its own
window, spend over time, spend by dimension, and a ledger where a replayed charge is visibly deduped.

**Independent test**: render the panel from a fixture snapshot; confirm the ceiling count comes
from the data, that no component reads the string `credit`, that parts reconcile to totals, and
that an exhausted limit renders a blocking notice rather than nothing.

**Acceptance**
1. Given a snapshot with N limits, when the panel renders, it shows N meters.
2. Given a unit other than credits, or a display scale, every figure reads in that unit and scale.
3. Given a limit past its cap, a blocking notice carries the deny code and a remedy.
4. Given a return from checkout, the state is **processing** until the server reports fulfilment.
5. Given a total shown beside its parts, the parts sum to the total or an explicit residual row is shown.

### User Story 4 — A host gates features by plan (Priority: P2)

**Acceptance**
1. Given a plan without a capability, when checked, it is denied with a reason and, where one exists, an upgrade path.
2. Given an override, when checked, it wins over the plan in both directions.
3. Given an undeclared key or an unreachable store, when checked, the result is **denied**.
4. Given a just-completed upgrade, when checked, the new capability is available immediately.

### User Story 5 — An operator runs it (Priority: P2)

An operator deploys the container, watches drift and outbox age, recovers from a hot-store loss or
failover, changes a price or a ceiling, and can answer where any charge came from.

**Independent test**: kill and restart the hot store mid-traffic, and fail it over to a lagging
replica; confirm the shard freezes, drains, rebuilds from the books and resumes with no double
charge, that deferred usage is applied, and that reconcile under steady traffic reports in-flight
intents as deferred rather than drift.

**Acceptance**
1. Given a lost or rolled-back hot store, when detected, the affected shards freeze, drain, and rebuild from the books before serving resumes; nothing booked is charged twice.
2. Given steady traffic, when reconcile runs, intents in flight are reported as deferred, and only a disagreement at the **same** sequence number is drift — which heals the hot side and pages.
3. Given a behind-head schema, `/readyz` fails.
4. Given any ledger row, when audited, it names its `rate_card_version` and can be re-priced exactly.
5. Given a price or ceiling change through the admin API or directly in SQL, it takes effect without a deploy, is audited with before/after, and reaches every cache immediately.

### Edge cases

- **Intents in flight during reconcile** — they are *behind*, not *in disagreement*: reconcile compares at equal sequence numbers only, so the in-flight window never pages and never "heals" a charge away.
- **Hot-store crash or asynchronous failover** — Redis loses the most recent acknowledged intents *together with* their balance change, so no reconcile can see it: silent **under-billing, never double-billing**, bounded by the AOF fsync interval or the replication lag. `HotAckWait` narrows it; `journal` settlement removes it.
- **The books ahead of a rolled-back hot store** — detected as a sequence regression or a replication-id change; the shard is rebuilt from the books. Nothing is lost.
- **A replay after its hot guard expired** — caught by the durable guard; the hot effect is reversed through suspense; the ledger is never double-charged.
- **An event older than the dedup window** — refused as `stale_event`, never re-applied.
- **Concurrent admits against a near-empty balance** — bounded overshoot of `concurrency × MaxCost`.
- **A provider replaying a webhook after inbox retention** — the permanent `credit_idem (realm, 'grant', key)` guard still prevents a second grant.
- **A provider whose verification API is down** (PayPal) — deliveries are **parked** with their raw body and retried; never processed optimistically, never dropped.
- **A missed `subscription.deleted`** — the drift sweep repairs it.
- **Two host products colliding on an idempotency key** — realm-scoped uniqueness keeps them independent.
- **A cancelled stream / aborted job** — settles on actual production.
- **An untrimmed stream on a `noeviction` Redis** — the hot function refuses with backpressure and `Record` defers to the journal; nothing is dropped.

## Requirements

### Functional — Metering & enforcement
- **FR-001**: The system MUST price a metered event as a pure, deterministic function of its rate key, its integer quantities and a versioned rate card, using **exact rational** prices rounded up once per event, and failing closed on an unknown key or unit.
- **FR-002**: The system MUST record the rate-card version on every ledger row so a price can be re-derived exactly.
- **FR-003**: The system MUST charge a retried or duplicated operation **at most once**, guarded durably — permanently for operations that mint or move money, and for a stated window for usage, refusing usage older than that window — and MUST refuse a reused key whose request differs.
- **FR-004**: The system MUST gate work before it is performed, evaluating every configured ceiling, and MUST refuse with a machine-readable deny code plus a human-readable reason. It MUST NOT pre-debit.
- **FR-005**: The system MUST accept an optional upper bound on a call's cost and refuse when it exceeds available allowance.
- **FR-006**: The system MUST warn at an operator-configurable near-limit fraction, coalesced per scope/limit/window.
- **FR-007**: The system MUST have exactly one durable writer of the ledger and the booked balances. Request-serving tiers write only the hot tier and the intent journal — including for transfers.
- **FR-008**: The system MUST write the usage record and the debit in one atomic step.
- **FR-009**: The system MUST reconcile the hot balance against `booked + open suspense` **at equal sequence numbers**, heal only the hot side, never book a ledger row to heal, and **alarm** on any disagreement beyond a configured tolerance.
- **FR-010**: The system MUST rebuild a stale or lost hot account from the books only after draining its shard, and MUST detect a hot-store restart, failover or sequence regression and freeze the affected shard until it is rebuilt.
- **FR-011**: The system MUST bill a cancelled unit of work for what it actually produced.
- **FR-012**: The system MUST represent internal amounts as signed 64-bit integers in a fine accounting unit and fiat as integer minor units plus ISO-4217. No floating point may touch money.
- **FR-013**: The system MUST treat a scope as opaque.
- **FR-014**: The system MUST isolate realms completely.
- **FR-015**: The system MUST apply an explicit, configured policy when a credit change would drive a pool negative, and MUST book a compensating row when it floors one.
- **FR-016**: The system MUST support an optional credit-expiry mode that consumes and expires grants oldest-first within each pool.
- **FR-017**: The system MUST load ceilings, pools and rate cards as per-realm configuration data, and MUST allow a ceiling's size to come from the scope's plan, override or free tier.
- **FR-017a**: The system MUST support a cumulative ceiling scoped to a single unit of work, as a counter rather than a granted balance.
- **FR-017b**: The system MUST provide a transfer between two scopes of one realm under one idempotency key that refuses rather than overdraws, conserves the realm's total including credits in transit at every step, and always completes both sides once it has begun.
- **FR-017c**: The system MUST key ceilings by a subject within the charged scope (a member, an API key, a job) and MUST refuse a call that omits a subject a configured ceiling needs.
- **FR-017d**: The system MUST support credit pools with a draw priority and an optional resource restriction.
- **FR-017e**: The system MUST NOT drop usage during a hot-store outage: it MUST accept it durably and apply it on recovery.
- **FR-017f**: The system MUST run its hot path on Redis Cluster: every atomic operation touches one hash slot.

### Functional — Payments
- **FR-018**: The system MUST verify every provider webhook's signature on the raw body, in constant time, within a timestamp tolerance, **before any parsing or side effect**, with the secret of the provider account named in the route.
- **FR-019**: The system MUST grant credits only on a verified provider event.
- **FR-020**: The system MUST persist a verified webhook durably **before** acknowledging it, and MUST process it from that store with retries, so a crash after acknowledgement can delay a grant but never lose it.
- **FR-021**: The system MUST resolve scope, realm, amount, currency and credit quantity from the verified provider object plus its own customer and catalogue records — never from client input or provider metadata.
- **FR-022**: The system MUST create credits only through the metering grant path.
- **FR-023**: The system MUST append refunds and chargebacks as new rows, revoke credits for partial refunds cumulatively and exactly, and reverse a chargeback whose dispute is won.
- **FR-024**: The system MUST support interchangeable providers and several accounts per provider, with each provider SDK confined to its own adapter and no migration needed to add one.
- **FR-025**: The system MUST hold entitlements through a configurable grace window on a failed payment.
- **FR-026**: The system MUST apply subscription state from the provider's current object, never letting an older event overwrite a newer state, and MUST sweep for missed webhooks.
- **FR-027**: The system MUST retain webhook records longer than the provider's replay window.
- **FR-028**: The system MUST name an explicit tax strategy and MUST NOT compute tax itself.
- **FR-029**: The system MUST apply a configured policy for mid-period plan changes.
- **FR-030**: The system MUST never receive, store or log card data, and MUST store provider secrets only by reference.
- **FR-030a**: The system MUST support auto top-up: an off-session charge when a pool crosses a threshold, bounded per day, fulfilled only by the verified payment webhook.

### Functional — Entitlements
- **FR-031**: The system MUST resolve plan capabilities as data with the precedence override > subscription > default, and report the provenance of every effective grant.
- **FR-032**: The system MUST deny an undeclared key and deny on a resolution error.
- **FR-033**: The system MUST accompany every refusal with a reason and, where one exists, an upgrade path.
- **FR-034**: The system MUST require a reason and write an audit row for every override.
- **FR-035**: The system MUST invalidate cached entitlements immediately on a subscription change, an override write, or a grace expiry.

### Functional — Surfaces & operations
- **FR-036**: The system MUST expose the whole credits snapshot in one round trip.
- **FR-037**: The system MUST report fulfilment state server-side.
- **FR-038**: The system MUST render the credits surface over an opaque unit and scale and an opaque scope, with the ceiling count, labels, columns and pools as data.
- **FR-039**: The system MUST scope a member's spend breakdown to their own usage unless they hold an administrative entitlement.
- **FR-040**: The system MUST authenticate callers, bind each credential to a realm set, and separate the `record`, `grant` and `admin` privileges.
- **FR-041**: The system MUST export the named metrics of the observability contract.
- **FR-042**: The system MUST fail its readiness probe when the schema is behind head or the shard count disagrees.
- **FR-043**: The system MUST park, never drop, an intent or message that exhausts its retry budget, and MUST alert when it does.
- **FR-044**: The system MUST run both as an embedded library and as a standalone service over the same ports.
- **FR-045**: The system MUST ship as a container with its migrations, requiring only a Redis URL and a Postgres DSN. A separate broker MUST NOT be required.
- **FR-046**: The system MUST implement its async seam behind a `Bus` port with **Redis Streams as the default**, the intent written by the same atomic operation that moves the balance, and MUST offer **NATS JetStream** as an alternative with identical subjects and handlers.
- **FR-047**: With a stream-backed bus the system MUST enforce an explicit retention floor, MUST NOT trim un-acked entries, and MUST refuse loudly rather than drop when a stream reaches its cap.
- **FR-048**: The system MUST deliver events to hosts as signed, retried webhooks and as a cursor feed, and MUST NOT require hosts to access its internal stores.
- **FR-049**: The system MUST offer an administrative API for configuration, and MUST audit and propagate every configuration change — including one made directly in the database — in the same transaction.
- **FR-050**: The system MUST keep storage bounded: create partitions ahead of need, drop usage dedup past its window, and archive ledger partitions only once checkpoints cover them; and MUST offer a batched ledger granularity with a per-event archive.
- **FR-051**: The system MUST version its API and give a breaking change at least 12 months of overlap.

### Key entities
- **Realm** — one host product on a deployment; the outermost isolation key.
- **Scope** — the opaque billing subject within a realm; **Subjects** — the members, keys and jobs within it.
- **Credits** — the signed internal accounting unit; **Pool** — a balance within a scope with its own priority and eligibility.
- **Money** — fiat in integer minor units plus a currency.
- **Rate card** — an immutable, versioned list of rational prices, naming its pricer.
- **Limit** — one ceiling: a subject, a unit, a maximum (possibly plan-sized), a window, a warn fraction and a deny code.
- **Intent** — the record of one hot-tier mutation, with its scope's sequence number.
- **Ledger** — the append-only account of record; **Suspense** — hot movements the books have not accepted.
- **Transfer** — credits moved between scopes, conserved including in transit.
- **Plan**, **Provider account**, **Payment**, **Webhook event**, **Entitlement**, **Outbound event**.

## Success Criteria

- **SC-001**: Admission adds no more than **5 ms at p99** co-located and **1 ms** embedded, at the scale envelope's target load.
- **SC-002**: No operation is charged twice under retry, duplicate submission, concurrent delivery, webhook replay, late replay or hot-store failover — and the hot balance equals `booked + open suspense` at equal sequence numbers on every reconcile.
- **SC-003**: An exhausted ceiling is always refused with a classified code and an actionable message.
- **SC-004**: The same conformance suites pass unchanged against the `table` pricer over **three disjoint unit spaces** and against a registered compiled pricer, and against every provider adapter.
- **SC-005**: A new host product is onboarded by publishing a rate card, a `Scope` binding and a realm's configuration rows — **no change to any file under `metering/`**.
- **SC-006**: `metering/` builds standalone with no dependency on the fiat, entitlement or events layers.
- **SC-007**: Tampered, unsigned and stale webhook bodies are rejected in 100% of seeded cases.
- **SC-008**: After a hot-store loss or failover, affected shards are rebuilt from the books before serving resumes, within the host's stated RTO.
- **SC-009**: Drift at equal sequence numbers pages in 100% of injected cases; steady traffic produces zero drift findings.
- **SC-010**: Every ledger row — or, under rollup granularity, every archived event behind it — can be re-priced exactly.
- **SC-011**: A capability check denies on an undeclared key and on a store outage in 100% of cases.
- **SC-012**: The credits surface renders correctly for one, three and five ceilings, pools, and a non-credit unit, with no component edit.
- **SC-012a**: A multi-step job halts at its own cap; a transfer interrupted at any point converges to both sides applied, never one.
- **SC-013**: A single `docker compose up` yields a service passing `/readyz`, with **no broker in the dependency set**.
- **SC-014**: The bus conformance suite passes unchanged against both adapters.
- **SC-015**: The reference hot functions run on Redis in cluster mode, and every operation touches one slot — verified in CI.
- **SC-016**: A crash at any point after a webhook is acknowledged never loses its grant.
- **SC-017**: Any sequence of partial refunds revokes exactly `min(granted, ceil(granted × refunded / paid))`.

## Out of scope

- **Post-paid usage invoicing** — designed as [feature 002](../002-postpaid-invoicing/spec.md).
- **Invoices, credit notes, discounts, coupons, trials, quantity-bearing subscriptions** — feature 002.
- **Tax computation, filing, and merchant-of-record duties** — provider or specialist service (FR-028).
- **Invoice rendering and notification templates** — the provider's hosted invoices and the host, fed by outbound events.
- **End-user authentication and authorization** — the host's.
- **Usage counting for quota entitlements** — the host owns its own counts.
- **Double-entry general-ledger accounting and statutory reporting** — Phase 3, not designed; export instead.
- **Fraud scoring and chargeback representment** — the provider's tooling.
- **Multi-currency FX** — prices are set per currency; no rate is applied.
- **Active-active multi-region** — one home region per deployment with a stated DR posture ([D45](./design-decisions.md)); per-scope home regions are the path if ever needed.
- **Release-engineering feature flags** — distinct from commercial entitlements.
