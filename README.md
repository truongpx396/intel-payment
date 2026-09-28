# intel-payment

**A reusable metering, credits and payments service.** Drop it in front of any product that needs
to count usage, enforce budgets, take money and gate features — as a Go library or as a
self-contained container.

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

> ## ⚠️ Status: design complete, **implementation not started**
> This repository contains the **specification** — contracts, data model, migrations, deployment
> scaffolding — and the parts of it that can be executed without Go: the schema is applied and
> behaviourally tested on PostgreSQL 16, and the reference hot-path functions run on **Redis in
> cluster mode**, both in CI. There is **no Go code yet**, so the service commands below describe the
> intended interface. Start at [ROADMAP.md](ROADMAP.md) for what exists and what is deliberately
> absent; [tasks.md](specs/001-metering-billing-core/tasks.md) is the build order.
>
> **Scope, stated honestly:** this is a *metering, credits and payment-collection engine* for
> products that need an internal credit unit with sub-millisecond refuse-before-spend enforcement.
> **Post-paid usage invoicing** is designed as [Phase 2](specs/002-postpaid-invoicing/) and not
> built. If you do not need credits plus hot-path enforcement, [ROADMAP.md](ROADMAP.md) names the
> tools you probably want instead.

```text
                 ┌─────────────────────────────────────────────────────────┐
  HOST PRODUCT   │  Admit(scope, subjects, maxCost) → allowed? headroom?  │  sub-ms gate,
  (any language) │  Record(event)  → priced exactly, settled exactly once │  BEFORE the work
                 └────────────────────────┬────────────────────────────────┘
                                          ▼                    ▲ signed webhooks / event feed
┌── intel-payment ───────────────────────────────────────────────────────────────────┐
│  metering/     price · admit · debit (one Redis Function per op, one shard slot)    │
│                · sole durable writer · reconcile at the sequence watermark          │
│  billing/      Stripe · Polar · PayPal accounts · webhook inbox · refunds · disputes│
│  entitlement/  flags · quotas · enums — and the size of every plan-dependent ceiling│
│  events/       signed outbound webhooks + a cursor feed                              │
│  ui/           credits-ui: balances per pool · ceilings · spend · ledger · plans     │
└────────────────────────────────────────────────────────────────────────────────────┘
   Redis (single or Cluster) — hot balances + the outbox STREAM, same slot   ·   Postgres — the books
```

## Why this exists

Every product that charges for usage rebuilds the same machine: a fast balance, a durable ledger,
idempotency so a retry does not double-charge, an admission gate that refuses before spending,
reconciliation because the fast copy drifts from the record, and a payments layer whose webhooks
are the only honest source of "they paid". It is a few weeks of work to get roughly right and
a long time to get actually right, because every mistake is a money bug.

This is that machine, factored so the product-specific part is **three parameters**:

| Parameter | Supplied by | Examples |
|---|---|---|
| **Pricing** | a rate card (data) for the built-in `table` pricer — or a `Pricer` you register | tokens · GB-days · seat-days · API calls · minutes |
| **`Scope`** + **`Subjects`** | your request context | `{organization, o_9}` with `{user: u_1, api_key: k_7}` |
| **`Realm`** | deployment config | which product this is, when one deployment serves several |

Everything else is generic, and the conformance suites run unchanged across rate cards over three
disjoint unit spaces, so that is a test result rather than a claim.

## What you get

| | |
|---|---|
| **Sub-millisecond enforcement** | One Redis Function per operation — pool draws, sequence number, idempotency guard, window counters and the outbox `XADD` — every key in the scope's **shard slot**, so it runs on Redis Cluster. No durable-store wait on the hot path |
| **Exact pricing** | Prices are rationals (`$0.15 per million` is exact), arithmetic is exact, rounding is up **once per event**, in a fine accounting unit. No float anywhere |
| **Exactly-once accounting** | Two durable guards — permanent for anything that mints money, window-bounded for usage — each with a request fingerprint: a reused key with a different request is an error, never a silent no-op |
| **Refuse before you spend** | The gate evaluates every ceiling *and* the call's cost bound, so overshoot is bounded rather than hoped for |
| **Ceilings that fit real products** | Per scope or per **subject** (member, API key, project); sized by the **plan** through entitlements; a `Job` budget that stops one runaway loop independently of every calendar limit |
| **Credit pools** | Promotional credits spent before paid ones; credits restricted to some resources; expiry per pool |
| **Pools and allocations** | `Transfer` moves credits between scopes in two phases, refusing to overdraw, with the realm's total conserved at every step |
| **Books that reconcile honestly** | A per-scope sequence watermark: reconcile compares the fast copy with the books *at the same sequence number*, so in-flight charges are never mistaken for drift, and real drift pages |
| **Recovery that is a procedure** | A Redis restart, failover or loss freezes the affected shards, drains them, and rebuilds them from the books; usage during an outage is journaled, never dropped |
| **Payments that cannot lose or double a grant** | Signature-verified webhooks persisted to an inbox *before* they are acknowledged, then processed with leases and retries; refunds, disputes and out-of-order events handled exactly |
| **Entitlements, not plan-code `if`s** | Flags, quotas and enums as data, with `override > subscription > default` precedence and provenance |
| **Events for any language** | Signed, retried webhooks and a cursor feed — hosts never touch internals |
| **Configuration you can trust** | An admin API; and the database itself audits every configuration change and signals every cache, even for a direct SQL write |
| **Two stores, not three** | Redis + Postgres. The bus defaults to Redis Streams; NATS JetStream is an option for downstream replication |
| **Library or container** | The same ports, embedded in-process or behind gRPC + REST, with generated TypeScript and Python clients |
| **A boundary you cannot cross by accident** | `go-arch-lint` + `depguard` make the portability guarantee a CI gate; `check-spec-drift` does the same for the contracts |

## Quick start

**Today** — the design verifies itself:

```bash
scripts/verify-hot-path.sh   # the reference hot path on Redis 7 in cluster mode (Docker)
make verify-schema           # migrations + seed + behavioural assertions on an empty Postgres
```

**With the Phase 1 implementation** — the intended interface:

```bash
cp .env.example .env
make up                          # postgres + redis + migrations + paymentd + worker (no broker)
make seed REALM=my-product       # pools, a rate card, three ceilings, two plans, a free tier

# Gate BEFORE the work. max_cost bounds overshoot — pass it.
curl -s localhost:8080/v1/admit -H "Authorization: Bearer $PAYMENT_TOKEN" \
  -d '{"scope":{"kind":"organization","id":"org_1"},"resource":"llm.chat","subjects":{"user":"u_1"},"max_cost":50000}'

# Settle with what was ACTUALLY produced — including on cancellation.
curl -s localhost:8080/v1/record -H "Authorization: Bearer $PAYMENT_TOKEN" -H 'Idempotency-Key: call-abc' \
  -d '{"scope":{"kind":"organization","id":"org_1"},"resource":"llm.chat","rate_key":"gpt-4o",
       "subjects":{"user":"u_1"},"occurred_at":"2026-09-28T10:00:00Z",
       "quantities":[{"unit":"llm_input_token","amount":1200},{"unit":"llm_output_token","amount":800}]}'
# → {"applied":true,"delta":-11000,"seq":2,"rate_card_version":"2026-01-01", …}   (1 credit = 1 µ$)

# Replay it. Nothing happens — that is the whole point.
```

Full walkthrough: [quickstart](specs/001-metering-billing-core/quickstart.md).

## Configurable where it matters

**Infrastructure and policy** are environment variables; **anything commercially volatile** is
Postgres rows behind an admin API, so launching a price, a ceiling, a pool or a plan needs no deploy.

| Policy | Options | Default | Decides |
|---|---|---|---|
| `Settlement` | `outbox` · `journal` | `outbox` | Sub-ms enforcement with a stated under-bill window, or Postgres first and none |
| `HotAckWait` | `none` · `aof_local` · `aof_replica` | `none` | How much of that window to close by waiting for Redis durability |
| `LedgerGranularity` | `event` · `rollup` | `event` | One ledger row per event, or per batch with per-event detail archived — the path past one Postgres's write budget |
| `AdmitFailPolicy` | `fail_closed` · `fail_open` | `fail_closed` | Hot store down: refuse, or serve and meter on recovery |
| `NegativeBalancePolicy` | `allow_debt` · `clamp_to_zero` · `block_and_flag` | `clamp_to_zero` | What a refund past consumed credits does |
| `CreditExpiry` | `off` · `lots_fifo` | `off` | Whether grants expire |
| `PlanChangePolicy` | `at_period_end` · `immediate_prorate` · `immediate_no_prorate` | `at_period_end` | Mid-period upgrades |
| `TaxStrategy` | `provider_managed` · `external_hook` · `none` | `provider_managed` | **Who** computes tax |
| Dunning | grace days, suspend-after | `7` / `14` | How long a failed payment keeps access |

Full reference: [docs/configuration.md](docs/configuration.md). The design's sizing:
[scale envelope](specs/001-metering-billing-core/plan.md#scale-envelope).

## Deployment shapes

**Embedded library** — a Go host imports `metering`; `Admit` is an in-process Redis call.

**Container service** — any language, over gRPC (hot path), REST and generated clients.
**Co-locate it**: `Admit` is on every request.

**Multi-product** — one deployment, one realm per product; plans, cards, ceilings, pools, provider
accounts, credentials and idempotency keys are all realm-partitioned.

**Redis** — a single primary or a cluster; the key layout is the same. It is a **money store**:
`noeviction`, AOF, and reachable by nothing but this service.

**Region** — one home region, with Postgres replicated to a DR region and Redis rebuilt from the books.

## Documentation

| | |
|---|---|
| [**ROADMAP.md**](ROADMAP.md) | What exists, what is designed, what is absent, and when to use something else |
| [**Contracts**](specs/001-metering-billing-core/contracts/) | The normative surface: ports, invariants, conformance suites |
| [Hot-path consistency](specs/001-metering-billing-core/contracts/hot-path-consistency.md) | How Redis and Postgres stay consistent: key layout, watermark, suspense, recovery |
| [Outbound events](specs/001-metering-billing-core/contracts/outbound-events.md) | How hosts hear what happened |
| [Post-paid invoicing (Phase 2)](specs/002-postpaid-invoicing/) | Rating, tiered schedules, commitments, immutable invoices, credit notes |
| [Spec](specs/001-metering-billing-core/spec.md) · [Data model](specs/001-metering-billing-core/data-model.md) | Requirements, success criteria; the schema and why each constraint exists |
| [Design decisions](specs/001-metering-billing-core/design-decisions.md) | Every decision, with what was rejected — including the two design reviews |
| [Plan](specs/001-metering-billing-core/plan.md) · [Tasks](specs/001-metering-billing-core/tasks.md) | The scale envelope, the build order, the task list |
| [**Integration guide**](docs/integration-guide.md) | Adopting it in a host, end to end, plus the anti-patterns |
| [Configuration](docs/configuration.md) · [Security](docs/security.md) · [Operations](docs/operations.md) | Every knob; the threat model; the runbook |

## The invariants

Each one exists because its absence produces a specific money bug. The full list is in
[metering-ports.md](specs/001-metering-billing-core/contracts/metering-ports.md#invariants-every-implementation-must-uphold);
these are the load-bearing few:

1. **One durable writer.** The worker books every intent; request tiers never write the books — not even for a transfer.
2. **Dual idempotency, fingerprinted.** A hot guard for speed; durable guards for correctness; a reused key with a different request is an error.
3. **Admission gates, never reserves.** Bounded overshoot — and the bound is only real when callers supply a cost bound.
4. **Partial settlement on cancel.** Billed for what was produced.
5. **Exact, replayable pricing, with the card version on the row.** Rational prices, rounded up once per event.
6. **Integer money only.** No float touches a balance, a price or a ledger row.
7. **The under-bill window is stated exactly.** A Redis crash can lose the last acknowledged intents *and* their balance change together, invisibly — under-billing, never double-billing. `HotAckWait` narrows it; `journal` removes it.
8. **Hot equals booked plus suspense, at every sequence number.** Reconcile compares only there, heals the fast copy, and never books a row.
9. **Verify webhooks first; persist before acknowledging.** A crash after `200` can delay a grant, never lose it.
10. **A negative balance is a policy, not an accident.** And flooring one always writes a compensating row.

## Status

Design complete and normative; **implementation not started**. The schema and the reference hot
path are executable and verified in CI; everything else, including the honest gap list, is in
[ROADMAP.md](ROADMAP.md).

## Provenance

Extracted from [aisat-intel](https://github.com/truongpx396/aisat-intel), where this engine was
designed as that product's credit backbone and deliberately factored for reuse.
[PROVENANCE.md](PROVENANCE.md) records what moved, what was generalized, and what was fixed on the way.

## License

MIT — see [LICENSE](LICENSE).
