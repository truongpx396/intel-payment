# Contract: Outbound Events (how a host hears what happened)

**Spec**: [../spec.md](../spec.md) | **Status**: Normative | **Decision**: [D35](../design-decisions.md#decisions-from-the-second-design-review)

A host needs to know when a scope is running low, was refused, paid, was refunded, or gained a
capability — to send an email, show a banner, or unlock a feature. It must learn this **without
connecting to this service's internals**. The internal bus is the balance Redis, and write access to
it is the ability to mint credits ([bus-subjects.md](./bus-subjects.md)); a host that can read it can
usually write it. So events leave through two doors built for outsiders, in any language:

| | **Signed webhooks** (push) | **Event feed** (pull) |
|---|---|---|
| For | hosts that can receive HTTPS | hosts behind a firewall, batch consumers, backfills |
| How | `POST` to each subscribed endpoint, HMAC-signed, retried | `GET /v1/events?after=<id>` with the service credential |
| Guarantee | at-least-once, retried with backoff for 72 h | every event for `EventRetention` (default 30 d), in `id` order |

Both read the same log (`outbound_events`), so a host can use webhooks for latency and the feed to
backfill anything a failed endpoint missed.

---

## The envelope

```json
{
  "id": "0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9d0e",
  "type": "credits.low",
  "api_version": "2026-09-01",
  "created_at": "2026-09-28T10:15:42.103Z",
  "realm": "my-product",
  "scope": { "kind": "organization", "id": "org_1" },
  "data": { "pool": "general", "threshold": 5000000, "balance": 4987000, "seq": 1843 }
}
```

- `id` is a UUID v7: unique, time-ordered, and the host's **dedup key** — delivery is at-least-once.
- `api_version` pins the payload shape. A breaking change ships as a new version; an endpoint keeps
  receiving the version it registered with until it opts in (see the deprecation policy in
  [rest-api.md](./rest-api.md#versioning-and-deprecation)).
- Amounts are integers: credits in the accounting unit, fiat in minor units plus `currency`.
- `data` carries what the host needs to act and a **version marker** (`seq` for a scope's balance,
  `provider_timestamp` for a subscription), because delivery order is not guaranteed and a host must
  be able to discard an event older than one it already applied.

## Event catalogue

| Type | When | `data` (key fields) |
|---|---|---|
| `credits.low` | a pool crossed a balance watch downward | `pool, watch_key, threshold, balance, seq` |
| `credits.warning` | a ceiling crossed its `WarnAt` (coalesced per window) | `limit, subject?, used, cap, window, resets_at` |
| `credits.blocked` | admission refused (coalesced per window) | `limit, deny_code, upgrade_path?` |
| `credits.granted` | a grant was booked | `pool, amount, reason, idem_key, seq` |
| `credits.revoked` | a refund or chargeback clawback was booked | `pool, amount, reason, writeoff?, seq` |
| `transfer.settled` | both sides of a transfer are booked | `from, to, amount, idem_key` |
| `payment.succeeded` · `payment.failed` · `payment.refunded` | the processor booked the payment state | `payment_id, amount, currency, refunded?, receipt_url?, failure_reason?` |
| `payment.disputed` · `payment.charged_back` · `payment.dispute_won` | dispute lifecycle | `payment_id, dispute_id, amount, currency` |
| `subscription.updated` · `subscription.canceled` | subscription state applied | `plan_code, status, current_period_end, grace_until?, provider_timestamp` |
| `entitlements.changed` | a plan change, override or grace expiry changed what the scope may do | `reason` — the host re-reads `GET /v1/entitlements` |
| `fulfilment.granted` | the credits for a checkout arrived | `checkout_ref, amount` — the event behind the UI's "processing → granted" |
| `auto_recharge.failed` · `auto_recharge.disabled` | an off-session top-up failed, or failures disabled it | `plan_code, failure_reason, consecutive_failures` |
| `account.blocked` · `account.unblocked` | `block_and_flag` held the scope, or an operator cleared it | `reason` |

Events carry scope ids and amounts, never card data, provider secrets or a billing email.

---

## Webhook delivery

**Endpoints** are realm configuration (`event_endpoints`), managed through the admin API and audited:
a URL (`https` only; `http://localhost` for development), the event types it subscribes to, and a
signing secret held by reference.

**Signature.** Every request carries:

```text
Intel-Payment-Event-Id:  <event id>
Intel-Payment-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<raw body>")>
```

During a secret rotation the header carries one `v1=` per active secret, so the host can roll its
side without a gap. The host MUST verify on the raw body, in constant time, and reject a `t` more
than 5 minutes from its clock — the same posture this service holds toward its own providers.

**Retries.** A `2xx` within 10 s is delivered. Anything else — a timeout, a `4xx`, a `5xx` — is
retried with exponential backoff and jitter (1 m, 5 m, 30 m, 2 h, then every 6 h) for 72 h, then
marked `dead`, counted (`events_delivery_failed_total`), and paged. A dead delivery can be
re-driven from the admin API, and the feed always has the event.

**Ordering** is not guaranteed — retries alone reorder deliveries. Use the version marker in `data`.

**Host obligations:** verify the signature; dedup on `id`; answer `2xx` fast and process
asynchronously; tolerate unknown event types and unknown fields (they are how the catalogue grows
without a version bump).

## The feed

```http
GET /v1/events?after=<event id>&types=credits.low,payment.succeeded&limit=100
→ { "data": [ <envelope>, … ], "next": "<event id>" }
```

Scoped to the credential's realms, ordered by `id`, retained for `EventRetention`. A consumer
stores the last `id` it processed and resumes from it; there is no server-side cursor state to lose.

## Security

- **Hosts never hold Redis credentials.** Events reach them only through these two doors.
- **Outbound requests are SSRF-guarded:** the resolved address of an endpoint must not be private,
  loopback or link-local (development excepted by configuration), redirects are not followed, and
  the body is capped.
- **Endpoint writes are privileged and audited** — a new endpoint is a new place money facts go.

## Contract-test obligations

- A delivery carries a signature that verifies with the endpoint's secret and fails with any other.
- A `5xx` endpoint is retried on the backoff schedule and marked `dead` after the window, with an alert.
- The same event delivered twice carries the same `id`.
- An endpoint receives only the types it subscribed to, and only its own realm's events.
- The feed returns every event after a cursor exactly once, in `id` order, across pages.
- An endpoint resolving to a private address is refused before any request is sent.
