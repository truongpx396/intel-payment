# Operations Runbook

One entry per alert, each naming the metric that fires it. Written for whoever is on call at 3am,
so each entry starts with what it means rather than what to type. The mechanism behind most of them
is [hot-path-consistency.md](../specs/001-metering-billing-core/contracts/hot-path-consistency.md);
this runbook is written from that reasoning and has **not yet been exercised in an incident** —
T131/T133 rehearse it before release.

Keys and streams below use a shard's hash tag, e.g. `{s42}`. To find a scope's shard:
`fnv1a64("<realm>/<kind>:<id>") mod PAYMENT_SHARDS`, or `GET /v1/admin/drift?scope_kind=&scope_id=`.

---

## Deploy

```bash
make migrate   # always before the new image serves traffic
make up
curl -sf localhost:8080/readyz   # {"status":"ready","migrations":"head","shards":256}
```

`/readyz` fails while the schema is behind head or the configured shard count disagrees with
`hot_config`, so wire it as the rollout gate. It does not ask Redis: while the hot tier is down `Record`
journals to Postgres and answers `202`, which a service pulled out of rotation could not. **Order:** migrate →
`payment-worker` → `paymentd`. The worker is the sole durable writer and opens the shards on first start.

**Stopping.** `paymentd` on `SIGTERM` closes its listeners at once and answers the calls it already took (a
`Record` settles or is journalled) up to `PAYMENT_SHUTDOWN_TIMEOUT`; past that it cuts the rest, exits 1 and says so,
and the caller retries with the same `Idempotency-Key`, which is booked once. `payment-worker` finishes the batch in
hand, then releases each shard by name, so a replica takes them over within `PAYMENT_OWNERSHIP_RETRY`. It then closes the entitlement cache's bus subscription, waiting up to one more second: a subscription stuck behind a Redis that is not
answering is logged (`did not stop in time`) and left to the exit, so a stalled Redis cannot hold a stop longer than that. Set the
orchestrator's kill grace period above `PAYMENT_SHUTDOWN_TIMEOUT` plus a second. A worker that is killed instead of stopped loses
its locks with its connections; whatever it had been handed and not acknowledged is claimed by the next worker after
`PAYMENT_BUS_ACK_WAIT`, and booked once.

**Connections.** A worker holds one PostgreSQL connection per shard it owns. At the default 256 shards one worker
needs ~272; the compose Postgres is started with `max_connections=400`. Run replicas to divide the shards, or lower
`PAYMENT_SHARDS` before the first start (it cannot be changed afterwards without a reshard).

---

## Dashboard

| Panel | Metric | Healthy |
|---|---|---|
| Admission latency | `metering_admit_latency_seconds` p99 | ≤ 5 ms co-located, ≤ 1 ms embedded |
| Denials | `metering_admit_denied_total` by limit | steady; spikes are abuse or a misconfigured limit |
| **Drift findings** | `metering_drift_findings_total` | **zero**. Steady traffic produces none, by construction |
| Deferred at reconcile | `metering_reconcile_deferred_total` | low; it is lag, not drift |
| **Outbox depth / age** | `metering_outbox_depth`, `_age_seconds` | depth low, age seconds |
| Sequence gaps | `metering_writer_seq_gap_seconds` | zero |
| **Frozen shards** | `metering_shard_frozen_seconds` | zero; seconds during a recovery |
| Regressions | `metering_hot_regression_total` | zero; each is a Redis restart or failover |
| Suspense / late replays / key conflicts | `metering_suspense_open`, `metering_late_replay_total`, `metering_idem_conflict_total` | ~zero |
| Transfers in transit | `metering_transfer_in_transit_age_seconds` | milliseconds |
| Deferred usage | `metering_record_deferred_total`, `metering_journal_pending` | zero outside an outage |
| Fail-open | `metering_admit_failopen_total` | **zero** on a `fail_closed` deployment |
| Uncapped admits | `metering_admit_uncapped_total` | zero once every caller passes `MaxCost` |
| Pricing errors | `metering_price_error_total` | zero; non-zero means a card is missing a SKU |
| **Webhook verification failures** | `billing_webhook_verify_failed_total` | zero. A security signal |
| **Webhook inbox age** | `billing_webhook_inbox_age_seconds` | seconds |
| Outbound deliveries | `events_delivery_failed_total` | ~zero |
| **Dead letters** | `metering_dead_letters` (and `metering_dead_lettered_total` per parking) | **zero** |
| DEFAULT partitions | `metering_default_partition_rows` | zero |

**Page on:** drift findings, regressions, a shard frozen longer than a minute, outbox age, sequence
gaps, transfers in transit past the alarm, fail-open, idempotency conflicts, webhook verification
failures, webhook inbox age or dead events, DLQ dead count, rows in a DEFAULT partition. Everything
else is a dashboard.

[`deploy/alerts.yml`](../deploy/alerts.yml) is that list as Prometheus alerting rules, each linking to its entry below, and a test holds the
two to one set: an alert whose link resolves to no entry, an entry no alert links to, and a metric either names that no component exports all
fail `make test`. The thresholds are defaults (they follow `PAYMENT_GAP_TIMEOUT`, `PAYMENT_TRANSFER_TRANSIT_ALARM` and `PAYMENT_BUS_MAX_LEN`);
the expressions are not parsed by that test, so run `promtool check rules deploy/alerts.yml` before loading them.

---

## Alert: drift finding

**Means:** at the **same** sequence number, the hot account and `booked + open suspense` disagree.
In-flight intents cannot cause this — reconcile skips scopes whose intents are still queued — so a
finding is a defect, not noise. Reconcile has already healed the hot side to the books; nothing was
booked.

```sql
SELECT f.realm, f.scope_kind, f.scope_id, f.pool, f.hot_seq, f.hot_balance, f.expected_balance, f.drift, f.healed
FROM reconcile_findings f WHERE f.outcome = 'drift' ORDER BY f.created_at DESC LIMIT 20;
```

**Read the direction:**

- **Hot above expected** (`drift > 0`): the hot tier holds credits the books do not — a grant applied
  hot but not booked, or a lost correction. Customers could spend what they did not have; the heal
  stopped that.
- **Hot below expected** (`drift < 0`): the hot tier charged something the books did not accept and
  no suspense entry explains it. Customers were briefly refused or over-charged on the fast copy; the
  heal restored it.

Either way, find the intents around `hot_seq` in the ledger and suspense for that scope. If the
**books** are wrong — rarer, and a real bug — correct them with an audited
`POST /v1/admin/adjustments`, never by editing a row.

---

## Alert: regression, or a shard frozen

**Means:** the hot tier lost history — a Redis restart from an AOF missing its last second, or a
failover to a replica that had not received everything. The writer noticed (a changed replication
id, or an intent reusing a sequence number the books already hold) and **froze the shard**:
`Admit` applies the fail policy, `Record` defers to the journal, nothing is lost.

Recovery is automatic — freeze → drain → bump generation → unfreeze — and takes as long as the drain.

1. `metering_shard_frozen_seconds` should fall back to zero within seconds. If it does not, the
   drain is stuck: see *outbox depth or age* below.
2. After unfreeze, scopes rebuild lazily from the books (`metering_rehydrate_total{cause="stale"}`
   spikes, then settles) and `metering_journal_pending` drains to zero.
3. Quantify what `outbox` mode could have lost: intents acknowledged in the fsync window or the
   replication lag before the event. They cannot be recovered from Redis; the host's own request logs
   are the only other record. If this recurs, move to `PAYMENT_HOT_ACK_WAIT=aof_local` (or
   `aof_replica`), or to `journal` settlement.

Force the procedure by hand, with a reason, if detection missed a failover you know happened:

```bash
curl -X POST localhost:8080/v1/admin/shards/42/recover -d '{"reason":"manual failover 2026-09-28"}'
```

---

## Alert: outbox depth or age rising

**Means:** the writer is behind. Every undrained intent is inside the RPO window.

```bash
redis-cli -c XINFO STREAM 'billing:outbox:{s42}'
redis-cli -c XINFO GROUPS 'billing:outbox:{s42}'            # pending per consumer
redis-cli -c XPENDING 'billing:outbox:{s42}' drain - + 10   # the oldest stuck entries
kubectl logs -l app=payment-worker --tail=200
```

1. Is a worker holding the shard? `SELECT * FROM pg_locks WHERE locktype = 'advisory'` — one owner per shard.
2. Postgres reachable, not at its connection limit, not lock-waiting?
3. **Age high with depth low** is a poisoned head-of-line: after `MaxAttempts` it is parked into
   `credit_outbox_dead` with a `poison` suspense entry and the scope moves on. If it is not parking,
   the writer is failing before it can.
4. **An abandoned consumer** holds entries until `XAUTOCLAIM` reclaims them past `PAYMENT_BUS_ACK_WAIT`.

**Do not scale by changing `PAYMENT_SHARDS`.** Add worker replicas; they divide the existing shards.

## Alert: sequence gap

**Means:** the writer holds intent `n+2` for a scope but never received `n+1`, for longer than
`PAYMENT_GAP_TIMEOUT`. Usually a redelivery is pending behind a stuck consumer (see above). If the
entry is truly gone from the stream — which Redis's prefix-ordered replication should make
impossible — force a shard recovery: the books are authoritative and the rebuilt account will match them.

## Alert: suspense open, late replays, idempotency conflicts

- **`metering_late_replay_total` rising** — producers are retrying after `PAYMENT_HOT_IDEM_TTL`. The
  books are safe (the writer reversed each), but raise the TTL or fix the producer's retry policy.
- **`metering_idem_conflict_total`** — a producer reused a key for a different request. That is a
  producer bug that would otherwise drop charges silently. Find the caller in the audit metadata.
- **Suspense older than a minute** — a correction intent is not landing: its shard is frozen or its
  writer is stuck.

## Alert: transfer in transit

**Means:** phase 1 debited the source, and the destination has not been credited within
`PAYMENT_TRANSFER_TRANSIT_ALARM`. The realm total is still conserved (the amount sits in
`credit_transfers`). `billing.transfer.tick` re-drives it; if the destination shard is frozen, it
completes when the shard unfreezes.

```sql
SELECT * FROM credit_transfers WHERE status = 'in_transit' ORDER BY created_at LIMIT 20;
```

## Alert: fail-open events or deferred usage

**Means:** work was served while the hot tier was unavailable (`fail_open`), and its usage went to the
journal. Nothing is unmetered: `metering_journal_pending` must drain to zero after recovery. If it
does not, `billing.journal.tick` is not running or its shard is still frozen. Balances may go
negative from deferred usage landing late — that is consumption overshoot, bounded by the outage.

---

## Alert: webhook verification failures

**Means:** a misconfigured secret (usually after a rotation) or someone probing.

```sql
SELECT provider_account, event_type, status, error, received_at
FROM payment_events WHERE status IN ('failed','unverifiable','dead') ORDER BY received_at DESC LIMIT 50;
```

- **All failing since a deploy** → the account's secret reference points at the wrong value. Fix it;
  the provider redelivers.
- **A trickle from unknown sources** → probing. Confirm the rejections are `400`, not `2xx`.
- **`unverifiable`** → the provider's verification API is down (PayPal). Deliveries are parked with
  their raw body and retried by `payment-worker` every `PAYMENT_WEBHOOK_TICK_INTERVAL`, backing off from 30 s to an hour; nothing to do but watch the count fall.

## Alert: webhook inbox age, or dead events

**Means:** a verified payment has not been fulfilled. The inbox guarantees it is not lost; this alert
is about *delay*. Check `payment-worker` is running and that its log has no
`payments: a stage failed and will run again` (the line names the stage: `webhook inbox`, `parked deliveries` or
`inbox retention`), then the `error` on the oldest rows. A `dead` event exhausted its retries and is never leased again:
fix the cause (commonly a plan with no `plan_provider_prices` mapping for the price bought: `PUT
/v1/admin/plans/{code}/provider-prices/{account}/{currency}`, which never replaces a mapping — a wrong one is a new plan or a database
fix, because a payment already made keeps the price it was made at). An auto top-up whose card was charged and whose plan the
catalogue no longer sells is also a `dead` event on purpose: the customer has paid, so it pages a person rather than being ignored —
restore the plan, or refund the charge at the provider. The admin API configures; it has no route to re-drive a dead event yet.

A `payment-worker` that cannot join the `<prefix>.balance.low.>` subscription exits `1` and says which; it does not run on
without auto top-up.

## Alert: outbound deliveries failing

**Means:** a host endpoint is refusing or timing out. A delivery is retried for 72 h (1 m, 5 m, 30 m, 2 h, then every 6 h) and then goes
`dead`: `events_delivery_failed_total{realm,endpoint}` moves once for each, and `events_dead_deliveries` is the number nobody has
re-driven. The host can always backfill from `GET /v1/events` — the event is in the log whatever became of its webhook.

```sql
SELECT ep.id AS endpoint, ep.url, count(*) AS dead, max(d.last_status) AS last_status, max(d.last_error) AS last_error
  FROM event_deliveries d JOIN event_endpoints ep ON ep.id = d.endpoint_id
 WHERE d.status = 'dead' GROUP BY ep.id, ep.url ORDER BY dead DESC;
```

1. **`last_status`** says what the host answered; **0 with an error** is no answer at all. A destination the SSRF guard refused reads
   `destination address is not allowed`: the URL points at a private, loopback or link-local address, or at a name that resolves to one
   (set `PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS` only in development). `cannot resolve the signing secret` is a `secret_ref` whose
   variable or file is missing on the worker — nothing is sent unsigned, and it is retried until the secret is back.
2. **A `3xx` is a failure**: a redirect is not followed. Register the final URL.
3. Fix the endpoint, and the retries that are still inside their window succeed on their own. A delivery already dead is re-driven by
   `Dispatcher.Redeliver`; **the admin route for it (`POST /v1/admin/event-deliveries/{id}/redeliver`) is designed and not served yet**,
   and the host can take what it missed from the feed with its last event id.
4. An endpoint is a row: to stop the pages from a host that is gone, set `event_endpoints.active = false`. Its owed deliveries end at
   the window, and are counted once.

## Scheduling the ticks

`payment-worker` drives its own periodic work, so a deployment runs without a scheduler. A scheduler (a cron, a CronJob) is for another
cadence or a manual run, and publishes through the worker's own binary — it needs the worker's `PAYMENT_*` variables and only the bus:

```bash
payment-worker tick audit '{"shard":0}'            # one per shard; a per-shard tick that names no shard is refused
payment-worker tick reconcile '{"shard":3,"kind":"full"}'
payment-worker tick partitions                     # journal, transfer, expiry, webhook, outbound, dunning, idem.purge, events.purge, trim
```

Publishing a tick twice is harmless: the second is `skipped` at the claim. `metering_tick_total{tick,outcome}` says what became of each:
`ran` (work was done), `idle` (nothing was due, nothing was written), `skipped` (another run held the claim, or the shard belongs to another
worker), `failed` (the bus redelivers it, then parks it in `dead_letters` and pages — see [Alert: DLQ dead count](#alert-dlq-dead-count)).
A tick that stops arriving shows as a counter that stops moving. `billing.subdrift.tick` is **not served**: nothing consumes it.

## Alert: a scheduled tick failing

**Means:** `metering_tick_total{outcome="failed"}` moved: a tick's work returned an error (the database or the hot tier was unreachable, a
per-shard audit found a mismatch, a payload the tick could not read). The bus redelivers it with backoff up to `PAYMENT_BUS_MAX_ATTEMPTS` and
then parks it in `dead_letters`, which is a page of its own (see [Alert: DLQ dead count](#alert-dlq-dead-count)); a tick that failed once and
then ran is not a fault. The `tick` label says which.

1. **`audit`**: `ErrAuditMismatch` means a booked balance is not provable from the ledger — treat it as a drift finding
   ([Alert: drift finding](#alert-drift-finding)), not as a scheduling problem.
2. **`reconcile`**, **`audit`** with `skipped` rising instead: the shard belongs to another worker, whose own loops work it; nothing to do.
3. **`partitions`**, **`idem.purge`**: the database refused DDL (a lock wait over two seconds is retried by the next hourly run). A
   `partitions` tick that keeps failing runs the daily partitions out in four days, and rows then land in a DEFAULT partition
   ([Alert: rows in a DEFAULT partition](#alert-rows-in-a-default-partition)).
4. **`trim`**: if it keeps failing the streams are never trimmed ([Alert: stream length approaching the cap](#alert-stream-length-approaching-the-cap)).
5. A parked tick is harmless to leave once the cause is fixed: the next one does the work.

## Alert: stream length approaching the cap

**Means:** the trim tick is not running, or a consumer group is so far behind that nothing is
trimmable. At the cap, the hot function refuses with backpressure and `Record` defers to the journal —
so the service keeps accepting usage, but the hot path has effectively become `journal` mode.

1. Is `billing.trim.tick` scheduled and succeeding?
2. Is a consumer stalled? Un-acked entries are **never** trimmed. Fix the drainer first.
3. **Do not `XTRIM` by hand.** Un-acked entries are money intents; the only correct order is drain, then trim.
4. If memory is genuinely critical, raise `maxmemory` — never change the eviction policy.

A stream that will not trim although every worker is healthy has a group nobody reads: see
[Retiring a paymentd replica](#retiring-a-paymentd-replica).

## Alert: entitlement checks failing

`entitlement_decisions_total{outcome="error"}` is rising: the entitlements could not be resolved, so hosts are being
told to deny (`503 unavailable`, never "entitled" and never "not entitled") and any limit sized by a plan answers
`503 quota_unavailable` (or is uncapped under `AdmitFail=fail_open` — `metering_admit_failopen_total` says so).

1. **It is the database `paymentd` reads entitlements from.** Check Postgres reachability and the pool first; the cause is in
   `paymentd`'s log (`entitlements could not be resolved`), and the response deliberately names none of it.
2. A warm cache entry is served through the outage until `PAYMENT_ENTITLEMENT_CACHE_TTL` (default 30 s); a rising error
   rate begins after that. It is not a sign the cache is broken.
3. Nothing to repair in the entitlements themselves: when the database is back, the next check succeeds.

## Alert: entitlement values its key cannot hold

`entitlement_invalid_total` is non-zero: a stored grant was **skipped** (never granted) because its key cannot hold it — a
key the realm never declared, a value of the wrong kind, or an enum member the key never ranked. The `source` label says
whether it was a `default`, a `subscription` plan row or an `override`.

1. Find it: `SELECT * FROM plan_entitlements e JOIN plans p ON p.id = e.plan_id WHERE e.feature_key NOT IN (SELECT key FROM entitlement_keys WHERE realm = p.realm);`
   (and the same over `realm_defaults` and `entitlement_overrides`).
2. Declare the key, or fix the row. The level below answered in the meantime — a customer may be on the free tier's value —
   so this is a customer-visible fault, not just untidiness.

## Alert: a replica's entitlement cache is not hearing announcements

`entitlement_cache_invalidations_total` is flat at zero over a day that had subscription changes, or a customer reports that
a plan change took minutes to apply. A lost announcement costs the cache TTL and no more, so nothing is *wrong* — but the
replica is serving up to `PAYMENT_ENTITLEMENT_CACHE_TTL` behind.

1. Is the replica's group alive? `redis-cli XINFO GROUPS <prefix>:events` lists `entitlement-cache-<id>|<prefix>.entitlement.>`
   and `…|<prefix>.config.>` for each replica, with `lag` and `pending`.
2. Are *configuration* changes reaching the bus? That is `payment-worker`'s config relay. It never stops the worker — it is a
   freshness hint and the worker books money — so one that cannot start is logged as an `ERROR` (`config relay cannot start:
   a configuration change reaches every replica's cache only when its TTL passes`) and retried every 1–30 s; a publication it
   could not make is logged as `config relay`. Grep the worker's log for both. Without the relay only subscription changes and
   override writes evict a cache; a change made by SQL waits for the TTL.
3. Two replicas with the same `PAYMENT_INSTANCE_ID` share a group and **each hears half the announcements**. Give each its own.

## Retiring a paymentd replica

Each replica subscribes to the entitlement announcements under a group of its own, `entitlement-cache-<PAYMENT_INSTANCE_ID>`
(the hostname when unset), so that every replica hears every announcement. The name must be stable across restarts.

A bus stream is only trimmed up to what **every** group has acknowledged. A replica retired for good leaves its two groups
behind, never acknowledging again, and the stream stops trimming — the symptom is
[stream length approaching the cap](#alert-stream-length-approaching-the-cap) with healthy workers. Nothing deletes a group
automatically. After scaling a replica away for good:

```bash
redis-cli XINFO GROUPS billing:events | grep entitlement-cache-          # find its groups
redis-cli XGROUP DESTROY billing:events 'entitlement-cache-<id>|billing.entitlement.>'
redis-cli XGROUP DESTROY billing:events 'entitlement-cache-<id>|billing.config.>'
```

A replica that restarts under the same id reuses its groups and loses nothing. One that restarts under a **new** name (a
container id as the hostname) abandons a pair each time: set `PAYMENT_INSTANCE_ID` to something stable, such as the pod name
of a StatefulSet. A brand-new group reads the stream from its beginning, which only evicts an empty cache and costs nothing.
Under JetStream the same applies to the durable consumers named for the group.

## Alert: rows in a DEFAULT partition

**Means:** `billing.partitions.tick` fell behind and rows landed in `credit_ledger_default` (or
`usage_idem_default`). Nothing was rejected — but PostgreSQL cannot create a partition whose range
overlaps rows already in DEFAULT, so fix it before the tick tries:

```sql
SELECT min(created_at), max(created_at) FROM credit_ledger_default;   -- how far ahead to create
BEGIN;
ALTER TABLE credit_ledger DETACH PARTITION credit_ledger_default;
SELECT ensure_monthly_partitions('credit_ledger', 6);            -- N months: enough to cover max(created_at)
INSERT INTO credit_ledger SELECT * FROM credit_ledger_default;   -- routes into the new partitions
TRUNCATE credit_ledger_default;
ALTER TABLE credit_ledger ATTACH PARTITION credit_ledger_default DEFAULT;
COMMIT;
```

(Verified against PostgreSQL 16. For `usage_idem`, the same with `ensure_daily_partitions`.)

---

## Recovery: hot store lost entirely

**Means:** every `meta:{s<n>}` key is gone — a fresh or emptied Redis. Every script returns
`COLD_SHARD`; `Admit` applies the fail policy; `Record` journals.

1. Confirm the replacement Redis is `noeviction` with AOF, and that hosts cannot reach it.
2. The worker opens each shard at `hot_shards.gen + 1` — the stream died with the store, so the drain
   is trivially complete — and scopes rebuild lazily from the books.
3. `metering_journal_pending` drains as deferred usage is replayed.
4. Quantify the loss window as in *regression* above.

## Resharding

A planned operation, for when the shard count itself is too small:

1. Announce a short window; `Admit` will apply the fail policy while shards are frozen, and usage will journal.
2. `POST /v1/admin/shards/*/recover` with `{"reason":"reshard", "new_shards": 1024}` — freezes every
   shard, drains every stream, records the new count in `hot_config`, bumps every generation, and
   unfreezes. Restart every `paymentd` and `payment-worker` with `PAYMENT_SHARDS=1024` as part of it
   (they refuse to start otherwise).
3. Expect: lazy rebuilds from the books; window counters restarted (a daily ceiling admits up to one
   extra window's worth today); hot idempotency guards gone (late replays are caught by the writer).

## Disaster recovery (region loss)

**Posture:** one home region per deployment ([D45](../specs/001-metering-billing-core/design-decisions.md)).
Postgres streams to a standby in a second region; Redis is **not** replicated across regions — it is
rebuilt from the books.

| | `outbox` settlement | `journal` settlement |
|---|---|---|
| **RPO** | undrained intents at the moment of loss + Postgres replication lag | Postgres replication lag |
| **RTO target** | standby promotion + service start + lazy rebuild — rehearse it (T133) | same |

1. Promote the Postgres standby. Point `PAYMENT_LEDGER_DSN` at it.
2. Start Redis (empty, `noeviction`, AOF), `payment-worker`, then `paymentd` in the DR region. Every
   shard opens at a new generation; scopes rebuild from the books.
3. Re-point provider webhook URLs for each provider account at the DR region's `/webhooks/...`.
   Deliveries that failed meanwhile are retried by the providers; the inbox dedups any that landed
   twice.
4. Re-point hosts. Outbound events resume; hosts backfill from the feed if needed.
5. Quantify the loss window from the promotion point and reconcile against the providers' payout
   reports (`billing_grant_applied_total`).

---

## Recovery: a missed webhook

The drift sweep repairs subscriptions automatically; force it with
`POST /v1/admin/subdrift {"account":"stripe-live"}`. For a payment, redeliver from the provider's
dashboard — safe by construction: the inbox dedups it, and the permanent grant guard makes a second
grant impossible.

## Alert: DLQ dead count

**Means:** a message exhausted its delivery budget (`PAYMENT_BUS_MAX_ATTEMPTS`) and `payment-worker` parked it in
`dead_letters`. It is kept, byte for byte, and nothing delivers it again until you say so.
`metering_dead_letters` is the number nobody has replayed (it clears when the last one is replayed);
`metering_dead_lettered_total` moves once per parking.

```sql
SELECT id, realm, subject, attempts, last_error, parked_at
  FROM dead_letters WHERE replayed_at IS NULL ORDER BY parked_at DESC;
SELECT encode(payload, 'escape') FROM dead_letters WHERE id = '<id>';  -- the message as it arrived
```

1. **Read `last_error` and the subject.** A subject of `billing.dlq.…` is a message that was not even a dead-letter envelope; its
   payload is the raw bytes, and it cannot be replayed (there is no subject to send it back to).
2. **Fix the cause first.** A replayed message that fails again is a new letter and a new page.
3. **Replay it.** The replay is `app.DeadLetters.Replay`: it claims the letter for a minute, publishes the payload to its
   original subject and marks it replayed (the row stays). Two replays at once publish once; a publish that fails can be retried
   at once; one killed midway is completed by the next after the minute. **The admin route for it
   (`POST /v1/admin/dead-letters/{id}/replay`) is designed and not served yet** ([rest-api.md](../specs/001-metering-billing-core/contracts/rest-api.md)):
   until it is, a replay is a call from a Go program that holds the bus and the store.
4. A parked **tick** (`….tick`) is harmless to leave: the next scheduled tick does the work.

---

## Repricing

Never edit a rate card — the database refuses it. Publish a version:

```bash
curl -X POST localhost:8080/v1/admin/rate-cards -d '{
  "version": "2026-10-01", "retire_previous": true,
  "entries": [ {"rate_key":"gpt-4o","unit":"llm_input_token","credits_per_block":2500000,"block_size":1000000} ]
}'
```

The API prices a test event under the new card before activating it, writes the audit row with your
name and reason, and every process picks the card up immediately. The SQL equivalent, for an
emergency, is `UPDATE rate_cards SET retired_at = now() WHERE realm = … AND retired_at IS NULL`
plus the inserts, in one transaction — still audited, attributed to your database role.

## Changing a ceiling

```bash
curl -X PUT localhost:8080/v1/admin/limits/project_hourly -d '{
  "subject_kind":"project","unit":"credit","max":500000,"window":"hourly","warn_at":0.8,"deny_code":"limit_reached"}'
```

No deploy and no UI change — the panel renders `LimitView[]`. If the ceiling should differ by plan,
set `max_entitlement` and give each plan (and the free tier) that quota; a per-customer exception is
an entitlement override with a reason.

## Answering "where did this charge come from?"

```sql
SELECT delta, pool, operation_type, rate_key, quantities, rate_card_version, gen, seq_from, seq_to, event_count, ref, occurred_at
FROM credit_ledger WHERE realm = :realm AND idem_key = :key;
```

Re-price it by running the card's pricer against the stored `quantities` under the stored
`rate_card_version`. Under `rollup` granularity, the row covers `seq_from..seq_to`; re-price the
archived events in `usage_events` for that range and compare their sum. A difference means a pricer
bug — an edited card is impossible.

## Backups and retention

- **Postgres** — the books. Test restores by restoring. Old ledger partitions are detached and
  archived only once `ledger_checkpoints` covers them, so a balance stays provable after archival.
- **Redis** — balances are rebuilt from the books, not restored. But it also holds undrained intents,
  which are not derived: AOF is load-bearing. Keep `appendfsync everysec` at minimum, use
  `HotAckWait`, or run `journal` settlement so an intent never depends on Redis alone.
