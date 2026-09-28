# Contract: REST API

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the HTTP surface of the standalone service. The machine-readable form is `api/openapi/v1.yaml`, from which the TypeScript and Python clients are generated; CI fails if the handlers and the document disagree.

Three audiences, three authentication models, and the split matters more than the routes:

| Surface | Caller | Authentication | Notes |
|---|---|---|---|
| **`/v1/*`** | a host's backend | service credential (mTLS or bearer), realm-bound, with privileges `record` · `grant` · `admin` | never called from a browser — a browser-reachable metering API is an unmetered-spend API |
| **`/webhooks/{provider}/{account}`** | a payment provider | **signature only**, no JWT, no CORS | see [payment-provider-ports.md](./payment-provider-ports.md) |
| **`/healthz`, `/readyz`, `/metrics`** | the platform | none / network-scoped | `/readyz` fails on a behind-head schema or a shard-count mismatch |

gRPC ([grpc-surface.md](./grpc-surface.md)) is the preferred transport for the hot path — `Admit` sits on every host request. REST exists for non-Go hosts, for the browser-facing snapshot the host proxies, for configuration, and for operations.

Conventions: `Idempotency-Key` required on every mutating request, and it **is** the operation's `idem_key` (a body `idem_key` that differs is `400`); realm resolved **from the credential**, never from the body; money as integer minor units; credits as signed `int64`; timestamps ISO-8601 UTC; cursor pagination.

---

## Metering

| Method | Path | Privilege | Purpose |
|--------|------|-----------|---------|
| POST | `/v1/admit` | `record` | Admission gate. `{scope, resource, subjects?, limits?, max_cost?}` → `{allowed, exceeded?, warning?, headroom, blocked}`. Hot path: co-locate, tight deadline, explicit fail policy. Prefer gRPC |
| POST | `/v1/record` | `record` | Price + settle. `{scope, resource, rate_key, quantities[], subjects?, occurred_at, attributes?}` → `Receipt`. **`occurred_at` is required and must be identical on every retry.** A replay returns `applied:false`; an outage returns **`202`** with `deferred:true` (journaled, applied on recovery) |
| POST | `/v1/grant` | `grant` | Mint/remove credits in a pool. `{scope, pool?, amount, reason, expires_at?, ref?}` → `Receipt` |
| POST | `/v1/transfer` | `grant` | Pool → allocation. `{from, to, from_pool?, to_pool?, amount, reason, max_dest_balance?}` → `{applied, status: in_transit, from_balance}` |
| GET | `/v1/transfers/{idem_key}` | `grant` | `{status: in_transit\|settled, …}` |
| GET | `/v1/balance` | `record` | `?scope_kind=&scope_id=` → `{pools: {general: …, promo: …}, total}` — the hot copy |
| GET | `/v1/credits` | `record` | The whole **`CreditsSnapshot`** in one round trip ([credits-ui-ports.md](./credits-ui-ports.md)). `?days=` on the series, `?cursor=&limit=` on the ledger |
| GET | `/v1/ledger` | `record` | Booked rows, cursor-paginated: `delta`, `pool`, `operation_type`, `idem_key`, `rate_card_version`, `seq`, `event_count` |

## Catalogue, checkout & subscription

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/v1/plans` | Active plans for the realm. `?currency=`. Host slugs, prices, allotments, pools and entitlements — **never a provider price id** |
| POST | `/v1/checkout` | `{scope, plan_code, currency, mode?: payment\|subscription\|setup, account?, success_url, cancel_url}` → `{redirect_url}`. **Never grants** |
| GET | `/v1/subscription` | `{plan_code, status, current_period_end, cancel_at_period_end, grace_until}` or `null` |
| POST | `/v1/subscription/change` | `{plan_code}` under the deployment's `PlanChangePolicy`. Status changes when the resulting webhook is processed |
| POST | `/v1/subscription/cancel` | Cancel at period end; local status syncs via webhook |
| GET | `/v1/portal` | `{portal_url}` — card management stays with the provider |
| GET / PUT | `/v1/auto-recharge` | `{enabled, pool, threshold, plan_code, currency, max_per_day}`; `PUT` requires a saved method from a `setup` checkout |
| GET | `/v1/payments` | Payment history, cursor-paginated, with refund and dispute state |
| GET | `/v1/fulfilment` | `{kind: none\|processing\|granted\|failed, …}` — **the only sanctioned way a UI learns credits arrived** |

## Entitlements

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/v1/entitlements` | Every effective grant with provenance |
| POST | `/v1/entitlements/check` | Batch: `{scope, checks:[{key, current?, delta?}]}` → per-key `Decision` |

## Events

| Method | Path | Purpose |
|--------|------|---------|
| GET | `/v1/events` | The event feed: `?after=<id>&types=&limit=` → `{data: [envelope…], next}` ([outbound-events.md](./outbound-events.md)) |

## Configuration (`admin` privilege)

Configuration is data ([D13](../design-decisions.md)), and this is its supported interface: every
write is validated, runs in one transaction with `intelpay.actor` and `intelpay.reason` set — so the
database's audit row names a person and a reason — and invalidates every cache immediately.
A direct SQL write still works and is still audited, attributed to the database role, but skips
validation; use it only in an emergency.

| Method | Path | Purpose |
|--------|------|---------|
| GET / POST | `/v1/admin/rate-cards` | List; **publish** a new version `{version, pricer?, entries[{rate_key, unit, credits_per_block, block_size, cost_micros_per_block?}], retire_previous: true}` — validated by pricing a test event under it before it becomes active. There is no update or delete (the database refuses them) |
| GET / PUT / DELETE | `/v1/admin/limits/{name}` | Ceilings: `{subject_kind?, resource?, unit, max, max_entitlement?, window, dur_seconds?, tz?, warn_at, deny_code}` |
| GET / PUT | `/v1/admin/pools/{name}` | Credit pools: `{priority, applies_to?}` |
| GET / POST / PATCH | `/v1/admin/plans[/{code}]` | Plans, per-currency prices, provider price mappings (`/provider-prices`), entitlements |
| GET / PUT | `/v1/admin/entitlement-keys/{key}` | Declared capabilities and enum orders |
| PUT / DELETE | `/v1/admin/entitlements/override` | A per-scope exception — including a per-customer metering ceiling. `reason` required |
| GET / PUT | `/v1/admin/provider-accounts/{id}` | Provider accounts: `{provider, realms, mode, secret_ref, webhook_secret_ref}` — secrets by reference only |
| GET / POST / PATCH / DELETE | `/v1/admin/event-endpoints[/{id}]` | Outbound webhook endpoints; `POST …/{id}/test` sends a signed test event |
| POST | `/v1/admin/event-deliveries/{id}/redeliver` | Re-drive a dead delivery |

## Operations (`admin` privilege)

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/v1/admin/reconcile` | `{shard, kind}` — idempotent under the tick's claim |
| POST | `/v1/admin/shards/{shard}/recover` | Freeze → drain → bump generation → unfreeze, with a `reason` |
| POST | `/v1/admin/rehydrate` | Rebuild one scope's hot account from the books |
| GET | `/v1/admin/drift` | Recent reconcile runs and findings per shard |
| POST | `/v1/admin/unblock` | Clear `block_and_flag` for a scope, with a `reason` |
| POST | `/v1/admin/adjustments` | An investigated correction to the books: `{scope, pool, amount, reason}` → an `admin_adjustment` intent through the normal path, audited |
| POST | `/v1/admin/dead-letters/{id}/replay` | Re-drive a parked intent or message |
| POST | `/v1/admin/subdrift` | Force the subscription drift sweep for an account |
| POST | `/webhooks/{provider}/{account}` | Provider webhook ingress — **verified by signature**; raw body; size-limited; no CORS |

---

## Errors

One shape everywhere:

```json
{ "error": { "code": "payment_required", "message": "Credit balance exhausted.",
             "details": { "limit": "scope_balance", "upgrade_url": "…" } } }
```

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_request` | malformed body, missing `Idempotency-Key` or `occurred_at`, negative quantity |
| 400 | `missing_subject` | a configured subject limit applies and the call does not carry that subject |
| 400 | `invalid_signature` | webhook verification failed. **A security event**, never `2xx` |
| 401 | `unauthenticated` | missing/invalid service credential |
| 403 | `forbidden` | valid credential, wrong privilege — e.g. `record` calling `/v1/grant` |
| 403 | `realm_mismatch` | the credential's realms do not include the scope's realm |
| 403 | `account_blocked` | the scope is held by `block_and_flag` pending an operator |
| 402 | `payment_required` | a `Balance` limit is exhausted. Carries an `upgrade_url` when a plan exists |
| 409 | `limit_reached` | a windowed limit (daily/hourly/rolling/job) is exhausted — "wait", distinct from 402's "buy more". Carries `resets_at` |
| 409 | `idempotency_conflict` | the `Idempotency-Key` was already used for a **different** request. Never silently a no-op |
| 409 | `insufficient_balance` | a transfer would overdraw its source |
| 422 | `unpriceable` | no rate for the `rate_key`/`unit`, or the card names an unregistered pricer. Fails closed |
| 422 | `stale_event` | `occurred_at` is older than the dedup window (or too far in the future). Retrying will not help |
| 422 | `amount_out_of_range` | a single operation above `MaxOperationAmount` |
| 429 | `rate_limited` | transport-level throttling of the caller itself |
| 503 | `hot_store_unavailable` | `Admit` under `fail_closed` with the shard unavailable (`Record` defers instead) |

> **`409 limit_reached` vs the original host's `429`.** `429` means "you are calling too fast" and invites a retry with backoff; a spent budget is not a rate problem. Back off on `429`, show the reset time on `409`, offer an upgrade on `402`. `PAYMENT_LEGACY_429_FOR_LIMITS=true` restores the old code for a host mid-migration.

## Versioning and deprecation

- **The path carries the major version** (`/v1/`), the gRPC package likewise (`metering.v1`). Within a major version, changes are additive only: new endpoints, new optional fields, new error codes, new event types. Clients MUST ignore unknown fields.
- **A breaking change ships as `/v2/` alongside `/v1/`**, never in place. `/v1/` then enters deprecation: responses carry `Deprecation` and `Sunset` headers (RFC 9745 / RFC 8594), `metering_deprecated_calls_total{caller}` shows who still calls it, and it is removed no sooner than **12 months** after `/v2/` is generally available.
- **Webhook payloads are versioned by `api_version`** per endpoint ([outbound-events.md](./outbound-events.md)), with the same 12-month window.

## Rules

- **`Idempotency-Key` is required on every mutating request.** A retry with the same key and the same request returns the original outcome; the same key with a different request is `409 idempotency_conflict`.
- **No browser ever holds a `/v1/*` credential.** The host proxies `/v1/credits` behind its own session.
- **Realm comes from the credential.** A body-supplied realm outside the credential's realms is `403 realm_mismatch`.
- **`grant` and `admin` are separate privileges from `record`,** at the transport, not just in code.
- **Webhook routes carry no CORS and no auth middleware.** A JWT check there rejects the provider.
- **`/readyz` fails when migrations are behind head**, so a rollout cannot serve money against a half-migrated schema.
