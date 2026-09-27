# Data Model

**Spec**: [spec.md](./spec.md) | **Decisions**: [design-decisions.md](./design-decisions.md)

Conventions, and the reason each one is not negotiable:

- **UUID v7 primary keys** — time-sortable, so an append-only ledger clusters by insertion order.
- **Every row is realm-scoped.** `realm TEXT NOT NULL DEFAULT 'default'` on every table, in every index, in every query predicate. A query without it is a defect (metering invariant 11).
- **The billing subject is the opaque triple** `(realm, scope_kind, scope_id)` — never a host's tenant column. This is the single change that makes the schema reusable; see [D3](./design-decisions.md).
- **Integer money only.** `BIGINT` minor units + `CHAR(3)` ISO-4217 for fiat; `BIGINT` signed for credits. No `NUMERIC`, no `FLOAT`, anywhere.
- **`TIMESTAMPTZ`, UTC.**
- **Append-only where it is a record.** `credit_ledger`, `payments`, `payment_events` and audit rows are never updated in place, except for the narrow status fields noted.
- **Row-level security is available but not assumed.** The service authenticates callers and filters by realm in the query. A host embedding the library may add RLS on the scope triple; the schema is shaped for it.

`scope_tag` is stored as a generated column (`realm || '/' || scope_kind || ':' || scope_id`) wherever a single-column index or a bus subject needs it, so the derivation lives in one place.

---

## Metering core

### `account_credits`
The durable mirror of the hot balance. The ledger is authoritative; this row exists so a cold start has somewhere to rehydrate *to* and so a report can avoid summing the ledger.
- PK `(realm, scope_kind, scope_id)`
- `balance BIGINT NOT NULL DEFAULT 0` — signed; negative is permitted only under `allow_debt`
- `updated_at TIMESTAMPTZ NOT NULL`
- Renamed from the originating `workspace_credits`: the name encoded one host's tenancy.

### `credit_ledger`
The account of record. Append-only, partitioned by `created_at` (monthly); expiry is a partition `DROP`.
- `id` UUID v7, `realm`, `scope_kind`, `scope_id`
- `delta BIGINT NOT NULL` — **signed** ([D1](./design-decisions.md)). Negative consumption, positive grant.
- `operation_type TEXT NOT NULL` — open vocabulary, not an enum: host consumption types plus `purchase`, `subscription_grant`, `signup_grant`, `promo`, `refund`, `chargeback`, `expiry`, `writeoff`, `admin_adjustment`, `reconcile`. **Not a Postgres enum**, because adding a value to one is a migration and a host will invent types this schema cannot predict.
- `idem_key TEXT NOT NULL` — indexed for lookup. **Uniqueness lives in `credit_idem`, not here** — see below and [D21](./design-decisions.md).
- `rate_card_version TEXT` — which card priced it ([D6](./design-decisions.md)). Null for non-priced rows (grants, adjustments).
- `cost_micros BIGINT` — informational upstream cost, for usage analytics only. Never a billing input.
- `resource TEXT`, `rate_key TEXT`, `quantities JSONB` — the immutable event, so a dispute can re-derive the price rather than trust the stored one.
- `actor_id TEXT` — who caused it, for attribution. Opaque to the engine.
- `ref JSONB` — trace id, call id, payment id. Audit metadata, never a cost input.
- `occurred_at`, `created_at TIMESTAMPTZ NOT NULL`
- Indexes: `(realm, scope_kind, scope_id, created_at DESC)` for the ledger page; `(realm, idem_key)` non-unique for lookup; `(realm, created_at)` for reconcile.

### `credit_idem`
**The idempotency guard** — the constraint every exactly-once guarantee in the system rests on (FR-003, SC-002).
- PK `(realm, idem_key)` — realm-scoped ([D4](./design-decisions.md)), so two host products minting the same key stay independent
- `ledger_id UUID NOT NULL`, `delta BIGINT NOT NULL`, `created_at`
- **Why it is a separate, non-partitioned table** ([D21](./design-decisions.md)): Postgres requires a unique constraint on a partitioned table to include the partition key, so `PARTITION BY RANGE (created_at)` and a global `UNIQUE (realm, idem_key)` on `credit_ledger` are mutually exclusive — the inherited design specified both and is not constructible as written. Splitting them keeps both properties and is better than either compromise: the guard is a narrow, hot, fully-cached table, and it **outlives ledger partitions**, so dropping an aged partition cannot resurrect the ability to double-charge an old key.
- Rules: a credit-affecting write inserts here **first**, in the same transaction as the ledger row. `INSERT … ON CONFLICT DO NOTHING` returning no row *is* the replay signal, and `delta` lets the replay report the original outcome without scanning a partition.

### `credit_outbox_dead`
Terminal parking for intents that exhausted their retry budget. A dropped money intent is silent under-billing, so nothing is ever dropped (FR-043).
- `id`, `realm`, `scope_kind`, `scope_id`, `payload JSONB`, `idem_key`, `attempts INT`, `last_error TEXT`, `parked_at`, `replayed_at`
- Live outbox queues are Redis lists (`outbox:{shard}`); only the poison tail is durable here.

### `spend_journal`
Used **only** under `SettlementDurability=journal`: the durable intent written synchronously before returning, removing the under-bill RPO window of `outbox` mode (metering invariant 7).
- `id`, `realm`, `scope_kind`, `scope_id`, `delta BIGINT`, `idem_key` **UNIQUE per realm**, `operation_type`, `rate_card_version`, `created_at`, `settled_at`
- Empty in `outbox` mode.

### `rate_cards` / `rate_card_entries`
Prices as immutable, versioned configuration data ([D13](./design-decisions.md)), so repricing is an operator action rather than a release.
- `rate_cards`: `(realm, version)` PK, `micros_per_credit BIGINT NOT NULL CHECK (> 0)`, `published_at`, `retired_at`
- `rate_card_entries`: `(realm, version, rate_key, unit)` PK, `micros_per_unit BIGINT NOT NULL`
- Rules: **never updated in place.** A price change inserts a new `version`; old rows keep pricing old ledger entries identically. Exactly one card per realm has `retired_at IS NULL`.

### `limits`
Ceilings as per-realm configuration data ([D13](./design-decisions.md)) — which is what makes "the ceiling count is data, not three hardcoded cards" true on both sides of the wire.
- `id`, `realm`, `name`, `scope_kind` (whose counter — may differ from the charged scope), `unit`, `max BIGINT`, `window_kind` (`balance`|`daily`|`hourly`|`rolling`), `dur_seconds`, `warn_at NUMERIC(3,2)`, `deny_code`, `resource` (null = all), `sort_order`, `active`
- `UNIQUE (realm, name)`

### `credit_lots`
Present only when `CreditExpiry=lots_fifo` ([D2](./design-decisions.md)).
- `id`, `realm`, `scope_kind`, `scope_id`, `granted BIGINT`, `remaining BIGINT`, `expires_at`, `source_idem_key`, `created_at`
- Rules: consumption draws from the oldest unexpired lot first; an expiry sweep appends an `expiry` ledger row for each lapsed remainder, so the balance and the ledger stay equal.

---

## Payments (fiat)

### `plans`
- `id`, `realm`, `code` — **`UNIQUE (realm, code)`**. `code` is the host's slug and the only plan identifier that reaches a UI.
- `name`, `description`, `kind` (`one_time`|`subscription`)
- `credit_allotment BIGINT` — nullable; null means custom/negotiated. **The only coupling between fiat and credits.**
- `billing_interval` (`month`|`year`|null), `active BOOL`, `sort_order INT`, `created_at`, `updated_at`

### `plan_prices`
Per-currency pricing ([D12](./design-decisions.md)).
- `(plan_id, currency)` PK, `minor_units BIGINT NOT NULL`
- Rules: a plan with no row for a requested currency is simply not offered in it — never silently converted, because an FX rate applied here would be a price nobody set.

### `plan_provider_prices`
Maps one logical plan to each provider's external identifier, per currency.
- `id`, `plan_id`, `provider`, `currency`, `provider_price_id TEXT`
- `UNIQUE (provider, provider_price_id)`, `UNIQUE (plan_id, provider, currency)`
- Rules: lets one catalogue entry sell through any provider. A `provider_price_id` must never leave the server.

### `plan_entitlements`
What a plan confers besides credits ([D8](./design-decisions.md)).
- `id`, `plan_id`, `feature_key`, `value_kind` (`flag`|`quota`|`enum`), `value_bool`, `value_int BIGINT`, `value_text`
- `UNIQUE (plan_id, feature_key)`
- Rules: `value_int = -1` means **unlimited**, distinct from null ("unset"). The distinction is load-bearing; see entitlement invariant 6.

### `realm_defaults`
The free tier: what a scope with no subscription gets.
- `(realm, feature_key)` PK, same value columns as `plan_entitlements`

### `entitlement_overrides`
Deliberate per-scope exceptions.
- `id`, `realm`, `scope_kind`, `scope_id`, `feature_key`, value columns, `expires_at`
- `reason TEXT NOT NULL`, `actor TEXT NOT NULL`, `created_at`
- `UNIQUE (realm, scope_kind, scope_id, feature_key)`
- Rules: `reason` and `actor` are `NOT NULL` at the schema level, because an unexplained override is indistinguishable from a bug and is usually found during a revenue investigation. Every write also inserts an audit row in the same transaction.

### `billing_customers`
- `id`, `realm`, `scope_kind`, `scope_id`, `provider`, `provider_customer_id TEXT`, `billing_email`, `created_at`, `updated_at`
- `UNIQUE (realm, scope_kind, scope_id, provider)`, `UNIQUE (provider, provider_customer_id)`
- Rules: the **only** sanctioned path from a verified webhook to a scope (payments invariant 4). No provider identifier lives anywhere else.

### `subscriptions`
- `id`, `realm`, `scope_kind`, `scope_id`, `plan_id`, `provider`, `provider_subscription_id TEXT`
- `status` (`trialing`|`active`|`past_due`|`paused`|`canceled`|`incomplete`)
- `current_period_start`, `current_period_end`, `cancel_at_period_end BOOL`
- `grace_until TIMESTAMPTZ` — dunning window ([D10](./design-decisions.md))
- `created_at`, `updated_at`, `canceled_at`
- `UNIQUE (provider, provider_subscription_id)`, partial unique on one active subscription per `(realm, scope)`
- Rules: **`status` has exactly one writer — the webhook path** (payments invariant 12). Each paid period grants `credit_allotment` through a ledger row keyed by the invoice id, so a duplicated or out-of-order delivery converges.

### `payments`
- `id`, `realm`, `scope_kind`, `scope_id`, `provider`, `provider_payment_id TEXT`
- `plan_id` (nullable for ad-hoc), `kind` (`one_time`|`subscription_invoice`)
- `amount_minor BIGINT`, `currency CHAR(3)`, `tax_minor BIGINT` (as reported by the provider; never computed here), `credits_granted BIGINT`
- `status` (`pending`|`succeeded`|`failed`|`refunded`|`partially_refunded`|`disputed`)
- `receipt_url`, `failure_reason`
- `idem_key TEXT NOT NULL` — the key of the matching ledger grant row. **The reconciliation key between fiat and credits.**
- `created_at`, `updated_at`
- `UNIQUE (provider, provider_payment_id)`
- Rules: a `succeeded` payment maps 1:1 to exactly one grant row via `idem_key`. Refunds append a new negative grant row and update only `status` — amounts are never mutated (payments invariant 8).

### `payment_events`
Verified webhook events, for idempotent processing and replay safety.
- `id`, `realm`, `provider`, `provider_event_id TEXT`, `event_type TEXT`
- `payload_hash TEXT` — SHA-256 of the verified raw body. **The body itself is not stored**: it carries PII and is not needed once the facts are extracted.
- `status` (`received`|`processed`|`ignored`|`failed`|`unverifiable`)
- `scope_kind`, `scope_id` (nullable — resolved after parse), `received_at`, `processed_at`, `error TEXT`
- `UNIQUE (realm, provider, provider_event_id)` — **this unique constraint is the replay guard.** Insert-on-receive after verification; a conflict short-circuits processing (payments invariant 5).
- Rules: retained longer than the provider's replay window ([FR-027](./spec.md)); default 90 days. `unverifiable` rows are parked deliveries awaiting replay ([D17](./design-decisions.md)) — never processed optimistically.

---

## Operations

### `usage_daily`
A rollup, refreshed on a tick, backing the burn rate, the time series and the breakdown. A materialized view or a table; either way it is derived and never a billing input.
- `(realm, scope_kind, scope_id, day, dimension)` PK, `credits BIGINT`, `cost_micros BIGINT`, `events BIGINT`
- Rules: the member-facing breakdown filters to the caller's own `actor_id` unless they hold the admin entitlement ([D20](./design-decisions.md)).

### `reconcile_runs`
- `id`, `realm`, `shard`, `bucket`, `expected BIGINT`, `observed BIGINT`, `drift BIGINT`, `tolerance BIGINT`, `healed BOOL`, `alarmed BOOL`, `ran_at`
- Rules: every run writes a row whether or not it healed, so drift has a history and a trend rather than only an alert.

### `dead_letters`
- `id`, `realm`, `subject`, `payload JSONB`, `attempts INT`, `last_error`, `parked_at`, `replayed_at`

### `audit_log`
- `id`, `realm`, `actor`, `action`, `subject_kind`, `subject_id`, `before JSONB`, `after JSONB`, `reason`, `created_at`
- Rules: written for every privileged action — a grant not originating from a verified webhook, an override, a manual reconcile or rehydrate, a rate-card publication, a plan change.

---

## Migrations

| File | Contents |
|---|---|
| `0001_metering_core.sql` | `account_credits`, `credit_ledger` (+ partitions), `credit_outbox_dead`, `spend_journal`, `rate_cards`, `rate_card_entries`, `limits` |
| `0002_payments.sql` | `plans`, `plan_prices`, `plan_provider_prices`, `billing_customers`, `subscriptions`, `payments`, `payment_events` |
| `0003_entitlements.sql` | `plan_entitlements`, `realm_defaults`, `entitlement_overrides` |
| `0004_operations.sql` | `usage_daily`, `reconcile_runs`, `dead_letters`, `audit_log` |
| `0005_credit_lots.sql` | `credit_lots` — applied always, used only when `CreditExpiry=lots_fifo` |

Migrations travel with the module and are run by `cmd/payment-migrate`. `/readyz` fails while
the schema is behind head ([D15](./design-decisions.md)), so a rollout cannot serve money
against a half-migrated database.
