# Tasks: Metering, Credits & Payments Core

**Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Contracts**: [contracts/](./contracts/)

`[P]` = parallelizable with its siblings. Every task that implements an invariant names it, so a
reviewer can check the test rather than the prose. `[x]` = done **and verified** — the verification
is named, and it runs in CI.

**Test-first pairs.** From Phase 2 on, every behavioural task is a pair sharing a number: **R###**
(red — a tests-only commit that *fails*, for the reason it states) and then **T###** (green — the least
code that makes it pass). The red task names its tests, derived from the contract clause that states
the invariant rather than from code that does not exist yet. The order is checked mechanically
(`scripts/verify-red-green.sh`, CI job `red-green`); a task that genuinely cannot be test-driven says
so as `TDD-Exempt: <reason>`, and "hard to test" is never one ([docs/testing.md](../../docs/testing.md)).
A conformance suite is itself held to the same standard: shown to fail against a deliberately broken
implementation, or it has not been shown to test anything.

Phases 0–1 predate the rule. Their suites and code mostly landed together, so nothing recorded a red
run; the [hardening record](#hardening-record) below says what was done about that afterwards, and what
it found.

Moved when Phases 2–7 were paired (no ID was reused for a different task): **T069** → **R060** (first)
plus **T069** (the closing check); **T082**/**T083**/**T101** → **R082**/**R083**/**R101**;
**T127** → **R122**; **T125** now precedes the components it guards.

---

## Phase 0 — Foundation (the boundary exists before the code it guards)

- [x] **T001** `go mod init github.com/truongpx396/intel-payment`; Go 1.26+ (the floor follows the dependencies: pgx ≥ 5.9.2 fixes GO-2026-5856 and needs 1.25; the current x/text, x/net and gRPC need 1.26). *Verified: `go build ./...`; `govulncheck` in CI; CI job `go` (setup-go reads `go.mod`).*
- [x] **T002** `.go-arch-lint.yml` — the component graph, including `events`. **Before** any port code. *Verified: `scripts/verify-boundary.sh` plants a violation per edge and expects rejection; CI job `go`.*
- [x] **T003** `.golangci.yml` — depguard rules 1–4 (core purity, engine standalone, ports-only access, provider-SDK containment), plus `paralleltest`/`tparallel`. *Verified: `scripts/verify-boundary.sh`; CI job `go`.*
- [x] **T004** `.github/workflows/ci.yml` — build, vet, test (`-race -shuffle`), integration (Testcontainers), lint, arch-lint, `verify-boundary`, `verify-portability`, `govulncheck`, `deploy`, and Playwright `e2e` (runs once `cmd/paymentd` exists). *Verified: this workflow is the verification.*
- [x] **T005** `migrations/0001`–`0005` — the baseline schema, including the guards, watermarks, suspense, transfers, pools, the webhook inbox, outbound events and the configuration audit trigger (**FR-003, FR-050**). *Verified: `scripts/verify-schema.sh`, CI job `schema`.*
- [x] **T006** Phase 2 draft schema in `specs/002-…/draft-migrations/`, applied on top of the baseline in CI and never shipped (D38). *Verified: `scripts/verify-schema.sh`.*
- [x] **T007** `specs/…/contracts/reference/hot_path.lua` — the reference Redis Functions (**FR-017f**). *Verified: `scripts/verify-hot-path.sh` on Redis in cluster mode, CI job `hot-path`.*
- [x] **T008** `scripts/check-spec-drift.sh` — no retired mechanism described as current (D46). *Verified: CI job `drift`.*
- [x] **T010** `cmd/payment-migrate` + an embedded-migration check `/readyz` can call (**FR-042**): `migrate.Check` / `migrate.Ready`. *Verified: `migrate` unit tests and Testcontainers tests (idempotent, 4 concurrent migrators apply each file once, an edited migration is refused, a failed migration rolls back); `scripts/verify-deploy.sh`.* The shard-count half of `/readyz` lands with the Redis adapter (T050).
- [x] **T011** `deploy/docker-compose.yml` + `Dockerfile` (distroless, non-root). Redis `noeviction` + AOF, with an ACL that gives hosts nothing. *Verified: `scripts/verify-deploy.sh`, CI job `deploy` — builds the image, migrates an empty Postgres, and attempts every forbidden Redis command.* `paymentd`/`payment-worker` start when Phase 1 lands their `cmd/`.

## Phase 1 — Metering core

### Domain
- [x] **T020** [P] `realm.go`, `scope.go`, `subjects.go`, `pool.go` — `Tag()` as identity; `Subjects`; `GeneralPool` (**FR-014, FR-017c, FR-017d**). *Verified: `metering/domain` unit tests — tag ambiguity refused, shard = fnv1a64 cross-checked against an independent implementation, pool draw order.*
- [x] **T021** [P] `credits.go`, `money.go` — signed `Credits int64`; `Money{MinorUnits, Currency}` (**FR-012**). *Verified: `metering/domain` unit tests — overflow refused, no mixed currencies.*
- [x] **T022** [P] `event.go`, `ratecard.go` — `Event` with required `OccurredAt`; `Price` with `RateCardVersion`; `RateCard`/`RateEntry` rationals (**FR-001, FR-002**, D31). *Verified: `metering/domain` unit tests; `PricerContract`.*
- [x] **T023** [P] `limit.go` — `Window` incl. `Job`; `Limit` with `Subject`, `Resource`, `TZ`; `AdmitRequest` with `Resource`, `Subjects`, `MaxCost`; `Admission` with `Headroom`, `Blocked` (**FR-004…006, FR-017a, FR-017c**). *Verified: `metering/domain` unit tests — `Evaluate`, `MergeLimits`, window buckets across DST.*
- [x] **T024** [P] `receipt.go` — `Charge`, `Grant` (pool), `Transfer` (pools), `Receipt` (`Seq`, `Deferred`), `TransferReceipt` (`Status`). *Verified: `metering/domain` unit tests — validation and canonical fingerprints.*
- [x] **T025** [P] `policy.go`, `errors.go` — every policy enum; typed errors (`ErrIdemConflict`, `ErrStaleEvent`, `ErrMissingSubject`, …). *Verified: `metering/domain` unit tests.*
- [x] **T026** `metering/config.go` — `Config` + `withDefaults` + `Validate`, exactly as in the contract. *Verified: `metering` config tests — defaults applied to a copy, every refusal, all problems reported at once.*

### Ports & conformance suites (before adapters — the suites are the specification)
- [x] **T030** `ports/driven.go` — `Pricer`, `PricerRegistry`, `BalanceStore`, `BookStore`, `LimitStore`, `PoolStore`, `RateCardStore`, `QuotaSource`, `JournalStore`, `UsageArchive`, `Bus`, `IntentStream`, `Clock`, `IDSource`, `Metrics`. *Verified: compiles under `go-arch-lint` and `depguard`; every port that has an adapter is checked against it by `var _ ports.X = …` in the adapter's package; the `Bus` adapters land with T052/T052a. `BookStore` grew the reconcile, audit, expiry and sweep queries with the writer (`WriterBooks`), and `IntentStream` is `Read(shard, count, minIdle)` / `Ack` / `Stats`.*
- [x] **T031** `ports/driving.go` — `Meter` (Admit, Record, Grant, Transfer), `LedgerWriter` (Drain, Reconcile, Rehydrate, Recover, Audit). *Verified: compiles against `app` in the next PR; `ports.Meter` and `ports.LedgerWriter` as in the contract.*
- [x] **T032** `PricerContract` — determinism, attributes/subjects never price, fail-closed, **sub-credit prices exact**, **rounds once per event**, monotonicity, card version, overflow refused (**FR-001**, D31). *Verified: `metering/contracts.PricerContract` over the `table` pricer on three disjoint cards **and** over `llmtoken`, `seat` and `storagebyte` through the registry (SC-004).*
- [x] **T033** `LedgerContract` — idempotent debit; **idempotency conflict**; admit without reserving; `MaxCost` + headroom; **pool draw order**; realm-independent keys; clamp with writeoff and no forgiven debt; job budget via subjects; **missing subject refused**; transfer refuses, then applies both sides once (**FR-003…005, FR-015, FR-017a…d**). *Verified: all 29 subtests green on Redis standalone and cluster (hot half), and again in `integration/` with the real writer and Postgres, where the book-side subtests run instead of skipping — `balance == SUM(ledger)` after a clamp, Σ ledger + Σ in-transit invariant mid-transfer, a replayed allocation a no-op.*
- [x] **T034** `LedgerWriterContract` — in-order booking; redelivery vs gap vs regression; batching; suspense opened and closed; reconcile at equal seq heals hot only and never books. *Verified: 24 subtests in `integration/` against real Redis and Postgres, parallel and shuffled under `-race`, repeated without a flake. Beyond the task text: a poison intent parked after its retry budget, a malformed entry parked whole, two writers racing on one shard, per-pool drift that does not net, tolerance, block-and-flag, rollup with its archive, a transfer's second leg re-driven, a correction re-issued, expiry trued up through suspense, the deep audit. Mutation-checked: a writer that stops recognising redeliveries, skips a gap, or books on reconcile fails the suite.*
- [x] **T034a** `ConsistencyContract` — [hot-path-consistency.md §9](./contracts/hot-path-consistency.md#9-verification) against Testcontainers Redis **in cluster mode** + Postgres, including a Redis restart from a truncated AOF and a failover to a lagging replica mid-traffic: no double charge, the shard freezes and rebuilds, deferred usage lands (**SC-002, SC-008**). *Verified: `integration/consistency_test.go` runs four scenarios on each of two rollbacks — Redis restarted from an AOF truncated at a known boundary (`appendfsync always`), and traffic failed over, through a retargeted proxy, to a real replica promoted at a checkpoint — and each of those with the intents read from Redis Streams and through the JetStream relay. Rollback of intents the books hold, of intents the writer never saw, producers charging before the writer notices, and a transfer caught between its legs: nothing charged twice, the shard rebuilt, the realm conserved. Mutation-checked against a writer that ignores the replication id. **Scope note:** the deferred-usage half (the journal replay) is asserted at the Meter, in `integration/meter_test.go`; Redis Cluster mode is covered by `LedgerContract` and the shard-lifecycle tests, while these destructive scenarios run on standalone Redis because a cluster node cannot be truncated or promoted in isolation.*

### App
- [x] **T040** `app/admit.go` — resolve realm limits for the resource, size `max_entitlement` limits via `QuotaSource` (error → `AdmitFailPolicy`), merge caller limits (tighten only), refuse a missing subject, one `ip_admit` read, evaluate (**FR-004…006, FR-017, FR-017c**). *Verified: `app` unit tests over programmable fakes (limit merging that only tightens, entitlement sizing with every failure mode, refused uncountable and subject-less limits, `fail_closed`/`fail_open`, denials counted and notifications coalesced) and `integration/` against real Redis + PostgreSQL — a plan-sized ceiling binds through the real hot path.*
- [x] **T041** `app/record.go` — refuse stale `OccurredAt`; price under the active card via the registry; fingerprint; `ip_debit`; on `FROZEN`/`COLD_SHARD`/unavailable/`BACKPRESSURE`, journal and return `Deferred` (**FR-008, FR-017e**). *Verified: `app` unit tests (stale/future events, every fail-closed pricing path counted, outage → journal, both stores down → an error, journal-first settlement, conflicts loud) and `integration/`: an outage mid-traffic defers usage and the replay tick applies it exactly once.*
- [x] **T042** `app/grant.go` — `ip_grant` per pool with `NegativeBalancePolicy` (**FR-015**). *Verified: `app` unit tests and `LedgerContract`; a grant during an outage is an error, never "accepted".*
- [x] **T042a** `app/transfer.go` — phase 1 via `ip_transfer_out` with the `MaxDestBalance` pre-check (**FR-017b**, D33). *Verified: `app` unit tests and `LedgerContract` (refuses to overdraw, cross-realm, destination cap); a replay reads its status from the books.*
- [x] **T042b** Window counters — daily/hourly in `Limit.TZ`, rolling as sub-buckets, job TTL; keyed under the charged scope's shard (**FR-017a**). *Verified: `LedgerContract` on real Redis — per-subject daily ceilings, a daily reset at local midnight in `Asia/Ho_Chi_Minh`, a sliding rolling window, a job budget, a metered-unit counter; all on cluster mode.*
- [x] **T043** `app/writer.go` — shard ownership (advisory lock), `XAUTOCLAIM` on takeover, per-scope in-order batches, the watermark cases, the op table, suspense and corrections, transfer phases 2–4, `lw` → `billing.balance.low` (**FR-007**). *Verified: `LedgerWriterContract`; `TestTheWriterPublishesBalanceLowOnceWhenAThresholdIsCrossed`. Shard ownership is `Books.Acquire` (T051); correctness does not rest on it — two writers racing on one shard book each intent once. The worker loop that acquires and drains is T068.*
- [x] **T044** `app/reconcile.go` — incremental + full; outcomes; findings per scope and pool; `ip_heal` compare-and-set; never books (**FR-009**). *Verified: `LedgerWriterContract` — healthy, deferred, drift healed on the hot side with no ledger row, per-pool findings, tolerance, and regression rebuilding the shard.*
- [x] **T045** `app/rehydrate.go` — lazy on `COLD_SCOPE` under a per-scope lock; eager for recently active scopes after a bump (**FR-010**). *Verified: lazy rebuild in `app` and `integration/` (a cold account is rebuilt from booked + open suspense and the hot seq resumes at `applied_seq`); the eager pass runs at the end of `Recover`.*
- [x] **T045a** `app/recover.go` — replication-id watch; freeze → drain → bump → unfreeze; resharding (**FR-010**, D40). *Verified: `ConsistencyContract`. Resharding is an operator procedure over the same steps and lands with the worker (T068).*
- [x] **T045b** `app/audit.go` — deep audit and checkpoints; partition detach eligibility (**FR-050**). *Verified: `LedgerWriterContract` (clean audit) and `postgres/sweeps_test.go` (a tampered balance is found; a checkpoint advances only past rows the audit proved).*
- [x] **T046** `app/expiry.go` — FIFO lots per pool; expiry pre-applied at the watermark's remainder, trued up by the writer (**FR-016**). *Verified: `LedgerWriterContract` — 300 spent in flight while a 1,000 lot expires: the hot tier over-expires by 300, the ledger books the exact 700, the 300 is an `expiry_trueup` suspense entry that is corrected and closed, hot and booked both 5,000.*
- [x] **T047** `app/journal.go` — the replay tick through `ip_debit`; settle on booking (**FR-017e**, D41). *Verified: the replay tick in `integration/meter_test.go` against a real outage; settle-on-booking in `TestBookingSettlesTheJournalEntryInTheSameTransaction`.*
- [x] **T048** `app/new.go` — `Deps`, validation, return `ports.Meter`. *Verified: `app` unit tests — every missing dependency and an invalid config are refused at construction.*

### Driven adapters
- [x] **T050** `adapters/driven/redis` — load `reference/hot_path.lua` **unchanged**; the key layout; `HotAckWait` via `WAITAOF`; the shard lifecycle (**FR-017f**, D29). *Verified: `redis` adapter unit tests (every key of a scope shares one slot — CRC16 reimplemented independently; the embedded library is byte-identical to the contract's reference file) and integration tests on Redis standalone **and cluster mode**: `LedgerContract` (hot half), shard lifecycle, heal as a compare-and-set, backpressure, `WAITAOF`, and the adapter running as the service role of the deploy ACL. `payment-migrate functions` installs it; `scripts/verify-deploy.sh`.*
- [x] **T051** `adapters/driven/postgres` — books, watermarks, suspense, both guards (window probe under the per-scope lock), transfers, journal, limits, pools, cards (+ cache invalidated by `LISTEN intelpay_config`). *Verified: `postgres` integration tests against PostgreSQL 16 — watermark never moves backwards, the usage guard is window-bounded and the credit guard permanent and realm-scoped (the unique constraint is the guarantee), `balance == SUM(ledger)` checked in SQL, atomic rollback, 25 concurrent bookings of one scope serialise on the advisory lock with no lost update, FIFO lots, exact expiry remainder, transfers, journal claims that never hand out one entry twice, shard ownership that survives a killed session, configuration served from a cache invalidated by the database's own `LISTEN` (and dropped on a listener reconnect, and by TTL). Two mutations of the SQL were made to check the tests fail.*
- [x] **T052** [P] `adapters/driven/redisstreams` — the default `Bus` + `IntentStream` (**FR-046, FR-047**). *Verified on Redis standalone and cluster: `IntentStream` (in-order, at-least-once, oldest-first reclaim, ack never deletes, a malformed entry surfaced whole, a lost consumer group created again, stats, trim) and `Bus` — `BusContract` (17 subtests per Redis mode) plus tests that read the stream to show `Trim` removes exactly what every group has acknowledged and nothing else. The contract found a real defect while it was being written: one Redis group per caller-chosen name let a consumer of one subject pattern swallow another's messages; a group is now per (name, pattern), and a subtest holds it.*
- [x] **T052a** [P] `adapters/driven/natsjetstream` — the optional `Bus`, with the outbox relay. *Verified against a real NATS server with JetStream: `BusContract` (17 subtests); `IntentStreamContract` through the relay; a relay that dies between publish and ack republishes and JetStream recognises it (and never acknowledges in Redis before JetStream has stored); the outbox is trimmable once forwarded; a recovery read forwards the outbox first and `Stats` counts its backlog; the past-`MaxAttempts` DLQ rule. The whole `LedgerWriterContract` (24 subtests) and `ConsistencyContract` (both rollbacks) also pass with the intents travelling by JetStream. JetStream stops redelivering at `MaxDeliver` without parking the message, so the adapter runs unlimited `MaxDeliver` and dead-letters by attempt count itself — otherwise a message would be silently dropped.*
- [x] **T052b** `BusContract` against **both** adapters (**SC-014**). *Verified: one suite, 17 subtests, run on Redis Streams (standalone and cluster) and on NATS JetStream; the same `IntentStreamContract` (9 subtests) is run on the Redis outbox and on the relay.*
- [x] **T053** [P] `adapters/driven/otel` — every metric of the observability contract (**FR-041**). *Verified: `otel_test.go` against the SDK's manual reader — every catalogued metric exports with its declared kind, unit and description; one label set is one series whatever the order; a counter only rises; a gauge keeps its last value; latency buckets resolve the 5 ms target; unknown names are created on first use; 16 goroutines lose no increment under `-race`. `ports.Catalog` is held equal to the contract's table by `metrics_test.go`, and four terminal states the writer reaches that had no metric now do.*
- [x] **T054** [P] `pricing/table` — the default pricer, exact, rounds once (**FR-001**, D31). *Verified: `table_test.go` — the contract worked examples, indexed and hand-built cards.*
- [x] **T055** [P] `pricing/llmtoken`, `seat`, `storagebyte` — unit validation, then `table`. *Verified: `table_test.go` — the same suite through the registry; a foreign unit fails closed.*
- [x] **T056** [P] `archive/postgres` — `UsageArchive` over `usage_events` for `rollup` granularity (**FR-050**). *Verified: `archive/postgres` integration tests — every event returns exactly as archived (SC-010), and a retried batch adds nothing.*
- [x] **T057** Wire T032 against `table` over three cards and a registered pricer; T033/T034/T034a against the real adapters (**SC-004**). *Verified: T032 in `pricing/table` (three cards and the registry); T033 and T034 in `integration/` over real Redis and Postgres, and T034 again over JetStream; T034a on a truncated AOF and a promoted replica, over both transports. Redis Cluster mode is held by `LedgerContract`, the `BusContract` and the `IntentStreamContract`.*

## Phase 2 — Transports

The equivalence suite comes first: it is the red for the whole phase, and the transports exist to make it pass.

- [ ] **R060** **Equivalence suite (FR-044)** *(was T069 — moved first)*. `metering/contracts.MeterEquivalence`: the `Meter`-level behaviour of `LedgerContract` plus deferral and error identity, written once and parameterised by a `Meter` factory. Today only the in-process `app.Meter` can be plugged in, so the wire half fails to build — that failure is the recorded red. Every typed error in `domain/errors.go` must survive the wire: `errors.Is(err, domain.ErrIdemConflict)` after a round trip, not a string match.
- [ ] **R061** gRPC status mapping, from [grpc-surface.md § Status mapping](./contracts/grpc-surface.md): one table test row per condition (code **and** `ErrorInfo.reason`); a test that enumerates every exported `Err*` in `domain` and fails on any with no row, so a new error cannot ship unmapped; a missing `idem_key` or `occurred_at` is `INVALID_ARGUMENT`, never a generated default; a `Scope.realm` outside the credential's is `PERMISSION_DENIED`; `Grant` and `Transfer` are refused for a `record`-only credential **at the RPC**; a deferred `Record` is `OK` with `deferred = true`; a descriptor test that no money-bearing message has a `float` or `double` field.
- [ ] **T060** `api/meteringv1` proto per [metering-ports.md § Service surface](./contracts/metering-ports.md#service-surface-meteringservice-grpc-contract-locked). `TDD-Exempt: declarations only` — guarded by R061's descriptor test and `buf breaking` against `main`.
- [ ] **T061** `grpcserver` with the fixed status mapping — makes **R061** green.
- [ ] **R062** `grpcclient`: the inverse of R061 (every row, status → typed error, `errors.Is` holds); the caller's deadline is the deadline on the wire; a cancelled context settles nothing it did not already settle; `var _ ports.Meter = (*Client)(nil)`.
- [ ] **T062** `grpcclient` — **wrapped to satisfy `ports.Meter`** (**FR-044**).
- [ ] **R063** REST error table, from [rest-api.md § Errors](./contracts/rest-api.md#errors): a row per `(HTTP, code)` including `details` (`resets_at` on `409 limit_reached`, `upgrade_url` on `402`); missing `Idempotency-Key` is `400`; the same key and request returns the original outcome, the same key and a *different* request is `409 idempotency_conflict`; a body-supplied realm outside the credential's is `403 realm_mismatch`; `PAYMENT_LEGACY_429_FOR_LIMITS`; webhook routes carry no CORS headers and no auth middleware (a route-table test, so a JWT check added later fails it).
- [ ] **T063** `resthandler` — every `/v1/*` route of [rest-api.md](./contracts/rest-api.md); `Idempotency-Key` is the key.
- [ ] **R063a** Admin API: a configuration write without `intelpay.actor` / `intelpay.reason` is refused; the audit row is written by the database trigger in the same transaction (integration — a write that bypasses the API is still audited); a card is published only after a test pricing, and a card that overflows, prices negative, or names an unregistered pricer is refused (**FR-049**).
- [ ] **T063a** Admin API — configuration writes with `intelpay.actor`/`intelpay.reason`, card publication validated by a test pricing.
- [ ] **R063b** Handler and document agree, in both directions: every registered route is in `api/openapi/v1.yaml` and every path in it is registered, so a route added without a document entry fails.
- [ ] **T063b** `api/openapi/v1.yaml` + that check in CI; generated TypeScript and Python clients (a generated client compiles and round-trips one `Admit` against the handler).
- [ ] **R064** `GET /v1/credits`: the parts of the snapshot sum to its total, or an explicit residual is present ([credits-ui-ports.md](./contracts/credits-ui-ports.md) invariant 5); pools are included; every number in the JSON is an integer (a decoder that rejects a fractional value); a member-scoped call never returns another member's rows.
- [ ] **T064** `GET /v1/credits` — the whole `CreditsSnapshot`, pools included (**FR-036**).
- [ ] **R065** Authorization matrix, **generated from the route table** so a new route with no row fails: privilege (`record` / `grant` / `admin`) × route → allowed or `403 forbidden`; no credential → `401`; realm binding; mTLS and bearer both.
- [ ] **T065** Caller auth — mTLS/bearer, realm binding, `record`/`grant`/`admin` privileges (**FR-040**).
- [ ] **R066** `/readyz` is `503` while migrations are behind head and while the shard count disagrees with `Config`; `/healthz` makes no dependency call; `/metrics` exports every name in `ports.Catalog`.
- [ ] **T066** `/healthz`, `/readyz` (schema head + shard count), `/metrics`.
- [ ] **R067** `cmd/paymentd`: reads the environment in `cmd/` only; refuses to start on each invalid `Config` that `Validate` refuses; a `Record` in flight at shutdown settles or is journalled, never lost; the Playwright `e2e` suite (already gated on `cmd/paymentd`) goes from skipped to real.
- [ ] **T067** `cmd/paymentd`.
- [ ] **R068** `cmd/payment-worker`: a worker killed mid-batch is replaced by another that resumes, and no intent is booked twice (the writer's two-writers scenario at process level, integration); shutdown releases shard ownership.
- [ ] **T068** `cmd/payment-worker`.
- [ ] **T069** Close the phase: **R060** runs in CI in both modes — in-process, and through `grpcclient` → `grpcserver` — with the same assertions, both green (**FR-044**).

## Phase 3 — Payments

Contract-derived: the invariants and the test skeleton are in [payment-provider-ports.md](./contracts/payment-provider-ports.md#invariants-every-implementation-must-uphold). Each red below names the invariant it pins.

- [ ] **R080** `billing/domain` — unit and fuzz. **Partial refunds (invariant 16):** `revoke_total = min(granted, ceil(granted × refunded / amount))` against an independent `math/big` computation; any partition of a total into partial refunds revokes the same as one refund of that total; never more than granted; a replayed report revokes nothing further. **Ordering (15):** any permutation of a set of subscription events converges to the newest state. **Disputes (17):** `funds_withdrawn` then `funds_reinstated` returns exactly what was clawed back, and each is keyed on dispute id and phase. **Grace (13):** the instant `GraceUntil` is reached, on both sides of it.
- [ ] **T080** `billing/domain` — provider accounts, `Sub.LastEventAt`, payments with cumulative refunds and dispute state, normalized `EventObject`, `Ignored`.
- [ ] **T081** `billing/ports` — `PaymentProvider` (incl. `ChargeSaved`, setup mode), `Catalog.PlanForPrice`, `Inbox`, `Payments`, `Subs.ApplyIfNewer`, `TaxStrategy`. `TDD-Exempt: declarations only` — guarded by `go-arch-lint`, `depguard` and the compile-time `var _ ports.X = …` in each adapter.
- [ ] **R082** `ProviderContract`, from the skeleton: tampered body, missing and stale signature, a body signed for another account, verify-and-normalize with the provider's timestamp and a body hash, refund amounts normalized as **cumulative**, `Ignored`, checkout never grants (**FR-018, FR-019**). **The suite must have teeth:** run against a fake provider that passes, *and* against one deliberately broken fake per subtest (skips verification, ignores the timestamp, reports a per-event refund) — each must be rejected by name.
- [ ] **R083** `IngressContract` + `ProcessorContract`, from the skeleton and every invariant it names: **verify before anything (1)** — a fake store records call order and nothing is touched before verification; **persist before acknowledging (5)**; **a replay answers 200 (6)**; **unverifiable is parked with its raw body, never dropped (14)**; a crash after ack still grants once; concurrent processors grant once; metadata cannot redirect a grant (4); partial refunds exact; a won dispute reverses; out-of-order subscription events; grace; a realm the account does not serve is ignored and counted (19); **only the processor writes subscription status (12)** (**FR-020…026, SC-016, SC-017**). Same teeth requirement as R082.
- [ ] **T084** `billing/app/ingress.go` + `processor.go` — stage 1 and stage 2 exactly as in the contract; the `billing.webhook.tick` sweep and unverifiable-retry — makes **R083** green.
- [ ] **R085** Checkout in payment, subscription and setup modes: `Meter.Grant` is never called by any of them, including on a simulated return-URL callback (invariant 2); the amount, currency and credit quantity come from `Catalog`, never from the request.
- [ ] **T085** `billing/app/checkout.go` — payment, subscription and setup modes. Grants nothing.
- [ ] **R086** Stripe, against **recorded** signed fixtures: `R082` run against the adapter; the timestamp tolerance exactly at its bound, one second inside and one outside; per-account secrets (a body signed with account B's secret fails on account A); signatures compared in constant time (`hmac.Equal`); grants keyed on payment/invoice id, so a redelivery is the same key.
- [ ] **T086** `adapters/driven/stripe` — HMAC, tolerance, per-account secrets; key grants on payment/invoice id; refunds and dispute events.
- [ ] **R087** Polar and PayPal: `R082` run against each; PayPal's remote verification down → the delivery is parked **with its raw body** and answered `202` (IngressContract), never `2xx`-and-dropped.
- [ ] **T087** [P] `adapters/driven/polar`, **T088** [P] `adapters/driven/paypal` (remote verification, **park unverifiable with the raw body**).
- [ ] **R089** `TaxStrategy` — each of the three modes: `provider_managed` adds nothing the provider does not compute, `external_hook` is called before checkout and its failure fails the checkout closed, `none` annotates nothing; there is no fourth mode (**FR-028**). **R090** `PlanChangePolicy` — a table over upgrade / downgrade × each policy, including a change that would take an entitlement below current usage (**FR-029**). **R091** the catalogue is per account and currency: an unknown price id is `!ok`, never a default plan.
- [ ] **T089** `TaxStrategy` impls. **T090** `PlanChangePolicy`. **T091** Catalogue store per account and currency.
- [ ] **T092** Run **R082** against **every** adapter (**SC-004**) — a new adapter is not registered until it passes.
- [ ] **R093** Auto top-up (**invariant 18**): at most `max_per_day` distinct charges per scope per day however many times the trigger fires; the idempotency key is `recharge:<realm>/<scope>:<day>:<n>`; three consecutive failures disable it, emit `failed`, and record `disabled_reason`; the handler never grants — credits arrive only when the verified payment is processed.
- [ ] **T093** Auto top-up — balance watch, `billing.balance.low` handler, bounded idempotent `ChargeSaved`, disable after failures (**FR-030a**).

## Phase 4 — Entitlements

Contract-derived: [entitlement-ports.md § Invariants](./contracts/entitlement-ports.md#invariants) and its test skeleton.

- [ ] **R100** `entitlement/domain` — unit and fuzz: `-1` is handled explicitly and is never compared numerically, coerced to a large number, or confused with unset (invariant 6) — a property over every operation on `Value`; enum order follows `entitlement_keys`, not lexicographic.
- [ ] **T100** `entitlement/domain` — `Key`, `Kind`, `Value` (`-1` = unlimited), `Grant`, `Source`, `KeyDecl`.
- [ ] **R101** `EntitlerContract`, from the skeleton (unknown key denied; outage denies, never permits; override beats subscription; grace holds and then lapses; quota refusal is actionable; unlimited is `-1`; an upgrade is visible at once; `Resolve` reports provenance; realms do not leak) **plus** declared keys and enum order, and **no `if plan ==`** — a source scan, in the manner of `check-spec-drift.sh`, that fails on a plan-code branch in production Go (invariant 3). Shown to have teeth against a broken entitler per subtest.
- [ ] **T101** `EntitlerContract` passes over the real store — makes **R101** green.
- [ ] **R102** Precedence `override > subscription > default`: all eight combinations of the three being present resolve to exactly one winner; an override that names no reason is refused; the override audit row is written by the database trigger, including for a write that bypasses the API (invariant 5, integration).
- [ ] **T102** Resolver with precedence `override > subscription > default`.
- [ ] **R103** Cache (invariant 9), on a fake clock: an upgrade, an override write, and a grace expiry each invalidate immediately; a lost invalidation message is still bounded by the TTL; invalidation on `billing.entitlement.<tag>` and `billing.config.<realm>` and on nothing else (**FR-035**).
- [ ] **T103** Cache + invalidation on `billing.entitlement.<tag>` and `billing.config.<realm>` (**FR-035**).
- [ ] **R104** `QuotaSource`: a plan change resizes a ceiling on the next `Admit`; `-1` applies no ceiling; a non-numeric quota key is refused when configured, not when spent; a store error is `AdmitFailPolicy`'s to decide (already pinned in T040 — this pins the adapter half).
- [ ] **T104** The `QuotaSource` adapter for plan-sized limits (**FR-017**, D30).
- [ ] **R105** Endpoints: a batch `Check` answers every key it was given, in request order, and an unknown key is denied inside the batch, not an error; the realm comes from the credential; REST and gRPC return byte-identical decisions for the same input.
- [ ] **T105** REST + gRPC endpoints; batch `Check`.

## Phase 5 — Operational surface

- [ ] **R110** Tick handlers, one table test over **all** of them: two concurrent runs of one tick do the work once; a run killed midway is completed by the next; a tick with nothing due writes nothing.
- [ ] **T110** Tick handlers — reconcile, audit, transfer, journal, webhook, outbound, expiry, dunning, subdrift, partitions, idem purge, events purge, trim — each an idempotent claim.
- [ ] **R111** DLQ (**FR-043**), as a property: for any interleaving of failures and re-drives an item is in exactly one of *pending* and *parked*; a parked item is never delivered again; the page fires once per parking; nothing is ever deleted.
- [ ] **T111** DLQ: park at the cap, alert, never re-drive again, never drop.
- [ ] **R112** Admin operations: each requires `actor` and `reason`, is audited in the same transaction, is refused to a non-`admin` credential, and is idempotent under retry.
- [ ] **T112** Admin operations endpoints: reconcile, recover, rehydrate, unblock, adjustments, replay — each audited.
- [ ] **R113** `usage_daily` (**FR-039**), as a property: for any partition of a set of events into days — including a zone that skips a calendar date, the class of defect fuzzing found in `window.go` — the daily rows sum to the events; a member-scoped breakdown never contains another member's rows.
- [ ] **T113** `usage_daily` rollup + member-scoped breakdown (**FR-039**).
- [ ] **R114** The alert catalogue and the entries of [docs/operations.md](../../docs/operations.md) are the same set (the way `ports.Catalog` is held equal to the contract's metric table): an alert with no runbook entry, or an entry with no alert, fails.
- [ ] **T114** [docs/operations.md](../../docs/operations.md) — one runbook entry per alert.
- [ ] **R115** `events/`, from [outbound-events.md § Contract-test obligations](./contracts/outbound-events.md#contract-test-obligations): a signature verifies with the endpoint's secret and fails with any other; a `5xx` endpoint is retried on the backoff schedule then marked `dead` with an alert; the same event delivered twice carries the same `id`; an endpoint gets only the types it subscribed to and only its realm; the feed returns every event after a cursor exactly once, in `id` order, across pages; an endpoint resolving to a private, loopback, link-local or IPv4-mapped address is refused **before any request is sent**, a redirect is not followed, and the body is capped. A fuzz target over the address classifier: anything `net/netip` calls private, loopback, link-local, unspecified or multicast is refused.
- [ ] **T115** `events/` — outbound log, signed delivery with backoff, SSRF guard, feed (**FR-048**).

## Phase 6 — UI package

Contract-derived: [credits-ui-ports.md § Invariants](./contracts/credits-ui-ports.md#invariants-every-implementation-must-uphold). The guards come before the code they guard.

- [ ] **R125** The boundary guards, planted-violation style (as `scripts/verify-boundary.sh`): the ESLint `no-restricted-paths` rule and a scan for the string `credit` inside `credits-ui/**` each **reject** a planted violation, so an unarmed guard fails the build (invariant 6).
- [ ] **T125** ESLint `no-restricted-paths` + a scan asserting the string `credit` never appears inside `credits-ui/**`.
- [ ] **R120** `ports.ts` — type-level tests: `UnitLabels.scale` and pools present; no field on a money type is `number` where an integer is meant without a branded type.
- [ ] **T120** [P] `ports.ts` — framework-free, with `UnitLabels.scale` and pools.
- [ ] **R121** `model.ts` — properties (fast-check): fill is `used / cap` and a pre-computed percentage is refused (1); `warnAtPct` comes from the limit (2); no float reaches a balance, delta or total, and display rounding never changes a value (4); parts sum to the total or an explicit residual row is emitted (5).
- [ ] **T121** [P] `model.ts` — pure derivation; the kernel-window → view-window mapping lives server-side.
- [ ] **R122** *(was T127)* Fixture-driven render tests for **1 / 3 / 5** ceilings, pools and a non-credit unit, plus one named test per invariant: an exhausted limit renders a blocking notice with `denyCode` and a remedy (3); every series shows its label and value as text (7); every meter is a `role="progressbar"` with `aria-valuenow` and `aria-valuetext` (9); `blocked` exposes no checkout action (11); `canPurchase = false` renders every purchase control inert with the reason (12); no provider price id reaches the package (13).
- [ ] **T122** Components: `BalanceHero` (per pool), `LimitMeter`, `SpendChart`, `Breakdown`, `LedgerTable`, `PlanCatalog`.
- [ ] **R123** `CreditsPanel`: on return from checkout it renders `processing` and shows `granted` **only** when `CheckoutSource.fulfilment()` reports it (10) — a fake source that never reports must leave it `processing` forever.
- [ ] **T123** `CreditsPanel` composition root.
- [ ] **R124** The reference binding builds and renders against fixtures with no import outside its own package.
- [ ] **T124** [P] `ui/examples/llm-credits` — the reference binding.
- [ ] **T126** Portability check: `npm pack`, install into an unrelated app, build clean (**SC-012**). *(A check, not a feature: it is its own red.)*

## Phase 7 — Hardening

These are measurements, not features, so there is no pair: each is its own red, and a miss is a design finding.

- [ ] **T130** Load test against the [scale envelope](./plan.md#scale-envelope) and **SC-001**. A miss is a design finding.
- [ ] **T131** Chaos: kill Redis; fail it over to a lagging replica; restart it from a truncated AOF; replay webhooks at volume; kill a processor mid-event; inject drift; poison an intent. Each ends with no double charge and a page where the contract says so.
- [ ] **T132** Security review against the checklists; `gitleaks` + `govulncheck` in CI; Redis ACLs verified from a host's credentials.
- [ ] **T133** Recovery rehearsals: shard recover under traffic; region failover per the DR runbook; both timed against the RTO (**SC-008**).
- [ ] **T134** Two full adoption walkthroughs — one embedded, one containerized from a non-Go host with only a rate card — and fix whatever the second reveals.
- [ ] **T135** Mutation over the **integration** tier, nightly, with Docker: booking, reconcile, recover and the writer in `metering/app` are exercised only by real Redis and Postgres, so the unit-tier run cannot see them. Survivors are classified as the domain's were (see the hardening record), and the `app` mutant-coverage floor in `scripts/mutation.sh` rises from what the unit tier can reach to what this run measures.

---

## Dependencies

```text
T001→T004  ──▶  Phase 1 domain (T020…T026)        [T005–T008 done and verified]
                     ▼
              ports + suites (T030…T034a)   ← written BEFORE app
                     ▼
              app (T040…T048)  ──▶  adapters (T050…T057)
                     ▼
              Phase 2 transports: R060 (equivalence) first, then R06x → T06x, closing with T069
                     ├──▶ Phase 3 payments (R080…T093) ──▶ Phase 4 (R100…T105)
                     └──▶ Phase 6 UI (R125 first, then R12x → T12x, T126)   [independent]
              Phase 5 (R110…T115) after Phase 3
              Phase 7 last (T135 needs Docker)

Inside every pair:   R### (tests-only, fails)  ──▶  T### (passes)  ──▶  refactor (tests untouched)
```

## Definition of done (every task)

1. **Red before green.** The branch holds a tests-only commit that fails for the reason it states,
   *before* the production change, and the tip passes it — `make verify-red-green`. Otherwise the change
   says `TDD-Exempt: <reason>` and the reason survives review.
2. **The behaviour has a test named for the invariant or requirement it implements**, at the lowest
   tier that can express it ([docs/testing.md](../../docs/testing.md)); the failure paths — replay,
   concurrency, cancellation, tampering, outage, drift, a refund past the spend — first.
3. **Properties where examples are not enough.** Arithmetic, canonical encodings and time logic carry a
   fuzz target held to an independent reference; its seed corpus holds every known-bad input, and a
   failing input is committed with its fix.
4. **A conformance suite has teeth**: each subtest is shown to reject a deliberately broken
   implementation.
5. **The lines the change touched survive mutation** — `make mutation-diff`. A survivor is killed by a
   new test, or is an equivalent mutant listed with a reason in `.mutation-equivalents`; there is no
   tolerance on the lines the change touched. A threshold in `scripts/mutation.sh` only rises.
6. `make ci` passes, and the output is pasted in the PR — not summarized.
7. No `os.Getenv` outside `cmd/`; no float near money; no provider SDK outside its adapter; no host
   access to Redis.
8. Any contract change lands in `specs/001-metering-billing-core/contracts/` **in the same commit** as
   the code, and `check-spec-drift.sh` still passes.

## Hardening record

Phases 0–1 (T001–T057) were built before [constitution principle X](../../.specify/memory/constitution.md)
was made checkable. Their suites and the code they verify mostly landed in single commits, so nothing
recorded a red run, and mutation testing had been done by hand on three tasks. They were hardened
afterwards, on the branch that introduced the gates, and the results are recorded here so the claims
above can be checked rather than trusted. Every figure below was **measured** with `scripts/mutation.sh`
(gremlins v0.6.0, `.gremlins.yaml`) on the tree that carries this text — except the two "before" figures
marked *(local run)*, which were taken by the session that wrote the first tests and not re-measured.

### What it found

Two defects, in code that had passed review, each found by a property rather than an example. Both are
now regression tests (a failing test-only commit, then the fix — `verify-red-green.sh` passes on this
branch):

| Defect | Found by | Consequence | Now pinned by |
|---|---|---|---|
| A request's fingerprint treated its quantities as an ordered list, so a retry that reordered a twice-named unit was an **idempotency conflict** | `FuzzChargeFingerprintIgnoresQuantityOrder` | a legitimate retry refused as a different request | `TestChargeFingerprintIsTheDocumentedEncoding` and the fuzz seed; quantities are a multiset |
| A daily counter in a zone that skipped a calendar date (Pacific/Apia, 30 Dec 2011) was given a **negative lifetime** | `FuzzWindowCounterOutlivesItsWindow` | the limit's counter deleted at once — the limit never binds | `TestDailyBucketSurvivesAZoneThatSkippedACalendarDate` and the fuzz seed |

Beyond those two, the first boundary and fuzz pass pinned what no test had: the exact overflow boundary
of `Credits` and `Money` addition, the documented byte encoding of the persisted charge, grant and
transfer fingerprints (a silent change would turn every in-flight retry into a conflict), the exact
ceiling of a window limit, pool draw order including a negative priority, and — against an independent
128-bit computation — that a price is exactly `ceil(q × c / b)` or refused, and that rounding happens
once per event.

A third finding is about the gate, not the code. The first pull-request-form run
(`scripts/mutation.sh --diff origin/main`) **failed this branch on its own lines** — two equivalent mutants
in `fingerprint.go` — because the tool had no way to honour what the docs promised, that an equivalent mutant
is "named in the PR". It now has one: `.mutation-equivalents`, where each excuse names the file, operator,
column and the exact text of its line and must give a reason, and a pull request has no tolerance for any
survivor that is not excused. Both halves are held by cases in `scripts/verify-tdd-gates.sh` that were
written first and seen to fail.

### What the tests are worth

Efficacy is killed ÷ (killed + lived): of the mutants a test reaches, the share a test catches.

| Package | Efficacy before | Efficacy now | Survivors | Mutant coverage |
|---|---|---|---|---|
| `metering/domain` | 81.8% *(local run)* | **93.3%** raw; **100%** with the 13 equivalents set aside (182 killed) | 13 — every one an equivalent mutant (below), listed in `.mutation-equivalents` | 82.6% |
| `pricing/table` | 83.3% *(local run)* | **100%** (6 of 6) | 0 | 100% |
| `metering/app`, unit tier | not measured | **97.0%** raw; **100%** with the 4 equivalents set aside (127–128 killed) | 4 — every one an equivalent mutant (below), listed in `.mutation-equivalents` | about 32%, on purpose (see *What is not claimed*) |

"Raw" counts the equivalents as survivors; the gate sets them aside, because a mutant no test could kill is
not a gap. The thresholds in `scripts/mutation.sh` rose with it and only rise, measured on that basis: `domain`
90 → 98% efficacy and 80 → 81% coverage, `app` 80 → 97% and 25 → 30%. A `TIMED OUT` mutant is never counted as killed. Across runs, 0 or 1
mutant timed out in `domain` (of 197) and in `app` (of ~130), which moves a figure by a fifth of a point at
most; the gate fails a package if more than 5% time out.

### The survivors, classified

**`metering/domain` — 13 survivors, all equivalent** (no test can tell the mutant from the code):

| Where | Mutant | Why it cannot be observed |
|---|---|---|
| `credits.go:25` ×2, `money.go:34–35` ×2 | `o > 0` → `o >= 0`, `o < 0` → `o <= 0` in the overflow guard | at `o == 0` the second operand (`c > MaxInt64 − 0`) is false for every `c` |
| `fingerprint.go:45` | `<` → `<=` on `Unit` | reached only under `Unit != Unit` |
| `fingerprint.go:47` | `<` → `<=` on `Amount` | ties are equal in both fields — indistinguishable elements |
| `limit.go:148` | `c.Max < out[i].Max` → `<=` | at equality the assignment writes the same value |
| `limit.go:206`, `:209` | `h < 0` → `<=`; `h < headroom` → `<=` | at equality the assignment writes the same value |
| `pool.go:70` | `<` → `<=` on `Priority` | reached only under `Priority != Priority` |
| `pool.go:72` | `<` → `<=` on `Name` | `EligiblePools` returns names only, so swapping two equal names changes nothing |
| `receipt.go:79` | `g.Amount < 0` → `<= 0` | the zero amount was already refused two lines above |
| `window.go:105` | `w < time.Second` → `<=` | at equality the assignment writes the same value |

Of the 40 mutants no test reaches, 39 sit on a `case` expression, which gremlins reports as *not covered*
whatever the tests do (Go's coverage profile has no block for a case condition), and 1 on a constant. There
were 44: four sat on lines no `domain` test executed — `RateCard.Rates` on a hand-built card, which must
answer as an indexed one does or a price would depend on how the card was assembled, and the policy enum
check — and are now pinned.

**`metering/app`, unit tier — 22 survivors over three passes: 18 were missing tests, now pinned; 4 are
equivalent.** The first pass left 17 (14 missing tests, 3 equivalent). Pinning those made the tests reach
code no test had reached, which exposed 3 more, and reaching `Expirer.Tick` exposed the last 2 — the sign of
the expiry delta, below. Each of the 18 was killed by hand-planting its mutant and watching the new test
fail (the `test(mutation)` commits).

| Where | What no test pinned | Test |
|---|---|---|
| `record.go:106` ×2 | the Meter's own range check on a compiled pricer: zero accepted, exactly `MaxOperationAmount` accepted, one past either end refused | `TestRecordHoldsACompiledPricerToTheRangeExactly` |
| `record.go:109` | a row names its card when the pricer does not (invariant 5), and keeps the pricer's own version when it does | `TestRecordNamesTheCardOnTheRowWhenThePricerDoesNot` |
| `record.go:74` ×2 | the stale refusal states the bound it enforces | `TestRecordStaleRefusalNamesTheBoundItEnforces` |
| `admit.go:176` | a denial names its limit and deny code in the metric and the notification | `TestADenialNamesItsLimitAndIsPublishedOnALiveBoundedContext` |
| `admit.go:204` | a notification is published on a live context with a deadline of at most 100 ms | same |
| `rehydrate.go:71` | a shard whose generation never settles is read exactly three times | `TestRehydrateReadsTheBooksExactlyThreeTimesBeforeGivingUp` |
| `coalesce.go:33` | the coalescing window reopens exactly at its end | `TestTheCoalescerReopensExactlyAtTheEndOfItsWindow` |
| `coalesce.go:37` (negation) | aged-out keys are dropped, so the map is bounded | `TestTheCoalescerForgetsWhatHasAgedOut` |
| `coalesce.go:39` (negation) | the cleanup never forgets a key still inside its window | `TestTheCoalescerKeepsWhatIsStillInsideItsWindowWhenItCleansUp` |
| `expiry.go:33` ×3 | each missing dependency is refused on its own | `TestNewExpirerRefusesEachMissingDependencyAlone` |
| `writer.go:58` | a `Metrics` recorder the writer is given is the one it reports to | `TestTheWriterReportsTheOutboxToTheMetricsItWasGiven` |
| `expiry.go:37` | the same, for the expirer | `TestTheExpirerReportsThroughTheMetricsItWasGiven` |
| `expiry.go:57` ×2 | **an expiry removes the lot's remainder** — a negative delta, capped at what the pool holds, keyed on the lot. The sign was unasserted: a flipped one credits every customer whose credits expire | `TestAnExpiryRemovesTheLotsRemainderCappedAndKeyedOnTheLot` |

The 4 equivalent: `rehydrate.go:80` (`len(a)+len(b)` → `len(a)-len(b)` is a map capacity *hint*; a
negative hint is treated as zero), `coalesce.go:37` (`> 4096` → `>= 4096` prunes one entry sooner, and only
ever drops entries that had already expired, so no result changes), `coalesce.go:39` (`>=` → `>` keeps an
entry exactly one window old, which `allow` treats as expired either way), and `journal.go:93` (`delay > 5m` →
`>= 5m`; the delay is `1s << n`, a power of two of seconds, and 300 s is not one).

### What is not claimed

- **`metering/app` is measured at the unit tier only.** Booking, reconcile, recover and the writer's drain
  are verified by the integration tier against real Redis and Postgres, which a unit-tier mutation run
  cannot reach — hence a mutant coverage of about 32% and a floor of 30% in `scripts/mutation.sh`. Those
  paths carry the mutation-checking done by hand in T034, T034a and T051, and are the subject of
  **T135**. The session that produced these figures had no Docker, so the integration tier was not
  executed in it at all.
- **The other packages have no mutation target yet** — the adapters, `metering/config.go`, and the
  pricers other than `table`. The `table` pricer is the shared engine behind `llmtoken`, `seat` and
  `storagebyte`, so its 6 mutants stand for all four; the adapters are held by the conformance suites,
  which are themselves not mutation-tested until T135.
- **Fuzzing is bounded.** The targets in `metering/domain` and the `table` pricer found two defects in
  their first run; that is evidence about those targets over the search that was done, not a proof. CI
  searches 20 s per target per change and 10 minutes nightly. One 15 s sweep of all ten targets, run for
  this record, reported `FuzzPriceIsTheExactCeilingOfTheRational` as failed at the deadline with no failing
  input written; five further runs of that target (about four million executions) passed. The cause of
  the one failure was **not established** — a fuzz-engine artifact at the time limit is the likely
  explanation, and it is unverified. If it recurs in CI, the run's output names the input.
- **The order of the original commits was not recorded**, and cannot be reconstructed: for T020–T057 the
  claim is *"these invariants are now pinned and shown able to fail"*, not *"these were test-driven"*.
  Only work from Phase 2 on can be held to the second, which is what the `R###` tasks and `red-green` are
  for.
