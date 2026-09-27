# Configuration

Two layers, and the split is deliberate:

- **Deployment config** (env → `Config` structs) — infrastructure and policy. Changing it needs a restart.
- **Realm config** (Postgres rows) — rate cards, ceilings, plans, entitlements. Changing it needs **nothing**.

Anything a host might want to change without waiting on this service's release cycle is in the
second layer. That is what "highly configurable" means here: not more env vars, but the
commercially volatile things living in tables.

---

## Deployment configuration

Every variable maps 1:1 to a `Config` field. The last column is what happens if you get it
wrong, because that is usually the only thing worth knowing.

### Identity

| Variable | Default | Failure mode if wrong |
|---|---|---|
| `PAYMENT_REALM` | `default` | Two products sharing a deployment share plans, limits **and an idempotency key space** — B's `invoice-1` is silently swallowed as a replay of A's |

### Stores

| Variable | Default | Failure mode if wrong |
|---|---|---|
| `PAYMENT_BALANCE_REDIS_URL` | — (required) | — |
| `PAYMENT_LEDGER_DSN` | — (required) | — |

> **The balance Redis MUST be `noeviction` with AOF on, and MUST NOT be shared with a cache.**
> An LRU policy is correct for a cache and catastrophic here: an eviction silently discards a
> balance, and the next read rebuilds it from the ledger — losing every un-drained charge. This
> is the single most common way to break the system from the outside.

### Bus & sharding

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_BUS` | `redis_streams` | `redis_streams` \| `nats_jetstream`. See the comparison below |
| `PAYMENT_BUS_URL` | — | Empty with `redis_streams` reuses the balance Redis. Required for JetStream |
| `PAYMENT_SUBJECT_PREFIX` | `billing` | Fit into a host's existing namespace |
| `PAYMENT_SHARDS` | `16` | **Fixed for a deployment's life.** Changing it re-partitions the outbox key space; entries queued under the old count are orphaned |
| `PAYMENT_BUS_RETENTION` | `168h` | Acked entries kept this long, then trimmed. Streams only |
| `PAYMENT_BUS_MAX_LEN` | `1000000` | Per-stream soft cap. Nearing it **alerts**; it never trims un-acked entries |
| `PAYMENT_BUS_MAX_ATTEMPTS` | `5` | Deliveries before an entry goes to the DLQ |
| `PAYMENT_BUS_ACK_WAIT` | `30s` | In-flight reclaim threshold (`XAUTOCLAIM` / ack-wait) |

**Why Redis Streams is the default:** it removes a failure class rather than just a dependency. With
a broker, the hot path writes an outbox entry and something else publishes it later — so an intent can
exist in the outbox and not on the bus. With a stream outbox, `XADD` runs inside the same atomic
script as `DECRBY`, so the intent is durable on the bus the instant the balance moves. It also takes
required infrastructure down to **Redis + Postgres**.

**Choose `nats_jetstream` when** you need quorum (R3/R5) replication or cross-region mirroring for
money intents, you run `journal` settlement and want the bus as durable as the journal, or you
already operate JetStream. Subjects, handlers and invariants are identical, and the same
`BusContract` passes against both.

> **With a stream bus, `PAYMENT_BUS_RETENTION` and the trim tick are not optional.** Nothing in Redis
> reclaims stream memory on its own. On a `noeviction` instance — which this store must be — an
> untrimmed stream eventually fills memory and **stops the hot path**. Trim never removes an un-acked
> entry, so the floor can be set conservatively.

### Settlement durability

| Value | Hot-path cost | On hot-store loss | Choose it when |
|---|---|---|---|
| `outbox` (default) | one atomic Redis step, no durable wait | bounded silent **under-bill**; reconcile heals the balance upward | Commodity metering where a sub-cent RPO gap beats sub-ms enforcement |
| `journal` | +1 synchronous durable write | no under-bill — the intent survives | High-value units, regulated billing, or revenue-loss RPO must be ~zero |

**The direction matters more than the mode.** In `outbox`, a Redis loss before the drain means
the ledger never gets the row, so reconcile heals *upward* and the charge is forgotten. That is
under-billing, never double-billing. Reconcile does not make every loss whole; it bounds and
alarms the drift. If you need zero revenue-loss RPO, that is what `journal` is for — and it is a
per-deployment switch with no code change.

### Reconciliation

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_RECONCILE_INTERVAL` | `1h` | Shorter narrows the drift window and costs a full-ledger sum per shard |
| `PAYMENT_RECONCILE_TOLERANCE` | `0` | `\|drift\| >` this **pages**. `0` means any drift pages — correct for most, noisy at very high volume |
| `PAYMENT_DEFAULT_WARN_AT` | `0.8` | Used when a `limits` row omits its own |

### Hot-path safety

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_ADMIT_TIMEOUT` | `250ms` | Generous. Co-located, set ~50 ms |
| `PAYMENT_RECORD_TIMEOUT` | `500ms` | Settlement is off the user's critical path |
| `PAYMENT_ADMIT_FAIL_POLICY` | `fail_closed` | `fail_open` serves work **unmetered** during a hot-store outage: bounded overspend, healed by reconcile, and every occurrence counted. A revenue-vs-availability decision — make it deliberately |
| `PAYMENT_REQUIRE_MAX_COST` | `false` | `true` rejects an `Admit` with no cost bound, making the overshoot bound a precondition. Turn it on once `metering_admit_uncapped_total` is flat at zero |

### Money policies

| Variable | Values | Default | What it decides |
|---|---|---|---|
| `PAYMENT_NEGATIVE_BALANCE_POLICY` | `allow_debt` · `clamp_to_zero` · `block_and_flag` | `clamp_to_zero` | What a refund past consumed credits does. Clamping **always** writes a compensating `writeoff` row — otherwise `balance == SUM(ledger)` breaks and the next reconcile pages for drift the system created itself |
| `PAYMENT_CREDIT_EXPIRY` | `off` · `lots_fifo` | `off` | Whether grants expire. `lots_fifo` tracks per-grant lots and consumes oldest-first |
| `PAYMENT_PLAN_CHANGE_POLICY` | `at_period_end` · `immediate_prorate` · `immediate_no_prorate` | `at_period_end` | Mid-period upgrade/downgrade. Proration is the provider's math |
| `PAYMENT_TAX_STRATEGY` | `provider_managed` · `external_hook` · `none` | `provider_managed` | **Who computes tax.** There is no compute-it-here mode by design |

### Dunning

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_DUNNING_GRACE_DAYS` | `7` | Entitlements hold this long after a failed payment. `0` revokes instantly — which cuts off a paying customer over a transient decline they cannot diagnose |
| `PAYMENT_DUNNING_SUSPEND_AFTER_DAYS` | `14` | Suspension after grace |

### Retention

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_PAYMENT_EVENT_RETENTION` | `90d` | **MUST exceed the longest provider replay window.** Purge inside it and the dedup guard vanishes; the ledger's unique index still catches it, which is exactly why that backstop exists |
| `PAYMENT_IDEMPOTENCY_CACHE_TTL` | `24h` | Redis fast path only. Any value is safe — correctness rests on Postgres. Shorter only costs a DB round-trip on a late replay |
| `PAYMENT_LEDGER_PARTITION_RETENTION` | `unlimited` | Ledger partitions are the audit trail. Dropping one destroys the ability to reconcile that period |

### Transports & auth

| Variable | Default | Notes |
|---|---|---|
| `PAYMENT_GRPC_ADDR` | `:9090` | Hot path |
| `PAYMENT_HTTP_ADDR` | `:8080` | REST + webhooks + `/healthz` + `/readyz` + `/metrics` |
| `PAYMENT_SERVICE_AUTH` | `bearer` | `none` \| `mtls` \| `bearer`. **`none` is permitted only on loopback or a Unix socket** |
| `PAYMENT_SERVICE_TOKENS` | — | `<caller-id>:<sha256>,…` with the realms each may address |
| `PAYMENT_GRANT_CALLERS` | — | The **only** callers allowed to mint credits. Empty means only the internal webhook path |
| `PAYMENT_LEGACY_429_FOR_LIMITS` | `false` | Restores `429` instead of `409` for spent windows, for a host mid-migration |

### Providers

Enable only what you sell through. All secrets are server-side only; a provider secret in a
client bundle is a full compromise of your revenue.

| Variable | Notes |
|---|---|
| `PAYMENT_PROVIDERS_ENABLED` | `stripe,polar,paypal` — comma-separated |
| `STRIPE_SECRET_KEY` / `STRIPE_WEBHOOK_SECRET` | Local HMAC verification |
| `POLAR_ACCESS_TOKEN` / `POLAR_WEBHOOK_SECRET` | Local HMAC verification |
| `PAYPAL_CLIENT_ID` / `_SECRET` / `_WEBHOOK_ID` / `PAYPAL_ENV` | **Remote** verification — an API call per webhook. Needs a deadline; a PayPal outage means deliveries can be neither verified nor rejected, so they park |

---

## Realm configuration (data, no deploy)

| Table | What you change | How often |
|---|---|---|
| `rate_cards` / `rate_card_entries` | prices per unit; `micros_per_credit` | on repricing — **insert a new version, never edit a row** |
| `limits` | the ceilings, **including how many there are**, windows, warn fractions, deny codes | on policy change |
| `plans` / `plan_prices` / `plan_provider_prices` | the catalogue, per currency, per provider | on a launch |
| `plan_entitlements` / `realm_defaults` | what plans and the free tier unlock | on a packaging change |
| `entitlement_overrides` | per-scope exceptions | per deal — `reason` and `actor` are `NOT NULL` |

Two rules make this safe:

1. **Rate cards are insert-only.** Editing a row silently re-prices history and breaks every
   dispute audit and reconcile replay against it. Publish a version; retire the old one.
2. **Limit count is data on both sides.** The engine evaluates `[]Limit` and the UI renders
   `LimitView[]`, so adding a third ceiling is one row — not a migration and not a component edit.

---

## Common shapes

**Free internal metering (no money).** No providers enabled, `credit_allotment` null, one
`Balance` limit per scope, `TaxStrategy=none`. You get enforcement and a ledger and never touch fiat.

**Prepaid credit packs.** `plans.kind=one_time`, `CreditExpiry=lots_fifo`,
`NegativeBalancePolicy=clamp_to_zero`. Expiry is the norm for prepaid, so turn it on deliberately.

**Seat-based SaaS.** A seat `Pricer`, `PlanChangePolicy=immediate_prorate`, entitlements carrying
the seat quota, one `Balance` limit. Most gating is entitlements, not credits.

**Usage-based API product.** `Hourly` + `Rolling` limits per api-key scope, `RequireMaxCost=true`,
`Settlement=journal` if revenue-loss RPO must be zero.

**Multi-product platform.** One deployment, one realm per product, per-realm cards/limits/plans,
per-realm caller credentials. Nothing crosses.
