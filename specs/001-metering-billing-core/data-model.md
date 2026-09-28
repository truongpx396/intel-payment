# Data Model

**Spec**: [spec.md](./spec.md) | **Decisions**: [design-decisions.md](./design-decisions.md) | **Mechanism**: [hot-path-consistency.md](./contracts/hot-path-consistency.md) | **Verified by**: [`scripts/verify-schema.sh`](../../scripts/verify-schema.sh)

Conventions, and the reason each one is not negotiable:

- **UUID v7 primary keys** from the application — time-sortable, so an append-only table clusters by insertion order. (`gen_random_uuid()` defaults are a fallback only.)
- **Every money row is realm-scoped.** `realm TEXT NOT NULL DEFAULT 'default'` in every table, index and predicate (metering invariant 11). The exceptions are deployment-wide infrastructure — `hot_config`, `hot_shards`, `reconcile_runs` — and `provider_accounts`, which lists the realms it serves.
- **The billing subject is the opaque triple** `(realm, scope_kind, scope_id)` — never a host's tenant column ([D3](./design-decisions.md)).
- **Integer money only.** `BIGINT` credits in a fine accounting unit; `BIGINT` minor units + `CHAR(3)` for fiat; prices as integer rationals. No `NUMERIC` money, no `FLOAT`, anywhere.
- **`TIMESTAMPTZ`, UTC.**
- **Append-only where it is a record.** `credit_ledger`, `payments`, `payment_events` and `audit_log` are never rewritten, except the narrow status fields noted; rate cards are insert-only **by trigger**.
- **Configuration is audited by the database.** Every write to a configuration table writes an `audit_log` row and a `NOTIFY intelpay_config` in the same transaction ([D37](./design-decisions.md)).
- **Partitions are created ahead of need** by `ensure_monthly_partitions` / `ensure_daily_partitions` (run at migration and by `billing.partitions.tick`); a DEFAULT partition catches a gap and pages if it ever holds a row.

---

## Hot tier state (deployment-wide)

### `hot_config`
One row: `shards` — the Redis placement count. A process configured with a different count refuses to start.

### `hot_shards`
Per shard: `gen` (the authoritative generation that `meta:{s<n>}` mirrors), `frozen`, `frozen_reason`, `node_replid` (the Redis replication id last seen serving it — how a restart or failover is detected). A generation bump after freeze + drain is the one recovery procedure ([§5](./contracts/hot-path-consistency.md#5-cold-start-loss-regression-and-freeze)).

---

## Metering core

### `credit_pools`
Per realm: `name`, `priority` (lower drawn first), `applies_to TEXT[]` (resources; `NULL` = all). `general` is implicit — always present, applies to everything, drawn last — and may not be redefined.

### `account_credits`
**Booked** balances: the ledger's running sum per `(realm, scope_kind, scope_id, pool)`, maintained by the sole durable writer in the same transaction as every ledger row. The hot tier equals `booked + open suspense` at every sequence number.

### `account_watermarks`
Per scope: `gen`, `applied_seq` (the highest **contiguously** booked hot sequence number), `blocked`, `updated_at` (indexed — reconcile checks scopes booked since its last run, so its cost follows activity, not history).

### `credit_ledger`
The account of record. Append-only, partitioned by `created_at` (monthly).
- `id`, `realm`, `scope_kind`, `scope_id`, **`pool`**
- `delta BIGINT` — **signed** ([D1](./design-decisions.md))
- `operation_type` — open vocabulary. Engine-reserved: `writeoff`, `expiry`, `allocation_out`, `allocation_in`, `admin_adjustment`. **There is no `reconcile` type**: reconcile heals the hot side and never books a row.
- `counter_scope_kind` / `counter_scope_id` — the counterparty of a transfer row
- `idem_key` — lookup only; uniqueness lives in the guards below
- `gen`, `seq_from`, `seq_to`, `event_count` — the hot sequence numbers the row books. `event` granularity: one event per row. `rollup` granularity: one row per (scope, pool, resource, rate key, card version) per drain batch, with per-event detail in `usage_events`
- `rate_card_version`, `cost_micros`, `resource`, `rate_key`, `quantities JSONB`, `subjects JSONB`, `actor_id`, `ref JSONB`, `occurred_at`, `created_at`
- Old partitions are **detached and archived**, never dropped, and only once `ledger_checkpoints` covers them.

### `ledger_checkpoints`
Per (scope, pool): `through_created_at`, `balance`, `rows_counted`. The deep audit proves `booked == checkpoint + Σ ledger since`; checkpoints are what make archiving a partition safe without losing the ability to prove a balance.

### `credit_idem`
**The guard for operations that mint or move money** — grants, refunds, chargebacks, transfers (both legs), expiry, corrections, admin adjustments.
- PK `(realm, op, idem_key)` — unique per realm **and operation**: one provider payment can be granted once, for one scope, and a usage key never collides with a grant key
- `fingerprint`, `scope_kind`, `scope_id`, `gen`, `seq` (the intent that used it — how a redelivery is told apart from a regression), `ledger_id`, `created_at`
- **Permanent and non-partitioned**: low volume, and it must outlive ledger partitions so archiving one cannot resurrect a double grant ([D21](./design-decisions.md)).

### `usage_idem`
**The guard for metered consumption** — high volume, so **window-bounded** ([D32](./design-decisions.md)).
- `(realm, scope_kind, scope_id, idem_key)`, `fingerprint`, `gen`, `seq`, `created_at`; partitioned **daily**, and a partition is dropped once past `UsageIdemWindow` (default 72 h)
- Uniqueness across the window is enforced by the writer under a per-scope advisory lock, probing at most window-days + 1 partitions; the per-partition primary key is the backstop within a day
- `Record` refuses an event whose `occurred_at` is older than the window (`422 stale_event`), so a retry that outlives its guard fails loudly instead of charging twice

### `credit_suspense`
Hot-tier movements the books do not accept — `duplicate` (a replay after its hot guard expired), `idem_conflict`, `poison`, `expiry_trueup` — with `pool`, `delta`, the opening `(gen, seq)`, `status open|closed`. Opened in the transaction that advances the watermark; closed when the writer's correction intent is booked. Reconcile compares hot against `booked + open suspense`.

### `credit_transfers`
One row per transfer: source and destination scope and pool, `amount`, `status in_transit|settled`. `Σ credit_ledger + Σ amount WHERE in_transit` is invariant across every step ([§4](./contracts/hot-path-consistency.md#4-transfer-two-phases-conserved-at-every-step)); a transfer in transit too long pages.

### `credit_outbox_dead`
Intents that exhausted their retry budget: `op`, `gen`, `seq`, `payload`, `attempts`, `last_error`. Parked, never dropped; parking opens a `poison` suspense entry so the watermark advances past it.

### `spend_journal`
The durable home of a usage intent that has not reached the hot tier yet — on every call under `Settlement=journal`, and during a hot-store outage in any mode. `(realm, scope, idem_key)` unique, `fingerprint`, `amount` (priced once, at `Record`), `rate_card_version`, `payload`, `status pending|settled`, retry fields. `billing.journal.tick` replays pending rows through the hot function.

### `usage_events`
Per-event detail under `LedgerGranularity=rollup` (the default `UsageArchive` adapter; a columnar store is the scale-out adapter). Daily partitions, archived on `UsageArchiveRetention`. Unused under `event` granularity, where the ledger row is the event.

### `rate_cards` / `rate_card_entries`
Prices as immutable, versioned data ([D13](./design-decisions.md), [D31](./design-decisions.md)).
- `rate_cards`: `(realm, version)` PK, `pricer` (registry name, default `table`), `params JSONB`, `published_at`, `retired_at`. Exactly one active card per realm (a partial unique index).
- `rate_card_entries`: `(realm, version, rate_key, unit)` PK, **`credits_per_block`, `block_size`** — a price is a rational, so $0.15 per million tokens is exactly `(150000, 1000000)` at 1 credit = 1 µ$ — and `cost_micros_per_block` (informational).
- **Insert-only, enforced by trigger.** The only permitted update is retiring an active card; repricing is retire + publish.

### `limits`
Ceilings as per-realm data.
- `name` (unique per realm), `subject_kind` (NULL = the charged scope; otherwise the subject a call must carry), `resource` (NULL = all), `unit`, `max`, **`max_entitlement`** (size the ceiling from this quota key: plan, override or free tier), `window_kind` (`balance` · `daily` · `hourly` · `rolling` · `job`), `dur_seconds`, `tz`, `warn_at`, `deny_code`, `sort_order`, `active`
- Checks: a `balance` limit is the charged scope's and counts credits, and its `max` is the permitted overdraft; a `job` limit's subject is `job`; `rolling` and `job` need a duration.

### `balance_watches`
A low-water mark on one pool of one scope, with a `key` naming what it triggers (`auto_recharge`, or a host-defined notice). The hot function flags the intent that crosses it.

### `credit_lots`
Only under `CreditExpiry=lots_fifo`: per-grant lots **per pool**, `granted`, `remaining`, `expires_at`. Maintained by the writer as it books intents in sequence order, so `remaining` is exact at the watermark; expiry is pre-applied at that upper bound and trued up.

---

## Payments (fiat)

### `provider_accounts`
One set of provider credentials: `id` (the `{account}` in `/webhooks/{provider}/{account}`), `provider` (an open vocabulary — a new provider needs no migration), `realms TEXT[]` it may serve, `mode live|test`, `secret_ref` / `webhook_secret_ref` (names of env vars or secret-manager paths — **never the secrets**).

### `plans`
`realm`, `code` (**`UNIQUE (realm, code)`**, the only plan id a UI sees), `name`, `kind one_time|subscription`, `credit_allotment` (the only coupling between fiat and credits), **`pool`** (where the allotment is granted), `billing_interval`, `active`, `sort_order`.

### `plan_prices`
`(plan_id, currency)` PK, `minor_units`. A plan without a row for a currency is not offered in it — never silently converted.

### `plan_provider_prices`
`plan_id`, `provider_account`, `currency`, `provider_price_id` — `UNIQUE (provider_account, provider_price_id)`, `UNIQUE (plan_id, provider_account, currency)`. A provider price id never leaves the server.

### `billing_customers`
`(realm, scope)` ↔ `(provider_account, provider_customer_id)`, unique both ways. **The only sanctioned path from a verified webhook to a scope — and so to a realm** (payments invariant 4).

### `subscriptions`
`realm`, scope, `plan_id`, `provider_account`, `provider_subscription_id`, `status`, `current_period_*`, `cancel_at_period_end`, `grace_until`, **`last_event_at`** (the provider timestamp of the newest state applied — an older event never overwrites a newer state), one live subscription per scope.

### `payments`
`provider_account`, `provider_payment_id` (unique together), `kind one_time|subscription_invoice|auto_recharge`, `amount_minor`, `currency`, `tax_minor` (as reported), `credits_granted`, **`pool`**, **`refunded_minor`** and **`credits_revoked`** (cumulative — partial refunds revoke `ceil(credits_granted × refunded / amount)` in total, never more than granted), `status` (… `disputed`, `charged_back`), `dispute_id`, `dispute_status`, `idem_key` (the grant's key — the reconciliation key between fiat and credits).

### `payment_events` — the webhook inbox
- `provider_account`, `provider`, `provider_event_id` — **`UNIQUE (provider_account, provider_event_id)` is the replay guard**
- `event_type`, `event_created_at` (the provider's clock), `payload_hash`, **`object JSONB`** (the normalized, verified facts — not the raw body)
- `parked_body` / `parked_headers` — only while `unverifiable`, deleted when verification succeeds (a check constraint enforces it)
- resolved `realm` and scope; `status received|processing|processed|ignored|failed|dead|unverifiable`; `attempts`, `next_attempt_at`, `lease_until`, `error`
- Rules: persisted and acknowledged `200` before any processing; processed from the table by lease with retries ([D34](./design-decisions.md)). Retained past the provider's replay window (default 90 days).

### `auto_recharge`
Per scope: `enabled`, `pool`, `threshold`, `plan_code` (a one-time pack), `currency`, `provider_account`, `payment_method_ref` (the provider's reference — never card data), `max_per_day`, `consecutive_failures`, `disabled_reason`.

---

## Entitlements

### `entitlement_keys`
The realm's declared capabilities: `key`, `kind flag|quota|enum`, `enum_order` (required for enums). An undeclared key is denied.

### `plan_entitlements` · `realm_defaults` · `entitlement_overrides`
Values per plan, the free tier, and audited per-scope exceptions (`reason` and `actor` `NOT NULL`). `value_int = -1` is **unlimited**, distinct from `NULL` ("unset"). A quota here can size a metering limit through `limits.max_entitlement`.

---

## Operations

### `usage_daily`
A derived rollup backing the burn rate, series and breakdown; never a billing input. The member-facing breakdown filters to the caller's own `actor_id` unless they hold the admin entitlement ([D20](./design-decisions.md)).

### `reconcile_runs` · `reconcile_findings`
A run per shard (`incremental` or `full`) with **counts** — checked, in sync, deferred, cold, regressions, drifted — and `abs_drift` (Σ |drift|, never a net figure, so a +100 and a −100 on two scopes are two findings, not zero). A finding per scope and pool that disagreed at the **same** sequence number, with the hot and expected balances and whether it was healed and paged.

### `dead_letters`
Messages that exhausted their retry budget; parked, never dropped.

### `audit_log`
Every privileged action and **every configuration write**, with `actor` (the API user via `intelpay.actor`, or `db:<role>` for a direct SQL write), `action`, `subject_kind`, `subject_id`, `before`, `after`, `reason`.

### `event_endpoints` · `outbound_events` · `event_deliveries`
How hosts hear what happened ([outbound-events.md](./contracts/outbound-events.md)): subscribed `https` endpoints with a signing-secret reference; the event log that also backs `GET /v1/events`; one delivery per (event, endpoint) with backoff state.

---

## Migrations

| File | Contents |
|---|---|
| `0001_metering_core.sql` | partition helpers, `hot_config`, `hot_shards`, `credit_pools`, `account_credits`, `account_watermarks`, `credit_ledger` (+ partitions), `ledger_checkpoints`, `credit_idem`, `usage_idem` (+ partitions), `credit_suspense`, `credit_transfers`, `credit_outbox_dead`, `spend_journal`, `usage_events` (+ partitions), `rate_cards` / `rate_card_entries` (+ insert-only trigger), `limits`, `balance_watches` |
| `0002_payments.sql` | `provider_accounts`, `plans`, `plan_prices`, `plan_provider_prices`, `billing_customers`, `subscriptions`, `payments`, `payment_events`, `auto_recharge` |
| `0003_entitlements.sql` | `entitlement_keys`, `plan_entitlements`, `realm_defaults`, `entitlement_overrides` |
| `0004_operations.sql` | `usage_daily`, `reconcile_runs`, `reconcile_findings`, `dead_letters`, `audit_log` + the configuration audit trigger, `event_endpoints`, `outbound_events`, `event_deliveries` |
| `0005_credit_lots.sql` | `credit_lots` — applied always, used only under `lots_fifo` |

The Phase 2 schema is a **draft** kept with its spec ([002 draft-migrations](../002-postpaid-invoicing/draft-migrations/)), verified in CI on top of this baseline and shipped only with Phase 2's code ([D38](./design-decisions.md)).

Migrations travel with the module and are run by `cmd/payment-migrate`. `/readyz` fails while the schema is behind head. They are forward-only from the first release tag ([migrations/README.md](../../migrations/README.md)).
