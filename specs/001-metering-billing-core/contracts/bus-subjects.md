# Contract: Bus — subjects, streams, and the two adapters

**Spec**: [../spec.md](../spec.md) | **Status**: Normative — the async seam between spend producers and the sole durable writer.

The bus decouples *producing* a money intent from *recording* it. That decoupling is what keeps exactly one durable writer (invariant 1) while any number of services produce spend.

`app` depends on a **`Bus` port**, never a broker. Two adapters ship:

| | **Redis Streams** (default) | **NATS JetStream** (optional) |
|---|---|---|
| Extra infrastructure | **none** — the same Redis the balances already require | a NATS cluster |
| Hot-path publish | **in the same Lua script as the balance update** — the outbox *is* the stream | a second hop after the outbox drain |
| Durability | AOF (`appendfsync everysec` default, `always` available) | file-backed store, per-stream fsync policy |
| Replication | Redis replicas (asynchronous) | **R3/R5 quorum**, synchronous |
| Cross-region | manual | stream mirrors / sources, built in |
| Redelivery of a dead consumer's work | `XAUTOCLAIM` on a min-idle threshold (the adapter runs it) | native ack-wait redelivery |
| Retention | **explicit trim required** (`MAXLEN`/`MINID`) or the stream grows forever | stream limits + policies |
| Operational surface | one system | two systems |

**Redis Streams is the default**, and the reason is not preference: it removes an entire class of failure. With a broker, the hot path writes an outbox entry and *something else* later publishes it — so an intent can exist in the outbox and not on the bus. With a stream outbox, `XADD` happens inside the same atomic script as `DECRBY`, so the intent is on the durable bus the instant the balance moves. It also takes the required infrastructure down to **Redis + Postgres**, which is the difference between a service you can stand up in an afternoon and one that needs a broker first.

**Choose JetStream when** you need quorum replication or cross-region mirroring for money intents, you are running `journal` settlement and want the bus as durable as the journal, or you already operate JetStream and would rather have one broker than one more Redis responsibility.

Switching is a `cmd/` wiring change. No subject name, payload, handler or invariant differs between them.

---

## Naming: one logical subject, two dialects

`<tag>` is `Scope.Tag()` = `<realm>/<kind>:<id>`. The realm is inside the token, so a subject can never be consumed by the wrong product's worker. `SubjectPrefix` (default `billing`) is configurable, because a host with an existing namespace must be able to fit in.

| Logical subject | Redis Streams | NATS JetStream |
|---|---|---|
| `billing.deduct.<tag>` | stream `billing:outbox:{shard}`, group `drain` | subject `billing.deduct.<tag>`, stream `BILLING`, durable pull consumer |
| `billing.grant.<tag>` | stream `billing:outbox:{shard}`, group `drain` | subject `billing.grant.<tag>`, same stream |
| `billing.warn.<tag>` | stream `billing:events`, group `notify` | subject, consumer `notify` |
| `billing.*.tick` | stream `billing:ticks`, group `worker` | subject, consumer `worker` |
| `billing.dlq.<tag>` | stream `billing:dlq`, group `sweeper` | subject, consumer `sweeper` |

Redis Streams has no subject wildcards, so the adapter shards by key (`{shard}`) and carries the logical subject as a field on the entry. Consumers filter on it. The port hides this: a handler registers for `billing.deduct.*` either way.

---

## Subjects

| Logical subject | Publisher | Consumer | Payload (key fields) |
|---|---|---|---|
| `billing.deduct.<tag>` | the hot-path atomic script, or any spend producer | `payment-worker` (group) | `{realm, scope, delta, operation_type, rate_card_version, cost_micros, idem_key, actor, job_scope?, trace_id}` → applies idempotently and writes the `credit_ledger` row. **The worker is the sole ledger writer** |
| `billing.grant.<tag>` | webhook ingress post-verify; audited admin tooling | `payment-worker` (group) | `{realm, scope, delta (signed), operation_type, payment_id, idem_key, trace_id}` → one positive or negative ledger row. Replays converge |
| `billing.allocate.<tag>` | `Transfer` (after its Postgres settle) | `payment-worker` (group) | `{realm, from, to, amount, idem_key}` → refreshes both hot balances. **Advisory**: the ledger pair is already durable, so a lost message costs a reconcile, not money |
| `billing.warn.<tag>` | the `Admit` path crossing `WarnAt` | the host's notification system | `{realm, scope, limit_name, used, cap, window, resets_at, idem_key}` — **coalesced** per scope+limit+window, so approaching a ceiling notifies once, not once per request |
| `billing.blocked.<tag>` | the `Admit` path on a denial | the host's notification system | `{realm, scope, limit_name, deny_code, upgrade_path?, idem_key}` |
| `billing.payment.<tag>` | webhook ingress | the host's notification system | `{realm, scope, event: succeeded\|failed\|refunded\|renewed\|canceled, amount, currency, grace_until?, receipt_url?, idem_key}` |
| `billing.entitlement.<tag>` | subscription change · override write · grace expiry | entitlement cache invalidators | `{realm, scope, reason}` → evict cached grants, so a customer who just paid does not wait out a TTL |
| `billing.reconcile.tick` | external scheduler | `payment-worker` (group) | `{shard, bucket, trace_id}` → one reconcile under `SET NX reconcile:lock:{shard}:{bucket}` |
| `billing.expiry.tick` | external scheduler | `payment-worker` (group) | `{realm, trace_id}` → expires credit lots FIFO. Only under `CreditExpiry=lots_fifo` |
| `billing.dunning.tick` | external scheduler | `payment-worker` (group) | `{trace_id}` → suspends subscriptions past `GraceUntil` |
| `billing.subdrift.tick` | external scheduler | `payment-worker` (group) | `{provider, trace_id}` → `FetchSubscription` for locally-active subs and repairs divergence. **The backstop for a missed webhook** |
| `billing.events.purge.tick` | external scheduler | `payment-worker` (group) | `{trace_id}` → purges `payment_events` past retention, never inside a provider replay window |
| `billing.trim.tick` | external scheduler | `payment-worker` (group) | `{trace_id}` → **Redis Streams only**: trims acked stream history below the retention floor. Without it a stream grows until Redis runs out of memory, which on a `noeviction` instance stops the hot path |
| `billing.dlq.<tag>` | any consumer after exhausting retries | DLQ sweeper (scheduled) | parked intents with `dlq_attempts`. Re-driven under backoff below the cap, then parked in `dead_letters` with an alert. **Never dropped** — a dropped money intent is silent under-billing |

---

## The `Bus` port

```go
package ports

// Bus is the durable, at-least-once async seam. Both adapters satisfy it identically;
// nothing above this interface knows which is running.
type Bus interface {
	// Publish appends to the logical subject's stream. Durable on return.
	// NOTE: the hot path does NOT call this — its atomic script publishes inline
	// (Redis) or the drainer forwards (JetStream). Publish is for everything else.
	Publish(ctx context.Context, subject string, payload []byte) error

	// Subscribe joins a named consumer group and delivers at-least-once. The handler
	// returning nil acks; returning an error redelivers after the adapter's backoff,
	// up to MaxAttempts, then the entry goes to the DLQ subject.
	//
	// Group semantics are the SAME in both adapters: one delivery per group per entry,
	// N replicas share the work, and a replica that dies has its in-flight entries
	// reclaimed (XAUTOCLAIM / ack-wait) rather than lost.
	Subscribe(ctx context.Context, subject, group string, h Handler) (Subscription, error)

	// Pending reports un-acked, in-flight work for a group: the number and the age of
	// the oldest. This is what `metering_outbox_depth` and `_age_seconds` export, and
	// age is what catches a poisoned head-of-line that depth alone hides.
	Pending(ctx context.Context, subject, group string) (count int64, oldest time.Duration, err error)

	// Trim drops acked history below a retention floor. A no-op on JetStream, where
	// stream limits handle it; REQUIRED on Redis Streams, where nothing does.
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

## Rules

- **At-least-once, idempotent handlers.** Every handler's effect is an idempotent atomic claim (`credit_idem (realm, idem_key)`, `SET NX`, a conditional `UPDATE`). Correctness never rests on "only one replica runs" or "the broker delivers once".
- **Security context is authoritative from the publisher.** A consumer never reads scope, realm or amount from untrusted content; it uses what the publisher stamped, and the publisher resolved it server-side.
- **Scheduled work is externally triggered and single-owner.** Request-serving tiers run no in-process ticker. A scheduler publishes the `*.tick`; the group delivers it to exactly one worker; the handler is idempotent, so a duplicate tick produces no duplicate effect.
- **`billing.warn` is coalesced; `billing.deduct` is not.** A warning is advisory and repeats every request past a threshold, so it collapses per scope+limit+window. A deduction is money and never collapses.
- **Retention is explicit.** On Redis Streams, `billing.trim.tick` enforces the floor: keep entries until acked **and** older than `BusRetention` (default 7d), whichever is later. Never trim un-acked entries — that is deleting money intents. On JetStream the equivalent is a stream limit with `WorkQueue` or `Limits` retention plus the same floor.
- **Backpressure is visible, never silent.** When a stream nears `MaxLen`, the adapter alerts rather than trimming un-acked entries. A full outbox must fail the hot path loudly (or fail open, per `AdmitFailPolicy`) — it must never quietly discard.
- **Subject prefix is configurable**; `billing` is a convention.

## Contract-test obligations

`BusContract` runs against **both** adapters — which is the only way "swappable" means anything.

- A duplicated `billing.deduct` with the same `idem_key` inserts **exactly one** ledger row.
- A duplicated `billing.grant` inserts one row and applies one hot-balance change.
- A negative `billing.grant` exceeding the balance obeys `NegativeBalancePolicy`, and under `clamp_to_zero` writes the compensating `writeoff` row.
- A consumer killed mid-handler has its in-flight entry **redelivered**, not lost — `XAUTOCLAIM` on Redis, ack-wait on JetStream.
- Two schedulers publishing the same `billing.reconcile.tick` produce exactly one reconcile for the bucket.
- A `billing.subdrift.tick` against a subscription cancelled at the provider but active locally repairs the local row.
- A parked DLQ intent under the attempt cap is re-driven and applied at most once; at the cap it lands in `dead_letters`, emits an alert, and is never re-driven again.
- Publishing to realm A's subject never affects a balance in realm B.
- **Trim never removes an un-acked entry**, even when the stream is over its length target.
- `Pending` reports a non-zero oldest-age within one ack-wait of a stalled consumer — the signal the runbook depends on.
