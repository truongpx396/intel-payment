# intel-payment

**A reusable metering, credits and payments service.** Drop it in front of any product that needs
to count usage, enforce budgets, take money and gate features — as a Go library or as a
self-contained container.

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

> ## ⚠️ Status: design complete, **implementation not started**
> This repository currently contains the **specification** — contracts, data model, verified
> migrations and deployment scaffolding. There is **no Go code yet**, so the commands below
> describe the intended interface rather than something you can run today. Start at
> [ROADMAP.md](ROADMAP.md) for what exists, what is designed, and what is deliberately absent;
> [tasks.md](specs/001-metering-billing-core/tasks.md) is the build order.
>
> **Scope, stated honestly:** this is a *metering, credits and payment-collection engine* for
> products that need an internal credit unit with sub-millisecond refuse-before-spend enforcement.
> **Post-paid usage invoicing** ("invoice us monthly for what we used") is designed as
> [Phase 2](specs/002-postpaid-invoicing/) and not built. If you do not need credits plus hot-path
> enforcement, [ROADMAP.md](ROADMAP.md) names the tools you probably want instead.

```text
                 ┌─────────────────────────────────────────────────────────┐
  HOST PRODUCT   │  Admit(scope, maxCost) → allowed? headroom?             │  sub-ms gate,
  (any language) │  Record(event)         → priced, settled, exactly once  │  BEFORE the work
                 └────────────────────────┬────────────────────────────────┘
                                          ▼
┌── intel-payment ───────────────────────────────────────────────────────────────────┐
│  metering/     the engine: price · admit · debit · reconcile · single durable writer │
│  billing/      the fiat boundary: Stripe · Polar · PayPal, webhook-driven grants     │
│  entitlement/  what a plan unlocks besides credits: flags · quotas · enums           │
│  ui/           credits-ui: balance · ceilings · spend · ledger · plan catalogue      │
└────────────────────────────────────────────────────────────────────────────────────┘
   Redis — hot balance + outbox STREAM (the bus, by default)   ·   Postgres — the account of record
              (NATS JetStream is an optional swap, not a requirement)
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
| **`Pricer`** | your code, one interface | tokens · GB-days · seats · API calls · minutes |
| **`Scope`** | your request context | `{workspace, w_1}` · `{organization, o_9}` · `{api_key, k_7}` |
| **`Realm`** | deployment config | which product this is, when one deployment serves several |

Everything else is generic. Adopting it changes no file under `metering/` — and the conformance
suites run unchanged against pricers over three disjoint unit spaces, so that is a test result
rather than a claim.

## What you get

| | |
|---|---|
| **Sub-millisecond enforcement** | One atomic Redis script per operation: `DECRBY` + outbox `LPUSH` + idempotency `SET NX`. No durable-store wait on the hot path |
| **Exactly-once accounting** | A realm-scoped idempotency guard in Postgres. Retries, double-clicks, redelivered bus messages and replayed webhooks all converge on one effect |
| **Refuse before you spend** | The gate evaluates every ceiling *and* the call's cost bound, so overshoot is bounded rather than hoped for |
| **Runaway jobs have their own ceiling** | A `Job` window caps one multi-step task's cumulative spend, independently of every calendar limit — because a daily budget alone still lets one loop burn the day |
| **Pools and allocations** | `Transfer` moves credits between scopes atomically: a parent buys once, children draw allocations with an optional cap, and the realm's total is invariant |
| **A ledger that reconciles** | Append-only, partitioned, with periodic drift detection that **alarms** past tolerance instead of silently healing |
| **Payments that cannot double-grant** | Signature-verified webhooks are the only fulfilment path; an atomic event claim plus the idempotency guard make replays no-ops |
| **Entitlements, not plan-code `if`s** | Flags, quotas and enums as data, with `override > subscription > default` precedence and provenance on every grant |
| **A credits UI that is not hardcoded** | The ceiling count, the unit, the ledger columns and the dimension members are all data. One ceiling or five, credits or GB-months, same components |
| **Two stores, not three** | The bus defaults to **Redis Streams**, so the outbox *is* the stream — published by the same atomic script that moves the balance. No broker to stand up; **NATS JetStream is a one-line swap** when you need quorum replication or cross-region mirroring |
| **Library or container** | The same ports, embedded in-process or behind gRPC + REST. Switching is a wiring change in `cmd/` |
| **A boundary you cannot cross by accident** | `go-arch-lint` + `depguard` make the portability guarantee a CI gate |

## Quick start *(intended interface — needs the Phase 1 implementation)*

```bash
cp .env.example .env
make up                          # postgres + redis + migrations + paymentd + worker
                                 # (no broker: the bus is Redis Streams by default)
curl -s localhost:8080/readyz    # {"status":"ready","migrations":"head"}
make seed REALM=my-product       # a rate card, three ceilings, two plans, a free tier
```

Then meter something:

```bash
# Gate BEFORE the work. max_cost is what bounds overshoot — pass it.
curl -s localhost:8080/v1/admit -H 'Authorization: Bearer dev-token' \
  -d '{"scope":{"realm":"my-product","kind":"organization","id":"org_1"},"max_cost":500}'

# Settle with what was ACTUALLY produced — including on cancellation.
curl -s localhost:8080/v1/record -H 'Authorization: Bearer dev-token' \
  -H 'Idempotency-Key: call-abc' \
  -d '{"scope":{"realm":"my-product","kind":"organization","id":"org_1"},
       "resource":"llm.chat","rate_key":"gpt-4o",
       "quantities":[{"unit":"llm_input_token","amount":1200},
                     {"unit":"llm_output_token","amount":800}],
       "idem_key":"call-abc"}'
# → {"applied":true,"delta":-11,"balance":99989,"rate_card_version":"2026-01-01"}

# Replay it. Nothing happens — that is the whole point.
```

Full walkthrough: [quickstart](specs/001-metering-billing-core/quickstart.md).

## Configurable where it matters

Two layers, and the split is the design: **infrastructure and policy** are environment
variables; **anything commercially volatile** is Postgres rows, so launching a price, a ceiling
or a plan needs no deploy of this service — which matters most when you do not control its
release cycle.

| Policy | Options | Default | Decides |
|---|---|---|---|
| `Settlement` | `outbox` · `journal` | `outbox` | Sub-ms enforcement with a bounded under-bill RPO, or one synchronous durable write and none |
| `AdmitFailPolicy` | `fail_closed` · `fail_open` | `fail_closed` | Hot store down: refuse, or serve unmetered |
| `NegativeBalancePolicy` | `allow_debt` · `clamp_to_zero` · `block_and_flag` | `clamp_to_zero` | What a refund past consumed credits does |
| `CreditExpiry` | `off` · `lots_fifo` | `off` | Whether grants expire |
| `PlanChangePolicy` | `at_period_end` · `immediate_prorate` · `immediate_no_prorate` | `at_period_end` | Mid-period upgrades |
| `TaxStrategy` | `provider_managed` · `external_hook` · `none` | `provider_managed` | **Who** computes tax |
| Dunning | grace days, suspend-after | `7` / `14` | How long a failed payment keeps access |

Data-configured per realm: rate cards (immutable, versioned), ceilings (**including how many
there are**), plans, per-currency prices, provider price mappings, entitlements, the free tier.

Full reference: [docs/configuration.md](docs/configuration.md).

## Deployment shapes

**Embedded library** — a Go host imports `metering`; `Admit` is an in-process Redis op. Depend on
`ports.Meter`, and a later swap to the client stub changes nothing downstream.

**Container service** — any language, over gRPC (hot path) or REST. **Co-locate it**: `Admit` is
on every request, so a cross-region hop taxes all of them.

**Multi-product** — one deployment, one realm per product. Plans, cards, ceilings, credentials and
idempotency keys are all realm-partitioned, so nothing crosses.

**Bus** — Redis Streams by default, which keeps required infrastructure to Redis + Postgres and
removes a failure class: with a broker, an intent can sit in the outbox but not yet on the bus,
whereas a stream outbox is published by the same atomic script that moves the balance. Swap to
**NATS JetStream** (`PAYMENT_BUS=nats_jetstream`) for quorum replication or cross-region mirroring.
Same subjects, same handlers, same invariants — see [bus-subjects.md](specs/001-metering-billing-core/contracts/bus-subjects.md).

Either way the durable writer is its own process — the sole writer of the ledger, never a
request-serving tier.

## Documentation

| | |
|---|---|
| [**ROADMAP.md**](ROADMAP.md) | What exists, what is designed, what is absent, and when to use something else |
| [**Contracts**](specs/001-metering-billing-core/contracts/) | The normative surface: ports, invariants, conformance suites |
| [Post-paid invoicing (Phase 2)](specs/002-postpaid-invoicing/) | Rating, tiered schedules, commitments, immutable invoices, credit notes, discounts, trials |
| [Spec](specs/001-metering-billing-core/spec.md) | 45 functional requirements, 13 success criteria, the edge cases |
| [Data model](specs/001-metering-billing-core/data-model.md) | The schema and why each constraint exists |
| [Design decisions](specs/001-metering-billing-core/design-decisions.md) | Every resolved question and refinement, with what was rejected |
| [Plan](specs/001-metering-billing-core/plan.md) · [Tasks](specs/001-metering-billing-core/tasks.md) | Build order and the task list |
| [**Integration guide**](docs/integration-guide.md) | Adopting it in a host, end to end, plus the anti-patterns |
| [Configuration](docs/configuration.md) | Every knob and its failure mode |
| [Security](docs/security.md) | Caller auth, realm binding, webhook posture, pre-production checklist |
| [Operations](docs/operations.md) | The runbook: drift, outbox, rehydrate, missed webhooks, repricing |

## The invariants

Each one exists because its absence produces a specific money bug. The full lists are in the
contracts; these are the load-bearing few:

1. **One durable writer.** Every other producer publishes intents. No second money-writer — not a host service, not a provider callback, not a request handler.
2. **Dual idempotency, realm-scoped.** A Redis guard for speed; a Postgres unique constraint for correctness. The Redis half is never the guarantee.
3. **Admission gates, never reserves.** Bounded overshoot, healed at settlement — and the bound is only real when callers supply a cost bound, so omissions are counted.
4. **Partial settlement on cancel.** Billed for what was produced. Zero would make "start then abort" free; the ceiling would over-bill.
5. **Deterministic pricing, with the card version on the row.** Otherwise a replay silently uses today's rates and disagrees with the charge it verifies.
6. **Integer money only.** No float touches a balance, a price or a ledger row.
7. **The under-bill direction is documented.** In `outbox` mode a hot-store loss forgets a charge; reconcile heals *upward*. Never double-billing. `journal` removes it.
8. **Verify webhooks before anything.** Raw body, constant time, timestamp tolerance, before any parse or side effect.
9. **Fulfilment is webhook-driven.** Never granted on a checkout return — the tab closes, the redirect replays, the payment fails afterwards.
10. **A negative balance is a policy, not an accident.** And flooring one always writes a compensating row, or the books stop closing.

## Status

Design complete and normative; **implementation not started**. Contracts, data model, migrations and
deployment scaffolding are in place — all six migrations are verified against PostgreSQL 16 and the
seed path is exercised. Everything else, including the honest gap list, is in
[ROADMAP.md](ROADMAP.md). Build order: [tasks.md](specs/001-metering-billing-core/tasks.md).

## Provenance

Extracted from [aisat-intel](https://github.com/truongpx396/aisat-intel), where this engine was
designed as that product's credit backbone and deliberately factored for reuse. The git history of
the core contracts came across intact. [PROVENANCE.md](PROVENANCE.md) records what moved, what was
generalized, and what was fixed on the way — including one constraint in the inherited schema that
[turned out not to be constructible](specs/001-metering-billing-core/design-decisions.md).

## License

MIT — see [LICENSE](LICENSE).
