# Security

This service holds the ability to **create money** and to **refuse work**. Its threat model is
therefore not a generic web app's, and two properties carry most of the weight: only a narrow,
authenticated path may mint credits, and no caller may reach into another realm.

---

## Trust boundaries

```text
BROWSER ──────► HOST BACKEND ──────► intel-payment ◄────── PAYMENT PROVIDER
   │                  │  ▲                 │                      │
   │ host session     │  │ signed events   │ signature only       │
   │                  │  └─────────────────┤ (no JWT, no CORS)    │
   └── NEVER holds ───┘ service credential │                      │
       a /v1/* credential (mTLS / bearer), └── owns Redis + Postgres — NO other principal may reach Redis
                          realm-bound
```

Four rules follow from the diagram and are not negotiable:

1. **A browser never holds a `/v1/*` credential.** The host proxies `/v1/credits` behind its own
   session. A metering endpoint reachable from a browser is an endpoint anyone can spend against,
   and a `Grant` endpoint reachable from a browser is free money.
2. **Webhook routes carry no JWT and no CORS.** They are server-to-server and authenticated by
   signature. A session check there rejects the provider; a CORS policy there is meaningless.
3. **The service authenticates *callers*, not end users.** The host authenticates its people and
   hands over a `Scope`. Putting an end-user identity model in a billing service means putting a
   customer's org chart there too.
4. **Hosts never touch the hot store.** They hear about events through signed webhooks or the event
   feed ([outbound-events.md](../specs/001-metering-billing-core/contracts/outbound-events.md)).

---

## The hot store is a money store

The balance Redis holds every hot balance **and** the outbox streams the durable writer books from.
An `XADD` to `billing:outbox:{s<n>}` is indistinguishable from a real grant, and an `HINCRBY` on an
`acct:` key is a balance change. So **write access to this Redis is the ability to mint credits**,
and it is treated like a provider secret:

- Redis ACLs grant `paymentd` and `payment-worker` the key patterns and Redis Functions they need,
  and **no other principal anything** — not a host, not a dashboard, not a debugging shell in
  production. `FUNCTION LOAD` is restricted to the deploy role.
- It is network-isolated, never shared with a cache, `noeviction`, AOF on.
- Monitoring reads go through `/metrics`, never a direct Redis connection.

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
- **a privilege set** — any of `record` (admit, record, read), `grant` (mint, remove and transfer
  credits) and `admin` (configuration and operations).

Credentials are configured as `PAYMENT_SERVICE_TOKENS=<caller-id>:<sha256>:<realms>:<privileges>`
(bearer) or map a client-certificate subject to the same triple (mTLS).

### Realm binding

Every request's `Scope.realm` is checked against the credential's realm set. A mismatch is
`PERMISSION_DENIED` / `403 realm_mismatch`. **A realm is never taken from a request body**, which
is what makes the isolation real rather than advisory: a compromised caller for product A cannot
read, debit or grant in product B by naming it.

### Privilege separation

`Grant` and `Transfer` create and move credits. `Record` spends them. Configuration changes prices.
These are three privileges, separated at the RPC boundary, the HTTP route, the authorization check
and the audit trail — so a compromised spend producer (the component most exposed to host traffic)
can debit with idempotency but **never mint, move or reprice**.

Most deployments give no host `grant` at all: the internal webhook processor is the only minter.
`admin` belongs to operators' tooling, not to a service.

---

## Webhook posture

The full flow is in [payment-provider-ports.md](../specs/001-metering-billing-core/contracts/payment-provider-ports.md).
The security-critical parts:

- **The route names the provider account** (`/webhooks/{provider}/{account}`), so the secret — and
  the realms that account may touch — are known before a byte is parsed. An unknown account is `404`.
- **Verify on the raw body, before anything.** No parse, no DB read, no log of the contents,
  before the signature checks out. A verification that happens after a parse has already run
  attacker-controlled input through a parser.
- **Constant-time comparison.** A timing-variable compare leaks the signature a byte at a time.
- **Timestamp tolerance.** Without it, a captured valid payload replays forever. The dedup guard
  limits the damage, but only while the event row exists.
- **Body-size limit.** An unbounded webhook body is a trivial memory-exhaustion vector on a route
  that by design has no authentication in front of it.
- **No raw payload stored.** Only a SHA-256 of the verified body and the normalized facts. The one
  exception is a delivery whose verification is *unavailable* (PayPal's API down): its raw body is
  parked until it verifies, then deleted — a database constraint allows it for no other status.
- **Persist before acknowledging.** A verified delivery is durable in the inbox before `200`, so a
  crash cannot turn a provider retry into a lost grant.
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

- Provider keys and webhook secrets come from the environment or a secret manager, **named** by
  `provider_accounts.secret_ref` / `webhook_secret_ref`. Never a client bundle, never a
  `PUBLIC_`/`NEXT_PUBLIC_` variable, never a log line, never a database value.
- Outbound-webhook signing secrets are referenced the same way (`event_endpoints.secret_ref`), and
  support two active secrets during a rotation.
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

## Outbound events

- Every delivery is HMAC-SHA256 signed over `timestamp.body`, with the event id in a header; hosts
  verify in constant time within a 5-minute tolerance and dedup on the id.
- **SSRF guard:** an endpoint's resolved address must not be private, loopback or link-local
  (development excepted by configuration); redirects are not followed; bodies and time are capped.
- Events carry scope ids and amounts — never card data, secrets or email addresses.
- Creating an endpoint is an `admin` action and is audited: it is a new place money facts go.

## Audit

Every privileged action writes an `audit_log` row with actor, action, subject, before/after and a
reason: a grant not originating from a verified webhook, an entitlement override, a manual
reconcile, recovery or unblock, an adjustment, a rate-card publication, a plan or limit change.

**Configuration is audited by the database**, not by the application: a trigger on every
configuration table writes the row — and a cache-invalidation `NOTIFY` — in the same transaction as
the change. The admin API names the person and the reason (`intelpay.actor`, `intelpay.reason`); a
direct SQL write is still recorded, attributed to the database role that made it. `reason` and
`actor` are also `NOT NULL` on overrides at the schema level, because an unexplained override is
indistinguishable from a bug.

---

## Pre-production checklist

- [ ] `PAYMENT_SERVICE_AUTH` is `mtls` or `bearer`; `none` only on loopback/UDS.
- [ ] Every caller credential is realm-bound and privilege-scoped; no host holds `grant` or `admin` unless it must.
- [ ] No browser holds a `/v1/*` credential; the host proxies.
- [ ] Webhook routes: signature verified first, constant-time, timestamp tolerance, body-size limit, no CORS, no auth middleware, no body-rewrite before verification.
- [ ] Provider secrets in a secret manager; only publishable keys client-side; `gitleaks` green.
- [ ] Balance Redis: `noeviction`, AOF, **not shared with a cache**, network-isolated, and ACLs that give every principal other than `paymentd`/`payment-worker` nothing — verified by trying from a host's network with a host's credentials.
- [ ] Each provider account's `realms` is as narrow as possible; secrets are references, not values.
- [ ] Outbound endpoints are `https`; the SSRF guard refuses a private address.
- [ ] Postgres: TLS, least-privilege role, backups tested by an actual restore.
- [ ] `/metrics` and admin endpoints not publicly reachable.
- [ ] Verification-failure and drift alerts reach a human.
- [ ] A tampered webhook, a webhook for an unknown account, a cross-realm scope, a `record`-only caller attempting `Grant`, `Transfer` and an admin write, and a direct SQL config change (which must appear in `audit_log`) have each been tried against the deployment.
