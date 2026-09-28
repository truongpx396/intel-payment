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
`hot_config`, so wire it as the rollout gate. **Order:** migrate → `payment-worker` → `paymentd`.
The worker is the sole durable writer and opens the shards on first start.

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
| DEFAULT partitions | `metering_default_partition_rows` | zero |

**Page on:** drift findings, regressions, a shard frozen longer than a minute, outbox age, sequence
gaps, transfers in transit past the alarm, fail-open, idempotency conflicts, webhook verification
failures, webhook inbox age or dead events, DLQ dead count, rows in a DEFAULT partition. Everything
else is a dashboard.

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
  their raw body and retried by `billing.webhook.tick`; nothing to do but watch the count fall.

## Alert: webhook inbox age, or dead events

**Means:** a verified payment has not been fulfilled. The inbox guarantees it is not lost; this alert
is about *delay*. Check `billing.webhook.tick` is running, then the `error` on the oldest rows. A
`dead` event exhausted its retries: fix the cause (commonly a plan with no `plan_provider_prices`
mapping for the price bought) and re-drive it through the admin API.

## Alert: outbound deliveries failing

**Means:** a host endpoint is refusing or timing out. Deliveries retry for 72 h, then go `dead`; the
host can always backfill from `GET /v1/events`. Re-drive a dead delivery with
`POST /v1/admin/event-deliveries/{id}/redeliver`.

## Alert: stream length approaching the cap

**Means:** the trim tick is not running, or a consumer group is so far behind that nothing is
trimmable. At the cap, the hot function refuses with backpressure and `Record` defers to the journal —
so the service keeps accepting usage, but the hot path has effectively become `journal` mode.

1. Is `billing.trim.tick` scheduled and succeeding?
2. Is a consumer stalled? Un-acked entries are **never** trimmed. Fix the drainer first.
3. **Do not `XTRIM` by hand.** Un-acked entries are money intents; the only correct order is drain, then trim.
4. If memory is genuinely critical, raise `maxmemory` — never change the eviction policy.

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

**Means:** a message exhausted its retry budget. It is parked, never dropped.

```sql
SELECT subject, attempts, last_error, parked_at FROM dead_letters WHERE replayed_at IS NULL ORDER BY parked_at DESC;
```

Fix the cause, then `POST /v1/admin/dead-letters/{id}/replay`. Replay is idempotent.

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
