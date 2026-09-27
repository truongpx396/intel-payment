# Tasks: Metering, Credits & Payments Core

**Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Contracts**: [contracts/](./contracts/)

`[P]` = parallelizable with its siblings. Every task that implements an invariant names it, so a
reviewer can check the test rather than the prose.

---

## Phase 0 — Foundation (the boundary exists before the code it guards)

- [ ] **T001** `go mod init github.com/truongpx396/intel-payment`; Go 1.23+; `Makefile` targets `build test test-integration lint verify-portability up down seed migrate`.
- [ ] **T002** `.go-arch-lint.yml` — the component graph from [metering-ports.md](./contracts/metering-ports.md). **Before** any port code: a boundary added after the fact has already been crossed.
- [ ] **T003** `.golangci.yml` — depguard rules 1–4 (core purity, engine standalone, ports-only access, provider-SDK containment).
- [ ] **T004** `.github/workflows/ci.yml` — build, vet, test, lint, arch-lint, `verify-portability`, `govulncheck`, migration-up-down.
- [ ] **T005** [P] `migrations/0001_metering_core.sql` — `account_credits`, `credit_ledger` + monthly partitions, `credit_outbox_dead`, `spend_journal`, `rate_cards`, `rate_card_entries`, `limits`. `UNIQUE (realm, idem_key)` on the ledger (**FR-003**).
- [ ] **T006** [P] `migrations/0002_payments.sql`, **T007** [P] `0003_entitlements.sql`, **T008** [P] `0004_operations.sql`, **T009** [P] `0005_credit_lots.sql`.
- [ ] **T010** `cmd/payment-migrate` + an embedded-migration check `/readyz` can call (**FR-042**, D15).
- [ ] **T011** `deploy/docker-compose.yml` + `Dockerfile` (distroless, non-root, multi-stage). Redis configured `noeviction` + AOF — **not** a default to be discovered later.

## Phase 1 — Metering core

### Domain
- [ ] **T020** [P] `metering/domain/realm.go`, `scope.go` — `Realm`, `Scope`, `Tag()` with the realm inside it (**FR-014**, D4).
- [ ] **T021** [P] `credits.go`, `money.go` — signed `Credits int64`, `Money{MinorUnits, Currency}` (**FR-012**).
- [ ] **T022** [P] `event.go` — `Unit`, `Quantity`, `Event`, `Price` **with `RateCardVersion`** (**FR-002**, D6).
- [ ] **T023** [P] `limit.go` — `Window` (incl. **`Job`**), `Limit`, `AdmitRequest` **with `MaxCost`**, `Admission` **with `Headroom`** (**FR-005**, **FR-017a**, D5, D22).
- [ ] **T024** [P] `receipt.go` — `Charge` (with its optional `Job` scope), `Grant`, `Transfer`, `Receipt`, `TransferReceipt` (**FR-017b**, D23).
- [ ] **T025** [P] `policy.go` — `SettlementDurability`, `NegativeBalancePolicy`, `CreditExpiry`, `AdmitFailPolicy`, `PlanChangePolicy`, `TaxMode` (D7, D2, D11, D9).
- [ ] **T026** `metering/config.go` — `Config` + `withDefaults` + `Validate`. No `os.Getenv` in any core package.

### Ports & conformance suites (before adapters — the suites are the specification)
- [ ] **T030** `metering/ports/driven.go` — `Pricer`, `BalanceStore`, `LedgerStore`, `LimitStore`, `RateCardStore`, `Bus`, `Clock`, `IDSource`, `Metrics`.
- [ ] **T031** `metering/ports/driving.go` — `Meter`, `LedgerWriter`.
- [ ] **T032** `PricerContract` — determinism, attributes-never-price, empty-is-free, fail-closed on unknown key/unit, round-up, monotonicity, **rate-card version reported**.
- [ ] **T033** `LedgerContract` — debit idempotent, admit gates without reserving, exhausted refusal carries the code, grant idempotent, **`MaxCost` bounds overshoot**, **idem keys do not collide across realms**, **`clamp_to_zero` writes a `writeoff` row**, **a job budget halts without touching a calendar ceiling**, **a transfer applies both sides or neither and refuses rather than overdrawing** (D5, D4, D7, D22, D23).
- [ ] **T034** `LedgerWriterContract` — drain exactly-once under redelivery, reconcile heals *and* alarms past tolerance, rehydrate rebuilds under a per-scope lock.

### App
- [ ] **T040** `app/admit.go` — resolve realm limits from `LimitStore`, merge caller limits, evaluate windows, apply `MaxCost`, compute `Headroom`, emit warn/blocked, honour `AdmitFailPolicy` (**FR-004…006**).
- [ ] **T041** `app/record.go` — price via `Pricer`, then **one atomic step** writing the usage record and enqueueing the debit (**FR-008**, invariant 8).
- [ ] **T042** `app/grant.go` — signed grants, idempotent, `NegativeBalancePolicy` **with the compensating `writeoff` row** (**FR-015**, invariant 12).
- [ ] **T042a** `app/transfer.go` — atomic pool→allocation: one Lua script over both scopes' keys, paired `allocation_out`/`allocation_in` under one idem key; refuse on insufficient pool, cross-realm destination, or `MaxDestBalance` breach. Reconcile asserts the pair sums to zero (**FR-017b**, invariant 14, D23).
- [ ] **T042b** `Window: Job` counters — keyed on an ephemeral scope with a TTL; a counter, not a granted balance, so no ledger rows accrue per job (**FR-017a**, D22).
- [ ] **T043** `app/writer.go` — drain; at-least-once + `credit_idem (realm, idem_key)` collapses retries (**FR-007**).
- [ ] **T044** `app/reconcile.go` — expected vs observed, heal row, **alarm past tolerance**, `reconcile_runs` row every run (**FR-009**).
- [ ] **T045** `app/rehydrate.go` — rebuild from ledger under a per-scope lock before serving (**FR-010**).
- [ ] **T046** `app/expiry.go` — FIFO lot consumption + expiry sweep, active only under `lots_fifo` (**FR-016**, D2).
- [ ] **T047** `app/new.go` — `Deps`, validation, return `ports.Meter`.

### Driven adapters
- [ ] **T050** `adapters/driven/redis` — one Lua script per operation: `DECRBY` + `LPUSH outbox:{shard}` + `SET NX applied` all-or-nothing. Window counters. Locks.
- [ ] **T051** `adapters/driven/postgres` — ledger store, limit store, rate-card store (+ in-process cache), reconcile queries, journal mode.
- [ ] **T052** [P] `adapters/driven/nats` — `Bus` over JetStream, subjects per [bus-subjects.md](./contracts/bus-subjects.md).
- [ ] **T053** [P] `adapters/driven/otel` — `Metrics`, every name from the observability contract (**FR-041**).
- [ ] **T054** [P] `pricing/llmtoken` — the reference pricer, pure, fails closed, rounds up, reports its card version.
- [ ] **T055** [P] `pricing/seat`, **T056** [P] `pricing/storagebyte` — **the reuse proof.** Same `PricerContract`, disjoint unit spaces (**SC-004**).
- [ ] **T057** Wire T032 against all three pricers and T033/T034 against the real adapters via Testcontainers.

## Phase 2 — Transports

- [ ] **T060** `api/meteringv1` proto per [grpc-surface.md](./contracts/grpc-surface.md) — realm on `Scope`, `max_cost`, `headroom`, `rate_card_version`.
- [ ] **T061** `adapters/driving/grpcserver`; fixed status mapping (`RESOURCE_EXHAUSTED`, `FAILED_PRECONDITION`, `PERMISSION_DENIED`, `UNAVAILABLE`).
- [ ] **T062** `adapters/driving/grpcclient` — **wrapped to satisfy `ports.Meter`**. This wrapper is what makes library↔service a wiring change (**FR-044**).
- [ ] **T063** `adapters/driving/resthandler` — every `/v1/*` route from [rest-api.md](./contracts/rest-api.md); one error shape; `Idempotency-Key` required on mutations.
- [ ] **T064** `GET /v1/credits` returning the whole `CreditsSnapshot` in one round trip (**FR-036**).
- [ ] **T065** Caller auth middleware — mTLS/bearer, **realm binding**, `Grant` privilege separation (**FR-040**, invariants 11 and 13).
- [ ] **T066** `/healthz`, `/readyz` (**fails behind head**), `/metrics`.
- [ ] **T067** `cmd/paymentd`, **T068** `cmd/payment-worker` (sole durable writer + every tick).
- [ ] **T069** **Equivalence test**: one integration suite run twice — against the in-process meter and against the client stub — passing identically. This *is* FR-044.

## Phase 3 — Payments

- [ ] **T080** `billing/domain` — `Plan`, `Customer`, `Sub` **with `GraceUntil`**, `Payment`, `Event`, **normalized `EventObject`**, `EventType` **including `Ignored`** (D10, D17, D18).
- [ ] **T081** `billing/ports` — `PaymentProvider`, `Catalog`, `TaxStrategy`, `Customers`, `Events`, `Payments`, `Subs`.
- [ ] **T082** `ProviderContract` — tampered / unsigned / stale-signed rejected; good body verified and normalized; unknown type is `Ignored`; **checkout never grants** (**FR-018, FR-019**).
- [ ] **T083** `IngressContract` — duplicate grants once; **8-way concurrent grants once**; metadata cannot redirect the scope; unknown customer ignored; refund appends without mutating; failed payment sets grace (**FR-020…023, FR-025**).
- [ ] **T084** `billing/app/webhook.go` — verify → claim → resolve → dispatch → mark, exactly the flow in the contract. **Every money branch goes through `Meter.Grant`** (**FR-022**).
- [ ] **T085** `billing/app/checkout.go` — catalogue resolve, customer upsert, provider checkout. Returns a URL and grants nothing.
- [ ] **T086** `adapters/driven/stripe` — `Stripe-Signature` HMAC, constant-time, timestamp tolerance; **key grants on payment/invoice id, never the session** (the double-grant trap).
- [ ] **T087** [P] `adapters/driven/polar`, **T088** [P] `adapters/driven/paypal` (remote verification: deadline, retry budget, **park unverifiable — never fail open**, payments invariant 14).
- [ ] **T089** `TaxStrategy` impls: `provider_managed`, `external_hook`, `none` (**FR-028**, D9).
- [ ] **T090** `PlanChangePolicy` in `ChangePlan` (**FR-029**, D11).
- [ ] **T091** Catalogue store — plans, per-currency prices, provider price mapping (D12).
- [ ] **T092** Run T082 against **every** adapter (**SC-004**).

## Phase 4 — Entitlements

- [ ] **T100** `entitlement/domain` — `Key`, `Kind`, `Value` (**`-1` = unlimited, distinct from unset**), `Grant`, `Source`.
- [ ] **T101** `entitlement/ports` + `EntitlerContract` — unknown key denies, store outage denies, override beats subscription, grace keeps / past-grace lapses, refusal is actionable, unlimited handled, **upgrade visible without waiting a TTL**, provenance present, realms do not leak (**FR-031…035**).
- [ ] **T102** `entitlement/app/resolve.go` — precedence `override > subscription > default`; within a level: flag ORs, quota maxes, enum ranks.
- [ ] **T103** Cache + **event-driven invalidation** on `billing.entitlement.<tag>` (**FR-035**, invariant 9).
- [ ] **T104** `PutOverride` — reason and actor required, audit row in the same transaction (**FR-034**).
- [ ] **T105** REST + gRPC endpoints; batch `Check`.

## Phase 5 — Operational surface

- [ ] **T110** Tick handlers: `reconcile`, `expiry`, `dunning`, **`subdrift`** (D16), `events.purge`, `dlq.sweep` — each an idempotent atomic claim so a duplicate tick is a no-op.
- [ ] **T111** DLQ: park at the cap into `dead_letters`, alert, **never re-drive again, never drop** (**FR-043**).
- [ ] **T112** Admin endpoints: force reconcile, rehydrate, drift; every one audited.
- [ ] **T113** `usage_daily` rollup + the member-scoped breakdown (**FR-039**, D20).
- [ ] **T114** [docs/operations.md](../../docs/operations.md) — one runbook entry per alert, each naming the metric that fires it.

## Phase 6 — UI package

- [ ] **T120** [P] `ui/credits-ui/src/ports.ts` — framework-free (D19).
- [ ] **T121** [P] `model.ts` — pure derivation; fill from `used/cap` **never** a precomputed percentage; threshold state from `warnAtPct`.
- [ ] **T122** Components: `BalanceHero` (with "runs out in ~N days"), `LimitMeter` (`role="progressbar"`, `aria-valuetext` in host units), `SpendChart`, `Breakdown`, `LedgerTable` (registry-driven columns), `PlanCatalog` (blocked offers expose no action; non-purchasers see inert controls).
- [ ] **T123** `CreditsPanel` composition root; `BillingSlots`/`BillingAnchor` optional.
- [ ] **T124** [P] `ui/examples/llm-credits` — the reference binding: labels, columns, source.
- [ ] **T125** ESLint `no-restricted-paths` + a scan asserting the string `credit` never appears inside `credits-ui/**`.
- [ ] **T126** Portability check: `npm pack`, install into an unrelated app, build clean (**SC-012**).
- [ ] **T127** Fixture-driven render tests for 1 / 3 / 5 ceilings and a non-credit unit.

## Phase 7 — Hardening

- [ ] **T130** Load test `Admit` against **SC-001** (p99 ≤ 5 ms co-located, ≤ 1 ms embedded). A miss here is a design finding, not a tuning task.
- [ ] **T131** Chaos: kill Redis mid-traffic (fail policy holds, rehydrate restores); inject drift (alarm fires); replay webhooks at volume (one grant); poison an outbox entry (parks, alerts).
- [ ] **T132** Security review against both checklists ([metering](./contracts/metering-ports.md), [payments](./contracts/payment-provider-ports.md)); `gitleaks` + `govulncheck` in CI.
- [ ] **T133** Recovery rehearsal: restore a ledger copy, `Rehydrate`, measure against the RTO (**SC-008**).
- [ ] **T134** Two full adoption walkthroughs from [docs/integration-guide.md](../../docs/integration-guide.md) — one embedded, one containerized, different `Pricer`s — and fix whatever the second one reveals about the first.

---

## Dependencies

```text
T001→T004  ──▶  T005→T011  ──▶  Phase 1 domain (T020…T026)
                                     ▼
                              ports + suites (T030…T034)   ← written BEFORE app
                                     ▼
                              app (T040…T047)  ──▶  adapters (T050…T057)
                                     ▼
                              Phase 2 transports (T060…T069)
                                     ├──▶ Phase 3 payments (T080…T092) ──▶ Phase 4 (T100…T105)
                                     └──▶ Phase 6 UI (T120…T127)   [independent]
                              Phase 5 (T110…T114) after Phase 3
                              Phase 7 last
```

## Definition of done (every task)

1. The behaviour has a test named for the invariant or requirement it implements.
2. `make test lint verify-portability` passes, and the output is pasted in the PR — not summarized.
3. No `os.Getenv` outside `cmd/`; no float near money; no provider SDK outside its adapter.
4. Any contract change lands in `specs/001-metering-billing-core/contracts/` **in the same commit** as the code.
