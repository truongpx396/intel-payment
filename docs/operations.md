# Operations Runbook

One entry per alert, each naming the metric that fires it. Written for whoever is on call at 3am,
so each entry starts with what it means rather than what to type.

---

## Deploy

```bash
make migrate   # always before the new image serves traffic
make up
curl -sf localhost:8080/readyz   # {"status":"ready","migrations":"head"}
```

`/readyz` fails while the schema is behind head, so wire it as the rollout gate. A service
serving money against a half-migrated schema is worse than one that is down.

**Order:** migrate → start `payment-worker` → start `paymentd`. The worker is the sole durable
writer; starting the API first means charges queue with nothing draining them.

---

## Dashboard

| Panel | Metric | Healthy |
|---|---|---|
| Admission latency | `metering_admit_latency_seconds` p99 | ≤ 5 ms co-located, ≤ 1 ms embedded |
| Denials | `metering_admit_denied_total` by limit | steady; spikes are abuse or a misconfigured limit |
| **Drift** | `metering_ledger_drift_credits` | within tolerance |
| **Outbox depth / age** | `metering_outbox_depth`, `_age_seconds` | depth low, age seconds not minutes |
| Heal direction | `metering_reconcile_healed_credits{direction}` | ~zero. Steady *up* is silent under-billing |
| Fail-open | `metering_admit_failopen_total` | **zero** on a `fail_closed` deployment |
| Uncapped admits | `metering_admit_uncapped_total` | zero once every caller passes `MaxCost` |
| Pricing errors | `metering_price_error_total` | zero; non-zero means a rate card is missing a SKU |
| **Webhook verification failures** | `billing_webhook_verify_failed_total` | zero. A security signal |
| Grants | `billing_grant_applied_total` | reconciles against the provider's payout report |

Page on: drift beyond tolerance, outbox age, fail-open, webhook verification failures, DLQ dead
count. Everything else is a dashboard.

---

## Alert: drift beyond tolerance

**Means:** the hot balance and the ledger disagree by more than you allowed. The ledger is
authoritative, so customers were charged the ledger's amount but *saw* the hot number.

```sql
SELECT shard, expected, observed, drift, healed, alarmed, ran_at
FROM reconcile_runs WHERE realm = :realm ORDER BY ran_at DESC LIMIT 20;
```

**Read the direction — it is the diagnosis:**

- **Observed > expected** (hot balance too high ⇒ healed *down*): charges reached Redis but not
  the ledger. Check outbox depth and whether the worker was down. Customers were briefly
  under-charged; the heal corrects it.
- **Observed < expected** (hot balance too low ⇒ healed *up*): double-applied hot-path effects, or
  a grant that reached the ledger but not Redis. Look for duplicate `idem_key`s — there should be
  none, and finding one is a real bug, not an operational blip.

**Steady healing in the *up* direction is the serious case**: it is the `outbox` mode's accepted
under-bill RPO showing up as a pattern rather than an incident. If the amount matters, switch that
deployment to `journal`.

**Do not raise the tolerance to silence it.** Tolerance exists to absorb the in-flight window, not
a real discrepancy.

---

## Alert: outbox depth or age rising

**Means:** the sole durable writer is behind. The RPO window is widening — every un-drained entry
is a charge that a Redis loss would forget.

```bash
redis-cli LLEN outbox:0          # per shard
kubectl logs -l app=payment-worker --tail=200
```

1. Is the worker running? One replica per shard, and exactly one owner.
2. Postgres reachable, not at a connection limit, not locked?
3. **Age high with depth low** means a poisoned head-of-line entry. Inspect it, and if it cannot
   be applied, move it to `credit_outbox_dead` rather than leaving it blocking every entry behind it.

Scale drainers by raising `PAYMENT_SHARDS`… **no.** Shards are fixed for a deployment's life —
changing the count orphans queued entries. Add worker replicas bound to existing shards instead.

---

## Alert: fail-open events

**Means:** work was served **unmetered**. On a `fail_closed` deployment this should be impossible;
non-zero means the policy is not what you think it is.

Check the hot store first — this metric only increments when it was unreachable. Reconcile bounds
the resulting overspend, and the amount is `events × average cost`, which is worth computing
before deciding whether to write it off.

---

## Alert: webhook verification failures

**Means:** either a misconfigured secret (after a rotation, usually) or someone probing.

```sql
SELECT provider, event_type, status, error, received_at
FROM payment_events WHERE status IN ('failed','unverifiable')
ORDER BY received_at DESC LIMIT 50;
```

- **All failing since a deploy** → the secret is wrong. Fix it; the provider will redeliver.
- **A trickle from unknown sources** → probing. Confirm the rejections are `400`, not `2xx`.
- **`unverifiable`** → the provider's verification API is down (PayPal). These are parked, not
  lost. Replay them once it recovers; never process them optimistically.

---

## Recovery: hot store lost

**Means:** balances are gone. The ledger is intact — it is the record.

```bash
curl -X POST localhost:8080/v1/admin/rehydrate \
  -d '{"scope":{"realm":"…","kind":"organization","id":"…"}}'
```

1. Confirm the replacement Redis is `noeviction` with AOF **before** serving. Restoring onto an
   LRU instance simply schedules the same incident.
2. Rehydrate affected scopes; serving must not resume on an empty balance.
3. Expect drift healed *upward* for charges whose outbox entries died with the store. That is the
   documented under-bill direction, not a new bug.
4. Quantify it from the ledger gap and decide whether to write it off.

---

## Recovery: a missed webhook

**Means:** local state disagrees with the provider. The drift sweep repairs subscriptions
automatically; payments need the provider's redelivery.

```bash
# Subscriptions: force the sweep
curl -X POST localhost:8080/v1/admin/subdrift -d '{"provider":"stripe"}'
```

For a payment, redeliver from the provider's dashboard. It is safe by construction: the event
claim and the idempotency guard make a redelivery a no-op if it already landed.

---

## Alert: DLQ dead count

**Means:** an intent exhausted its retry budget. It is parked, never dropped — a dropped money
intent is silent under-billing.

```sql
SELECT subject, idem_key, attempts, last_error, parked_at
FROM dead_letters WHERE replayed_at IS NULL ORDER BY parked_at DESC;
```

Fix the cause, then replay. Replay is idempotent, so a duplicate replay is harmless.

---

## Repricing

Never edit a rate-card row — it silently re-prices history and breaks every dispute audit against
it.

```sql
BEGIN;
INSERT INTO rate_cards (realm, version, micros_per_credit) VALUES (:realm, '2026-07-01', 1000);
INSERT INTO rate_card_entries (realm, version, rate_key, unit, micros_per_unit) VALUES …;
UPDATE rate_cards SET retired_at = now() WHERE realm = :realm AND retired_at IS NULL AND version <> '2026-07-01';
COMMIT;
```

Exactly one card per realm has `retired_at IS NULL`. Old rows keep pricing old ledger entries
identically, which is the whole reason for versioning.

---

## Changing a ceiling

```sql
UPDATE limits SET max = 200000 WHERE realm = :realm AND name = 'scope_balance';
INSERT INTO limits (realm, name, scope_kind, unit, max, window, warn_at, deny_code)
VALUES (:realm,'project_hourly','project','credit',500,'hourly',0.8,'limit_reached');
```

No deploy, and no UI change — the credits panel renders `LimitView[]`, so a fourth ceiling appears
as a fourth meter on its own.

---

## Answering "where did this charge come from?"

```sql
SELECT delta, operation_type, rate_key, quantities, rate_card_version, ref, occurred_at
FROM credit_ledger WHERE realm = :realm AND idem_key = :key;
```

Re-price it by running the `Pricer` against the stored `quantities` under the stored
`rate_card_version`. If the result differs from `delta`, you have found either a pricer bug or an
edited rate card — and the second one should be impossible.

---

## Backups

- **Postgres** — the ledger is the record. Test restores by actually restoring; an untested backup
  is a hypothesis. Never drop a ledger partition you might need to reconcile.
- **Redis** — a derived cache. AOF shortens the rebuild, but recovery is `Rehydrate`, not a
  restore. Rehearse it and record the measured time against your RTO.
