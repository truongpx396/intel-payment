# Security

This service holds the ability to **create money** and to **refuse work**. Its threat model is
therefore not a generic web app's, and two properties carry most of the weight: only a narrow,
authenticated path may mint credits, and no caller may reach into another realm.

---

## Trust boundaries

```text
BROWSER ──────► HOST BACKEND ──────► intel-payment ◄────── PAYMENT PROVIDER
   │                  │                    │                      │
   │ host session     │ service credential │ signature only        │
   │                  │ (mTLS / bearer),   │ (no JWT, no CORS)     │
   └── NEVER holds ───┘ realm-bound        │                      │
       a /v1/* credential                  └── owns Redis + Postgres
```

Three rules follow from the diagram and are not negotiable:

1. **A browser never holds a `/v1/*` credential.** The host proxies `/v1/credits` behind its own
   session. A metering endpoint reachable from a browser is an endpoint anyone can spend against,
   and a `Grant` endpoint reachable from a browser is free money.
2. **Webhook routes carry no JWT and no CORS.** They are server-to-server and authenticated by
   signature. A session check there rejects the provider; a CORS policy there is meaningless.
3. **The service authenticates *callers*, not end users.** The host authenticates its people and
   hands over a `Scope`. Putting an end-user identity model in a billing service means putting a
   customer's org chart there too.

---

## Caller authentication

| Mode | Use when |
|---|---|
| `mtls` | Production across a network. Mutual TLS with a private CA; the client certificate's subject is the caller id |
| `bearer` | Production inside a trusted mesh, or development. Tokens stored as SHA-256 digests, compared in constant time |
| `none` | **Only** when bound to loopback or a Unix socket, e.g. a sidecar. The service refuses to start with `none` on a routable address |

Each credential carries:

- **a caller id** — for audit rows and per-caller metrics;
- **a realm set** — the realms it may address;
- **a privilege set** — `record` and/or `grant`.

### Realm binding

Every request's `Scope.realm` is checked against the credential's realm set. A mismatch is
`PERMISSION_DENIED` / `403 realm_mismatch`. **A realm is never taken from a request body**, which
is what makes the isolation real rather than advisory: a compromised caller for product A cannot
read, debit or grant in product B by naming it.

### Privilege separation

`Grant` mints credits. `Record` spends them. These are separated at the RPC boundary, the HTTP
route, the authorization check and the audit trail — so a compromised spend producer (the
component most exposed to host traffic) can debit with idempotency but **never mint**.

`PAYMENT_GRANT_CALLERS` is the allowlist. Empty means only the internal webhook path can grant,
which is the right default for most deployments.

---

## Webhook posture

The full flow is in [payment-provider-ports.md](../specs/001-metering-billing-core/contracts/payment-provider-ports.md).
The security-critical parts:

- **Verify on the raw body, before anything.** No parse, no DB read, no log of the contents,
  before the signature checks out. A verification that happens after a parse has already run
  attacker-controlled input through a parser.
- **Constant-time comparison.** A timing-variable compare leaks the signature a byte at a time.
- **Timestamp tolerance.** Without it, a captured valid payload replays forever. The dedup guard
  limits the damage, but only while the event row exists.
- **Body-size limit.** An unbounded webhook body is a trivial memory-exhaustion vector on a route
  that by design has no authentication in front of it.
- **No raw payload stored.** Only a SHA-256 of the verified body. Provider payloads carry PII and
  are not needed once the facts are extracted.
- **Never trust metadata for authority.** Scope, amount, currency and credit quantity come from
  the verified object plus this deployment's own customer and catalogue records. A validly-signed
  event whose metadata names someone else's scope must not move their balance — and an attacker
  who can buy something from you can produce a validly-signed event.
- **Failures are security events.** `billing_webhook_verify_failed_total` is on a security
  dashboard, not only an error log. A rising rate means either a misconfigured secret or someone
  probing.
- **Unverifiable is not unverified.** When verification itself is unavailable (PayPal's API down),
  park the delivery for replay. Do not process it optimistically; do not discard it.

---

## Secrets

- Provider keys and webhook secrets come from the environment or a secret manager. Never a client
  bundle, never a `PUBLIC_`/`NEXT_PUBLIC_` variable, never a log line, never a database row.
- Only **publishable** keys may reach a browser.
- `gitleaks` runs in CI. A leaked provider secret is a full compromise of revenue — rotate at the
  provider first, then redeploy.

---

## Data protection

| Data | Handling |
|---|---|
| Card numbers, CVV, expiry | **Never received.** Hosted checkout and hosted portal, always. The safest posture toward a card number is not to have one |
| Provider customer / price / subscription ids | Server-side only. A `provider_price_id` reaching a UI is a defect |
| Webhook payloads | Hashed, not stored |
| Billing email | Stored for receipts; treated as personal data |
| Usage attribution (`actor_id`) | Personal data. A member-facing breakdown is **self-only** unless the caller holds the admin entitlement |
| Ledger rows | Retained as the audit trail. Dropping a partition destroys the ability to reconcile that period |

---

## Authorization the host still owns

This service does not know your roles. The host gates these before calling:

| Operation | Typical gate |
|---|---|
| `POST /v1/checkout` | billing admin or owner |
| `POST /v1/subscription/cancel` \| `/change` | owner, with re-authentication |
| `GET /v1/portal` | billing admin or owner |
| `PUT /v1/entitlements/override` | internal staff only, and it is audited |
| `GET /v1/credits` | any member, but the breakdown narrows to self without the admin entitlement |

Implement each check **once**, in the host. A second copy of an authorization rule is one copy
too many, and it is always the stale one that gets hit.

---

## Audit

Every privileged action writes an `audit_log` row with actor, action, subject, before/after and a
reason: a grant not originating from a verified webhook, an entitlement override, a manual
reconcile or rehydrate, a rate-card publication, a plan change. `reason` and `actor` are
`NOT NULL` on overrides at the schema level, because an unexplained override is indistinguishable
from a bug and is usually discovered during a revenue investigation.

---

## Pre-production checklist

- [ ] `PAYMENT_SERVICE_AUTH` is `mtls` or `bearer`; `none` only on loopback/UDS.
- [ ] Every caller credential is realm-bound and privilege-scoped; `PAYMENT_GRANT_CALLERS` is as narrow as possible.
- [ ] No browser holds a `/v1/*` credential; the host proxies.
- [ ] Webhook routes: signature verified first, constant-time, timestamp tolerance, body-size limit, no CORS, no auth middleware, no body-rewrite before verification.
- [ ] Provider secrets in a secret manager; only publishable keys client-side; `gitleaks` green.
- [ ] Balance Redis: `noeviction`, AOF, **not shared with a cache**, network-isolated.
- [ ] Postgres: TLS, least-privilege role, backups tested by an actual restore.
- [ ] `/metrics` and admin endpoints not publicly reachable.
- [ ] Verification-failure and drift alerts reach a human.
- [ ] A tampered webhook, a cross-realm scope, and a `Record`-only caller attempting `Grant` have each been tried against the deployment and were refused.
