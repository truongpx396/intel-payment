# Contract: Bus — streams, subjects, and the two adapters

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the async seam between the hot tier, the sole durable writer, and everything that reacts to money moving.

The bus decouples *producing* a money intent from *booking* it. That decoupling is what keeps exactly one durable writer (metering invariant 1) while any number of request-serving replicas move balances.

It is **internal**. Hosts never connect to it — they receive signed webhooks or read the event feed ([outbound-events.md](./outbound-events.md)). With the default adapter the bus *is* the balance Redis, and write access to it is the ability to mint credits.

---

## Two kinds of traffic

| | **Intents** | **Events and ticks** |
|---|---|---|
| What | the record of one hot-tier mutation: usage, grant, transfer leg, expiry, correction | everything else: warnings, payments, cache invalidation, scheduled work |
| Written by | **only** the hot functions ([reference/hot_path.lua](./reference/hot_path.lua)), in the same atomic call that moves the balance | the worker, the webhook processor, the config relay, the scheduler |
| Where | `billing:outbox:{s<n>}` — one stream per shard, in the **same hash slot** as the shard's accounts | `billing:events`, `billing:ticks` |
| Read by | the shard's single owning writer, in per-scope sequence order | consumer groups (below) |
| Format | [hot-path-consistency.md §2](./hot-path-consistency.md#2-the-hot-account-and-the-intent) | JSON payloads below |

An intent is never published by `Bus.Publish`, by a host, or by a handler: an XADD to an outbox stream that did not come from a hot function would be a balance change the hot tier never made. The Redis ACL for every principal other than `paymentd` and `payment-worker` excludes `billing:outbox:*`.

---

## The two adapters

`app` depends on a **`Bus` port**, never a broker.

| | **Redis Streams** (default) | **NATS JetStream** (optional) |
|---|---|---|
| Extra infrastructure | **none** — the Redis the balances already require | a NATS cluster |
| Intents | written by the hot function into `billing:outbox:{s<n>}`, atomically with the balance | **still** written to the Redis outbox stream (a Redis Function cannot publish to NATS); a relay forwards them to JetStream once they are durable there |
| Events and ticks | Redis streams | JetStream subjects |
| Durability | AOF (`everysec` by default; `WAITAOF` via `HotAckWait`) | file-backed, per-stream fsync |
| Replication | Redis replicas (asynchronous) | **R3/R5 quorum**, synchronous |
| Cross-region | manual | stream mirrors / sources |
| Redelivery of a dead consumer's work | `XAUTOCLAIM` past `AckWait` (the adapter runs it) | ack-wait redelivery |
| Retention | **explicit trim required** (`billing.trim.tick`) | stream limits + policies |

**Redis Streams is the default** because it removes a failure class: the intent is written by the same atomic function, in the same slot, that moves the balance — so there is no moment at which a balance has moved and its intent is not durable on the bus. It also keeps the required infrastructure at **Redis + Postgres**.

**Choose JetStream** for quorum-replicated or cross-region-mirrored *downstream* traffic, or to consolidate on an existing JetStream estate. Be precise about what it buys for money intents: they are durable in the Redis outbox first and the relay adds a hop behind it, so JetStream improves what happens *after* the outbox, not the hot path's own loss window. That window is set by `HotAckWait` or removed by `journal` settlement ([hot-path-consistency.md §5](./hot-path-consistency.md#5-cold-start-loss-regression-and-freeze)).

Switching is a `cmd/` wiring change. No subject, payload, handler or invariant differs.

---

## Subjects

`<tag>` is `Scope.Tag()` = `<realm>/<kind>:<id>`. The realm is inside the token, so a subject can never be consumed as another product's. `SubjectPrefix` (default `billing`) is configurable.

| Subject | Publisher | Consumer | Payload (key fields) |
|---|---|---|---|
| *intents* on `billing:outbox:{s<n>}` | the hot functions only | `payment-worker`, the shard's owner | see [hot-path-consistency.md §2](./hot-path-consistency.md#2-the-hot-account-and-the-intent) |
| `billing.balance.low.<tag>` | the writer, after booking an intent that crossed a balance watch | billing auto top-up; outbound events (`credits.low`) | `{realm, scope, pool, watch_key, threshold, balance, seq}` |
| `billing.warn.<tag>` | the `Admit` path crossing `WarnAt` | outbound events (`credits.warning`) | `{realm, scope, limit_name, subject?, used, cap, window, resets_at}` — **coalesced** per scope + limit + window |
| `billing.blocked.<tag>` | the `Admit` path on a denial | outbound events (`credits.blocked`) | `{realm, scope, limit_name, deny_code, upgrade_path?}` — coalesced likewise |
| `billing.payment.<tag>` | the webhook processor | outbound events (`payment.*`) | `{realm, scope, event: succeeded\|failed\|refunded\|disputed\|charged_back\|renewed\|canceled, amount, currency, grace_until?, receipt_url?}` |
| `billing.entitlement.<tag>` | subscription change · override write · grace expiry | entitlement caches; outbound events (`entitlements.changed`) | `{realm, scope, reason}` — evicts cached grants, so a customer who just paid does not wait out a TTL |
| `billing.config.<realm>` | the config relay (Postgres `LISTEN intelpay_config`) | every process caching cards, limits, pools, plans | `{table, realm}` — so configuration changes need no deploy and no TTL wait |
| `billing.webhook.received` | webhook ingress, after persisting a verified delivery | the webhook processor | `{event_id}` — a latency hint only; the inbox sweep is the guarantee |
| `billing.dlq.<subject>` | any consumer after `MaxAttempts` | the DLQ sweeper | the original message + attempts. **Never dropped** |

### Scheduled ticks

Published by an **external scheduler** onto `billing:ticks`; the `worker` group delivers each to one replica; every handler is an idempotent claim, so a duplicated tick has no second effect.

| Tick | Does | Cadence |
|---|---|---|
| `billing.reconcile.tick` | incremental reconcile per shard ([§8](./hot-path-consistency.md#8-reconcile-audit-and-resharding)); `{shard, kind: full}` on `ReconcileFullSweepInterval` | 5 min / 24 h |
| `billing.audit.tick` | deep audit per shard; advances `ledger_checkpoints` | daily, off-peak |
| `billing.transfer.tick` | re-drives transfers `in_transit` longer than `TransferRedrive` | 1 min |
| `billing.journal.tick` | replays pending `spend_journal` rows through the hot function | 10 s |
| `billing.webhook.tick` | processes `received`/`failed` inbox rows due for (re)processing; retries `unverifiable` verification | 10 s |
| `billing.outbound.tick` | delivers pending outbound webhooks with backoff | 5 s |
| `billing.expiry.tick` | expires credit lots, pre-applying the upper bound ([§3](./hot-path-consistency.md#3-the-durable-writer-ordered-batched-idempotent)) | hourly, under `lots_fifo` |
| `billing.dunning.tick` | suspends subscriptions past `GraceUntil` | hourly |
| `billing.subdrift.tick` | `FetchSubscription` for locally active subscriptions; repairs divergence — **the backstop for a missed webhook** | daily |
| `billing.partitions.tick` | creates ledger (monthly) and dedup/archive (daily) partitions ahead of need; alarms on rows in a DEFAULT partition | daily |
| `billing.idem.purge.tick` | drops `usage_idem` partitions older than `UsageIdemWindow` | daily |
| `billing.events.purge.tick` | purges `payment_events` past retention (never inside a provider replay window) and delivered outbound events past theirs | daily |
| `billing.trim.tick` | **Redis Streams only**: trims acked history below the retention floor | hourly |

---

## The `Bus` port

```go
package ports

// Bus is the durable, at-least-once async seam for events and ticks. Both adapters satisfy it
// identically. Intents do not go through Publish: the hot functions write them.
type Bus interface {
	// Publish appends to a subject's stream. Durable on return.
	Publish(ctx context.Context, subject string, payload []byte) error

	// Subscribe joins a named consumer group and delivers at-least-once. The handler returning
	// nil acks; an error redelivers after backoff, up to MaxAttempts, then the DLQ.
	Subscribe(ctx context.Context, subject, group string, h Handler) (Subscription, error)

	// Pending reports un-acked work for a group: the count and the age of the oldest. This is
	// what `metering_outbox_depth` and `_age_seconds` export.
	Pending(ctx context.Context, subject, group string) (count int64, oldest time.Duration, err error)

	// Trim drops acked history below a retention floor. A no-op on JetStream; REQUIRED on Redis
	// Streams, where nothing else does it.
	Trim(ctx context.Context, subject string, keep RetentionPolicy) error
}

type Handler func(ctx context.Context, m Message) error

type Message struct {
	Subject  string
	ID       string // stream entry id / sequence — the adapter's, not ours
	Payload  []byte
	Attempts int    // delivery count; drives DLQ escalation
}
```

The writer reads intents through a narrower port of the same adapter — `IntentStream.Read(shard, count, minIdle)` (entries un-acked past `minIdle` are reclaimed first, oldest first, then new ones) / `Ack(shard, ids)` / `Stats(shard)` — because it needs per-shard ownership and in-order batches, not a generic subscription.

## Rules

- **At-least-once, idempotent handlers.** Every effect is an idempotent claim (`credit_idem`, `usage_idem`, a lease, a conditional `UPDATE`). Correctness never rests on "only one replica runs" or "the broker delivers once".
- **Only hot functions write intents.** Not a host, not a handler, not an admin script. Redis write access to `billing:outbox:*` or `acct:*` is mint access and is granted to `paymentd` and `payment-worker` only.
- **Security context is authoritative from the publisher.** A consumer never reads scope, realm or amount from untrusted content; it uses what the publisher stamped after resolving it server-side.
- **Scheduled work is externally triggered and single-owner.** Request-serving tiers run no in-process ticker.
- **`billing.warn` and `billing.blocked` are coalesced; intents are not.** A warning repeats every request past a threshold; money never collapses.
- **Retention is explicit.** On Redis Streams, `billing.trim.tick` keeps entries until acked **and** older than `BusRetention` (default 7 d). Never trim un-acked entries — that is deleting money intents.
- **Backpressure is visible, never silent.** At `BusMaxLen` the hot function returns `BACKPRESSURE`, and `Record` defers to the journal (invariant 16). Nothing is trimmed or dropped to make room.
- **Hosts are not consumers.** Everything a host needs to hear arrives through [outbound-events.md](./outbound-events.md).

## Contract-test obligations

`BusContract` runs against **both** adapters — the only way "swappable" means anything.

- A consumer killed mid-handler has its in-flight entry **redelivered**, not lost — `XAUTOCLAIM` on Redis, ack-wait on JetStream.
- Two schedulers publishing the same `billing.reconcile.tick` produce exactly one reconcile for the shard.
- A parked DLQ message under the attempt cap is re-driven and applied at most once; at the cap it lands in `dead_letters`, alerts, and is never re-driven again.
- Publishing to realm A's subject never affects anything in realm B.
- **Trim never removes an un-acked entry**, even when the stream is over its length target.
- `Pending` reports a non-zero oldest-age within one ack-wait of a stalled consumer.
- Under JetStream, an intent reaches the writer only after it is durable in the Redis outbox, and the relay's redelivery never books it twice.

The intent-specific obligations — duplicate intents book once, a redelivery is told apart from a regression, a negative grant obeys `NegativeBalancePolicy` — belong to the `LedgerWriterContract` (booking, redelivery vs gap vs regression, suspense, reconcile) and the `ConsistencyContract` ([hot-path-consistency.md §9](./hot-path-consistency.md#9-verification): a rollback of the hot tier).
