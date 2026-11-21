<div align="center">

# 💳 Intel Payment

### Metering, Credits and Payments Engine

A reusable credit-metering and payments service: refuse-before-spend on Redis, the books on Postgres, money in from Stripe, Polar and PayPal

[![CI](https://github.com/truongpx396/intel-payment/actions/workflows/ci.yml/badge.svg)](https://github.com/truongpx396/intel-payment/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](#-testing)
[![Stack](https://img.shields.io/badge/Stack-Redis%20%C2%B7%20Postgres%20%C2%B7%20gRPC%20%C2%B7%20REST-1f6feb)](#-architecture)
[![Money](https://img.shields.io/badge/Money-integer%20only%20%C2%B7%20exactly--once-d1242f)](#-safety-model)
[![Providers](https://img.shields.io/badge/Providers-Stripe%20%C2%B7%20Polar%20%C2%B7%20PayPal-8250df)](#-payments)
[![Tests](https://img.shields.io/badge/Tests-test--first%20%C2%B7%20fuzz%20%C2%B7%20mutation-2ea44f)](#-testing)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

</div>

**A hot-path admission gate, exact pricing, exactly-once accounting and a single durable writer**, behind gRPC and REST,
with a webhook door that persists a payment provider's delivery before it acknowledges it. Drop it in front of any
product that needs to count usage, enforce budgets and take money, as a Go library or as a self-contained container.

The product-specific part is three parameters: a **rate card** (data), a **scope** with its **subjects** (your request
context) and a **realm** (which product this is). Everything else is generic, and the conformance suites run unchanged
across rate cards over three disjoint unit spaces, so that is a test result rather than a claim.

> **Scope:** a *metering, credits and payment-collection engine* for products that need an internal credit unit with
> refuse-before-spend enforcement on the request path. If you do not need that, [ROADMAP.md](ROADMAP.md) names the
> tools you probably want instead.

* * *

## 📑 Table of Contents

- [🎯 Highlights](#-highlights)
- [🧭 Architecture](#-architecture)
- [🌍 Deployment shapes](#-deployment-shapes)
- [🚀 Quickstart](#-quickstart)
- [💬 Things to try](#-things-to-try)
- [🌐 API](#-api)
- [📐 Configuration](#-configuration)
- [💳 Payments](#-payments)
- [🔒 Safety model](#-safety-model)
- [📊 Observability](#-observability)
- [✅ Testing](#-testing)
- [🔍 Security scanning and load testing](#-security-scanning-and-load-testing)
- [🔧 Make targets](#-make-targets)
- [📁 Repo map](#-repo-map)
- [🚧 Roadmap and known gaps](#-roadmap-and-known-gaps)
- [🩺 Troubleshooting](#-troubleshooting)
- [📚 Further reading](#-further-reading)
- [📄 License](#-license)

* * *

## 🎯 Highlights

| Layer | Tool |
|---|---|
| 🧠 Engine | **Go 1.26**: `metering/` (`domain` → `ports` → `app` → `adapters`), standalone and portable; `billing/` on metering's ports only |
| ⚡ Hot path | **Redis 7**, single or Cluster: one Redis Function per operation, every key in the scope's shard slot; the outbox is a Stream in the same slot |
| 📒 Books | **PostgreSQL 16**: the ledger, the webhook inbox, configuration and its audit log; forward-only migrations |
| 🔌 Transports | **gRPC** and **REST** (OpenAPI) behind a service credential, bearer or mTLS; a Go client, and TypeScript and Python clients generated from the OpenAPI document |
| 🚌 Bus | **Redis Streams** by default; **NATS JetStream** optional, for downstream replication |
| 💳 Payments | **Stripe**, **Polar** and **PayPal** behind one provider port; signed webhooks into a Postgres inbox |
| 📣 Events | Signed, retried outbound webhooks to hosts, and a cursor feed (`GET /v1/events`) |
| 📈 Observability | **OpenTelemetry** metrics in the Prometheus format on a listener of their own, and a Prometheus alert catalogue ([`deploy/alerts.yml`](deploy/alerts.yml)) held equal to the runbook by a test |
| 🧪 Quality | Testcontainers, Playwright, `gremlins` mutation testing, fuzz targets, `go-arch-lint` + `depguard`, `govulncheck`, `gitleaks` |

**Built and verified**

- ⏱️ **No durable-store wait on the hot path.** Pool draws, the sequence number, the idempotency guard, window counters and the outbox `XADD` happen in one Redis Function.
- 🧮 **Exact pricing.** Prices are rationals (`$0.15 per million` is exact), rounding is up **once per event**, and no float touches a balance, a price or a ledger row.
- 1️⃣ **Exactly-once accounting.** A permanent guard for anything that mints money, a window-bounded one for usage, each with a request fingerprint: a reused key with a different request is an error, never a silent no-op.
- 🚫 **Refuse before you spend.** The gate evaluates every ceiling *and* the call's cost bound, so overshoot is bounded rather than hoped for.
- 🎚️ **Ceilings that fit real products.** Per scope or per **subject** (member, API key, project), in calendar or rolling windows, plus a `Job` budget that stops one runaway loop independently of every calendar limit.
- 🏊 **Credit pools.** Promotional credits are spent before paid ones; credits can be restricted to some resources; optional expiry spends the soonest-expiring lot first.
- 🔁 **Transfers.** Credits move between scopes in two phases, refusing to overdraw, with the realm's total conserved at every step.
- ⚖️ **Books that reconcile honestly.** Reconcile compares the fast copy with the books *at the same sequence number*, so in-flight charges are never mistaken for drift, and real drift pages.
- 🧯 **Recovery that is a procedure.** A Redis restart, failover or loss freezes the affected shards, drains them and rebuilds them from the books; usage during an outage is journaled, never dropped.
- 💳 **Payments that cannot lose or double a grant.** Webhooks are verified on the raw body and persisted *before* they are acknowledged, then processed with leases and retries.
- 📝 **Configuration you can trust.** An admin API for rate cards, ceilings, pools, plans, provider prices, provider accounts and customers; the database itself audits every change, even a direct SQL write.
- 🎫 **Entitlements as data.** Flags, quotas and enums resolved with `override > subscription > default`, a grace window that lapses at the nanosecond, and provenance for every grant; `-1` is unlimited and is never compared as a number. A refusal says why in words and names the plan that would permit it. Served over REST and gRPC as a batch `Check`, cached with a bound and evicted the moment a subscription, an override or the configuration changes; the same resolution sizes any ceiling a plan sizes (`max_entitlement`).
- 📦 **Library or container.** The same ports embedded in-process or behind gRPC and REST; a `Meter` behind gRPC is shown to be the same value as the engine, by a suite that is itself shown to reject five broken transports.
- 📣 **Outbound events, so hosts never touch internals.** `payment-worker` records what the engine announces, sends it to each subscribed endpoint as a signed, retried webhook behind an SSRF guard, and purges it past retention; `paymentd` serves the same events as a cursor feed. Only the types in [outbound-events.md](specs/001-metering-billing-core/contracts/outbound-events.md#what-is-built) are emitted, and an endpoint is a SQL row for now (no admin API).
- 🛠️ **The operator's requests and the scheduled work.** Reconcile, shard recovery, rehydrate, dead-letter replay, the subscription drift sweep, unblock and adjustment are recorded by `paymentd` (the person, a reason, an `Idempotency-Key`; it answers `202`) and done by `payment-worker`, which also serves fourteen idempotent scheduled ticks and parks every message the bus gave up on in `dead_letters`, paging once.
- 🖼️ **A credits UI** ([`ui/`](ui/credits-ui/README.md)): balances per pool, ceilings (however many), spend, a ledger and plans over an opaque unit — a framework-free model, React components, and a reference binding. It never says a payment succeeded because the browser came back from checkout, refuses a float where an integer is meant, and is shown to pack, install into an unrelated app and build. Tested in jsdom and in Chromium over a live server, with an accessibility audit; not on a registry.
- 🧱 **A boundary you cannot cross by accident.** `go-arch-lint` and `depguard` make the portability guarantee a CI gate, and `check-spec-drift` does the same for the contracts.

**Designed, not built**

- 🧾 **Post-paid invoicing**: rating, tiered schedules, commitments, immutable invoices and credit notes ([Phase 2](specs/002-postpaid-invoicing/)).

* * *

## 🧭 Architecture

Every operation that changes a balance is applied to Redis first, in one Function call that also appends an *intent* to
an outbox stream in the same slot. `payment-worker` is the **only** process that writes the books: it drains the outbox
and books each intent in Postgres, and reconcile compares the two at the same sequence number. A payment is the same
machine with a door in front: the provider's webhook is persisted to a Postgres inbox, and only then acknowledged and
processed, and a purchase mints credits by granting them through the engine.

```mermaid
flowchart TB
    subgraph host_sg["Host product (any language, or a Go host embedding metering/)"]
        Host["Admit before the work,<br/>Record what was used"]
        Buyer["Plans, checkout, subscription,<br/>portal, auto top-up"]
        Operator["Operator: rate cards, ceilings, pools,<br/>plans, provider accounts, customers"]
    end

    subgraph providers_sg["Payment providers"]
        Stripe["Stripe"]
        Polar["Polar"]
        PayPal["PayPal"]
    end

    subgraph paymentd_sg["paymentd (REST :8080, gRPC :9090)"]
        Metering["/v1 metering<br/>admit, record, grant, transfer,<br/>balance, ledger, credits"]
        PayRoutes["/v1 payments<br/>plans, checkout, subscription, portal,<br/>auto-recharge, payments, fulfilment"]
        Admin["/v1/admin<br/>every change audited;<br/>an operator's request is recorded, then done by the worker"]
        Entitlements["/v1 entitlements + entitlement.v1<br/>batch check, resolve"]
        Feed["GET /v1/events<br/>the cursor feed"]
        Door["POST /webhooks/{provider}/{account}<br/>verify the raw body, persist, then 200"]
    end

    subgraph worker_sg["payment-worker (the sole durable writer)"]
        Writer["Writer<br/>books every intent"]
        Reconcile["Reconcile and recovery<br/>compare at the sequence watermark"]
        Relay["Config relay<br/>a configuration change reaches every cache"]
        Processor["Webhook processor<br/>leases, retries, parked-delivery retry"]
        AutoTopUp["Auto top-up<br/>(Stripe only)"]
        Outbound["Outbound events<br/>record, sign, send, retry, purge"]
        Ticks["Scheduled ticks and operator requests<br/>reconcile, recover, replay, unblock, adjust"]
    end

    Redis[("Redis, single or Cluster<br/>hot balances + the outbox STREAM<br/>one Function per op, one shard slot")]
    Postgres[("Postgres<br/>the ledger, webhook inbox,<br/>configuration, audit log")]

    Host -->|"gRPC or REST, bearer or mTLS"| Metering
    Buyer --> PayRoutes
    Operator --> Admin
    Metering -->|"admit, record: one Function call"| Redis
    Redis -->|"outbox stream"| Writer
    Writer --> Postgres
    Reconcile -.->|"heal the hot copy, alarm on drift"| Redis
    Reconcile -.-> Postgres
    Admin --> Postgres

    PayRoutes -->|"create a checkout or portal session"| providers_sg
    providers_sg -->|"signed webhooks"| Door
    Door -->|"persist before acknowledging"| Postgres
    Postgres -->|"inbox"| Processor
    Processor -->|"grant, refund, dispute take-back"| Redis
    AutoTopUp -->|"charge a saved card"| Stripe
    AutoTopUp -->|"credit the pool"| Redis

    UI["credits-ui<br/>(ui/: built; jsdom and browser tested)"]

    subgraph planned_sg["Designed, not built"]
        Invoicing["Post-paid invoicing<br/>(Phase 2)"]
    end

    Host -->|"gRPC or REST: check, resolve"| Entitlements
    Entitlements -->|"cached, evicted by announcements"| Postgres
    Entitlements -->|"QuotaSource: sizes a limit by the plan"| Metering
    Postgres -->|"config relay: LISTEN to the bus"| Relay
    Host -->|"read the feed"| Feed
    Feed --> Postgres
    Outbound -->|"record, then send"| Postgres
    Outbound -.->|"signed, retried webhooks"| host_sg
    Postgres -->|"admin_operations"| Ticks
    UI -.-> Metering

    classDef planned stroke-dasharray: 5 5,color:#888
    class Invoicing planned
```

- 🧱 **Ports and adapters.** `metering/` and `billing/` are each `domain` → `ports` → `app` → `adapters/{driven,driving}`. `billing/` depends on metering's *ports* only; `metering/` never imports `billing`, `entitlement`, `events` or `cmd` (`make verify-portability`). `os.Getenv` is called only in `cmd/`, and there is no `time.Now` in the core.
- 🟥 **Redis is a money store.** `noeviction`, AOF, and an ACL that gives every principal other than `paymentd` and `payment-worker` nothing ([deploy/redis/](deploy/redis/README.md)). It holds the hot balances and the outbox; the books can rebuild it.
- 🗄️ **Postgres is the book of record.** Migrations are forward-only. `payment-worker` holds one connection per shard it owns: at the default 256 shards, about 272 connections per worker (the compose Postgres runs with `max_connections=400`).
- 📮 **The worker drains the Redis outbox.** The JetStream intent path exists in the engine and is refused by the binary; JetStream is an option for downstream replication.
- 🚪 **The webhook door is signature-only**: no JWT, no CORS. A delivery is verified on its raw body, persisted, then acknowledged.
- 🔢 **256 shards by default** (`PAYMENT_SHARDS`). It cannot change after the first start without a reshard.

* * *

## 🌍 Deployment shapes

| | 📚 Embedded library | 🐳 Container service |
|---|---|---|
| **Host** | A Go host importing `metering` | Any language, over gRPC (hot path), REST and the generated clients |
| **`Admit` is** | An in-process Redis call | A network call, so **co-locate it**: it is on every request |
| **Run** | `app.New(metering.Config{…}, app.Deps{…})` | `make up`: Postgres, Redis, `payment-migrate`, `paymentd`, `payment-worker` (no broker) |

- 🏷️ **Multi-product**: one deployment, one realm per product. Plans, cards, ceilings, pools, provider accounts, credentials and idempotency keys are all realm-partitioned.
- 🌐 **Region**: one home region, with Postgres replicated to a DR region and Redis rebuilt from the books. Active-active is out of scope by decision ([D45](specs/001-metering-billing-core/design-decisions.md)).
- 📦 **What ships for production**: a distroless, non-root image, the Redis ACL, the Prometheus alert rules ([`deploy/alerts.yml`](deploy/alerts.yml)), and `make verify-deploy`, which builds the image, migrates an empty Postgres and proves the ACL by attempting what it forbids. **No production compose file, orchestrator manifests or infrastructure-as-code ship here**, nor do dashboards or the routing of an alert to a person; [docs/production.md](docs/production.md) lists what a production manifest must say, and [the runbook](docs/operations.md) was exercised against injected faults, not in an incident.
- 🔀 **Rollout order**: `make migrate` → `payment-worker` → `paymentd`. `/readyz` fails while the schema is behind head, so wire it as the rollout gate.

* * *

## 🚀 Quickstart

**Prerequisites:** 🐳 Docker with Compose v2 · free host ports **8080** (REST) and **9090** (gRPC) · `openssl`, `shasum` and `curl` for the steps below. 🐹 Go 1.26+ only to run the tests; 🟢 Node ≥ 22 and 🐍 Python ≥ 3.11 only for `make verify-clients`.

**1. Configure.** A caller authenticates with a bearer token; the configuration holds only its SHA-256 digest, never the
token, and an empty `PAYMENT_SERVICE_TOKENS` accepts no caller. One token per privilege, because a spend producer must
never hold the credential that mints credits:

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

**3. See what sizes the per-member ceiling.** There is nothing to configure: the seed sizes `user_daily` from the plan's
entitlement `daily_user_credits` (10 000 000 on the free tier, 50 000 000 on `pro_monthly`), and `paymentd` resolves it
through the same entitlements it serves. To see the picture the server enforces for a scope:

```bash
curl -s "localhost:8080/v1/entitlements?scope_kind=organization&scope_id=org_1" -H "Authorization: Bearer $PAYMENT_TOKEN"
# → {"data":[{"key":"daily_user_credits","kind":"quota","value":10000000,"source":"default","reason":""}, …]}
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

**5. Take a payment.** Register a provider account and map a plan to the provider's price through `/v1/admin`, point the
provider's webhook at `/webhooks/{provider}/{account}`, and start a checkout with `POST /v1/checkout`. Credits appear
when the verified webhook is *processed*, not on the redirect. [docs/sandbox.md](docs/sandbox.md) runs this against each
provider, and the [quickstart](specs/001-metering-billing-core/quickstart.md) has the long walkthrough.

**Without the stack**, the design verifies itself:

```bash
make test                # unit tests and every conformance suite
make verify-hot-path     # the reference Redis Functions on Redis 7 in cluster mode (Docker)
make verify-schema       # migrations, seed and behavioural assertions on an empty Postgres (Docker)
```

### 🚨 Three things that bite on a first run

- 🔑 **An empty `PAYMENT_SERVICE_TOKENS` accepts no caller.** Every call is a `401 unauthenticated` until step 1 is done.
- 🕰️ **`occurred_at` must be recent.** An event older than the usage window is refused as `422 stale_event`, so a copy-pasted fixed date stops working within three days.
- ✍️ **Admin writes need `actor` and `reason`** in the body; without them the call is `400 invalid_request`.

<details>
<summary>🔌 Ports the stack publishes</summary>

| Service | Host port | Move it with |
|---|---|---|
| `paymentd` REST | 8080 | fixed in `deploy/docker-compose.yml` |
| `paymentd` gRPC | 9090 | fixed in `deploy/docker-compose.yml` |
| Postgres | 5432, **loopback only** | `PAYMENT_POSTGRES_PORT` |
| Redis | 6379, **loopback only** | `PAYMENT_REDIS_PORT` |
| NATS (`make up-jetstream` only) | 4222, **loopback only** | fixed |

The data stores are bound to `127.0.0.1`, never to the network (a security review, [docs/hardening.md](docs/hardening.md), found them published on every interface beside credentials this repository documents). A host process on another machine reaches them through its own network path, not through this stack's published ports.

</details>

* * *

## 💬 Things to try

Every call below was run against a freshly seeded stack. Set up the tokens and the fixed ceiling from the
[Quickstart](#-quickstart) first.

**⚙️ The metering API**

| Try | What it shows |
|---|---|
| `admit` for a scope with no credit | 🚫 HTTP 402 `payment_required`, `allowed:false`, `exceeded.name` = `scope_balance`: refused before any work |
| `grant` twice with one `Idempotency-Key` | 🔁 The second answers `applied:false`, `delta:0` |
| `record` twice with one key, then with different quantities | 1️⃣ The replay applies nothing; the changed request is `409 idempotency_conflict` |
| `record` with an `occurred_at` days old | 🕰️ `422 stale_event`: usage older than the dedup window is refused |
| `transfer` more than the source holds | ⚖️ `409 insufficient_balance`: refused rather than overdrawn |
| `transfer`, then `GET /v1/transfers/{key}` | 🔁 `in_transit`, then `settled`: both sides, never one |
| `GET /v1/ledger` for the scope | 📒 The rows the worker booked, newest first, with a rising `seq` and the rate-card version on the usage row |
| `GET /v1/credits` | 🖼️ Everything a credits screen shows, in one round trip |
| `GET /v1/plans?currency=USD` | 🛒 What the realm sells: slugs, prices, credit allotments, never a provider's price id |

```bash
# Move credits between two scopes (privilege: grant), then follow it
curl -s localhost:8080/v1/transfer -H "Authorization: Bearer $PAYMENT_GRANT_TOKEN" -H 'Idempotency-Key: move-1' \
  -d '{"from":{"kind":"organization","id":"org_1"},"to":{"kind":"organization","id":"org_2"},
       "from_pool":"promo","to_pool":"promo","amount":1000000,"reason":"allocation"}'
# → {"idem_key":"move-1","applied":true,"amount":1000000,"from_balance":3989000,"status":"in_transit"}
curl -s localhost:8080/v1/transfers/move-1 -H "Authorization: Bearer $PAYMENT_GRANT_TOKEN"   # → "status":"settled"

curl -s "localhost:8080/v1/balance?scope_kind=organization&scope_id=org_1" -H "Authorization: Bearer $PAYMENT_TOKEN"
# → {"pools":{"promo":3989000},"total":3989000}
curl -s "localhost:8080/v1/ledger?scope_kind=organization&scope_id=org_1&limit=10" -H "Authorization: Bearer $PAYMENT_TOKEN"
# → {"data":[{…"operation_type":"allocation_out","delta":-1000000,"seq":3},{…"llm.chat","delta":-11000,"seq":2},{…"signup_grant","delta":5000000,"seq":1}],"next":null}
```

**🔐 The credential model**

| Try | What it shows |
|---|---|
| Any call with no `Authorization` header | 🔑 `401 unauthenticated` |
| The `record` token on `POST /v1/grant`, or on `/v1/admin/*` | 🚦 `403 forbidden`: the credential holds `record`, the call needs `grant` or `admin` |
| `GET /readyz` | 🩺 `{"status":"ready","migrations":"head","shards":256}`; it fails while the schema is behind head |
| `GET /metrics` on `PAYMENT_METRICS_ADDR` (`:9092` in the compose stack) | 📈 Prometheus text: `metering_admit_latency_seconds`, `metering_admit_denied_total`, … It has no credential, so it is on a listener of its own that the compose stack deliberately does not publish: `:8080` does not serve it |

**🔬 Beyond the API**

| Try | What it shows |
|---|---|
| `make verify-hot-path` | ⚡ The reference Redis Functions behave on a real Redis 7 in cluster mode |
| `make verify-equivalence` | 🧪 A `Meter` behind gRPC is the same value as the engine, and the suite rejects five broken transports by name |
| `make fuzz FUZZTIME=20s` | 🎲 Arithmetic, encodings and time logic searched beyond their seeds |
| `make mutation-diff REF=origin/main` | 🧬 Whether the tests fail when the lines you changed are wrong |
| `make verify-boundary` | 🧱 One planted violation per boundary rule, and each is rejected |

* * *

## 🌐 API

Every route is under `/v1` and carries a **privilege**: `record` for a spend producer, `grant` for anything that mints or
moves credits or changes a subscription, `admin` for configuration. The normative surface is
[rest-api.md](specs/001-metering-billing-core/contracts/rest-api.md) and
[grpc-surface.md](specs/001-metering-billing-core/contracts/grpc-surface.md); the OpenAPI document is
`api/openapi/v1.yaml`.

| Endpoint | Privilege | Purpose |
|---|---|---|
| `POST /v1/admit` | record | Decide whether an operation may start |
| `POST /v1/record` | record | Record what an operation used, exactly once |
| `POST /v1/grant` | grant | Mint credits into a pool, or claw them back |
| `POST /v1/transfer` · `GET /v1/transfers/{key}` | grant | Move credits between two scopes of one realm · where a transfer is |
| `GET /v1/balance` · `/v1/ledger` · `/v1/credits` | record | A scope's balance · its booked rows, newest first · everything a credits screen shows |
| `GET /v1/plans` | record | What the realm sells, in one currency |
| `GET /v1/entitlements` · `POST /v1/entitlements/check` | record | Every effective grant for a scope, with where it came from · a batch check whose refusal says why (also gRPC `entitlement.v1`) |
| `GET /v1/events` | record | The outbound event feed, a cursor over every realm the credential covers |
| `POST /v1/checkout` · `GET /v1/portal` | record | Start a hosted checkout · the provider's page for cards and invoices |
| `GET /v1/subscription` | record | The scope's subscription, or `null` |
| `POST /v1/subscription/change` · `/cancel` | grant | Ask the provider to move or end the subscription (accepted, never reported as made) |
| `GET /v1/auto-recharge` · `PUT /v1/auto-recharge` | record · grant | The standing instruction to top up · set it |
| `GET /v1/payments` · `GET /v1/fulfilment` | record | Payment history · what the server says about the credits the latest purchase should bring |
| `/v1/admin/rate-cards` | admin | Publish a rate card version; list them |
| `/v1/admin/limits/{name}` · `/v1/admin/pools/{name}` | admin | Create, replace, read or remove a ceiling · a pool |
| `/v1/admin/plans` · `/{code}` · `/{code}/provider-prices/{account}/{currency}` | admin | Plans, taking them on or off sale, and mapping a price to the provider's |
| `/v1/admin/provider-accounts` · `/v1/admin/customers` | admin | Provider accounts (secrets are references) · linking a scope to a provider customer |
| `POST /v1/admin/reconcile` · `/shards/{shard}/recover` · `/rehydrate` · `/dead-letters/{id}/replay` · `/subdrift` · `/unblock` · `/adjustments` | admin | An operator's request. `paymentd` has no writer, so it records the request and answers `202`; `payment-worker` does it. Each needs an `Idempotency-Key`, an `actor` and a `reason` |
| `GET /v1/admin/operations/{id}` · `GET /v1/admin/drift` | admin | How a request went · the recent reconcile runs and the asking realm's own findings |
| `POST /webhooks/{provider}/{account}` | signature | The provider's delivery. No credential, no CORS: the signature is the credential |
| `GET /healthz` · `/readyz` · `/metrics` | none | Up · may be sent traffic · Prometheus exposition (`/metrics` is on `PAYMENT_METRICS_ADDR`, which `paymentd` requires unless the API listener is loopback or a unix socket) |

Admin writes take `actor` and `reason` in the body; every change is audited.

| `PAYMENT_SERVICE_AUTH` | A caller is | Use it for |
|---|---|---|
| `bearer` (what `.env.example` selects) | `<caller-id>:<sha256 of the token>:<realms>:<privileges>`, comma-separated, in `PAYMENT_SERVICE_TOKENS` | Most deployments. The configuration holds the digest, never the token; a plaintext token is refused at start-up |
| `mtls` | `<caller-id>:<certificate common name>:<realms>:<privileges>` in `PAYMENT_SERVICE_CERTS` | Callers that hold certificates from a private CA (`PAYMENT_TLS_*`) |
| `none` | Anyone | Loopback or a Unix socket only; the service refuses to start otherwise |

Realms are joined with `+`, privileges with `+`. No browser should hold a `/v1` credential; the host proxies. See
[docs/security.md](docs/security.md).

* * *

## 📐 Configuration

**Infrastructure and policy** are environment variables; **anything commercially volatile** is Postgres rows behind an
admin API, so launching a price, a ceiling, a pool or a plan needs no deploy; **secrets** are named, never stored.

| Kind | Lives in | Example |
|---|---|---|
| Infrastructure and policy | `PAYMENT_*` environment variables (`.env.example` is the reference) | Redis and Postgres URLs, `PAYMENT_SHARDS`, the policies below |
| Commercial configuration | Postgres rows behind `/v1/admin`, audited | Rate cards, ceilings, pools, plans, provider prices, customers |
| Secrets | The environment or a secret manager, referenced by name | A provider account holds `env:STRIPE_SECRET_KEY`, not the key |

| Policy | Options | Default | Decides |
|---|---|---|---|
| `Settlement` | `outbox` · `journal` | `outbox` | Sub-ms enforcement with a stated under-bill window, or Postgres first and none |
| `HotAckWait` | `none` · `aof_local` · `aof_replica` | `none` | How much of that window to close by waiting for Redis durability |
| `LedgerGranularity` | `event` · `rollup` | `event` | One ledger row per event, or per batch with per-event detail archived: the path past one Postgres's write budget |
| `AdmitFailPolicy` | `fail_closed` · `fail_open` | `fail_closed` | Hot store down: refuse, or serve and meter on recovery |
| `NegativeBalancePolicy` | `allow_debt` · `clamp_to_zero` · `block_and_flag` | `clamp_to_zero` | What a refund past consumed credits does |
| `CreditExpiry` | `off` · `lots_fefo` | `off` | Whether grants expire; the lot that lapses soonest is spent first |
| `PlanChangePolicy` | `at_period_end` · `immediate_prorate` · `immediate_no_prorate` | `at_period_end` | Mid-period upgrades |
| `UnderpaymentPolicy` | `hold_zero` · `hold` · `grant` | `hold_zero` | A verified payment that settled for less than its plan costs. Credits come from the plan, not the amount: `hold_zero` holds a payment of nothing for an operator and grants a discounted one in full (counted), `hold` holds anything below the price, `grant` holds nothing |
| `TaxStrategy` | `provider_managed` · `none` (`external_hook` when embedded as a library) | `provider_managed` | **Who** computes tax. The binary refuses `external_hook`: it cannot be given your tax service |
| Dunning | grace days · suspend-after | `7` · `14` | How long a failed payment keeps access. Grace is enforced by the entitlements, which lapse at the instant it ends; suspend-after is read by nothing (no suspension stage is built) |

Full reference: [docs/configuration.md](docs/configuration.md). The design's sizing:
[scale envelope](specs/001-metering-billing-core/plan.md#scale-envelope).

* * *

## 💳 Payments

A purchase is only ever believed when the provider's webhook says so. The flow, end to end:

1. The host calls `POST /v1/checkout` naming a **plan** (never a price) and gets a redirect URL. Credits do **not** appear on the redirect.
2. The provider delivers a signed webhook to `POST /webhooks/{provider}/{account}`. `paymentd` verifies it on the raw body, persists it to the inbox, and only then answers `200`. A crash after the `200` can delay a grant, never lose it.
3. `payment-worker` takes the delivery from the inbox with a lease, and processes it with retries.
4. A purchase grants credits through `Meter.Grant`, exactly once. The host polls `GET /v1/fulfilment` and watches `processing` → `granted`.

- ↩️ **Refunds are cumulative** and take back exactly the credits owed. A **chargeback** and a **won dispute** are exact inverses. Out-of-order deliveries are handled exactly.
- 🧮 **Credits come from the plan, never from the amount paid.** A verified payment of *nothing* for a paid plan (a trial invoice, a 100% coupon) is held for an operator rather than granted, retried, and pages when its retries run out; a discounted one is granted in full and counted. `PAYMENT_UNDERPAYMENT_POLICY` (`hold_zero` by default) changes that.
- ⏳ **A failed payment opens a grace window** (`PAYMENT_DUNNING_GRACE_DAYS`, 7 by default).
- 🔋 **Auto top-up** charges the saved card when a pool falls under its threshold, at most `max_per_day` times a day. It works on **Stripe only**: Polar and PayPal cannot charge a saved method.
- 🅿️ **PayPal** verifies each delivery with an API call to PayPal, and parks the delivery for retry when PayPal cannot answer.
- 🧾 **Tax** is `provider_managed` by default.

| Provider | Rehearsed against | Not yet rehearsed |
|---|---|---|
| **Stripe** | A test-mode account (API 2024-06-20): one-time packs, subscriptions (create, change, cancel), hosted checkout in `payment`, `subscription` and `setup` modes, the billing portal, a failed payment, partial refunds, a dispute lost then won, an auto top-up charging a saved card | Test clocks (dunning and grace over real time), a live-mode account's own settings |
| **Polar** | The sandbox: webhook signature, a free order's `order.created` and `order.paid`, product and checkout creation, the grant | A paid order, refund and renewal |
| **PayPal** | The sandbox and its webhook simulator: OAuth, order creation and its approval link, PayPal's own verify call, the dispute events | A buyer approving an order (the v2 capture, refund and reversal shapes), subscriptions |

The rehearsal found and fixed two defects (an auto top-up that charged the card and granted nothing; a first checkout on a
new Stripe account that failed), and the deliveries it captured are checked in: every adapter is tested against them. The
full record, and how to repeat it for your own account, is [docs/sandbox.md](docs/sandbox.md).

* * *

## 🔒 Safety model

From the [constitution](.specify/memory/constitution.md): thirteen principles, whose non-negotiable ones are **Money
Integrity**, **Exactly-Once by Construction** and **Verification Before Completion**.

- 🔢 **Integer money only.** Amounts are signed `int64`; fiat is integer minor units plus an ISO-4217 currency. Credits and money are distinct types. Arithmetic that could wrap is refused, not wrapped.
- ✍️ **One durable writer.** Request tiers publish intents; the worker books every one, not even a transfer excepted.
- 🚫 **Fail closed, fail loud.** An unknown rate key or unit, or an unreachable store, **denies**: nothing is priced at zero and nothing is granted by default. Drift beyond tolerance alarms; it is never healed silently.
- 🚦 **Refuse before you spend.** The gate runs before the expensive work.
- 🔏 **Webhooks are verified first**: on the raw body, in constant time, inside a timestamp tolerance, before any parse or side effect.
- 🔐 **Least privilege.** A credential is bound to realms and to `record`, `grant` and/or `admin`, so a spend producer cannot mint credits.

| Failure | Defence |
|---|---|
| 🔁 A caller retries a charge | An idempotency key with a request fingerprint: a hot guard for speed, durable unique constraints for correctness. A reused key with a different request is a `409`, never a silent replay |
| 💥 Redis loses acknowledged intents | The **under-bill window is stated exactly**: the last acknowledged intents *and* their balance change are lost together, so the result is under-billing, never double-billing. `HotAckWait` narrows it; `Settlement=journal` removes it |
| 🧮 The fast copy drifts from the books | Reconcile compares only at the same sequence number, heals the fast copy, never books a row, and pages on real drift |
| 🔌 Redis restarts, fails over or is lost | The affected shards freeze, drain and rebuild from the books; usage during the outage is journaled, never dropped |
| 📮 A webhook is replayed, reordered or forged | Verified before anything else, persisted before the `200`, processed with leases; a grant is guarded by a permanent idempotency key |
| 💸 A refund lands after the credits were spent | `NegativeBalancePolicy` decides, and flooring a balance always writes a compensating row |
| 🛠️ A configuration change by direct SQL | Database triggers audit it and signal every cache |

<details>
<summary>📜 The ten load-bearing invariants</summary>

Each one exists because its absence produces a specific money bug. The full list is in
[metering-ports.md](specs/001-metering-billing-core/contracts/metering-ports.md#invariants-every-implementation-must-uphold).

1. **One durable writer.** The worker books every intent; request tiers never write the books, not even for a transfer.
2. **Dual idempotency, fingerprinted.** A hot guard for speed; durable guards for correctness; a reused key with a different request is an error.
3. **Admission gates, never reserves.** Bounded overshoot, and the bound is only real when callers supply a cost bound.
4. **Partial settlement on cancel.** Billed for what was produced.
5. **Exact, replayable pricing, with the card version on the row.** Rational prices, rounded up once per event.
6. **Integer money only.** No float touches a balance, a price or a ledger row.
7. **The under-bill window is stated exactly.** A Redis crash can lose the last acknowledged intents *and* their balance change together, invisibly: under-billing, never double-billing. `HotAckWait` narrows it; `journal` removes it.
8. **Hot equals booked plus suspense, at every sequence number.** Reconcile compares only there, heals the fast copy, and never books a row.
9. **Verify webhooks first; persist before acknowledging.** A crash after `200` can delay a grant, never lose it.
10. **A negative balance is a policy, not an accident.** And flooring one always writes a compensating row.

</details>

* * *

## 📊 Observability

- 📈 **Metrics:** OpenTelemetry instruments exposed at `/metrics` in the Prometheus format, on a listener of their own (`PAYMENT_METRICS_ADDR`) because the route carries no credential; `payment-worker` serves its own on `PAYMENT_WORKER_ADDR`. Every terminal state emits a **named** metric, and the names are part of the contract (`metering/ports/metrics.go`, `billing/ports/metrics.go`).
- 🩺 **Health:** `/healthz` says the process is up; `/readyz` says it may be sent traffic and fails while the schema is behind head.
- 🪪 **Audit:** the database writes every configuration change to `audit_log`, even a direct SQL change.
- 🚨 **Alerts:** [`deploy/alerts.yml`](deploy/alerts.yml) holds the Prometheus rules, each linking to its runbook entry. A test holds the two to one set and every metric either names to the ones the components export (`make test`); it does not parse the expressions, so run `promtool check rules deploy/alerts.yml` before loading them, and treat each threshold as a default.
- 📓 **Runbook:** [docs/operations.md](docs/operations.md) has one entry per alert, each naming the metric that fires it.

| Watch | Metric | Healthy |
|---|---|---|
| Admission latency | `metering_admit_latency_seconds` p99 | ≤ 5 ms co-located, ≤ 1 ms embedded (targets, not measurements) |
| **Drift findings** | `metering_drift_findings_total` | **Zero.** Steady traffic produces none, by construction |
| Outbox depth and age | `metering_outbox_depth`, `metering_outbox_age_seconds` | Depth low, age in seconds |
| Frozen shards · regressions | `metering_shard_frozen_seconds`, `metering_hot_regression_total` | Zero outside a recovery; each regression is a Redis restart or failover |
| Fail-open | `metering_admit_failopen_total` | **Zero** on a `fail_closed` deployment |
| **Webhook verification failures** | `billing_webhook_verify_failed_total` | Zero. A security signal |
| Webhook inbox age | `billing_webhook_inbox_age_seconds` | Seconds |

⚠️ **No dashboards ship in this repository, and nothing routes an alert to a person** (the Alertmanager receivers and the on-call schedule are the deployment's). The runbook was exercised in part against injected faults, not in an incident ([docs/hardening.md](docs/hardening.md)).
The table is what to chart; [docs/operations.md](docs/operations.md) says what to page on.

* * *

## ✅ Testing

Test-first is the rule, and the evidence is checked: a production change arrives as a failing tests-only
`test(red)` commit, then its fix, and CI verifies that order. [docs/testing.md](docs/testing.md) has the loop, the commit
shape, properties and fuzzing, and when mutation testing applies.

| Tier | Command | Needs | In CI | Proves |
|---|---|---|---|---|
| 🧪 Unit and conformance | `make test` | nothing | `go` | The domain, `app`, the pricers, every conformance suite (`PricerContract`, `LedgerContract`, `LedgerWriterContract`, `ConsistencyContract`, `BusContract`), and the check that the alert rules and the runbook are one set, with `-race -shuffle=on` |
| 🐳 Integration | `make test-integration` | Docker | `go` | A real Redis and Postgres (Testcontainers): the writer, recovery, migrations, the transports and the payment routes |
| 🔁 Equivalence | `make verify-equivalence` | Docker | `go` | A `Meter` behind gRPC is the same value as the engine, and the suite rejects five broken transports |
| 🎲 Fuzz | `make fuzz` | nothing | `fuzz` (short) and nightly (deep) | Arithmetic, encodings and time logic |
| 🧬 Mutation | `make mutation-diff REF=origin/main` | `gremlins` | `mutation` and nightly (whole targets) | The tests fail when the changed lines are wrong |
| 🔴 Red-green | `make verify-red-green` | a branch | `red-green` | A failing tests-only commit precedes every production change |
| 🧱 Boundary | `make lint` · `make verify-portability` · `make verify-boundary` | `golangci-lint`, `go-arch-lint` | `go` | The architecture graph holds, `metering/` stands alone, and the gates reject planted violations |
| 🗄️ Schema | `make verify-schema` | Docker | `migrations` | Every migration, the seed and behavioural assertions on an empty Postgres, plus the Phase 2 draft |
| ⚡ Hot path | `make verify-hot-path` | Docker | `hot-path` | The reference Redis Functions on Redis in cluster mode |
| 🚢 Deploy | `make verify-deploy` | Docker | `deploy` | The image builds, an empty Postgres migrates, and the Redis ACL refuses what it should |
| 📏 Hardening | `make hardening` | Docker, an otherwise idle machine | none (not in `make ci` either) | Load, chaos, security and recovery **measurements**, not gates; what they showed and did not show is [docs/hardening.md](docs/hardening.md) |
| 🌐 End to end | `make e2e` | `make up`, Node (the `ui` project also needs a service token and `make e2e-seed`, [e2e/README.md](e2e/README.md)) | `e2e` | Playwright against the built image: health and credential checks (the `api` project, 10 tests), and the credits screen in Chromium over a live `paymentd` with an axe-core audit (the `ui` project, 21 tests) |
| 🧰 Clients | `make verify-clients` | Node ≥ 22, Python ≥ 3.11, network | `clients` | The TypeScript and Python clients generated from OpenAPI compile and round-trip an `Admit` |
| 🖼️ Credits UI | `make ui-ci` | Node ≥ 22.12, network | `ui` | Lint and the boundary guards (each shown to reject a planted violation), types, 211 unit / property / render tests, and the package packed, installed into an unrelated app and built |
| 📜 Documents | `scripts/check-spec-drift.sh` | nothing | `drift`, `docs` | No document describes a retired mechanism as current, ROADMAP's task counts match `tasks.md`, and no relative link dangles |

`make ci` runs the Go side: build, `proto-check`, tests, fuzz, integration, lint, portability, boundary, the TDD-gate checks,
equivalence, `vuln`, hot path and deploy. It does **not** run `verify-schema`, `verify-clients`, `make ui-ci`, the e2e suite,
`scripts/check-spec-drift.sh`, `make secrets`, `make hardening` or the two gates that need a branch history
(`verify-red-green`, `mutation-diff`): run the ones your change touches before opening a pull request.
`internal/chaos` is a fault-injecting proxy the integration tests use; `internal/pgtest` is their Postgres fixture.

**Contributing.** Go 1.26+. Work happens on a branch and arrives as a pull request. The rules a change to the money path
answers to are the [constitution](.specify/memory/constitution.md); [CLAUDE.md](CLAUDE.md) is the working guide, and the
[pull-request template](.github/pull_request_template.md) says what a PR must show.

* * *

## 🔍 Security scanning and load testing

| Tool | Checks | Run | CI |
|---|---|---|---|
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | Known vulnerabilities in the dependencies and the standard library | `make vuln` | Every change, and nightly with a fresh database |
| [gitleaks](https://github.com/gitleaks/gitleaks) | Secrets in the tree and the history | `make secrets` | Gate |
| `depguard` · `go-arch-lint` | No provider SDK outside its adapter, no `os.Getenv` outside `cmd/`, no float for money, metering imports nothing above it | `make lint` | Gate |
| `buf` | The protobuf definitions lint, the generated Go is current, and nothing released breaks | `make proto-check` | Gate |

- ⚠️ **Done in part, on a laptop** ([docs/hardening.md](docs/hardening.md)): what a repository can execute of the [docs/security.md](docs/security.md) checklist was executed against the real binaries and a real Redis, and every attack tried was refused; the hot path was load tested once, and its 99th percentile **at the target load** was not shown (the [scale envelope](specs/001-metering-billing-core/plan.md#scale-envelope) states the targets). **Not done:** every checklist item a deployment owns (a secret manager, Postgres TLS, a restored backup, network policy), Redis Cluster, and a production network. [docs/production.md](docs/production.md) lists them.
- 🔑 A real `.env` is never committed; `.env.example` holds names, never values.

* * *

## 🔧 Make targets

`make help` lists everything. The ones you will use:

| Target | Does |
|---|---|
| **▶️ Run** | |
| `make up` · `make down` | Start the stack (Postgres, Redis, migrations, `paymentd`, `payment-worker`) · stop it, keeping data |
| `make up-jetstream` | The same with NATS JetStream as the bus instead of Redis Streams |
| `make migrate` · `make seed REALM=…` | Run migrations to head · seed a realm: pools, a rate card, ceilings, plans, a free tier |
| `make logs` · `make build` | Tail `paymentd` and `payment-worker` · build every binary |
| **✅ Test** | |
| `make test` · `make test-integration` | Unit and conformance (parallel, shuffled, race detector) · real Redis and Postgres (Docker) |
| `make fuzz` · `make mutation` · `make mutation-diff` | Search the fuzz targets · mutation-test every target · mutation-test only the lines changed since `REF` |
| `make verify-red-green` · `make verify-tdd-gates` | The branch was test-driven · the TDD gates refuse what they should |
| `make hardening` | Load, chaos, security and recovery measurements (Docker; minutes; on an idle machine; not in `make ci`) |
| **🔎 Verify** | |
| `make verify-schema` · `make verify-hot-path` · `make verify-deploy` | Migrations and seed · the Redis Functions on a cluster · the image and the Redis ACL |
| `make verify-clients` · `make verify-equivalence` | The generated clients · the transport-equivalence suite |
| `make ui-ci` (`ui-install` · `ui-lint` · `ui-test` · `verify-ui-boundary` · `verify-ui-portability`; `ui-build` builds the package alone) | The credits UI: install · lint and guards · types and tests · each guard rejects a plant · pack, install into an unrelated app, build |
| `make verify-portability` · `make verify-boundary` | `metering/` stands alone · the boundary gates reject violations |
| **🧹 Quality** | |
| `make fmt` · `make lint` · `make arch-lint` | Format · `golangci-lint` and the architecture graph · the architecture graph alone |
| `make check-plan-branches` | No `if plan == "pro"` in production Go: entitlements are data |
| `make vuln` · `make secrets` | `govulncheck` · `gitleaks` |
| **🔌 API** | |
| `make proto-tools` · `make proto` · `make proto-check` | Install the pinned generators · regenerate `api/meteringv1` · the generated code is current and breaks nothing |
| **🌐 End to end** | |
| `make e2e-install` · `make e2e-seed` · `make e2e` | Install the e2e dependencies · give the running stack's realm the rate card and ceilings the browser tests need · run Playwright against `E2E_BASE_URL` (the local stack by default) |
| **🏁 Everything** | |
| `make ci` | What CI runs on every change (see [Testing](#-testing) for what it leaves out) |

⚠️ `make down-volumes` **deletes all data, the ledger included.** There is deliberately no down-migration target:
migrations are forward-only, and in development `make down-volumes && make up` rebuilds the schema from scratch.

* * *

## 📁 Repo map

```
metering/              the engine, standalone and portable; imports nothing above it
  domain/              money, credits, limits, events, intents, request fingerprints — imports nothing
  ports/               what the engine needs (books, bus, clock, metrics, …) and what it offers (Meter)
  app/                 admit, record, grant, transfer, the writer, reconcile, recover, expiry, journal
  adapters/driven/     redis (hot_path.lua), postgres, redisstreams, natsjetstream, otel, pricing, archive, system
  adapters/driving/    grpcserver, grpcclient, resthandler, wire, auth
  contracts/           the conformance suites every adapter must pass
billing/               the payment layer; depends on metering's ports only
  domain/ ports/ app/  checkout, webhook ingress, the processor, plan change, auto top-up, admin
  adapters/driven/     postgres, stripe, polar, paypal
  adapters/driving/    webhookhttp (the door), paymenthttp (/v1 payments and /v1/admin)
  contracts/           the provider conformance suites and the captured sandbox deliveries
entitlement/           what a plan unlocks; depends on metering's ports only (never the payment layer)
  domain/ ports/ app/  the closed Value union (-1 = unlimited), precedence, the cache, the batch Check, the QuotaSource adapter
  adapters/driven/     postgres
  adapters/driving/    entitlementhttp, entitlementgrpc, and the entitlementwire they share
  contracts/           the Entitler and store conformance suites and the in-memory store
events/                outbound delivery to hosts; depends on metering's ports only
  (root)               the envelope, signature, retry schedule, SSRF guard and ids — pure
  app/                 the recorder, the dispatcher (signed, retried webhooks) and the cursor feed
  adapters/driven/     postgres, httpsend
  adapters/driving/    eventshttp (GET /v1/events)
  contracts/           the store conformance suite and the in-memory store
cmd/                   paymentd, payment-worker, payment-migrate; internal/{entitlements,envcfg,events,payments,wiring}
api/                   proto/ (buf), meteringv1/ and entitlementv1/ (generated Go), openapi/v1.yaml
migrations/ migrate/   0001–0005, forward-only, and the migrator that applies them
specs/                 001-metering-billing-core (spec, plan, tasks, decisions, contracts/) and 002-postpaid-invoicing (draft)
integration/           Testcontainers stack tests: writer, recovery, transports, payment routes
internal/              pgtest (Postgres fixture), chaos (fault-injecting proxy)
e2e/                   Playwright against the built image
scripts/               every verification gate, seed.sql, the drift and task-pair checks
deploy/                docker-compose.yml, the Dockerfile, the Redis ACL, alerts.yml (the Prometheus rules)
docs/                  configuration, integration-guide, operations, production, security, hardening, sandbox, testing
ui/                    credits-ui (the package) and examples/llm-credits (the reference binding); an npm workspace
design-system/         the credits screen design the UI was built from
.specify/memory/       the constitution
.github/workflows/     ci.yml
```

* * *

## 🚧 Roadmap and known gaps

**❌ Not built** (the phases and reasoning are in [ROADMAP.md](ROADMAP.md)):

- **An admin API for entitlements.** Keys, plan entitlements, the free tier and overrides are SQL rows today — each write audited by the database, and an override refused without a reason and an actor, whichever way it is written. `Overrides.Put` exists as a use case (it validates, writes and announces) and nothing serves it yet.
- **An admin API for event endpoints.** Outbound events are built (a signed, retried webhook per subscribed endpoint, and `GET /v1/events`), but an endpoint is an `event_endpoints` row for now, and a dead delivery or a parked message is re-driven by a use case nothing serves yet.
- **A member's own spend.** The credits snapshot serves a burn rate, a spend series and a breakdown from the `usage_daily` rollup to a scope's own view, but a member's view carries none: the rollup is keyed by an actor an ordinary event does not carry, so a member's own days cannot be told from a colleague's. Serving it means keying the rollup by member.
- **The credits UI on a registry, in more than one browser.** It is built (211 jsdom tests, boundary guards, a pack-and-install check) and 21 Playwright tests drive it in Chromium over a live `paymentd`, worker and Postgres, with an axe-core audit that finds nothing in ten states. It is not published; only Chromium has run it, against a stand-in host rather than a product's own session; no screen reader has; the chart has no per-feature toggle (the server does not serve spend per day and per feature); and the spend figures trail the ledger by the rollup's five minutes.
- **Post-paid invoicing** ([Phase 2](specs/002-postpaid-invoicing/)), including coupons, discounts, trials, quantity subscriptions and invoice generation. Phase 1 collects payments; it issues no invoices.
- **A saved payment method captured automatically** from a `setup` checkout. The host passes `payment_method_ref` to `PUT /v1/auto-recharge` itself.
- **The JetStream intent path in the binary.** It exists in the engine; `payment-worker` drains the Redis outbox only.
- **Client libraries on a registry.** The Go client and the generated TypeScript and Python clients are checked to compile and round-trip an `Admit`, and are not published.
- **Revenue recognition and a general-ledger export** ([Phase 3](ROADMAP.md#phase-3--finance-grade)): not designed, and the recommendation is to export to an accounting system instead.
- **Active-active multi-region**: out of scope by decision.

**⚠️ Known gaps:**

- **Every `paymentd` replica needs a bus group of its own, and a retired one must be deleted.** The entitlement cache is evicted by announcements, which each replica must receive; it subscribes as `entitlement-cache-<PAYMENT_INSTANCE_ID>` (the hostname if unset). The name must be stable across restarts, and a bus stream is only trimmed up to what every group has acknowledged, so a group left behind by a replica retired for good holds the stream open until it is deleted ([docs/operations.md](docs/operations.md)).
- **The provider adapters have met their sandboxes once.** Still owed before a real order: PayPal with a buyer approving an order; a paid Polar order, refund and renewal; PayPal subscriptions; Stripe test clocks; and a live-mode account's own settings. PayPal's `PAYMENT.SALE.REFUNDED` is ignored.
- **Auto top-up works on Stripe only**; Polar and PayPal cannot charge a saved method.
- **A provider's `4xx` reaches a host as `500 internal`** rather than `502 provider_error`, and a webhook about a customer not yet linked (`PUT /v1/admin/customers`) is ignored.
- **Load tested, security reviewed and the runbook exercised, each in part, once, on a laptop** ([docs/hardening.md](docs/hardening.md)). The 99th percentile at the target load, Redis Cluster, a production network, the Postgres half of a region failover and every checklist item a deployment owns are not shown; [docs/production.md](docs/production.md) lists what to try before real money.
- **Retention is the operator's.** Nothing detaches or archives a ledger partition, and `PAYMENT_LEDGER_ONLINE_RETENTION` and `PAYMENT_USAGE_ARCHIVE_RETENTION` are documented as **not read**: the ledger (569 bytes a row measured) and the per-event usage archive grow until an operator removes them. The shard-0 audit reports which ledger partitions are covered by checkpoints and so may be detached ([docs/operations.md](docs/operations.md#backups-and-retention)). What *is* bounded: the usage idempotency guard rotates its daily partitions, and `payment-worker` purges processed webhook events and outbound events past their retention.
- **A GDPR erasure story** is designed nowhere yet. The billing tables hold `billing_email` and `actor_id`, and ledger rows must survive an erasure request for audit, so the answer is pseudonymization rather than deletion.
- **Dunning's `suspend-after` is read by nothing.** Entitlements lapse when the grace window ends; no suspension stage after it is built.
- **One Postgres connection per shard** per `payment-worker`, so a worker at 256 shards needs about 272.

* * *

## 🩺 Troubleshooting

| Symptom | Fix |
|---|---|
| Every call is `401 unauthenticated` | `PAYMENT_SERVICE_TOKENS` is empty (it accepts no caller), or the digest in it is not the SHA-256 of the token you send. Redo [Quickstart](#-quickstart) step 1 |
| `403 forbidden`: the credential holds `record`, the call needs `grant` (or `admin`) | Use the token that carries that privilege; there is one per privilege on purpose |
| `503 quota_unavailable` from `/v1/admit` | A ceiling is sized by an entitlement that could not be resolved: the database the entitlements are read from is unreachable (`entitlement_decisions_total{outcome="error"}` rises). It is never treated as unlimited |
| `402 payment_required` from `/v1/admit` | The scope has no credit. Grant some, or complete a purchase |
| `422 stale_event` from `/v1/record` | `occurred_at` is older than the usage window (71h by default). Use the time the work happened, which must be recent |
| `400 invalid_request: actor is required` on an admin write | Admin writes take `actor` and `reason` in the body |
| `409 idempotency_conflict` | Expected: the same `Idempotency-Key` was sent with a different request. A new request needs a new key |
| `paymentd` exits at start-up: `PAYMENT_CHECKOUT_RETURN_HOSTS must be a comma-separated list…` | Your `.env` was copied from an older `.env.example`: Compose reads `KEY=   # comment` as the value `# comment`. Put the comment on its own line (the current `.env.example` does) |
| `paymentd` exits at start-up: `PAYMENT_METRICS_ADDR: is required while PAYMENT_HTTP_ADDR=… can be reached by others` | `/metrics` carries no credential, so it needs a listener of its own. Set `PAYMENT_METRICS_ADDR` (`.env.example` has `:9092`) and keep that port off any public network, or keep the API listener on loopback or a unix socket |
| A verified purchase never reaches `granted` | If it settled for **nothing** (a trial invoice, a 100% coupon), the default `PAYMENT_UNDERPAYMENT_POLICY=hold_zero` holds it for an operator and the `BillingPaymentHeldUnderpaid` alert pages: [the runbook entry](docs/operations.md#alert-a-payment-of-nothing-was-held) |
| `make up` cannot start `paymentd` | Something else holds host port 8080 or 9090; `paymentd`'s ports are fixed in `deploy/docker-compose.yml`. Move Postgres and Redis with `PAYMENT_POSTGRES_PORT` and `PAYMENT_REDIS_PORT` |
| `/readyz` is not ready | The schema is behind head, or the configured shard count disagrees with `hot_config`. Run `make migrate` |
| Changed `PAYMENT_SHARDS` after the first start | It cannot be changed without a reshard ([operations](docs/operations.md#resharding)) |
| Integration tests fail with `unexpected container status created` | A Testcontainers start-up race when many package binaries start at once; rerun before debugging |

* * *

## 📚 Further reading

| Document | What is in it |
|---|---|
| [**ROADMAP.md**](ROADMAP.md) | What exists, what is designed, what is absent, and when to use something else |
| [**Quickstart (long form)**](specs/001-metering-billing-core/quickstart.md) | Seed a realm, meter something, take a payment, verify the guarantees |
| [**Integration guide**](docs/integration-guide.md) | Adopting it in a host, end to end, plus the anti-patterns |
| [**Contracts**](specs/001-metering-billing-core/contracts/) | The normative surface: ports, invariants, conformance suites |
| [Hot-path consistency](specs/001-metering-billing-core/contracts/hot-path-consistency.md) | How Redis and Postgres stay consistent: key layout, watermark, suspense, recovery |
| [Payment providers](specs/001-metering-billing-core/contracts/payment-provider-ports.md) · [Outbound events](specs/001-metering-billing-core/contracts/outbound-events.md) | The provider port every adapter meets · how hosts hear what happened, and which types are emitted so far |
| [Sandbox rehearsal](docs/sandbox.md) | What the provider adapters have met, what they found, and what is still owed |
| [Spec](specs/001-metering-billing-core/spec.md) · [Data model](specs/001-metering-billing-core/data-model.md) | Requirements and success criteria · the schema and why each constraint exists |
| [Design decisions](specs/001-metering-billing-core/design-decisions.md) | Every decision, with what was rejected, including the two design reviews |
| [Plan](specs/001-metering-billing-core/plan.md) · [Tasks](specs/001-metering-billing-core/tasks.md) | The scale envelope, the build order, the task list |
| [Post-paid invoicing (Phase 2)](specs/002-postpaid-invoicing/) | Rating, tiered schedules, commitments, immutable invoices, credit notes |
| [Configuration](docs/configuration.md) · [Production](docs/production.md) · [Security](docs/security.md) · [Operations](docs/operations.md) · [Hardening](docs/hardening.md) · [Testing](docs/testing.md) | Every knob · what a deployment decides and has not yet tried · the threat model · the runbook · what was measured, and what was not · how the TDD gates work |
| [Migrations](migrations/README.md) · [Redis ACL](deploy/redis/README.md) · [End-to-end tests](e2e/README.md) | Why they are forward-only · the money store's access rules · running Playwright |
| [Constitution](.specify/memory/constitution.md) | The thirteen principles every change to the money path answers to |

* * *

## 📄 License

MIT — see [LICENSE](LICENSE).
