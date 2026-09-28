# Tasks: Metering, Credits & Payments Core

**Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Contracts**: [contracts/](./contracts/)

`[P]` = parallelizable with its siblings. Every task that implements an invariant names it, so a
reviewer can check the test rather than the prose. `[x]` = done **and verified** — the verification
is named, and it runs in CI.

---

## Phase 0 — Foundation (the boundary exists before the code it guards)

- [ ] **T001** `go mod init github.com/truongpx396/intel-payment`; Go 1.23+.
- [ ] **T002** `.go-arch-lint.yml` — the component graph, including `events`. **Before** any port code.
- [ ] **T003** `.golangci.yml` — depguard rules 1–4 (core purity, engine standalone, ports-only access, provider-SDK containment).
- [ ] **T004** `.github/workflows/ci.yml` — build, vet, test, lint, arch-lint, `verify-portability`, `govulncheck` (the Go steps activate with `go.mod`).
- [x] **T005** `migrations/0001`–`0005` — the baseline schema, including the guards, watermarks, suspense, transfers, pools, the webhook inbox, outbound events and the configuration audit trigger (**FR-003, FR-050**). *Verified: `scripts/verify-schema.sh`, CI job `schema`.*
- [x] **T006** Phase 2 draft schema in `specs/002-…/draft-migrations/`, applied on top of the baseline in CI and never shipped (D38). *Verified: `scripts/verify-schema.sh`.*
- [x] **T007** `specs/…/contracts/reference/hot_path.lua` — the reference Redis Functions (**FR-017f**). *Verified: `scripts/verify-hot-path.sh` on Redis in cluster mode, CI job `hot-path`.*
- [x] **T008** `scripts/check-spec-drift.sh` — no retired mechanism described as current (D46). *Verified: CI job `drift`.*
- [ ] **T010** `cmd/payment-migrate` + an embedded-migration check `/readyz` can call (**FR-042**).
- [ ] **T011** `deploy/docker-compose.yml` + `Dockerfile` (distroless, non-root). Redis `noeviction` + AOF, with an ACL that gives hosts nothing.

## Phase 1 — Metering core

### Domain
- [ ] **T020** [P] `realm.go`, `scope.go`, `subjects.go`, `pool.go` — `Tag()` as identity; `Subjects`; `GeneralPool` (**FR-014, FR-017c, FR-017d**).
- [ ] **T021** [P] `credits.go`, `money.go` — signed `Credits int64`; `Money{MinorUnits, Currency}` (**FR-012**).
- [ ] **T022** [P] `event.go`, `ratecard.go` — `Event` with required `OccurredAt`; `Price` with `RateCardVersion`; `RateCard`/`RateEntry` rationals (**FR-001, FR-002**, D31).
- [ ] **T023** [P] `limit.go` — `Window` incl. `Job`; `Limit` with `Subject`, `Resource`, `TZ`; `AdmitRequest` with `Resource`, `Subjects`, `MaxCost`; `Admission` with `Headroom`, `Blocked` (**FR-004…006, FR-017a, FR-017c**).
- [ ] **T024** [P] `receipt.go` — `Charge`, `Grant` (pool), `Transfer` (pools), `Receipt` (`Seq`, `Deferred`), `TransferReceipt` (`Status`).
- [ ] **T025** [P] `policy.go`, `errors.go` — every policy enum; typed errors (`ErrIdemConflict`, `ErrStaleEvent`, `ErrMissingSubject`, …).
- [ ] **T026** `metering/config.go` — `Config` + `withDefaults` + `Validate`, exactly as in the contract.

### Ports & conformance suites (before adapters — the suites are the specification)
- [ ] **T030** `ports/driven.go` — `Pricer`, `PricerRegistry`, `BalanceStore`, `BookStore`, `LimitStore`, `PoolStore`, `RateCardStore`, `QuotaSource`, `JournalStore`, `UsageArchive`, `Bus`, `IntentStream`, `Clock`, `IDSource`, `Metrics`.
- [ ] **T031** `ports/driving.go` — `Meter` (Admit, Record, Grant, Transfer), `LedgerWriter` (Drain, Reconcile, Rehydrate, Recover, Audit).
- [ ] **T032** `PricerContract` — determinism, attributes/subjects never price, fail-closed, **sub-credit prices exact**, **rounds once per event**, monotonicity, card version, overflow refused (**FR-001**, D31).
- [ ] **T033** `LedgerContract` — idempotent debit; **idempotency conflict**; admit without reserving; `MaxCost` + headroom; **pool draw order**; realm-independent keys; clamp with writeoff and no forgiven debt; job budget via subjects; **missing subject refused**; transfer refuses, then applies both sides once (**FR-003…005, FR-015, FR-017a…d**).
- [ ] **T034** `LedgerWriterContract` — in-order booking; redelivery vs gap vs regression; batching; suspense opened and closed; reconcile at equal seq heals hot only and never books.
- [ ] **T034a** `ConsistencyContract` — [hot-path-consistency.md §9](./contracts/hot-path-consistency.md#9-verification) against Testcontainers Redis **in cluster mode** + Postgres, including a Redis restart from a truncated AOF and a failover to a lagging replica mid-traffic: no double charge, the shard freezes and rebuilds, deferred usage lands (**SC-002, SC-008**).

### App
- [ ] **T040** `app/admit.go` — resolve realm limits for the resource, size `max_entitlement` limits via `QuotaSource` (error → `AdmitFailPolicy`), merge caller limits (tighten only), refuse a missing subject, one `ip_admit` read, evaluate (**FR-004…006, FR-017, FR-017c**).
- [ ] **T041** `app/record.go` — refuse stale `OccurredAt`; price under the active card via the registry; fingerprint; `ip_debit`; on `FROZEN`/`COLD_SHARD`/unavailable/`BACKPRESSURE`, journal and return `Deferred` (**FR-008, FR-017e**).
- [ ] **T042** `app/grant.go` — `ip_grant` per pool with `NegativeBalancePolicy` (**FR-015**).
- [ ] **T042a** `app/transfer.go` — phase 1 via `ip_transfer_out` with the `MaxDestBalance` pre-check (**FR-017b**, D33).
- [ ] **T042b** Window counters — daily/hourly in `Limit.TZ`, rolling as sub-buckets, job TTL; keyed under the charged scope's shard (**FR-017a**).
- [ ] **T043** `app/writer.go` — shard ownership (advisory lock), `XAUTOCLAIM` on takeover, per-scope in-order batches, the watermark cases, the op table, suspense and corrections, transfer phases 2–4, `lw` → `billing.balance.low` (**FR-007**).
- [ ] **T044** `app/reconcile.go` — incremental + full; outcomes; findings per scope and pool; `ip_heal` compare-and-set; never books (**FR-009**).
- [ ] **T045** `app/rehydrate.go` — lazy on `COLD_SCOPE` under a per-scope lock; eager for recently active scopes after a bump (**FR-010**).
- [ ] **T045a** `app/recover.go` — replication-id watch; freeze → drain → bump → unfreeze; resharding (**FR-010**, D40).
- [ ] **T045b** `app/audit.go` — deep audit and checkpoints; partition detach eligibility (**FR-050**).
- [ ] **T046** `app/expiry.go` — FIFO lots per pool; expiry pre-applied at the watermark's remainder, trued up by the writer (**FR-016**).
- [ ] **T047** `app/journal.go` — the replay tick through `ip_debit`; settle on booking (**FR-017e**, D41).
- [ ] **T048** `app/new.go` — `Deps`, validation, return `ports.Meter`.

### Driven adapters
- [ ] **T050** `adapters/driven/redis` — load `reference/hot_path.lua` **unchanged**; the key layout; `HotAckWait` via `WAITAOF`; the shard lifecycle (**FR-017f**, D29).
- [ ] **T051** `adapters/driven/postgres` — books, watermarks, suspense, both guards (window probe under the per-scope lock), transfers, journal, limits, pools, cards (+ cache invalidated by `LISTEN intelpay_config`).
- [ ] **T052** [P] `adapters/driven/redisstreams` — the default `Bus` + `IntentStream` (**FR-046, FR-047**).
- [ ] **T052a** [P] `adapters/driven/natsjetstream` — the optional `Bus`, with the outbox relay.
- [ ] **T052b** `BusContract` against **both** adapters (**SC-014**).
- [ ] **T053** [P] `adapters/driven/otel` — every metric of the observability contract (**FR-041**).
- [ ] **T054** [P] `pricing/table` — the default pricer, exact, rounds once (**FR-001**, D31).
- [ ] **T055** [P] `pricing/llmtoken`, `seat`, `storagebyte` — unit validation, then `table`.
- [ ] **T056** [P] `archive/postgres` — `UsageArchive` over `usage_events` for `rollup` granularity (**FR-050**).
- [ ] **T057** Wire T032 against `table` over three cards and a registered pricer; T033/T034/T034a against the real adapters (**SC-004**).

## Phase 2 — Transports

- [ ] **T060** `api/meteringv1` proto per [metering-ports.md § Service surface](./contracts/metering-ports.md#service-surface-meteringservice-grpc-contract-locked).
- [ ] **T061** `grpcserver` with the fixed status mapping of [grpc-surface.md](./contracts/grpc-surface.md).
- [ ] **T062** `grpcclient` — **wrapped to satisfy `ports.Meter`** (**FR-044**).
- [ ] **T063** `resthandler` — every `/v1/*` route of [rest-api.md](./contracts/rest-api.md); `Idempotency-Key` is the key.
- [ ] **T063a** Admin API — configuration writes with `intelpay.actor`/`intelpay.reason`, card publication validated by a test pricing (**FR-049**).
- [ ] **T063b** `api/openapi/v1.yaml` + a CI check that handlers and document agree; generated TypeScript and Python clients.
- [ ] **T064** `GET /v1/credits` — the whole `CreditsSnapshot`, pools included (**FR-036**).
- [ ] **T065** Caller auth — mTLS/bearer, realm binding, `record`/`grant`/`admin` privileges (**FR-040**).
- [ ] **T066** `/healthz`, `/readyz` (schema head + shard count), `/metrics`.
- [ ] **T067** `cmd/paymentd`, **T068** `cmd/payment-worker`.
- [ ] **T069** **Equivalence test**: one suite, run in-process and through the client stub (**FR-044**).

## Phase 3 — Payments

- [ ] **T080** `billing/domain` — provider accounts, `Sub.LastEventAt`, payments with cumulative refunds and dispute state, normalized `EventObject`, `Ignored`.
- [ ] **T081** `billing/ports` — `PaymentProvider` (incl. `ChargeSaved`, setup mode), `Catalog.PlanForPrice`, `Inbox`, `Payments`, `Subs.ApplyIfNewer`, `TaxStrategy`.
- [ ] **T082** `ProviderContract` — tampered/unsigned/stale/wrong-account rejected; normalization incl. cumulative refunds; `Ignored`; checkout never grants (**FR-018, FR-019**).
- [ ] **T083** `IngressContract` + `ProcessorContract` — persist-before-ack; **crash after ack still grants once**; concurrent processors grant once; metadata cannot redirect; partial refunds exact; won dispute reverses; out-of-order subscription events; grace; cross-realm ignored (**FR-020…026, SC-016, SC-017**).
- [ ] **T084** `billing/app/ingress.go` + `processor.go` — stage 1 and stage 2 exactly as in the contract; the `billing.webhook.tick` sweep and unverifiable-retry.
- [ ] **T085** `billing/app/checkout.go` — payment, subscription and setup modes. Grants nothing.
- [ ] **T086** `adapters/driven/stripe` — HMAC, tolerance, per-account secrets; key grants on payment/invoice id; refunds and dispute events.
- [ ] **T087** [P] `adapters/driven/polar`, **T088** [P] `adapters/driven/paypal` (remote verification, **park unverifiable with the raw body**).
- [ ] **T089** `TaxStrategy` impls (**FR-028**). **T090** `PlanChangePolicy` (**FR-029**). **T091** Catalogue store per account and currency.
- [ ] **T092** Run T082 against **every** adapter (**SC-004**).
- [ ] **T093** Auto top-up — balance watch, `billing.balance.low` handler, bounded idempotent `ChargeSaved`, disable after failures (**FR-030a**).

## Phase 4 — Entitlements

- [ ] **T100** `entitlement/domain` — `Key`, `Kind`, `Value` (`-1` = unlimited), `Grant`, `Source`, `KeyDecl`.
- [ ] **T101** `EntitlerContract` — as before, plus declared keys and enum order from `entitlement_keys`.
- [ ] **T102** Resolver with precedence `override > subscription > default`.
- [ ] **T103** Cache + invalidation on `billing.entitlement.<tag>` and `billing.config.<realm>` (**FR-035**).
- [ ] **T104** The `QuotaSource` adapter for plan-sized limits (**FR-017**, D30).
- [ ] **T105** REST + gRPC endpoints; batch `Check`.

## Phase 5 — Operational surface

- [ ] **T110** Tick handlers — reconcile, audit, transfer, journal, webhook, outbound, expiry, dunning, subdrift, partitions, idem purge, events purge, trim — each an idempotent claim.
- [ ] **T111** DLQ: park at the cap, alert, never re-drive again, never drop (**FR-043**).
- [ ] **T112** Admin operations endpoints: reconcile, recover, rehydrate, unblock, adjustments, replay — each audited.
- [ ] **T113** `usage_daily` rollup + member-scoped breakdown (**FR-039**).
- [ ] **T114** [docs/operations.md](../../docs/operations.md) — one runbook entry per alert.
- [ ] **T115** `events/` — outbound log, signed delivery with backoff, SSRF guard, feed (**FR-048**).

## Phase 6 — UI package

- [ ] **T120** [P] `ports.ts` — framework-free, with `UnitLabels.scale` and pools.
- [ ] **T121** [P] `model.ts` — pure derivation; the kernel-window → view-window mapping lives server-side.
- [ ] **T122** Components: `BalanceHero` (per pool), `LimitMeter`, `SpendChart`, `Breakdown`, `LedgerTable`, `PlanCatalog`.
- [ ] **T123** `CreditsPanel` composition root.
- [ ] **T124** [P] `ui/examples/llm-credits` — the reference binding.
- [ ] **T125** ESLint `no-restricted-paths` + a scan asserting the string `credit` never appears inside `credits-ui/**`.
- [ ] **T126** Portability check: `npm pack`, install into an unrelated app, build clean (**SC-012**).
- [ ] **T127** Fixture-driven render tests for 1 / 3 / 5 ceilings, pools and a non-credit unit.

## Phase 7 — Hardening

- [ ] **T130** Load test against the [scale envelope](./plan.md#scale-envelope) and **SC-001**. A miss is a design finding.
- [ ] **T131** Chaos: kill Redis; fail it over to a lagging replica; restart it from a truncated AOF; replay webhooks at volume; kill a processor mid-event; inject drift; poison an intent. Each ends with no double charge and a page where the contract says so.
- [ ] **T132** Security review against the checklists; `gitleaks` + `govulncheck` in CI; Redis ACLs verified from a host's credentials.
- [ ] **T133** Recovery rehearsals: shard recover under traffic; region failover per the DR runbook; both timed against the RTO (**SC-008**).
- [ ] **T134** Two full adoption walkthroughs — one embedded, one containerized from a non-Go host with only a rate card — and fix whatever the second reveals.

---

## Dependencies

```text
T001→T004  ──▶  Phase 1 domain (T020…T026)        [T005–T008 done and verified]
                     ▼
              ports + suites (T030…T034a)   ← written BEFORE app
                     ▼
              app (T040…T048)  ──▶  adapters (T050…T057)
                     ▼
              Phase 2 transports (T060…T069)
                     ├──▶ Phase 3 payments (T080…T093) ──▶ Phase 4 (T100…T105)
                     └──▶ Phase 6 UI (T120…T127)   [independent]
              Phase 5 (T110…T115) after Phase 3
              Phase 7 last
```

## Definition of done (every task)

1. The behaviour has a test named for the invariant or requirement it implements.
2. `make ci` passes, and the output is pasted in the PR — not summarized.
3. No `os.Getenv` outside `cmd/`; no float near money; no provider SDK outside its adapter; no host access to Redis.
4. Any contract change lands in `specs/001-metering-billing-core/contracts/` **in the same commit** as the code, and `check-spec-drift.sh` still passes.
