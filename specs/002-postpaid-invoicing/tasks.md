# Tasks: Post-paid Usage Invoicing

**Spec**: [spec.md](./spec.md) | **Contract**: [contracts/invoicing-ports.md](./contracts/invoicing-ports.md)

> **Gate**: do not start this until [001](../001-metering-billing-core/tasks.md) Phases 0–3 are implemented and passing. Post-paid rates the ledger 001 produces; building it first means rating a schema that does not exist yet.

## Phase A — Schema & settlement model
- [ ] **T201** `migrations/0006_postpaid_invoicing.sql` + `.down.sql` — every table in [data-model.md](./data-model.md).
- [ ] **T202** The finalize-immutability trigger on `invoices`/`invoice_lines`. Enforced in the database, not the application: invariant 3 is the artifact's only real property and application code is not where you defend it.
- [ ] **T203** `invoice_numbers` allocation under `FOR UPDATE` inside the finalize transaction — **not** a Postgres `SEQUENCE`, which gaps on rollback (invariant 8).
- [ ] **T204** `scope_settlement`; `limits.window_kind` gains `period_to_date`; `deny_code = 'credit_limit_reached'`.
- [ ] **T205** Admission honours the post-paid spend cap and skips `NegativeBalancePolicy` for post-paid scopes (**FR-102**, **FR-103**, invariant 12).

## Phase B — Rating (pure, no infra)
- [ ] **T210** `invoicing/domain` — schedules, tiers, commitments, periods, lines, invoices, credit notes.
- [ ] **T211** `RaterContract` **before** the implementation: graduated ≠ volume on identical input, package rounds up, mid-period card change prices historically, re-rating is byte-identical, minimum adds a shortfall without inflating usage, lines sum to subtotal, trial rates to zero with the would-be cost visible.
- [ ] **T212** Fixture suite from **published vendor pricing pages** with hand-checked arithmetic (**SC-106**). A rating engine that cannot reproduce a real public price table cannot be trusted with a customer's.
- [ ] **T213** `invoicing/app/rate.go` — graduated, volume, package, flat. Micros throughout; round **once** at the line (invariant 11).
- [ ] **T214** Commitments: minimum shortfall as its own line; prepaid drawdown as a negative line reporting the remainder.
- [ ] **T215** `DiscountEngine` with the documented order (`line → product → invoice`; `fixed → percentage`; ties by code). Record every non-application with its reason (**FR-119**).
- [ ] **T216** Trials: rate normally, zero the amounts, keep the would-be cost (**FR-121**).

## Phase C — Periods & close
- [ ] **T220** `Periods` — `ClaimClose` as an atomic conditional `UPDATE`; resumable `closing` (**FR-105**).
- [ ] **T221** `billing.period.close.tick` handler: claim → gather events → rate → discount → draft → finalize → collect. Idempotent end to end.
- [ ] **T222** Concurrency test: 8 simultaneous closes on one period produce exactly **one** invoice (**SC-105**).
- [ ] **T223** `LateEventPolicy` — `next_period` (default) | `reopen` (draft only) | `reject`, with a `late_events` row every time (**FR-111**).
- [ ] **T224** Period anchoring: calendar month and subscription anniversary.

## Phase D — Invoices & collection
- [ ] **T230** `Invoices` store; `Finalize` asserts lines sum to subtotal or writes a `residual` line (invariant 2), and refuses a line missing `source_*` (invariant 4).
- [ ] **T231** `IssueCreditNote` — the only correction path; `reason` and `actor` required.
- [ ] **T232** `Collector` over the existing Stripe adapter (hosted invoices); status from webhooks only (**FR-117**, invariant 15).
- [ ] **T233** Tax as a carried line from the provider or the `external_hook` (**FR-118**).
- [ ] **T234** REST + gRPC: list invoices, get invoice with lines, download link, list credit notes.
- [ ] **T235** `subscriptions.quantity` + proration under `PlanChangePolicy` (**FR-122**).

## Phase E — Verification
- [ ] **T240** Re-rate every closed period in a seeded year; assert byte-identical (**SC-101**).
- [ ] **T241** Assert `SUM(lines) = subtotal` across every invoice in the corpus (**SC-102**).
- [ ] **T242** Every line's `source_ledger_query` re-executed and its events re-priced to the line amount (**SC-103**).
- [ ] **T243** Attempt to mutate a finalized invoice through every path; all must fail (**SC-104**).
- [ ] **T244** **Hand-check a real period against a spreadsheet** with someone who would notice being overcharged. Not a unit test — the thing unit tests cannot do.
- [ ] **T245** Gapless-number audit across a simulated year including rollbacks and voids.

## Definition of done
Same as [001](../001-metering-billing-core/tasks.md), plus: no invoice in any test corpus presents arithmetic that does not close, and no rating result differs between two runs.
