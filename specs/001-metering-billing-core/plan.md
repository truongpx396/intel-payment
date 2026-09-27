# Implementation Plan: Metering, Credits & Payments Core

**Spec**: [spec.md](./spec.md) | **Branch**: `001-metering-billing-core`

## Summary

Build `intel-payment` as a self-contained Go service with a hexagonal core, shipping both as an
importable library and as a container. The design is complete and normative
([contracts/](./contracts/)); this plan is the build order and the constraints the build holds to.

## Technical context

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.23+ | The hot path is a sub-millisecond admission check; the durable writer is a long-lived single-owner worker. Both want a compiled language with cheap concurrency, and the ports were designed in Go |
| Hot store | Redis 7+, **`noeviction` + AOF** | Atomic `DECRBY` + `XADD` + `SET NX` in one Lua script is the whole fast path. The eviction policy is not a tuning choice: an LRU policy here turns a cache miss into a billing incident — and with a stream outbox, evicting an entry drops a money intent |
| Durable store | PostgreSQL 15+ | `UNIQUE(realm, idem_key)` is the correctness backstop for every guarantee in the system. Partitioned ledger, generated `scope_tag`, `JSONB` for immutable event payloads |
| Bus | **Redis Streams (default)**, NATS JetStream optional, both behind a `Bus` port | Streams give durable entries + consumer groups + `XAUTOCLAIM` redelivery — everything needed — **without a second system**, and the outbox *becomes* the stream, published by the same atomic script that moves the balance. JetStream when quorum replication or cross-region mirroring is required. A third adapter (Kafka/SQS) is one implementation |
| Transports | gRPC (hot path) + REST (everything else) | gRPC for typed, deadline-bounded, connection-reused `Admit`; REST for non-Go hosts, browser snapshots, webhooks and operations |
| UI package | TypeScript, framework-free ports + React reference components | `ports.ts`/`model.ts` carry no framework, so a non-React host reuses the derivation |
| Providers | Stripe, Polar, PayPal adapters | Each SDK confined to its own package by a lint rule |
| Testing | table-driven units; conformance suites per port; Testcontainers for integration | The suites are the reuse proof: the same `PricerContract` runs against three disjoint unit spaces, the same `ProviderContract` against every adapter |
| Boundary | `go-arch-lint` + `depguard` | The portability guarantee is a CI gate, not a convention |

## Constitution check

| Principle | How this plan satisfies it |
|---|---|
| Clean architecture | `domain → ports → app → adapters`, one direction, machine-enforced. `domain` imports nothing |
| Contract-first | Every surface is a contract in [contracts/](./contracts/) before code; the proto and the SQL are generated from those shapes |
| Test-driven | Conformance suites exist per port before adapters; each invariant that is testable has a test named for it |
| Modular & configurable | One `Config` struct per module, no `os.Getenv` in a core; rate cards, limits and plans are data |
| Money integrity | Signed `int64` internally, minor units at the fiat boundary. A float touching money is a lint-level defect |
| Verification before completion | No task is done without the command and its output; `/readyz` gates rollout on schema head |

## Project structure

```text
metering/{domain,ports,app,adapters/{driven,driving}}   # the engine
billing/{domain,ports,app,adapters/driven/{stripe,polar,paypal,postgres}}
entitlement/{domain,ports,app,adapters/driven/postgres}
api/{meteringv1,billingv1,entitlementv1}                # generated protobuf
cmd/{paymentd,payment-worker,payment-migrate}
ui/{credits-ui,examples/llm-credits}
migrations/                                             # 0001…0005
deploy/{Dockerfile,docker-compose.yml}
docs/{integration-guide,configuration,security,operations}.md
```

Full annotated tree: [contracts/metering-ports.md § Code organization](./contracts/metering-ports.md).

## Build order

**Phase 0 — Foundation.** Module, lint config (`.go-arch-lint.yml`, `.golangci.yml`) and CI
*before* any port code, so the boundary can never be crossed even once. Migrations `0001`–`0005`
plus the migration runner.

**Phase 1 — Metering core.** `domain` (realm, scope, credits, money, event, limit, policy) →
`ports` → conformance suites → `app` (admit, record, grant, writer, reconcile, rehydrate) →
Redis/Postgres/NATS adapters → three reference pricers. **Done when** the same `PricerContract`
passes against all three and `LedgerContract` passes against the Redis adapter.

**Phase 2 — Transports.** Proto, gRPC server, gRPC client wrapped to satisfy `ports.Meter`,
REST handlers, `/healthz`, `/readyz`, metrics. **Done when** an integration test passes
identically against the in-process meter and the client stub — that equivalence *is* FR-044.

The `Bus` port gets **two adapters in this phase, not one**: Redis Streams and NATS JetStream, both
passing the same `BusContract`. Shipping one and claiming swappability is the thing this repo's own
conformance-suite discipline exists to prevent.

**Phase 3 — Payments.** `billing` domain and ports, `ProviderContract` and `IngressContract`,
the webhook ingress, the Stripe adapter, then Polar and PayPal against the same suite. **Done
when** a sandbox purchase, renewal, failure, refund and replay all behave per the contract.

**Phase 4 — Entitlements.** Store, resolver with precedence, cache with event-driven
invalidation, batch check endpoint.

**Phase 5 — Operational surface.** Scheduled ticks (reconcile, expiry, dunning, sub-drift,
event purge, DLQ sweep), admin endpoints, the runbook.

**Phase 6 — UI package.** `ports.ts`, pure `model.ts`, components, the LLM reference binding,
the `npm pack` portability check.

**Phase 7 — Hardening.** Load test the admission path against SC-001. Chaos: kill Redis
mid-traffic, replay webhooks at volume, inject drift, park poison messages. Then the security
review against both checklists.

Phases 3 and 6 are independent of each other and of Phase 4; the rest is a chain.

## Risks

| Risk | Mitigation |
|---|---|
| The hot path becomes the bottleneck it exists to prevent | One atomic Lua script per operation; coarse API; co-location required for service mode; SC-001 load-tested before release, not after |
| A provider adapter diverges from the others | `ProviderContract` is the gate; normalized `EventObject` means one dispatcher, not three |
| Under-billing goes unnoticed | Drift metric plus a direction-labelled heal counter — steady upward healing is the signal, and it is on a dashboard rather than in a log |
| The realm axis is treated as optional | It is in `Scope.Tag()` and in every unique index, so omitting it fails a test rather than degrading quietly |
| Rate-card edits in place | `retired_at` plus insert-only discipline, asserted by a test that repricing never alters an existing row's derived price |
| Scope creep into ERP accounting | The out-of-scope list in [spec.md](./spec.md) is explicit, and `TaxStrategy` has no compute mode |
