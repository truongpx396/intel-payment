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
| POST | `/v1/grant` | `grant` | Mint/remove credits in a pool. `{scope, pool?, amount, reason, expires_at?, source_key?, ref?}` → `Receipt`. `source_key` is allowed only on a negative `amount` and names the grant it reverses |
| POST | `/v1/transfer` | `grant` | Pool → allocation. `{from, to, from_pool?, to_pool?, amount, reason, max_dest_balance?}` → `{applied, status: in_transit, from_balance}` |
| GET | `/v1/transfers/{idem_key}` | `grant` | `{idem_key, status: in_transit\|settled, from, to, from_pool, to_pool, amount, created_at, settled_at?}`. A transfer is accepted in the hot tier first and the books learn of it when the writer reaches it (typically milliseconds): until then it is `404 not_found`, and the message says a transfer accepted a moment ago may not have arrived. The key is one path segment, so an escaped `/` in it stays in it. `?scope_realm=` names the realm when the credential spans several |
| GET | `/v1/balance` | `record` | `?scope_kind=&scope_id=` (and `&scope_realm=` when the credential spans several) → `{pools: {general: …, promo: …}, total}` — the hot copy. A cold account is rebuilt from the books, as `Admit` does; an outage is an error, never a zero |
| GET | `/v1/credits` | `record` | The whole **`CreditsSnapshot`** in one round trip ([credits-ui-ports.md](./credits-ui-ports.md)), in that contract's own camelCase names — the wire is a pass-through, so the panel needs no translation table. `?scope_kind=&scope_id=` (and `&scope_realm=`); `?limit=&cursor=` page the ledger (`ledgerNext` is the next cursor, `null` at the end). **Every figure is an integer.** `balance.pools` are the parts and `balance.remaining` the total, and they close exactly; `balance.granted` is everything the books credited (a transfer in counts, a writeoff does not). `limits[]` are the realm's ceilings in the realm's order, sized exactly as `Admit` sizes them, so the cap on the panel is the cap that refuses a call. **`?subject_kind=&subject_id=` (both, or neither) is a member's view**: the scope-wide ceilings (`scopeHint: "all members"`) and that member's own (`"you"`) with that member's counters, and only that member's ledger rows — a member must not learn colleagues' usage from a credits screen. The scope's own view has the scope-wide ceilings only. A burn rate, a spend series (`?days=` is checked and reserved), a breakdown by dimension, an anchor and notices need the `usage_daily` rollup and host-resolved context: until they exist they are absent (`breakdown` is `[]`), never invented |
| GET | `/v1/ledger` | `record` | Booked rows of `?scope_kind=&scope_id=` (and `&scope_realm=`), **newest first**, as `{data: [row…], next}` — `next` is an opaque cursor for `?cursor=`, `null` on the last page, and `?limit=` is 1–200 (default 50). A page is a keyset over `(created_at, id)`: every row appears exactly once across pages however many are booked between them. A row: `id`, `created_at`, `occurred_at?`, `pool`, `delta` (signed), `operation_type`, `idem_key`, `rate_card_version?`, `seq` (`seq_from` when it covers a range), `event_count`, `resource?`, `rate_key?`, `quantities?`, `subjects?`, `attributes?`, `counterparty?`. **`?subject_kind=&subject_id=` (both, or neither) is a member-scoped read**: only the rows that member's spend produced — a purchase attributed to nobody is not that member's — so a host can show a member their own ledger and no one else's |

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
| 400 | `invalid_request` | malformed body, an unknown field, money that is not an integer, a missing `Idempotency-Key` or `occurred_at`, a realm the credential cannot resolve (it spans several and the scope names none), negative quantity |
| 400 | `missing_subject` | a configured subject limit applies and the call does not carry that subject |
| 400 | `uncountable_limit` | a window limit the caller supplied that no configured limit backs |
| 400 | `invalid_signature` | webhook verification failed. **A security event**, never `2xx` |
| 401 | `unauthenticated` | missing/invalid service credential (`WWW-Authenticate: Bearer`) |
| 403 | `forbidden` | valid credential, wrong privilege — e.g. `record` calling `/v1/grant` |
| 403 | `realm_mismatch` | the credential's realms do not include the scope's realm |
| 403 | `cross_realm` | a transfer between two realms: a mint and a burn, never a transfer |
| 403 | `account_blocked` | the scope is held by `block_and_flag` pending an operator |
| 402 | `payment_required` | a `Balance` limit is exhausted. Carries `details.limit`, and `details.upgrade_url` when a plan exists |
| 404 | `not_found` | no such route (with or without a credential: whether a route exists is not for strangers to probe) |
| 405 | `method_not_allowed` | the path exists and does not answer this method (`Allow` says which) |
| 409 | `limit_reached` | a windowed limit (daily/hourly/rolling/job) is exhausted — "wait", distinct from 402's "buy more". Carries `details.limit` and, for a daily or hourly limit, `details.resets_at` (the instant its counter starts over, in the limit's own zone); a rolling or job limit names `details.window` and `details.dur_seconds` instead |
| 409 | `idempotency_conflict` | the `Idempotency-Key` was already used for a **different** request. Never silently a no-op |
| 409 | `insufficient_balance` | a transfer would overdraw its source |
| 413 | `invalid_request` | the body is over the cap (1 MiB) |
| 422 | `unpriceable` | no rate for the `rate_key`/`unit`, or the card names an unregistered pricer. Fails closed |
| 422 | `stale_event` | `occurred_at` is older than the dedup window (or too far in the future). Retrying will not help |
| 422 | `amount_out_of_range` | a single operation above `MaxOperationAmount` |
| 422 | `dest_balance_cap` | a transfer would leave its destination above `max_dest_balance` |
| 429 | `rate_limited` | transport-level throttling of the caller itself *(not built)* |
| 499 | `cancelled` | the caller went away; the engine saw it go |
| 500 | `internal` | anything the table does not name. The message is fixed text; the error is in the operator's log |
| 503 | `hot_store_unavailable` | `Admit` under `fail_closed` with the shard unavailable; any `Grant` or `Transfer` while the hot tier is (`Record` defers instead). A cold shard, a frozen shard, a full outbox and a cold account are all this to a REST caller |
| 503 | `quota_unavailable` | a limit sized by an entitlement that could not be resolved. Never treated as "unlimited" |
| 504 | `deadline_exceeded` | the caller's deadline passed |

The status table is the one table both transports read ([`wire.Table`](../../../metering/adapters/driving/wire/conditions.go)): a gRPC `ErrorInfo.reason` and a REST `code` are columns of the same row, and a test enumerates the exported errors of `domain/errors.go` and fails on any without one. A 5xx message is fixed text — the error chain names the topology (a dial address, a DSN), and goes to the operator's log, whole.

**A refused `Admit` is a decision, and over REST it is the status above with the error envelope *and* the Admission's own fields beside it** — `{ "error": {…}, "allowed": false, "exceeded": {…}, "headroom": 0, "blocked": false }` — so a client that reads `.error.code` and one that reads `.allowed` both find what they look for. An allowed `Admit` is `200` with `{allowed, warning?, headroom, blocked}`. A deferred `Record` is `202` with `deferred: true`: it succeeded, and has not been applied yet.

**Requests are decoded strictly.** An unknown field is a typo that would silently not apply, and a ceiling that silently does not apply is the worst kind — so it is `400`, naming the field. Money is an integer: a fraction, an exponent, a string or a number past `int64` is not an amount. A limit supplied by the caller must name its `window`; a default would make an unfinished limit a balance. `occurred_at` and `expires_at` are RFC 3339 *with a zone*; one without is ambiguous, and an ambiguous `occurred_at` is a retry that cannot be told from a new event. A request's `Content-Type` is not consulted.

> **`409 limit_reached` vs the original host's `429`.** `429` means "you are calling too fast" and invites a retry with backoff; a spent budget is not a rate problem. Back off on `429`, show the reset time on `409`, offer an upgrade on `402`. `PAYMENT_LEGACY_429_FOR_LIMITS=true` restores the old code for a host mid-migration.

## Versioning and deprecation

- **The path carries the major version** (`/v1/`), the gRPC package likewise (`metering.v1`). Within a major version, changes are additive only: new endpoints, new optional fields, new error codes, new event types. Clients MUST ignore unknown fields.
- **A breaking change ships as `/v2/` alongside `/v1/`**, never in place. `/v1/` then enters deprecation: responses carry `Deprecation` and `Sunset` headers (RFC 9745 / RFC 8594), `metering_deprecated_calls_total{caller}` shows who still calls it, and it is removed no sooner than **12 months** after `/v2/` is generally available.
- **Webhook payloads are versioned by `api_version`** per endpoint ([outbound-events.md](./outbound-events.md)), with the same 12-month window.

## Rules

- **`Idempotency-Key` is required on every request that moves or spends credits** — `record`, `grant`, `transfer` — **and it is the operation's `idem_key`**: exactly one header, non-blank, and a body `idem_key` (optional) must agree with it. A retry with the same key and the same request returns the original outcome; the same key with a different request — a different `occurred_at` included — is `409 idempotency_conflict`. There is no default: a server-generated key would make every retry a new charge. `admit` and the reads have no operation behind them and need none; a configuration write is idempotent by its natural key (a `PUT` by name, a card by version).
- **No browser ever holds a `/v1/*` credential.** The host proxies `/v1/credits` behind its own session.
- **Realm comes from the credential.** A body-supplied realm outside the credential's realms is `403 realm_mismatch`.
- **`grant` and `admin` are separate privileges from `record`,** at the transport, not just in code.
- **Webhook routes carry no CORS and no auth middleware.** A JWT check there rejects the provider.
- **`/readyz` fails when migrations are behind head**, so a rollout cannot serve money against a half-migrated schema.
