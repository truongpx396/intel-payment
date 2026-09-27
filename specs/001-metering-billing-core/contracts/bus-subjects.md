# Contract: Bus Subjects

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the async seam between spend producers and the sole durable writer.

The bus decouples *producing* a money intent from *recording* it. That decoupling is what keeps exactly one durable writer (metering invariant 1) while any number of services produce spend.

`<tag>` is `Scope.Tag()` = `<realm>/<kind>:<id>`. The realm is inside the token, so a subject can never be consumed by the wrong product's worker. `SubjectPrefix` (default `billing`) is configurable, because a host with an existing subject namespace must be able to fit in.

## Subjects

| Subject | Publisher | Consumer | Payload (key fields) |
|---|---|---|---|
| `billing.deduct.<tag>` | any spend producer, or the Redis outbox drain | `payment-worker` (queue group) | `{realm, scope, delta, operation_type, rate_card_version, cost_micros, idem_key, actor, trace_id}` → applies the hot-path effect idempotently and writes the `credit_ledger` row. **The worker is the sole ledger writer**; producers publish and never insert |
| `billing.grant.<tag>` | the webhook ingress, post-verify; audited admin tooling | `payment-worker` (queue group) | `{realm, scope, delta (signed), operation_type, payment_id, idem_key, trace_id}` → one positive (or negative) ledger row + one hot-balance apply. Replays converge |
| `billing.warn.<tag>` | the `Admit` path when a limit crosses `WarnAt` | the host's notification system | `{realm, scope, limit_name, used, cap, window, resets_at, idem_key}` → a near-limit warning. **Coalesced** per scope+limit+window, so approaching a ceiling notifies once, not once per request |
| `billing.blocked.<tag>` | the `Admit` path on a denial | the host's notification system | `{realm, scope, limit_name, deny_code, upgrade_path?, idem_key}` |
| `billing.payment.<tag>` | the webhook ingress | the host's notification system | `{realm, scope, event: succeeded\|failed\|refunded\|renewed\|canceled, amount, currency, grace_until?, receipt_url?, idem_key}` → dunning and receipt notifications |
| `billing.entitlement.<tag>` | subscription change · override write · grace expiry | entitlement cache invalidators | `{realm, scope, reason}` → evict the scope's cached grants, so a customer who just paid does not wait out a TTL (entitlement invariant 9) |
| `billing.reconcile.tick` | external scheduler (`CronJob` / timer / cron) | `payment-worker` (queue group) | `{shard, bucket, trace_id}` → one worker reconciles that bucket under `SET NX reconcile:lock:{shard}:{bucket}` |
| `billing.expiry.tick` | external scheduler | `payment-worker` (queue group) | `{realm, trace_id}` → expires credit lots FIFO. **Only when `CreditExpiry=lots_fifo`** |
| `billing.dunning.tick` | external scheduler | `payment-worker` (queue group) | `{trace_id}` → suspends subscriptions whose `GraceUntil` has passed; publishes `billing.payment.<tag>` |
| `billing.subdrift.tick` | external scheduler | `payment-worker` (queue group) | `{provider, trace_id}` → `FetchSubscription` for locally-active subs and repairs divergence. **This is the backstop for a missed webhook**, and without it a dropped `subscription.deleted` leaves a cancelled customer entitled indefinitely |
| `billing.events.purge.tick` | external scheduler | `payment-worker` (queue group) | `{trace_id}` → purges `payment_events` past retention (never inside the provider replay window) |
| `billing.dlq.<tag>` | any consumer after exhausting retries | DLQ sweeper (`payment-worker`, scheduled) | Parked intents with `dlq_attempts`. Re-driven under backoff while below the cap; then parked in `dead_letters` with an alert. **Never dropped** — a dropped money intent is silent under-billing |

## Rules

- **Durable transport, queue groups, idempotent handlers.** Every subject is a durable stream consumed by a queue group; at-least-once delivery is assumed, and every handler's effect is an idempotent atomic claim (`credit_idem (realm, idem_key)`, `SET NX`, atomic pop, conditional `UPDATE`). Correctness never rests on "only one replica runs."
- **Security context is authoritative from the publisher.** A consumer never reads scope, realm or amount from untrusted content; it uses what the publisher stamped, and the publisher resolved it server-side.
- **Scheduled work is externally triggered and single-owner.** Request-serving tiers run no in-process ticker. A scheduler publishes the `*.tick`; the queue group delivers it to exactly one worker; the handler is idempotent, so a duplicate tick or an overlapping replica produces no duplicate effect.
- **`billing.warn` is coalesced, `billing.deduct` is not.** A warning is advisory and repeats every request once a threshold is crossed, so it collapses per scope+limit+window. A deduction is money and never collapses.
- **The bus is a port, not NATS.** `app` depends on the `Bus` interface; the reference adapter is NATS JetStream. A host on Kafka, SQS or Pub/Sub writes one adapter and changes nothing else.
- **Subject prefix is configurable**, and its default (`billing`) is a convention, not a requirement.

## Contract-test obligations

- A duplicated `billing.deduct` with the same `idem_key` inserts **exactly one** ledger row.
- A duplicated `billing.grant` inserts exactly one row and applies one hot-balance change.
- A `billing.grant` with a **negative** delta exceeding the balance obeys the configured `NegativeBalancePolicy` — and under `clamp_to_zero` writes the compensating `writeoff` row, so `balance == SUM(ledger)` still holds.
- Two schedulers publishing the same `billing.reconcile.tick` produce exactly one reconcile for the bucket.
- A `billing.subdrift.tick` against a subscription cancelled at the provider but active locally repairs the local row.
- A parked `billing.dlq` intent under the attempt cap is re-driven and applied at most once; at the cap it lands in `dead_letters` and emits an alert, and is never re-driven again.
- Publishing to `billing.deduct.<realm-A-tag>` never affects a balance in realm B.
