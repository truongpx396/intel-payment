# intel-payment

**A reusable metering, credits and payments service.** Drop it in front of any product that needs
to count usage, enforce budgets and take money — as a Go library or as a self-contained container.

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

> ## ⚠️ Status: metering and payments are built and served; entitlements, events and the UI are not
>
> | | |
> |---|---|
> | **Built, verified in CI, served** | The **metering core** — domain, ports, the hot path on Redis, the durable writer on Postgres, the Redis Streams and JetStream buses, the pricers, and the conformance suites they all pass — behind two binaries: **`paymentd`** (gRPC and REST behind a service credential; generated TypeScript and Python clients) and **`payment-worker`** (the sole durable writer). `make up` runs the stack |
> | **Built, met its sandboxes once** | **Payments.** `paymentd` takes `POST /webhooks/{provider}/{account}` from Stripe, Polar and PayPal — verified on the raw body, persisted, *then* acknowledged — and `payment-worker` fulfils it: a purchase mints credits exactly once, refunds and chargebacks take back exactly what is owed, a failed payment opens a grace window, a low balance can charge a saved card. A host sells through `/v1`; an operator configures through `/v1/admin`, every change audited. The adapters were rehearsed against the providers' sandboxes ([docs/sandbox.md](docs/sandbox.md)), which found and fixed two defects; what a sandbox could not reach is listed there and is **still unrehearsed** |
> | **Designed, not built** | **Entitlements** (`entitlement/` is a stub), **outbound events** (`events/` is a stub), the operational surface, the **credits UI**, and **post-paid invoicing** ([Phase 2](specs/002-postpaid-invoicing/)) |
>
> **Limits you will meet first:**
> - `paymentd` wires no entitlement source, so a ceiling sized by a plan (`max_entitlement`) cannot be resolved. The seed's `user_daily` is one: on a freshly seeded realm `/v1/admit` answers `503 quota_unavailable` until that ceiling is given a fixed size ([Quick start](#quick-start), step 3).
> - A saved payment method is not captured automatically from a `setup` checkout; the host passes its reference when it switches auto top-up on. Auto top-up charges on Stripe only.
> - Sub-millisecond admission is the design target. Nothing has been load tested yet.
>
> The task count, the full gap list and what is deliberately absent are in [ROADMAP.md](ROADMAP.md);
> [tasks.md](specs/001-metering-billing-core/tasks.md) is the build order, and
> [docs/testing.md](docs/testing.md) is how it is being built — test-first, with the evidence checked.
>
> **Scope, stated honestly:** this is a *metering, credits and payment-collection engine* for products
> that need an internal credit unit with refuse-before-spend enforcement on the request path. If you do
> not need credits plus hot-path enforcement, [ROADMAP.md](ROADMAP.md) names the tools you probably want
> instead.

```text
                 ┌─────────────────────────────────────────────────────────┐
  HOST PRODUCT   │  Admit(scope, subjects, maxCost) → allowed? headroom?  │  gate that runs
  (any language) │  Record(event)  → priced exactly, settled exactly once │  BEFORE the work
                 └────────────────────────┬────────────────────────────────┘
                                          ▼  gRPC · REST · or in-process
┌── intel-payment ───────────────────────────────────────────────────────────────────────────────┐
│  metering/    price · admit · debit (one Redis Function per op, one shard slot) ·     [built]  │
│               sole durable writer · reconcile at the sequence watermark                        │
│  billing/     Stripe · Polar · PayPal accounts · webhook inbox · refunds · disputes · [built]  │
│               checkout · subscriptions · auto top-up                                           │
│  entitlement/ flags · quotas · enums — and the size of plan-dependent ceilings     [designed]  │
│  events/      signed outbound webhooks + a cursor feed                             [designed]  │
│  ui/          credits-ui: balances per pool · ceilings · spend · ledger · plans    [designed]  │
└────────────────────────────────────────────────────────────────────────────────────────────────┘
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

**Built today**

| | |
|---|---|
| **No durable-store wait on the hot path** | One Redis Function per operation — pool draws, sequence number, idempotency guard, window counters and the outbox `XADD` — every key in the scope's **shard slot**, so it runs on Redis Cluster. Sub-millisecond is the target; it has not been load tested |
| **Exact pricing** | Prices are rationals (`$0.15 per million` is exact), arithmetic is exact, rounding is up **once per event**, in a fine accounting unit. No float anywhere |
| **Exactly-once accounting** | Two durable guards — permanent for anything that mints money, window-bounded for usage — each with a request fingerprint: a reused key with a different request is an error, never a silent no-op |
| **Refuse before you spend** | The gate evaluates every ceiling *and* the call's cost bound, so overshoot is bounded rather than hoped for |
| **Ceilings that fit real products** | Per scope or per **subject** (member, API key, project), in calendar or rolling windows; a `Job` budget that stops one runaway loop independently of every calendar limit |
| **Credit pools** | Promotional credits spent before paid ones; credits restricted to some resources; expiry per pool |
| **Pools and allocations** | `Transfer` moves credits between scopes in two phases, refusing to overdraw, with the realm's total conserved at every step |
| **Books that reconcile honestly** | A per-scope sequence watermark: reconcile compares the fast copy with the books *at the same sequence number*, so in-flight charges are never mistaken for drift, and real drift pages |
| **Recovery that is a procedure** | A Redis restart, failover or loss freezes the affected shards, drains them, and rebuilds them from the books; usage during an outage is journaled, never dropped |
| **Payments that cannot lose or double a grant** | Signature-verified webhooks persisted to an inbox *before* they are acknowledged, then processed with leases and retries; refunds, disputes and out-of-order events handled exactly. Stripe, Polar and PayPal behind one contract |
| **Configuration you can trust** | An admin API for rate cards, ceilings, pools, plans, the provider's prices, provider accounts and customers; and the database itself audits every configuration change and signals every cache, even for a direct SQL write |
| **Two stores, not three** | Redis + Postgres. The bus defaults to Redis Streams; NATS JetStream is an option for downstream replication |
| **Library or container** | The same ports, embedded in-process or behind gRPC + REST. A Go client, and TypeScript and Python clients generated from the OpenAPI document (not published to a registry) |
| **A boundary you cannot cross by accident** | `go-arch-lint` + `depguard` make the portability guarantee a CI gate; `check-spec-drift` does the same for the contracts |

**Designed, not built**

| | |
|---|---|
| **Entitlements, not plan-code `if`s** | Flags, quotas and enums as data, with `override > subscription > default` precedence and provenance. The schema is in; the service is not, so ceilings sized by a plan are not resolved by the shipped binary |
| **Events for any language** | Signed, retried outbound webhooks and a cursor feed — hosts never touch internals |
| **A credits UI** | Balances per pool, ceilings, spend, ledger and plans |
| **Post-paid invoicing** | Rating, tiered schedules, commitments, immutable invoices — [Phase 2](specs/002-postpaid-invoicing/) |

## Quick start

You need Docker with Compose v2 and free host ports **8080** (REST) and **9090** (gRPC). Postgres and Redis
publish on 5432 and 6379; set `PAYMENT_POSTGRES_PORT` / `PAYMENT_REDIS_PORT` to move them.

**1. Configure.** A caller authenticates with a bearer token; the configuration holds only its SHA-256 digest,
never the token, and an empty `PAYMENT_SERVICE_TOKENS` accepts no caller. One token per privilege, because a
spend producer must never hold the credential that mints credits:

```bash
export PAYMENT_TOKEN=$(openssl rand -hex 16)         # privilege: record
export PAYMENT_GRANT_TOKEN=$(openssl rand -hex 16)   # privilege: grant  (mints credits)
export PAYMENT_ADMIN_TOKEN=$(openssl rand -hex 16)   # privilege: admin
digest() { printf %s "$1" | shasum -a 256 | cut -d' ' -f1; }

# .env = .env.example with the token line filled in (realm `default`, which is what `make seed` seeds)
{ grep -v '^PAYMENT_SERVICE_TOKENS=' .env.example
  echo "PAYMENT_SERVICE_TOKENS=app:$(digest "$PAYMENT_TOKEN"):default:record,granter:$(digest "$PAYMENT_GRANT_TOKEN"):default:grant,admin:$(digest "$PAYMENT_ADMIN_TOKEN"):default:admin"
} > .env
```

To serve a realm of your own, set `PAYMENT_REALM` in `.env`, name the same realm in each token, and run
`make seed REALM=<it>`.

**2. Start and seed.**

```bash
make up                         # postgres + redis + migrations + paymentd + payment-worker (no broker)
curl -s localhost:8080/readyz   # {"status":"ready","migrations":"head","shards":256}
make seed                       # a promo pool, a rate card, three ceilings, two plans, a free tier, a test Stripe account
```

**3. Size the per-member ceiling.** The seed sizes `user_daily` from the plan's entitlement, and `paymentd`
has no entitlement source yet, so on a seeded realm `/v1/admit` answers `503 quota_unavailable`. Until
entitlements exist, give the ceiling a fixed size (it applies to the next `admit`):

```bash
curl -s localhost:8080/v1/admin/limits/user_daily -X PUT -H "Authorization: Bearer $PAYMENT_ADMIN_TOKEN" \
  -d '{"actor":"quickstart","reason":"fixed size until entitlements exist","subject_kind":"user","unit":"credit",
       "max":10000000,"window":"daily","tz":"UTC","warn_at":0.8,"deny_code":"limit_reached"}'
```

**4. Meter something.**

```bash
# Credit the scope. In production only the webhook processor mints; this is the privileged shortcut.
curl -s localhost:8080/v1/grant -H "Authorization: Bearer $PAYMENT_GRANT_TOKEN" -H 'Idempotency-Key: signup-org_1' \
  -d '{"scope":{"kind":"organization","id":"org_1"},"pool":"promo","amount":5000000,"reason":"signup_grant"}'

# Gate BEFORE the work. max_cost bounds overshoot — pass it. With no credit this is HTTP 402 payment_required.
curl -s localhost:8080/v1/admit -H "Authorization: Bearer $PAYMENT_TOKEN" \
  -d '{"scope":{"kind":"organization","id":"org_1"},"resource":"llm.chat","subjects":{"user":"u_1"},"max_cost":50000}'
# → {"allowed":true,"headroom":2000000,"blocked":false}

# Settle with what was ACTUALLY produced — including on cancellation. occurred_at must be recent:
# an event older than the usage window (71h by default) is refused as stale_event.
NOW=$(date -u +%FT%TZ)
RECORD=$(cat <<EOF
{"scope":{"kind":"organization","id":"org_1"},"resource":"llm.chat","rate_key":"gpt-4o",
 "subjects":{"user":"u_1"},"occurred_at":"$NOW",
 "quantities":[{"unit":"llm_input_token","amount":1200},{"unit":"llm_output_token","amount":800}]}
EOF
)
curl -s localhost:8080/v1/record -H "Authorization: Bearer $PAYMENT_TOKEN" -H 'Idempotency-Key: call-abc' -d "$RECORD"
# → {"idem_key":"call-abc","applied":true,"deferred":false,"delta":-11000,"balance":4989000,"seq":2,"rate_card_version":"2026-01-01","writeoff":0}   (1 credit = 1 µ$)

# Replay it. Nothing happens — that is the whole point.
curl -s localhost:8080/v1/record -H "Authorization: Bearer $PAYMENT_TOKEN" -H 'Idempotency-Key: call-abc' -d "$RECORD"
# → {"idem_key":"call-abc","applied":false,"deferred":false,"delta":0,…,"seq":2,…}
# The same key with a different request is refused, never silently swallowed: HTTP 409 idempotency_conflict.
```

**5. Take a payment.** Register a provider account and map a plan to the provider's price through
`/v1/admin`, point the provider's webhook at `/webhooks/{provider}/{account}`, and start a checkout with
`POST /v1/checkout`. Credits appear when the verified webhook is *processed*, not on the redirect.
[docs/sandbox.md](docs/sandbox.md) runs this against each provider, and the
[quickstart](specs/001-metering-billing-core/quickstart.md) has the long walkthrough.

**Without the stack**, the design verifies itself:

```bash
make test                # unit tests and every conformance suite
make verify-hot-path     # the reference Redis Functions on Redis 7 in cluster mode (Docker)
make verify-schema       # migrations, seed and behavioural assertions on an empty Postgres (Docker)
```

## What is served

| Surface | Where | What |
|---|---|---|
| **Metering** (REST + gRPC) | `/v1/admit` `/v1/record` `/v1/grant` `/v1/transfer` `/v1/transfers/{key}` `/v1/balance` `/v1/ledger` `/v1/credits` | Gate, settle, mint, move, and read balances and the ledger |
| **Payments** (REST) | `/v1/plans` `/v1/checkout` `/v1/subscription` (`/change`, `/cancel`) `/v1/portal` `/v1/auto-recharge` `/v1/payments` `/v1/fulfilment` | What a host sells through. A plan change is accepted, never reported as made; a checkout names a plan, never a price |
| **Admin** (REST) | `/v1/admin/{rate-cards,limits,pools,plans,provider-accounts,customers}` and a plan's `provider-prices` | Commercial configuration as rows, every change audited |
| **Webhook door** | `POST /webhooks/{provider}/{account}` | Signature only — no JWT, no CORS. Verify, persist, then acknowledge |
| **Operations** | `/healthz` `/readyz` `/metrics` | `/readyz` fails while the schema is behind head |

The normative surface is [rest-api.md](specs/001-metering-billing-core/contracts/rest-api.md) and
[grpc-surface.md](specs/001-metering-billing-core/contracts/grpc-surface.md); the OpenAPI document is
`api/openapi/v1.yaml`. Each caller authenticates with a service credential (bearer or mTLS) scoped to
realms and to the privileges `record`, `grant` and `admin`, so a spend producer never holds the
credential that mints credits — see [docs/security.md](docs/security.md).

### Payment providers

| Provider | Rehearsed against | Notes |
|---|---|---|
| **Stripe** | a test-mode account (API 2024-06-20) | One-time packs, subscriptions, change and cancel, hosted checkout, billing portal, partial refunds, disputes, an auto top-up charging a saved card. The only provider that can charge a saved method |
| **Polar** | the sandbox | Signature, a free order, product and checkout creation, the grant. A paid order, refund and renewal are not yet rehearsed |
| **PayPal** | the sandbox and its webhook simulator | OAuth, order creation, PayPal's own verify call on simulator deliveries, dispute events. A buyer approving an order is not yet rehearsed |

Secrets are never stored in the database: a provider account holds a *reference* such as
`env:STRIPE_SECRET_KEY`. The full record, including how to repeat the rehearsal for your own account, is
[docs/sandbox.md](docs/sandbox.md).

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
| `CreditExpiry` | `off` · `lots_fefo` | `off` | Whether grants expire; the lot that lapses soonest is spent first |
| `PlanChangePolicy` | `at_period_end` · `immediate_prorate` · `immediate_no_prorate` | `at_period_end` | Mid-period upgrades |
| `TaxStrategy` | `provider_managed` · `none` (`external_hook` when embedded as a library) | `provider_managed` | **Who** computes tax. The binary refuses `external_hook`: it cannot be given your tax service |
| Dunning | grace days · suspend-after | `7` · `14` | How long a failed payment keeps access. Grace is enforced by `payment-worker`; suspend-after is read by nothing until entitlements exist |

Full reference: [docs/configuration.md](docs/configuration.md); every variable, with its default, is in
`.env.example`. The design's sizing: [scale envelope](specs/001-metering-billing-core/plan.md#scale-envelope).

## Deployment shapes

**Embedded library** — a Go host imports `metering`; `Admit` is an in-process Redis call.

**Container service** — any language, over gRPC (hot path), REST and generated clients.
**Co-locate it**: `Admit` is on every request. `make up` is the reference composition: Postgres, Redis,
`payment-migrate`, `paymentd` and `payment-worker`, with no broker.

**Multi-product** — one deployment, one realm per product; plans, cards, ceilings, pools, provider
accounts, credentials and idempotency keys are all realm-partitioned.

**Redis** — a single primary or a cluster; the key layout is the same. It is a **money store**:
`noeviction`, AOF, and reachable by nothing but this service.

**Region** — one home region, with Postgres replicated to a DR region and Redis rebuilt from the books.

## Repository layout

| Path | What it is |
|---|---|
| `metering/` | The engine, standalone and portable: `domain` → `ports` → `app` → `adapters/{driven,driving}`, plus the conformance suites in `contracts/` |
| `billing/` | The payment layer, same shape: adapters for Postgres, Stripe, Polar and PayPal, and the webhook and `/v1` HTTP handlers. It depends on metering's *ports* only |
| `entitlement/` `events/` | Designed; stubs today |
| `cmd/` | `paymentd`, `payment-worker`, `payment-migrate`. The only place that reads the environment |
| `api/` | The protobuf definitions, the generated Go, and the OpenAPI document |
| `migrations/` | Forward-only schema ([README](migrations/README.md)) |
| `specs/` | Spec, plan, tasks, design decisions and the normative contracts for Phase 1; the Phase 2 draft |
| `integration/` `e2e/` | Testcontainers stack tests; Playwright against the built image ([README](e2e/README.md)) |
| `scripts/` `deploy/` | Every verification gate; the compose stack, Dockerfile and Redis ACL |

## Development

```bash
make test               # unit tests and every conformance suite, -race -shuffle=on
make test-integration   # real Redis + Postgres via Testcontainers (Docker)
make lint               # golangci-lint (depguard boundary rules) + go-arch-lint
make ci                 # everything CI runs on every change
make help               # all of it
```

Go 1.26+. Work happens on a branch and arrives as a pull request. A
production change lands as a failing tests-only `test(red)` commit followed by its fix, and CI checks
that order — see [docs/testing.md](docs/testing.md) for that, for fuzzing and for mutation testing. The
rules every change to the money path answers to are the
[constitution](.specify/memory/constitution.md); [CLAUDE.md](CLAUDE.md) is the working guide.

## Documentation

| | |
|---|---|
| [**ROADMAP.md**](ROADMAP.md) | What exists, what is designed, what is absent, and when to use something else |
| [**Quickstart**](specs/001-metering-billing-core/quickstart.md) | The long walkthrough: seed a realm, meter something, take a payment, verify the guarantees |
| [**Integration guide**](docs/integration-guide.md) | Adopting it in a host, end to end, plus the anti-patterns |
| [**Contracts**](specs/001-metering-billing-core/contracts/) | The normative surface: ports, invariants, conformance suites |
| [Hot-path consistency](specs/001-metering-billing-core/contracts/hot-path-consistency.md) | How Redis and Postgres stay consistent: key layout, watermark, suspense, recovery |
| [Payment providers](specs/001-metering-billing-core/contracts/payment-provider-ports.md) · [Outbound events](specs/001-metering-billing-core/contracts/outbound-events.md) | The provider port every adapter meets; how hosts will hear what happened |
| [Sandbox rehearsal](docs/sandbox.md) | What the provider adapters have met, what they found, and what is still owed |
| [Post-paid invoicing (Phase 2)](specs/002-postpaid-invoicing/) | Rating, tiered schedules, commitments, immutable invoices, credit notes |
| [Spec](specs/001-metering-billing-core/spec.md) · [Data model](specs/001-metering-billing-core/data-model.md) | Requirements, success criteria; the schema and why each constraint exists |
| [Design decisions](specs/001-metering-billing-core/design-decisions.md) | Every decision, with what was rejected — including the two design reviews |
| [Plan](specs/001-metering-billing-core/plan.md) · [Tasks](specs/001-metering-billing-core/tasks.md) | The scale envelope, the build order, the task list |
| [Configuration](docs/configuration.md) · [Security](docs/security.md) · [Operations](docs/operations.md) · [Testing](docs/testing.md) | Every knob; the threat model; the runbook; how the TDD gates work |

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

## Provenance

Extracted from [aisat-intel](https://github.com/truongpx396/aisat-intel), where this engine was
designed as that product's credit backbone and deliberately factored for reuse.
[PROVENANCE.md](PROVENANCE.md) records what moved, what was generalized, and what was fixed on the way.

## License

MIT — see [LICENSE](LICENSE).
