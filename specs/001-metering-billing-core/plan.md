# Implementation Plan: Metering, Credits & Payments Core

**Spec**: [spec.md](./spec.md) | **Branch**: `001-metering-billing-core`

## Summary

Build `intel-payment` as a self-contained Go service with a hexagonal core, shipping both as an
importable library and as a container. The design is complete and normative
([contracts/](./contracts/)); the parts that can be verified without Go already are — the schema on
PostgreSQL 16 and the hot path on Redis Cluster, both in CI. This plan is the build order, the scale
the build is aimed at, and the constraints it holds to.

## Technical context

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.26+ | The hot path is a sub-millisecond admission check; the durable writer is a long-lived worker. Both want a compiled language with cheap concurrency, and the ports were designed in Go |
| Hot store | Redis 7+ (single primary or **Cluster**), **`noeviction` + AOF**, Redis Functions | One function per operation, every key in the scope's **shard slot** — account, guard, counters and the outbox stream ([hot-path-consistency.md](./contracts/hot-path-consistency.md)). The eviction policy is correctness, not tuning |
| Durable store | PostgreSQL 15+ | The idempotency guards, the watermarks and the books; partitioned ledger and dedup; database-enforced insert-only prices and configuration audit |
| Bus | **Redis Streams (default)**, NATS JetStream optional, behind a `Bus` port | The outbox *is* a stream in the account's slot, written by the same function that moves the balance. JetStream sits behind it via a relay for downstream replication |
| Transports | gRPC (hot path) + REST (everything else) + signed outbound webhooks | gRPC for typed, deadline-bounded `Admit`; REST for non-Go hosts, configuration and operations; webhooks so hosts never touch internals |
| Client libraries | Go (wraps gRPC, satisfies `ports.Meter`); TypeScript and Python generated from `api/openapi/v1.yaml` and the protos | A non-Go host should not hand-write a client for a money API |
| UI package | TypeScript, framework-free ports + React reference components | `ports.ts`/`model.ts` carry no framework |
| Providers | Stripe, Polar, PayPal adapters, several accounts each | Each SDK confined to its own package by a lint rule |
| Testing | table-driven units, **all `t.Parallel()`** (the `paralleltest`/`tparallel` linters enforce it) and run with `-race -shuffle=on`; **`goleak`** in `TestMain` of every package that starts a goroutine or holds a connection; conformance suites per port; Testcontainers; **Playwright** end-to-end against the running stack ([`e2e/`](../../e2e/README.md)); the design-level verifications (`verify-schema`, `verify-hot-path`, `verify-boundary`, `verify-deploy`, `check-spec-drift`) | The suites are the reuse proof; the verifications keep the design honest; parallel + shuffle make hidden shared state fail early; goleak catches a connection or lock that outlives its caller; e2e proves the deployed pieces work together |
| Boundary | `go-arch-lint` + `depguard` | The portability guarantee is a CI gate, not a convention |

## Scale envelope

"Highly scalable" is a claim until it has a number. These are the **design targets** the
architecture is sized for; T130 measures them, and a miss is a design finding, not a tuning task.

| Tier | Unit of scale | Target | Scales by |
|---|---|---|---|
| Hot path (`Admit`, `Record`) | one Redis primary | ≥ 20 000 mutations/s and ≥ 50 000 admits/s per primary, p99 ≤ 1 ms at the primary | Redis Cluster primaries, up to `Shards` / 4 before shards contend (64 at the default 256) |
| One scope | one shard slot | ≥ 2 000 mutations/s for a single scope | — a scope is never split across slots. A tenant hotter than that is billed at a finer scope (per workspace instead of per organization) |
| Durable writer, `event` granularity | one Postgres primary | ≥ 5 000 events/s sustained, batched (`DrainBatch` 500) | worker replicas up to one per shard; Postgres write capacity |
| Durable writer, `rollup` granularity | one Postgres primary | ≥ 50 000 events/s, per-event detail in a columnar `UsageArchive` | the archive's ingest rate |
| Reconcile | active scopes per interval | cost proportional to scopes booked since the last run, not to history | shards, run in parallel |

**Storage, as rules of thumb** (≈ 400 bytes per event row including indexes — to be measured):

| Sustained events/s | Ledger growth | Recommended configuration |
|---|---|---|
| < 1 000 | < 35 GB/day | `event` granularity; archive ledger partitions after 13 months |
| 1 000 – 5  000 | 35 – 170 GB/day | `event` granularity on a dedicated Postgres; archive after 3 – 6 months |
| > 5 000 | — | `rollup` granularity (ledger rows ∝ scopes × batches) with a columnar `UsageArchive` |

`usage_idem` holds `UsageIdemWindow × rate` rows (72 h at 5 000/s ≈ 1.3 B rows across daily
partitions, dropped as they age) — which is why the window is a configured, bounded number and
minting operations, which are rare, keep the permanent guard.

**Region.** One home region per deployment; DR is Postgres replication plus a cold Redis rebuilt
from the books ([operations.md](../../docs/operations.md#disaster-recovery-region-loss), [D45](./design-decisions.md)).

## Constitution check

| Principle | How this plan satisfies it |
|---|---|
| Money integrity | Signed `int64` credits in a fine unit; exact rational prices rounded once; minor units at the fiat boundary |
| Exactly-once | Two durable guards with fingerprints; a stale event is refused rather than risk a second charge; every intent is booked on an idempotency row |
| One durable writer | The worker, one owner per shard; transfers are two-phase through the hot tier, never a request-tier ledger write |
| Fail closed, fail loud | Pricing, entitlements and plan-sized limits fail closed; drift at equal sequence numbers and every regression page |
| Clean architecture | `domain → ports → app → adapters`, machine-enforced; metering imports neither billing, entitlement nor events |
| Contract-first | Every surface is a contract before code; the Redis half and the schema are *executable* contracts already |
| Configuration over code | Cards, limits, pools, plans, provider accounts and endpoints are data, audited by the database |
| Verification before completion | `verify-schema`, `verify-hot-path` and `check-spec-drift` run in CI; no task is done without its output |

## Project structure

```text
metering/{domain,ports,app,adapters/{driven,driving}}   # the engine
billing/{domain,ports,app,adapters/driven/{stripe,polar,paypal,postgres}}
entitlement/{domain,ports,app,adapters/driven/postgres}
events/{domain,app,adapters}                            # outbound webhooks + feed
api/{meteringv1,billingv1,entitlementv1,eventsv1}       # generated protobuf
api/openapi/v1.yaml                                     # the REST contract, machine-readable
cmd/{paymentd,payment-worker,payment-migrate}
ui/{credits-ui,examples/llm-credits}
migrations/                                             # 0001…0005 — the shipped baseline
specs/001-…/contracts/reference/hot_path.lua            # the reference hot functions
scripts/{verify-schema.sh,verify-hot-path.sh,check-spec-drift.sh,seed.sql}
deploy/{Dockerfile,docker-compose.yml}
docs/{integration-guide,configuration,security,operations}.md
```

## Build order

**Phase 0 — Foundation.** Module, lint config and CI *before* any port code. The migrations and the
design verifications already exist and pass.

**Phase 1 — Metering core.** `domain` → `ports` → conformance suites (`PricerContract`,
`LedgerContract`, `ConsistencyContract`) → `app` → adapters: the Redis adapter loads
`hot_path.lua` unchanged, so the reference that CI verifies is the code that runs. **Done when** the
suites pass against Testcontainers Redis **in cluster mode** and Postgres, including a forced
rollback mid-traffic.

**Phase 2 — Transports.** Proto, gRPC server, the wrapped client, REST handlers including the admin
API, `api/openapi/v1.yaml` with a handler-conformance check, generated TypeScript and Python
clients. **Done when** one integration suite passes identically against the in-process meter and
the client stub (FR-044).

**Phase 3 — Payments.** The webhook inbox (ingress + processor), provider accounts, the Stripe
adapter, then Polar and PayPal against the same suites; disputes, partial refunds, auto top-up.
**Done when** a sandbox purchase, renewal, failure, partial refunds, a dispute won and lost, a replay
and a processor killed mid-event all behave per the contract.

**Phase 4 — Entitlements.** Store, resolver, cache with event-driven invalidation, batch check, and
the `QuotaSource` adapter for plan-sized limits.

**Phase 5 — Operational surface.** Every tick, outbound events, the admin endpoints, the runbook.

**Phase 6 — UI package.** Ports, pure model, components, the reference binding, the portability check.

**Phase 7 — Hardening.** Load test against the scale envelope and SC-001. Chaos: kill Redis, fail it
over to a lagging replica, truncate its AOF, replay webhooks at volume, kill a processor mid-event,
inject drift, park poison intents. Then the security review.

Phases 3 and 6 are independent of each other and of Phase 4; the rest is a chain.

## Risks

| Risk | Mitigation |
|---|---|
| The hot path becomes the bottleneck it exists to prevent | One function per operation; cluster-safe layout verified in CI; the scale envelope load-tested before release |
| One very hot scope saturates its slot | Stated in the envelope; the remedy is a finer billing scope, which is a `Scope` binding change, not a migration |
| Under-billing goes unnoticed | The loss window is stated and configurable (`HotAckWait`, `journal`); every regression pages; deferred and late-replay counters are on the dashboard |
| Reconcile noise trains people to ignore it | Reconcile compares at equal sequence numbers only, so steady traffic produces zero findings and any finding is real |
| A webhook's grant is lost | The inbox: persist before acknowledging, process with leases and retries, page on `dead` |
| Configuration changed behind the API | Audit and cache invalidation are database triggers |
| Rate-card edits in place | Refused by a trigger |
| The contracts drift apart again | `check-spec-drift.sh` in CI; contract and code change in the same commit (constitution VII) |
| Scope creep into ERP accounting | The out-of-scope list is explicit; `TaxStrategy` has no compute mode |
