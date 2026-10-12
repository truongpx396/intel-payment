# Tasks: Post-paid Usage Invoicing

**Spec**: [spec.md](./spec.md) | **Contract**: [contracts/invoicing-ports.md](./contracts/invoicing-ports.md)

> **Test-first.** The rule of [001's tasks](../001-metering-billing-core/tasks.md) holds here for every
> task: a behavioural task is a pair sharing a number — **R###** (red: a tests-only commit that *fails*,
> for the reason it states) and then **T###** (green: the least code that makes it pass). Each red names
> the [invariant](./contracts/invoicing-ports.md#invariants) or requirement it pins, so a reviewer can
> check the test rather than the prose. A task that cannot be test-driven says `TDD-Exempt: <reason>`;
> the verifications of Phase E are measurements and each is its own red. The order of the *commits* is
> checked by `scripts/verify-red-green.sh`; the order of *this list* is checked by
> `scripts/check-task-pairs.sh` (it runs inside `scripts/check-spec-drift.sh`, CI job `drift`), which
> refuses a green with no red above it and a red nothing makes pass.
>
> Moved when this list was paired (no ID was reused for a different task): **T211**, **T212** and
> **T222** were suites numbered as greens, "the red for the tasks that make them pass"; they are now
> **R211**, **R212** and **R222**, and **T213** / **T221** are the greens that name them.

> **Gate**: do not start this until [001](../001-metering-billing-core/tasks.md) Phases 0–3 are implemented and passing. Post-paid rates the ledger 001 produces; building it first means rating a schema that does not exist yet.

<!-- pairs:start -->
## Phase A — Schema & settlement model
- [ ] **R201** `scripts/verify-schema.sh` asserts the Phase 2 schema (`schema-assertions-002.sql`) on the schema `migrations/` produces **alone**, with no draft applied on top. It fails while `0101` is still a draft; the D38 rule that the draft never ships stays in force until this same change retires it.
- [ ] **T201** Move [`draft-migrations/0101_postpaid_invoicing.sql`](./draft-migrations/0101_postpaid_invoicing.sql) into `migrations/` as the next number **in the same change as the Phase 2 code** — never before (D38). Forward-only: there is no down migration. Makes **R201** green.
- [x] **T202** The finalize-immutability triggers on `invoices`/`invoice_lines` and on credit notes. Enforced in the database, not the application. *In the draft; verified by `scripts/schema-assertions-002.sql` in CI — its own red: the assertions fail against a schema without the triggers.*
- [ ] **R203** Invoice numbers are gapless per realm (**invariant 8**), integration on Postgres: *N* concurrent finalizes in one realm take exactly the numbers 1…*N*; a finalize that rolls back leaves no gap; a voided invoice keeps its number and `void` status and the number is never issued again. The test is shown to fail against an allocation from a Postgres `SEQUENCE`, which gaps on rollback.
- [ ] **T203** `invoice_numbers` allocation under `FOR UPDATE` inside the finalize transaction — **not** a Postgres `SEQUENCE`. Makes **R203** green.
- [ ] **R204** `scope_settlement` and the `period_to_date` window: the table's constraints are asserted in `schema-assertions-002.sql`; the window bucket resets exactly at the period boundary, as a property over start dates and zones including one that skips a calendar date (the class of defect fuzzing found in `window.go`); `credit_limit_reached` is a deny code distinct from every prepaid one, with a row in the status mapping of 001's R061.
- [ ] **T204** `scope_settlement`; `limits.window_kind` gains `period_to_date`; `deny_code = 'credit_limit_reached'`. Makes **R204** green.
- [ ] **R205** Admission for a post-paid scope (**FR-102**, **FR-103**, **invariant 12**): admitted below the spend cap and denied at it with `credit_limit_reached`, tested at the cap minus one micro, at the cap and above it, in integer micros; a negative balance is admitted and never clamped or written off; a *prepaid* scope in the same realm is still clamped by `NegativeBalancePolicy`.
- [ ] **T205** Admission honours the post-paid spend cap and skips `NegativeBalancePolicy` for post-paid scopes. Makes **R205** green.

## Phase B — Rating (pure, no infra)
- [ ] **R210** `invoicing/domain` — unit and fuzz: lines sum to the subtotal or an explicit residual line is present (**invariant 2**), against an independent `math/big` computation; every amount is integer micros and no message or struct has a `float` field (**invariant 11**); periods are half-open and consecutive periods tile the timeline with no gap or overlap.
- [ ] **T210** `invoicing/domain` — schedules, tiers, commitments, periods, lines, invoices, credit notes. Makes **R210** green.
- [ ] **R211** *(was T211)* **`RaterContract` before the implementation** (**invariants 1, 5, 6, 7**): graduated ≠ volume on identical input, package rounds up, mid-period card change prices historically, re-rating is byte-identical, minimum adds a shortfall without inflating usage, lines sum to subtotal, trial rates to zero with the would-be cost visible. Run first against a stub rater in the test package that rates everything at zero, so the red is an assertion and not a build failure; and shown to have teeth against a rater that computes volume as graduated.
- [ ] **R212** *(was T212)* Fixture suite from **published vendor pricing pages** with hand-checked arithmetic (**SC-106**). A rating engine that cannot reproduce a real public price table cannot be trusted with a customer's.
- [ ] **T213** `invoicing/app/rate.go` — graduated, volume, package, flat. Micros throughout; round **once** at the line (invariant 11). Makes **R211** and **R212** green.
- [ ] **R214** Commitments (**invariant 7**): a minimum shortfall is its own line and the usage lines are unchanged by it; a commitment met exactly adds no shortfall; a prepaid drawdown is a negative line that reports the remainder.
- [ ] **T214** Commitments: minimum shortfall as its own line; prepaid drawdown as a negative line reporting the remainder. Makes **R214** green.
- [ ] **R215** Discounts (**invariant 13**, **FR-119**): one table test of the documented order (`line → product → invoice`; `fixed → percentage`; ties by code) over every permutation of a mixed set, which must all produce the same result; every discount that did not apply is recorded with its reason.
- [ ] **T215** `DiscountEngine` with the documented order. Record every non-application with its reason. Makes **R215** green.
- [ ] **R216** Trials (**FR-121**): usage is rated normally, the amounts are zero and the would-be cost is kept; the instant the trial ends, on both sides of it; the lines still sum to the subtotal (**invariant 2**).
- [ ] **T216** Trials: rate normally, zero the amounts, keep the would-be cost. Makes **R216** green.

## Phase C — Periods & close
- [ ] **R220** `ClaimClose` (**invariant 9**, **FR-105**), integration: two concurrent claims on one `(scope, period)` — exactly one wins; a claim abandoned in `closing` is resumed by the next attempt and not re-claimed as new; a partial invoice is never `finalized`.
- [ ] **T220** `Periods` — `ClaimClose` as an atomic conditional `UPDATE`; resumable `closing`. Makes **R220** green.
- [ ] **R221** The close tick (**invariant 9**): run twice, it produces one invoice; killed after each of its steps (claim, aggregate, rate, discount, draft, finalize, collect) and run again, it completes with one invoice and no duplicate line; a period's events are never loaded into memory — a fake store fails the test if the per-event read is called, since the aggregate is computed in SQL.
- [ ] **R222** *(was T222)* **Concurrency test**: 8 simultaneous closes on one period produce exactly **one** invoice (**SC-105**).
- [ ] **T221** `billing.period.close.tick` handler: claim → aggregate the period's usage per (unit, schedule version) in SQL → rate → discount → draft → finalize → collect. Idempotent end to end, and never loads a period's events into memory. Makes **R221** and **R222** green.
- [ ] **R223** `LateEventPolicy` (**invariant 10**, **FR-111**): each of `next_period` (the default), `reopen` (a draft only) and `reject`; a `late_events` row every time; a finalized invoice is byte-identical before and after a late event, and the event keeps its original `occurred_at`.
- [ ] **T223** `LateEventPolicy` — `next_period` (default) | `reopen` (draft only) | `reject`, with a `late_events` row every time. Makes **R223** green.
- [ ] **R224** Period anchoring, as a property: for every start day — the 29th, 30th and 31st, 29 February, and a zone that skips a calendar date — calendar-month and subscription-anniversary periods tile the timeline with no gap and no overlap.
- [ ] **T224** Period anchoring: calendar month and subscription anniversary. Makes **R224** green.

## Phase D — Invoices & collection
- [ ] **R230** `Finalize` (**invariants 2, 3, 4**): lines sum to the subtotal or a `residual` line is written; a line missing its `source_*` is refused; a finalized invoice and its lines cannot be changed by any path the store exposes, nor by SQL (the triggers of T202).
- [ ] **T230** `Invoices` store; `Finalize` asserts lines sum to subtotal or writes a `residual` line, and refuses a line missing `source_*`. Makes **R230** green.
- [ ] **R231** `IssueCreditNote` is the only correction path: `reason` and `actor` are required; a draft is refused (it is edited, not credited); the original invoice is byte-identical afterwards.
- [ ] **T231** `IssueCreditNote` — the only correction path; `reason` and `actor` required. Makes **R231** green.
- [ ] **R232** `Collector` (**invariant 15**, **FR-117**): handing an invoice to the collector leaves it unpaid; `paid` arrives only from a verified provider webhook, through 001's verify-before-anything ingress; a replayed webhook changes nothing; an event for an unknown invoice is parked, not dropped.
- [ ] **T232** `Collector` over the existing Stripe adapter (hosted invoices); status from webhooks only. Makes **R232** green.
- [ ] **R233** Tax (**invariant 14**, **FR-118**): a provider-reported amount is carried verbatim as its own line; an `external_hook` that fails leaves the invoice a draft (fail closed); mode `none` adds no line; nothing in the repository computes a tax amount.
- [ ] **T233** Tax as a carried line from the provider or the `external_hook`. Makes **R233** green.
- [ ] **R234** Invoice endpoints: every route is in `api/openapi/v1.yaml` and every path in it is registered (001's R063b); the authorization matrix generated from the route table (001's R065) has a row for each; a member-scoped call never returns another member's invoice or credit note.
- [ ] **T234** REST + gRPC: list invoices, get invoice with lines, download link, list credit notes. Makes **R234** green.
- [ ] **R235** Proration (**FR-122**, **invariant 11**): a mid-period quantity change under each `PlanChangePolicy`; the prorated lines sum exactly to the rated total with rounding done once, at the line; a change at the exact period boundary prorates nothing.
- [ ] **T235** `subscriptions.quantity` + proration under `PlanChangePolicy`. Makes **R235** green.

## Phase E — Verification
These are measurements of the finished phase, so there is no pair: each is its own red, and a miss is a design finding.

- [ ] **T240** Re-rate every closed period in a seeded year; assert byte-identical (**SC-101**). *(A measurement: its own red.)*
- [ ] **T241** Assert `SUM(lines) = subtotal` across every invoice in the corpus (**SC-102**). *(A measurement: its own red.)*
- [ ] **T242** Every line's `source_ledger_query` re-executed and its events re-priced to the line amount (**SC-103**). *(A measurement: its own red.)*
- [ ] **T243** Attempt to mutate a finalized invoice through every path; all must fail (**SC-104**). *(A measurement: its own red.)*
- [ ] **T244** **Hand-check a real period against a spreadsheet** with someone who would notice being overcharged. Not a unit test — the thing unit tests cannot do. *(A measurement: its own red.)*
- [ ] **T245** Gapless-number audit across a simulated year including rollbacks and voids. *(A measurement: its own red.)*
<!-- pairs:end -->

## Definition of done
Same as [001](../001-metering-billing-core/tasks.md) — including red before green, properties for arithmetic, and mutation of the lines touched — plus: no invoice in any test corpus presents arithmetic that does not close, and no rating result differs between two runs.
