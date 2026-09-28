# Data Model — Post-paid Invoicing

**Spec**: [spec.md](./spec.md) | **Extends** [001's data model](../001-metering-billing-core/data-model.md), changes none of it.

Same conventions: realm-scoped rows, the opaque `(realm, scope_kind, scope_id)` triple, integer minor units, `TIMESTAMPTZ` UTC, append-only where it is a record.

Migration: a **draft**, [`draft-migrations/0101_postpaid_invoicing.sql`](./draft-migrations/0101_postpaid_invoicing.sql) — verified in CI on top of 001's baseline, shipped into `migrations/` only with the Phase 2 code, and forward-only ([D38](../001-metering-billing-core/design-decisions.md)).

## Settlement model

### `scope_settlement`
- PK `(realm, scope_kind, scope_id)`
- `model TEXT NOT NULL CHECK (model IN ('prepaid_credits','postpaid_invoice','hybrid'))`
- `spend_cap_minor BIGINT` — post-paid only: the period-to-date ceiling admission gates on. `NULL` = uncapped, which should be a deliberate decision rather than an omission.
- `currency CHAR(3)`, `period_anchor` (`calendar_month` | `anniversary`), `anchor_day SMALLINT`
- `effective_from TIMESTAMPTZ NOT NULL`
- Rules: a change takes effect at the **next** period boundary. `billing_periods.model` is stamped at open, so a customer's terms never change under them mid-cycle (FR-101).

## Periods

### `billing_periods`
- `id`, `realm`, `scope_kind`, `scope_id`, `period_start`, `period_end`
- `status` (`open`|`closing`|`closed`|`invoiced`), `model` (stamped at open)
- `closed_at`, `closing_claimed_by`, `closing_claimed_at`
- `UNIQUE (realm, scope_kind, scope_id, period_start)`; partial unique on one `open` period per scope
- Rules: `open → closing` is an atomic conditional `UPDATE` — that **is** the idempotent claim behind FR-105, so two schedulers produce one invoice. A `closing` period is resumable.

## Rating configuration

### `rate_schedules` / `rate_schedule_tiers`
- `rate_schedules`: PK `(realm, version, product, unit)`, `kind` (`flat`|`graduated`|`volume`|`package`), `package_size BIGINT`, `currency`, `published_at`, `retired_at`
- `rate_schedule_tiers`: PK `(realm, version, product, unit, tier_index)`, `up_to BIGINT` (NULL = unbounded), `unit_price_micros BIGINT`, `flat_micros BIGINT`
- Rules: **insert-only**, like metering rate cards. A price change publishes a new `version`; closed periods re-rate identically forever (FR-107). `kind` is stored, never inferred — graduated and volume differ materially on identical inputs (invariant 6). Exactly one non-retired version per `(realm, product, unit)`.

### `commitments`
- `id`, `realm`, `scope_kind`, `scope_id`, `kind` (`minimum`|`prepaid`)
- `amount_minor BIGINT`, `remaining_minor BIGINT` (prepaid), `currency`
- `starts_at`, `ends_at`, `rollover_unused BOOL`
- Rules: a `minimum` shortfall becomes its own invoice line and never inflates a usage line (invariant 7). A `prepaid` drawdown appends a negative line and reports what remains.

### `discounts`
- `id`, `realm`, `code`, `kind` (`percentage`|`fixed`), `percent NUMERIC(5,2)`, `amount_minor BIGINT`, `currency`
- `applies_to` (`invoice`|`product`|`line`), `product TEXT`
- `valid_from`, `valid_until`, `max_redemptions INT`, `redemptions INT NOT NULL DEFAULT 0`
- `UNIQUE (realm, code)`
- Rules: application order is `line → product → invoice`, and within each `fixed → percentage`, ties by `code` ascending. The order is **contract**, not implementation: percentage-then-fixed and fixed-then-percentage give different totals, and a customer who recomputes must land on our number (FR-119).

### `scope_discounts`
- `(realm, scope_kind, scope_id, discount_id)` PK, `applied_count`, `attached_at`

## Invoices

### `invoices`
- `id`, `realm`, `scope_kind`, `scope_id`, `period_id` → `billing_periods`
- `number TEXT NOT NULL` — **`UNIQUE (realm, number)`**, gapless per realm (invariant 8)
- `subtotal_minor`, `tax_minor`, `total_minor` BIGINT, `currency CHAR(3)`
- `status` (`draft`|`finalized`|`sent`|`paid`|`void`|`uncollectible`)
- `issued_at`, `due_at`, `finalized_at`, `provider_invoice_id`, `not_applied JSONB`
- `UNIQUE (period_id)` — one invoice per period, which is the durable half of FR-105
- Rules: **`draft` is the only mutable status.** Past `finalized` the row is immutable except `status` and `provider_invoice_id`; a DB trigger enforces this rather than trusting application code, because the artifact's only real property is that it does not change (invariant 3). A `void` keeps its number; numbers are never reused.

### `invoice_lines`
- `id`, `invoice_id`, `line_index INT`, `kind` (`usage`|`subscription`|`minimum_shortfall`|`commitment_drawdown`|`discount`|`credit`|`tax`|`residual`)
- `description`, `product`, `unit`, `quantity BIGINT`
- `schedule_kind`, `schedule_version`, `rate_card_versions TEXT[]`
- `amount_minor BIGINT` (**signed**), `currency`
- `source_period_start`, `source_period_end`, `source_resource`, `source_ledger_query TEXT`, `source_event_count BIGINT`
- `UNIQUE (invoice_id, line_index)`
- Rules: `SUM(amount_minor) = invoices.subtotal_minor`, or a `residual` line is present — asserted by a check at finalize (invariant 2). `source_*` is what makes a disputed line answerable (invariant 4); a line without it is not finalizable.

### `credit_notes` / `credit_note_lines`
- `credit_notes`: `id`, `realm`, `scope_*`, `number` (**`UNIQUE (realm, number)`**, its own gapless sequence), `against_invoice_id`, `total_minor`, `currency`, `reason TEXT NOT NULL`, `actor TEXT NOT NULL`, `issued_at`
- `credit_note_lines`: same shape as `invoice_lines`, amounts negative
- Rules: the **only** sanctioned correction to a finalized invoice. `reason` and `actor` are `NOT NULL` at the schema level, because an unexplained credit note is indistinguishable from fraud and is exactly what an auditor looks for.

### `invoice_numbers`
- `(realm, kind)` PK where `kind ∈ ('invoice','credit_note')`, `next_value BIGINT NOT NULL`, `format TEXT`
- Rules: allocated under `SELECT … FOR UPDATE` **inside the finalize transaction**, so a rolled-back finalize consumes no number. A gap is an audit finding in most jurisdictions, so the sequence must not be a Postgres `SEQUENCE` — those intentionally gap on rollback.

## Late events

### `late_events`
- `id`, `realm`, `scope_*`, `ledger_idem_key`, `original_occurred_at`, `closed_period_id`, `applied_period_id`, `policy_applied`, `recorded_at`
- Rules: every event arriving after its period closed is recorded here with the policy that handled it, so an invoice that *looks* short can be explained. The default `next_period` preserves `original_occurred_at` for audit while accruing into the open period (FR-111, invariant 10).

## Changes to 001 (additive only)

| 001 object | Change |
|---|---|
| `credit_ledger` | unchanged. Post-paid reads the same rows; nothing about recording differs by settlement model (FR-104) |
| `limits` | `window_kind` gains `period_to_date` for the post-paid spend cap, with `deny_code = 'credit_limit_reached'` — distinct from `payment_required` (FR-102) |
| `subscriptions` | gains `quantity INT NOT NULL DEFAULT 1` and `trial_end TIMESTAMPTZ` (FR-121, FR-122) |
| `plans` | gains `trial_days INT` |

Nothing is dropped and nothing is re-keyed, so 001 deployments migrate forward without touching money tables. The one non-additive statement widens two `limits` CHECK constraints, which rejects no existing row.
