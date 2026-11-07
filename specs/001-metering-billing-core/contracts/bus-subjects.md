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

`<tag>` is `Scope.SubjectToken()`: the scope's `Tag()` = `<realm>/<kind>:<id>` with the characters a subject reserves — `.`, `*`, `>`, whitespace and `%` itself — percent-encoded, so a scope id with a dot or a space in it can never split into two tokens or read as a wildcard. The realm is inside the token, so a subject can never be consumed as another product's. `SubjectPrefix` (default `billing`; `PAYMENT_SUBJECT_PREFIX`) is configurable, and the whole of this table follows it: the payment layer announces `<prefix>.payment.<tag>`, `<prefix>.entitlement.<tag>` and `<prefix>.webhook.received`, and auto top-up acts only on `<prefix>.balance.low.<tag>` — a low-balance message under another prefix is not this deployment's and is refused, not charged for.

| Subject | Publisher | Consumer | Payload (key fields) |
|---|---|---|---|
| *intents* on `billing:outbox:{s<n>}` | the hot functions only | `payment-worker`, the shard's owner | see [hot-path-consistency.md §2](./hot-path-consistency.md#2-the-hot-account-and-the-intent) |
| `billing.balance.low.<tag>` | the writer, after booking an intent that crossed a balance watch | billing auto top-up; outbound events (`credits.low`) | `{realm, scope, watch, seq}` — `scope` is the scope's tag (`<realm>/<kind>:<id>`) and `watch` is `<pool>:<threshold>`, the watch that was crossed. A **hint**: a consumer that acts on it re-reads what it needs, and treats a duplicate as normal (the intent that crossed is booked once, but the message is delivered at least once) |
| `billing.warn.<tag>` | the `Admit` path crossing `WarnAt` | outbound events (`credits.warning`) | `{realm, scope, limit_name, subject?, used, cap, window, resets_at}` — **coalesced** per scope + limit + window |
| `billing.blocked.<tag>` | the `Admit` path on a denial | outbound events (`credits.blocked`) | `{realm, scope, limit_name, deny_code, upgrade_path?}` — coalesced likewise |
| `billing.payment.<tag>` | the webhook processor; the auto top-up handler when it switches an instruction off | outbound events (`payment.*`) | `{realm, scope, event: succeeded\|failed\|refunded\|disputed\|charged_back\|renewed\|canceled, amount, currency, grace_until?, receipt_url?, reason?}` — `reason` is present only on a `failed` that announces an auto top-up being switched off, and says why (`charge_declined`, or the provider's own reason), so the host can ask the customer to update their card |
| `billing.entitlement.<tag>` | subscription change · override write · grace expiry | entitlement caches (each replica under a group of its own — see [entitlement-ports.md § The cache](./entitlement-ports.md#the-cache)); outbound events (`entitlements.changed`) | `{realm, scope:{kind,id}, reason}`, `reason` = `subscription` \| `grace_expired` \| `override` — evicts cached grants, so a customer who just paid does not wait out a TTL |
| `billing.config.<realm>` | the config relay (`ConfigRelay`: Postgres `LISTEN intelpay_config`, run by `payment-worker`) | every process caching cards, limits, pools, plans, entitlements | `{table, realm}` — so configuration changes need no deploy and no TTL wait. A table that belongs to no realm is announced as realm `(global)`, which every cache reads as "everything"; a notification the relay cannot read is announced as `(global)` too |
| `billing.webhook.received` | webhook ingress, after persisting a verified delivery | the webhook processor | `{event_id}` — a latency hint only; the inbox sweep is the guarantee |
| `billing.dlq.<subject>` | any consumer after `MaxAttempts` | the dead-letter sweeper (`payment-worker`, group `dead-letter-sweeper`) | the original message + attempts. **Never dropped** — see [Dead letters](#dead-letters) |

### Scheduled ticks

Published by an **external scheduler** onto `billing:ticks`; the `worker` group delivers each to one replica; every handler is an idempotent claim, so a duplicated tick has no second effect.

**How a tick is run.** `payment-worker tick <name> [json]` publishes one (`payment-worker tick journal`, `payment-worker tick reconcile '{"shard":3}'`): the door a cron or a CronJob uses, needing the worker's `PAYMENT_*` variables and only the bus. A worker replica receives it and runs it under a **claim** (`ports.Claimer`; PostgreSQL: a session advisory lock on `<tick>|<key>`, which writes nothing). The key is the tick alone, except for the per-shard ones — `reconcile` and `audit` carry `{"shard": N}` (and `reconcile` a `kind`, `incremental` by default or `full`), claim per shard and kind, and are **refused when they name no shard**: guessing "all of them" is how one worker would be handed every shard's work. Three things hold of every handler, and `app.Ticks` is the one place that makes them hold:

- **Two concurrent runs of one tick do the work once.** The second finds the claim held and is counted `skipped`.
- **A run killed midway is completed by the next.** The claim is a lock the server drops with the session, so a dead holder holds nothing; the work behind a tick is idempotent (every effect is keyed, every booking guarded), so the next run starts again from what the books say is left.
- **A tick with nothing due writes nothing.** It is counted `idle` (`metering_tick_total{tick,outcome}`: `ran`, `idle`, `skipped`, `failed`), and neither the claim nor the handler leaves a row. A reconcile asks first whether any scope was booked since the last one (`Writer.ReconcileDue`), because a reconcile that runs always records its run.

A per-shard tick runs under the shard's lease: this worker's own when it owns the shard, otherwise a lease taken for the duration; a shard another worker owns is that worker's, and the tick is `skipped`. A `failed` tick is an error to the bus, which delivers it again and then parks it in `dead_letters` ([Dead letters](#dead-letters)): a persistent failure pages.

**Served today:** every tick below. `billing.subdrift.tick` is the sweep that asks each provider for the current state of every live subscription (`Subs.LiveOf` by keyset, then `FetchSubscription`) and applies it through the one door that writes status, so a state the books already hold is not written, announced or counted as a repair. Its payload is `{"account": "..."}` for one account or nothing for every account that has a live subscription (a payload it cannot read is refused, never taken to mean "every account"); the claim is on the account, so two accounts are swept side by side. `payment-worker` also runs it itself every six hours, the first run an interval after start, with an hour to finish; `billing_subdrift_total{outcome}` counts what it found, and a steady count of `repaired` is a webhook being missed. `POST /v1/admin/subdrift` ([rest-api.md](./rest-api.md)) is the same sweep for one account, asked for by a person. A payment that never arrived is still repaired by redelivering it from the provider's dashboard, which the inbox makes safe.

**What also runs without a scheduler.** `payment-worker` keeps its own timers for the work it always did (journal replay, transfer re-drive, expiry, each owned shard's reconcile and audit, the webhook inbox, outbound deliveries and the purges) and, hourly and under the same claim, for the three nothing else would drive: `partitions`, `idem.purge` and `trim` (hourly) and for `usage.rollup` (every five minutes). A scheduler is therefore for a different cadence, an off-peak audit, or a manual run — not a requirement.

| Tick | Does | Cadence |
|---|---|---|
| `billing.reconcile.tick` | incremental reconcile per shard ([§8](./hot-path-consistency.md#8-reconcile-audit-and-resharding)); `{shard, kind: full}` on `ReconcileFullSweepInterval` | 5 min / 24 h |
| `billing.audit.tick` | deep audit per shard; advances `ledger_checkpoints` | daily, off-peak |
| `billing.transfer.tick` | re-drives transfers `in_transit` longer than `TransferRedrive` | 1 min |
| `billing.journal.tick` | replays pending `spend_journal` rows through the hot function | 10 s |
| `billing.webhook.tick` | processes `received`/`failed` inbox rows due for (re)processing; retries `unverifiable` verification | 10 s — **not published today**: `payment-worker` runs the same work from a timer of its own, `PAYMENT_WEBHOOK_TICK_INTERVAL` (default 5 s), and nothing subscribes to this subject |
| `billing.outbound.tick` | delivers pending outbound webhooks with backoff | 5 s — **not published today**: `payment-worker` runs the same work from a timer of its own, `PAYMENT_EVENTS_TICK_INTERVAL` (default 5 s), and nothing subscribes to this subject |
| `billing.expiry.tick` | expires credit lots, pre-applying the upper bound ([§3](./hot-path-consistency.md#3-the-durable-writer-ordered-batched-idempotent)) | hourly, under `lots_fefo` |
| `billing.dunning.tick` | suspends subscriptions past `GraceUntil` | hourly |
| `billing.subdrift.tick` | `FetchSubscription` for live subscriptions; repairs divergence — **the backstop for a missed webhook** | every six hours (the worker's own timer) |
| `billing.partitions.tick` | creates ledger (monthly, 3 ahead) and usage guard and usage events (daily, 4 ahead) partitions ahead of need; alarms on rows in a DEFAULT partition | daily (the worker also runs it hourly) |
| `billing.idem.purge.tick` | drops the daily `usage_idem` partitions that lie wholly before `now − UsageIdemWindow` (a day with any instant inside the window is kept; the DEFAULT partition and any partition not made by `ensure_daily_partitions` are never touched; each drop waits two seconds for its lock at most) | daily (the worker also runs it hourly) |
| `billing.events.purge.tick` | purges `payment_events` past retention (never inside a provider replay window) and delivered outbound events past theirs | daily — **the outbound half is not published today**: `payment-worker` purges events hourly from a timer of its own (`PAYMENT_EVENT_RETENTION`) |
| `billing.trim.tick` | **Redis Streams only**: trims acked history below the retention floor (`PAYMENT_BUS_RETENTION`) on the events and ticks streams and on each shard's outbox | hourly (the worker also runs it hourly) |
| `billing.usage.rollup.tick` | recomputes the `usage_daily` rollup (below) for the days still open to a late row — the usage dedup window and a day — and replaces them; `{"days": N}` (1 to 3660) reaches further back, for a backfill. A window nothing changed in writes nothing | every 5 min (the worker runs it itself) |

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

**Semantics both adapters share** (`BusContract` asserts each one):

- **Patterns** are NATS': tokens separated by `.`, `*` for exactly one token, `>` — last token only — for one or more. Publishing to a wildcard is refused.
- **A group is per (name, pattern).** Consumers that share both compete for the work, each message going to one of them; a different pattern under the same name is a different subscription, and a different name is another group that sees everything. (On Redis Streams all events share one stream, so a group keyed by name alone would hand a message to a consumer of another pattern, which would skip it.)
- **A message published before anyone subscribed is delivered** — a group starts at the beginning of the stream.
- **Redelivery.** A handler that returns an error or panics gets the message again after a backoff (`BackoffBase`, doubling, never longer than `AckWait`); a consumer that stalls without acknowledging has its message taken over by another in the group after `AckWait`. `Message.Attempts` is the delivery count.
- **Dead letters.** After `MaxAttempts` deliveries the message goes to `<prefix>.dlq.<subject>` as `{"subject", "id", "attempts", "error", "payload"}` (`payload` base64, the original bytes) and is acknowledged — it is **never dropped**, and never redelivered again.
- **`Pending`** is what the group has not finished with — delivered and un-acked, plus not yet delivered — and the age of the oldest of either. A group nobody has joined has none.
- **`Trim`** removes an entry only when every group has acknowledged it and it is older than `MinAge`; `MaxLen` is a length to trim *toward*. A stream no group has read is not trimmed. `IntentStream.Trim` does the same for an outbox.
- **Ordering** is per subject, in publish order, for a single consumer.
- **A message delivered more often than `MaxAttempts` without an outcome** — its consumers kept dying on it — goes to the DLQ without another attempt.

**Redis Streams layout.** Events go to `<prefix>:events` and ticks (a subject ending `.tick`) to `<prefix>:ticks`; each entry carries its `subject` and `payload`. Each group is `<name>|<pattern>`. Dead letters of a tick are events.

**NATS JetStream layout.** One stream, named after the prefix upper-cased (`BILLING`), holds `<prefix>.>` with file storage, the configured replication (R3/R5 in production) and `MaxAge = BusRetention`. Each subscription is a durable pull consumer named by `natsjetstream.ConsumerName(group, pattern)` (a sanitised group plus a hash of the pair, so consumers that share both compete and any other pair is separate), with explicit acks, `AckWait`, unlimited `MaxDeliver` — the adapter dead-letters by attempt count itself, because JetStream stops redelivering at `MaxDeliver` *without* parking the message, which would drop it. `Trim` is a no-op: **an un-acknowledged message older than `BusRetention` is lost to the stream's own limit**, so `BusRetention` must exceed the longest outage a consumer is allowed, and `Pending`'s age must page long before it.

**Intents under JetStream.** The hot functions still write `billing:outbox:{s<n>}` — a Redis Function cannot publish to NATS. A **Relay** reads each shard's outbox through a consumer group of its own, publishes every entry whole to `<prefix>_intents.<shard>` (deliberately *outside* `<prefix>.>`: a subscriber to the whole bus must never be handed a money intent) with the JetStream message id `<shard>-<Redis entry id>`, and acknowledges it in Redis only after JetStream has stored it. A relay that dies between the two republishes, and the stream — whose duplicate window is an hour — recognises the id; past the window the writer's own idempotent booking is the backstop, so an intent is never booked twice. The outbox is then trimmable through the relay's group. The writer reads `<PREFIX>_INTENTS` through the same `IntentStream` port, one durable consumer per shard: a takeover (`Read` with `minIdle <= 0`, the recovery drain, which owns the shard) relays the outbox first and recreates the consumer at its acknowledgement floor, so everything un-acknowledged is delivered at once and booking's idempotency absorbs the redeliveries; and `Stats` counts what still sits in the outbox as undelivered, so a recovery cannot finish a drain with money in flight.

The writer reads intents through a narrower port of the same adapter — `IntentStream.Read(shard, count, minIdle)` (entries un-acked past `minIdle` are reclaimed first, oldest first, then new ones) / `Ack(shard, ids)` / `Stats(shard)` / `Trim(shard, keep)` — because it needs per-shard ownership and in-order batches, not a generic subscription.

## Dead letters

A message that fails `MaxAttempts` times is the **Bus's** to redeliver until then, with backoff, and to publish whole to `<prefix>.dlq.<subject>` at the cap (above). What happens next is `payment-worker`'s sweeper (`app.DeadLetters`, consumer group `dead-letter-sweeper`, subject `<prefix>.dlq.>`), and it is the same on both adapters because it speaks only to the `Bus` port and to a store:

- **Park.** Each envelope is written to `dead_letters` — subject, payload byte for byte, attempts, the last error, the realm its payload names (`(global)` when it names none, as a tick's does) — and **an envelope that is not an envelope** (it does not parse, or names no subject or id) is parked as it arrived, under the dead-letter subject it arrived on: a message nobody can read is the one an operator most needs to see. The letter's identity is `(subject, source id)` — the subject it failed on and the Bus's id for that delivery — so an envelope delivered twice, or swept by two replicas at once, is **one letter and one page**. A failure to park is returned to the Bus, which delivers the envelope again: it is in the stream until it is in the store.
- **Page.** `metering_dead_lettered_total{realm}` counts once per parking, and `metering_dead_letters{realm}` is the number nobody has replayed, read from the store so it survives the process that parked them. The page is written against the gauge.
- **Never delivered again.** Nothing automatic re-publishes a parked message: redelivery below the cap is the Bus's, and at the cap the message is parked. The only way back is an operator's **replay**, which claims the letter for a lease, re-publishes the payload to its original subject, and records that it did. Two replays at once publish once; a publish that fails gives the claim back, so the next replay tries again at once; a replay killed between the publish and the record lets its lease lapse, and the next replay publishes again — a duplicate delivery, which every handler of a bus subject is built to absorb (below). A replayed letter is **kept**, marked with when, and never claimed again; if the replayed message fails again it is a new envelope, a new letter and a new page. A letter that is not an envelope has no subject to go back to and is refused a replay (`invalid`).
- **Nothing is ever deleted.** The database refuses a `DELETE` of a letter and any rewrite of what it says about the failure (`dead_letters_keep`), so this does not rest on the application.

The replay route (`POST /v1/admin/dead-letters/{id}/replay`, [rest-api.md](./rest-api.md)) is the door to `Replay`.

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
- Two schedulers publishing the same `billing.reconcile.tick` produce exactly one reconcile for the shard — and, for every tick: two concurrent runs do the work once, a run killed midway is completed by the next, and a tick with nothing due writes nothing (`app.Ticks` over all fourteen names; `ClaimerContract` over an in-memory claimer and PostgreSQL; and a real worker serving each tick, writing no row in an idle round).
- A message under the attempt cap is redelivered by the Bus and applied at most once; at the cap it lands in `dead_letters`, pages once however often its envelope is delivered, and is never delivered again except by an operator's replay, which delivers it once (`DeadLetterContract`, run over the in-memory store and PostgreSQL, and a property over any interleaving of failures, duplicate envelopes, replays — racing, failing and killed midway — in `metering/app`).
- Publishing to realm A's subject never affects anything in realm B.
- **Trim never removes an un-acked entry**, even when the stream is over its length target.
- `Pending` reports a non-zero oldest-age within one ack-wait of a stalled consumer.
- Under JetStream, an intent reaches the writer only after it is durable in the Redis outbox, and the relay's redelivery never books it twice.

The intent-specific obligations — duplicate intents book once, a redelivery is told apart from a regression, a negative grant obeys `NegativeBalancePolicy` — belong to the `LedgerWriterContract` (booking, redelivery vs gap vs regression, suspense, reconcile) and the `ConsistencyContract` ([hot-path-consistency.md §9](./hot-path-consistency.md#9-verification): a rollback of the hot tier).
