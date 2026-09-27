# Contract: REST API

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the HTTP surface of the standalone service.

Two audiences, two authentication models, and the split matters more than the routes:

| Surface | Caller | Authentication | Notes |
|---|---|---|---|
| **`/v1/*`** | a host's backend | service credential (mTLS or bearer), realm-bound | never called from a browser — a browser-reachable metering API is an unmetered-spend API |
| **`/webhooks/{provider}`** | a payment provider | **signature only**, no JWT, no CORS | see [payment-provider-ports.md](./payment-provider-ports.md) |
| **`/healthz`, `/readyz`, `/metrics`** | the platform | none / network-scoped | `/readyz` fails on a behind-head schema |

gRPC ([grpc-surface.md](./grpc-surface.md)) is the preferred transport for the hot path — `Admit` sits on every host request. REST exists for non-Go hosts, for the browser-facing snapshot the host proxies, and for operations.

Conventions: `Idempotency-Key` required on every mutating request; realm resolved **from the credential**, never from the body or a query parameter; money as integer minor units; credits as signed `int64`; timestamps ISO-8601 UTC; cursor pagination.

---

## Metering

| Method | Path | Purpose | Notes |
|--------|------|---------|-------|
| POST | `/v1/admit` | Admission gate | `{scope, limits?, max_cost?}` → `{allowed, exceeded?, warning?, headroom}`. Hot path: keep it co-located, set a tight deadline, and decide fail-open/closed explicitly. Prefer gRPC |
| POST | `/v1/record` | Price + settle a metered event | `{scope, resource, rate_key, quantities[], idem_key, occurred_at, attributes?}` → `Receipt`. Idempotent on `idem_key`; a replay returns `applied: false` |
| POST | `/v1/grant` | **Privileged** — mint/remove credits | `{scope, amount, idem_key, reason, ref?}` → `Receipt`. Restricted to the payment path and audited admin callers (metering invariant 13). `403` otherwise |
| GET | `/v1/balance` | Current hot balance | `?scope_kind=&scope_id=` → `{credits}` |
| GET | `/v1/credits` | The whole **`CreditsSnapshot`** | One round trip for the credits screen: `{balance, limits[], breakdown[], series[], ledger[], anchor?, notices[]}`. See [credits-ui-ports.md](./credits-ui-ports.md). `?days=` on the series, `?cursor=&limit=` on the ledger |
| GET | `/v1/ledger` | Paginated ledger rows | Cursor-paginated; each row carries `delta`, `operation_type`, `idem_key`, `rate_card_version` and whether it was a replay |

## Catalogue, checkout & subscription

| Method | Path | Purpose | Notes |
|--------|------|---------|-------|
| GET | `/v1/plans` | Active plans for the realm | `?currency=`. Returns host slugs, prices, allotments and entitlements. **Never a provider price id** |
| POST | `/v1/checkout` | Start a hosted checkout | `{scope, plan_code, currency, provider?, success_url, cancel_url}` → `{redirect_url}`. **Never grants** — fulfilment is the webhook's job |
| GET | `/v1/subscription` | Current subscription | `{plan_code, status, current_period_end, cancel_at_period_end, grace_until}` or `null` |
| POST | `/v1/subscription/change` | Upgrade / downgrade | `{plan_code}`; applies the deployment's `PlanChangePolicy`. Status changes when the resulting webhook arrives, not here |
| POST | `/v1/subscription/cancel` | Cancel at period end | Sets the flag at the provider; local status syncs via webhook (payments invariant 12) |
| GET | `/v1/portal` | Provider-hosted billing portal | `{portal_url}`. Card management stays with the provider |
| GET | `/v1/payments` | Payment / receipt history | Cursor-paginated from `payments` |
| GET | `/v1/fulfilment` | Post-checkout fulfilment state | `{kind: none\|processing\|granted\|failed, …}`. **The only sanctioned way a UI learns credits arrived** — a client must never infer it from a redirect |

## Entitlements

| Method | Path | Purpose | Notes |
|--------|------|---------|-------|
| GET | `/v1/entitlements` | Every effective grant with provenance | Backs both the plan card and support answers, so UI and server cannot disagree |
| POST | `/v1/entitlements/check` | Batch flag/quota check | `{scope, checks:[{key, current?, delta?}]}` → per-key `Decision`. Batched because a page render asks about several capabilities at once |
| PUT | `/v1/entitlements/override` | **Privileged** — write an exception | Requires `reason`; writes an audit row in the same transaction |

## Operations

| Method | Path | Purpose | Notes |
|--------|------|---------|-------|
| POST | `/v1/admin/reconcile` | Force a reconcile for a shard/bucket | Idempotent under the same lock as the scheduled tick |
| POST | `/v1/admin/rehydrate` | Rebuild a scope's hot balance from the ledger | Per-scope lock; used after a hot-store loss |
| GET | `/v1/admin/drift` | Current drift per shard | The same figure `metering_ledger_drift_credits` exports |
| POST | `/webhooks/{provider}` | Provider webhook ingress | **Unauthenticated by JWT, verified by signature.** Raw body required — no body-rewrite middleware may precede it. Body-size limited, no CORS |

---

## Errors

One shape everywhere:

```json
{ "error": { "code": "payment_required", "message": "Credit balance exhausted.",
             "details": { "limit": "scope_balance", "upgrade_url": "…" } } }
```

| HTTP | `code` | When |
|---|---|---|
| 400 | `invalid_request` | malformed body, missing `idem_key`, negative quantity |
| 400 | `invalid_signature` | webhook verification failed. **Counted as a security event**, never `2xx` |
| 401 | `unauthenticated` | missing/invalid service credential |
| 403 | `forbidden` | valid credential, wrong privilege — e.g. `Record`-only calling `/v1/grant` |
| 403 | `realm_mismatch` | the credential's realms do not include the requested scope's realm |
| 402 | `payment_required` | a `Balance` limit is exhausted. Carries an `upgrade_url` when a plan exists |
| 409 | `limit_reached` | a windowed limit (daily/hourly/rolling) is exhausted — **distinct from 402**, because one is "buy more" and the other is "wait", and a UI that conflates them tells the user to do the wrong thing |
| 422 | `unpriceable` | no rate for the `rate_key`/`unit`. Fails closed — never priced at zero |
| 429 | `rate_limited` | transport-level throttling of the caller itself |
| 503 | `hot_store_unavailable` | `Admit` under `fail_closed` with the hot store down |

> **`409 limit_reached` vs the original host's `429`.** The originating design returned `429` for an exhausted daily ceiling. `429` means "you are calling too fast" and invites a retry with backoff; a spent daily budget is not a rate problem and retrying will not fix it until the window rolls. Splitting them lets a client do the right thing automatically: back off on `429`, surface the reset time on `409`, offer an upgrade on `402`. `PAYMENT_LEGACY_429_FOR_LIMITS=true` restores the old code for a host mid-migration.

## Rules

- **`Idempotency-Key` is required on every mutating request** and is the `idem_key` used downstream. A retry of the same key returns the original outcome, not a second effect.
- **No browser ever holds a `/v1/*` credential.** The host proxies `/v1/credits` behind its own session. A metering endpoint reachable from a browser is an endpoint anyone can spend against.
- **Realm comes from the credential.** A body-supplied realm is ignored, and a scope outside the credential's realms is `403 realm_mismatch`.
- **Grant is separated at the transport, not just in code.** Distinct privilege, distinct audit, distinct metric.
- **Webhook routes carry no CORS and no auth middleware.** A JWT check there rejects the provider.
- **`/readyz` fails when migrations are behind head**, so a rollout cannot serve money against a half-migrated schema.
