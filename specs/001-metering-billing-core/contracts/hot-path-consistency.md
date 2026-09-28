# Contract: Hot-Path Consistency (Redis hot tier ↔ Postgres account of record)

**Spec**: [../spec.md](../spec.md) | **Status**: Normative. It defines *how* the invariants in
[metering-ports.md](./metering-ports.md) hold between two stores that never share a transaction.
**Decisions**: [D29, D32–D33, D39–D41](../design-decisions.md#decisions-from-the-second-design-review).

The engine keeps a fast copy of every balance in Redis and the account of record in Postgres. They
are connected by an at-least-once stream and a single durable writer. This contract is the part a
reader of the ports would otherwise have to guess: which keys a script may touch, how the writer
knows what it has applied, what "drift" means while intents are still in flight, and what happens
when Redis loses data or loses a shard.

The rule everything below serves:

> **At every per-scope sequence number, `hot == booked + open suspense`.** Where the stores disagree
> at the *same* sequence number, one of them has a defect, and the system says so. While intents are
> in flight they are *behind*, not *in disagreement*, and nothing treats the one as the other.

---

## 1. Key layout: one scope, one shard, one slot

A Redis Cluster script may only touch keys in one hash slot ([reproduced](#9-verification):
`CROSSSLOT Keys in request don't hash to the same slot`). Every hot-path operation touches the
scope's balance, its idempotency guard, its window counters **and the outbox stream**, so all of them
must share a slot. The hash tag is therefore the **shard**, never the scope:

```text
shard(scope)  = fnv1a64(scope.Tag()) mod Shards          # Shards: default 256, stored in Postgres
{s<n>}        = the hash tag for every key below          # e.g. {s42}

meta:{s42}                                   HASH  gen, frozen, frozen_reason        (shard state)
acct:{s42}:<tag>                             HASH  gen, seq, b:<pool>…, blocked      (the hot account)
idem:{s42}:<tag>:<op>:<idem_key>             STR   "<fingerprint>|<seq>"  PX HotIdemTTL
ctr:{s42}:<tag>:<limit>:<subject>:<bucket>   STR   counter  PEXPIRE at window end (NX)
billing:outbox:{s42}                         STREAM intents, consumer group `drain`
```

- `Scope.Tag()` (`<realm>/<kind>:<id>`) is **identity**; `shard()` is **placement**. The realm stays
  inside the tag, so keys of two realms never collide even though they share shards.
- Window counters — per-subject daily ceilings, `Job` budgets — are keyed **under the charged scope's
  shard**. A subject limit is therefore evaluated *within* the charged scope ("user u1's daily
  ceiling inside org o1"), which is what every reference binding means by it. A ceiling on one
  subject across *several* charged scopes is not a hot-path check; bill that subject as its own
  scope instead.
- `Shards` is the unit of Redis Cluster parallelism: 256 shards spread over up to ~64 primaries
  before two shards must share a node's CPU. It is recorded in `hot_config` and a process whose
  configured count disagrees refuses to start — two processes hashing scopes differently is two
  sets of books.

The same layout is used on a single Redis primary. Nothing is cluster-specific except that a
single primary is where the design's throughput ceiling sits (see [plan.md § Scale
envelope](../plan.md#scale-envelope)).

---

## 2. The hot account and the intent

`acct:{s}:<tag>` holds, for one scope:

| Field | Meaning |
|---|---|
| `gen` | the shard generation this state was built in (§5). A mismatch with `meta:{s}.gen` means *stale*: rebuild before use |
| `seq` | the last sequence number issued for this scope. Every hot mutation increments it by exactly one |
| `b:<pool>` | the hot balance of one credit pool (`b:general`, `b:promo`, …) — see [metering-ports.md § Pools](./metering-ports.md#credit-pools) |
| `blocked` | set by `block_and_flag`; `Admit` refuses while present |

Every mutation — usage debit, grant, transfer leg, expiry, correction — runs as **one Redis
Function** in the shard's slot that, atomically:

1. checks the shard is live (`meta.gen` present, not `frozen`) and the account is current
   (`acct.gen == meta.gen`); otherwise returns `COLD_SHARD` / `FROZEN` / `COLD_SCOPE` and changes nothing;
2. checks the idempotency guard: same key and same fingerprint → `REPLAY` with the original `seq`;
   same key, **different fingerprint → `IDEM_CONFLICT`** (never a silent no-op);
3. checks backpressure (`XLEN` against `BusMaxLen`) → `BACKPRESSURE`, never a silent drop;
4. applies the pool deltas, increments `seq`, writes the guard, increments the window counters;
5. `XADD`s the **intent** to `billing:outbox:{s}`.

The intent is the only thing the durable writer ever reads:

| Field | Content |
|---|---|
| `v` | intent schema version (`1`) |
| `op` | `usage` · `grant` · `transfer_out` · `transfer_in` · `expiry` · `correction` |
| `realm`, `kind`, `id` | the scope |
| `gen`, `seq` | the scope's generation and sequence number *as issued* |
| `idem`, `fp` | idempotency key and request fingerprint |
| `draws` | the pool deltas the hot tier actually applied: `general:-11000;promo:-500` |
| `wo` | a `clamp_to_zero` / `block_and_flag` writeoff, when a negative grant was floored |
| `lw` | a low-balance watch (`<pool>:<threshold>`) this mutation crossed downward — at most once per crossing, so it needs no coalescing |
| `p` | the app's JSON payload: resource, rate key, rate-card version, quantities, subjects, occurred-at, cost, reason, ref, actor, counterparty, lot, suspense id, trace id |

The reference implementation of every function is
[`reference/hot_path.lua`](./reference/hot_path.lua), exercised on a cluster-mode Redis by
[`scripts/verify-hot-path.sh`](../../../scripts/verify-hot-path.sh).

**Precision.** Redis Lua numbers are IEEE doubles, exact to ±2^53. The service refuses any single
operation above `MaxOperationAmount` (default 2^50 units) with `422 amount_out_of_range`, which keeps
every hot balance far inside the exact range. At the recommended accounting unit (1 credit = 1 µ$ of
list price, see [metering-ports.md § Pricing](./metering-ports.md#port-pricer--a-strategy-over-a-versioned-card))
that is about $1.1 billion per operation.

---

## 3. The durable writer: ordered, batched, idempotent

**Ownership.** One writer process owns a shard at a time, via a Postgres session advisory lock on
the shard id. Worker replicas divide shards among themselves; a replica that dies releases its locks
with its connection and another takes over, reclaiming the dead consumer's pending entries with
`XAUTOCLAIM` before reading new ones (older entries first, so order survives the takeover).
Ordering is the reason for single ownership. **Correctness does not rest on it**: every booking
transaction also takes a per-scope `pg_advisory_xact_lock`, and every effect is guarded by an
idempotency row, so two owners overlapping during a takeover still book each intent once.

**The watermark.** `account_watermarks(realm, scope, gen, applied_seq)` records, per scope, the
highest contiguously applied sequence number. For an intent `(gen, seq)`:

| Case | Meaning | Action |
|---|---|---|
| `seq == applied_seq + 1` | the next intent | book it; advance the watermark in the same transaction |
| `seq ≤ applied_seq` and its idempotency row carries this `(gen, seq)` | a redelivery (crash between commit and `XACK`) | acknowledge; no effect |
| `seq ≤ applied_seq` and no such row | **sequence regression** — the hot tier lost a suffix of its history and is reissuing numbers | book it (it is a real, new operation), never move the watermark backwards, and **freeze the shard** (§5) |
| `seq > applied_seq + 1` | a gap: an earlier intent is still pending redelivery | do not acknowledge; retry after `AckWait`. A gap older than `GapTimeout` pages (`metering_writer_seq_gap_seconds`) |

**Batching.** The writer reads up to `DrainBatch` entries, groups them by scope, and books each
scope's run in one transaction: multi-row inserts, one `account_credits` upsert per (scope, pool),
one watermark update — then `XACK`s the batch. A hot scope costs one row update per batch, not per
charge.

**What each op books.** `booked` is `account_credits` per (scope, pool): the ledger's running sum.

| `op` | Idempotency row | Booked (ledger rows) | Other |
|---|---|---|---|
| `usage` | `usage_idem (realm, scope, idem_key)` — window-bounded (§6) | one consumption row per pool draw (`event` granularity) or a share of a rollup row (`rollup`) | FIFO lot consumption under `lots_fifo`; journal row marked settled |
| `grant` | `credit_idem (realm, 'grant', idem_key)` — permanent | the grant row, plus a `writeoff` row when `wo` is set | a lot is opened for a positive grant under `lots_fifo` |
| `transfer_out` | `credit_idem (realm, 'transfer', idem_key)` | `allocation_out` on the source | `credit_transfers` row `in_transit`; then the writer credits the destination (§4) |
| `transfer_in` | `credit_idem (realm, 'transfer_in', idem_key)` | `allocation_in` on the destination | `credit_transfers` row → `settled` |
| `expiry` | `credit_idem (realm, 'expiry', lot_id)` | `expiry` row for the lot's **exact** remainder at this point in the sequence | the pre-applied upper bound minus the exact figure opens a suspense entry |
| `correction` | `credit_idem (realm, 'correction', suspense_id)` | nothing | closes the suspense entry it names |

Any intent carrying `lw` also publishes `billing.balance.low.<tag>` after it is booked — the trigger
for auto top-up and for the host's low-balance notification.

**Suspense.** A hot mutation that must not be booked — a duplicate the hot guard missed, a poison
intent, the unused part of an expiry upper bound — is recorded as an **open `credit_suspense` entry**
(scope, pool, delta, reason) in the same transaction that advances the watermark, and the writer
issues a `correction` intent that reverses its hot effect. When that correction is applied, the
entry closes. Suspense is how the hot tier can move for a reason the books do not accept *without*
the two stores ever disagreeing silently: the invariant at the top of this contract counts it.

| Suspense reason | Cause | Resolution |
|---|---|---|
| `duplicate` | a replay arrived after its hot guard expired (`HotIdemTTL`) but inside the durable window | automatic correction. `metering_late_replay_total` counts it; the caller's `Receipt` for that replay said `Applied:true`, and the correction is what makes the books right |
| `idem_conflict` | as above, but with a different fingerprint | automatic correction **and** a page: a producer is reusing keys |
| `poison` | an intent that cannot be booked after `MaxAttempts` | parked in `credit_outbox_dead`; the watermark advances past it so one bad entry cannot block a scope. Replay books it and closes the entry |
| `expiry_trueup` | expiry pre-applied the lot's remainder at the watermark (an upper bound); in-flight usage consumed part of the lot first | automatic correction for the difference |

---

## 4. Transfer: two phases, conserved at every step

A transfer's two scopes live in different shards, so no single script can move both sides
([D24](../design-decisions.md#d24--transfer-settles-in-postgres-not-on-the-redis-fast-path)). It is
therefore two hot mutations joined by an in-transit record — the suspense-account pattern banks use:

1. **Source (request path).** `ip_transfer_out` runs in the source's slot: it checks the source
   pool's **hot** balance — the most current figure there is — and refuses rather than overdrawing;
   otherwise it debits the source and emits `transfer_out`. `MaxDestBalance` is checked just before,
   against the destination's hot balance; concurrent credits to the destination can exceed the cap
   only by their own amounts (the cap guards against an errant admin action, not a race).
2. **Writer, source shard.** Books `allocation_out` and a `credit_transfers` row `in_transit`, in one
   transaction.
3. **Writer → destination.** After commit, calls `ip_apply_delta` in the destination's slot
   (`op=transfer_in`, idempotent on the transfer key). A failure here is retried; the
   `billing.transfer.tick` sweep re-drives any transfer `in_transit` longer than `TransferRedrive`.
4. **Writer, destination shard.** Books `allocation_in` and marks the transfer `settled`.

**Conservation** (metering invariant 14): `Σ ledger(realm) + Σ in_transit(realm)` is unchanged by
every step, and the reconcile pages on any transfer `in_transit` longer than `TransferTransitAlarm`
(`metering_transfer_in_transit_age_seconds`). Once step 1 succeeds, both sides apply — neither can be
refused later — so the outcome is always *both*, never *one*. The request-serving tier never writes
the ledger (metering invariant 1).

---

## 5. Cold start, loss, regression and freeze

**Shard generations.** `hot_shards(shard, gen, frozen, node_replid)` in Postgres is authoritative;
`meta:{s}` mirrors it. Bumping a shard's `gen` makes every `acct` hash in it stale at once — the
next operation on each scope returns `COLD_SCOPE` and rebuilds it — without scanning or deleting a
key. Counters and idempotency guards are untouched by a bump.

**Lazy rehydrate.** On `COLD_SCOPE` (a new scope, or a stale one) the caller takes a short per-scope
lock, reads `booked + open suspense` per pool and `applied_seq` from Postgres, and runs
`ip_rehydrate`, which installs them **only if the account is still stale** — so concurrent callers
converge on one rebuild. The hot `seq` resumes at `applied_seq`, so no sequence number is reissued.
A brand-new scope takes the same path and costs one indexed read the first time it is touched.

**Rehydrate is only safe once the stream is drained**, because an undrained intent is in neither
`booked` nor the rebuilt balance. Every generation bump is therefore preceded by a drain:

**Freeze → drain → bump → unfreeze** — the one recovery procedure, used for every loss:

| Trigger | Detected by |
|---|---|
| **Total loss** — `meta:{s}` is missing (a fresh or emptied Redis) | every script returns `COLD_SHARD`; the stream is gone too, so the drain is trivially complete |
| **Rollback** — AOF `everysec` lost its last second, or an asynchronous failover promoted a replica missing a suffix | the writer compares the node's `master_replid`/`run_id` with `hot_shards.node_replid` on every batch; and independently, any sequence regression (§3) |
| **Reshard** | an operator changing `Shards` (§8) |

1. **Freeze**: set `frozen` in `hot_shards` and `meta:{s}`. Scripts return `FROZEN`: `Admit` applies
   `AdmitFailPolicy`; `Record` defers to the journal (§7). Nothing is lost while frozen.
2. **Drain** the shard's stream to zero pending, booking everything the hot tier still holds —
   including intents issued with regressed sequence numbers, which are booked on their idempotency
   rows (§3).
3. **Bump** `gen` in Postgres, then in `meta:{s}`; record the node's current `master_replid`.
4. **Unfreeze.** Scopes rehydrate lazily from `booked + suspense`; recently active scopes are
   rehydrated eagerly in the background so the first requests after recovery do not all read
   Postgres at once.

Freeze lasts as long as the drain — seconds — and `metering_shard_frozen_seconds` exports it.

**What a loss costs, stated exactly** (metering invariant 7). Redis replicates and persists a
script's effects as one unit, so a rollback removes a charge's balance change *and* its intent
together: the two stores stay consistent, and **no reconcile can see the charge, because nothing
remembers it**. That is the under-bill RPO of `outbox` settlement, bounded by the AOF fsync interval
or the replication lag — not "healed upward by reconcile". Three ways to shrink it, cheapest first:

| `HotAckWait` | Mechanism | Cost | Loss window |
|---|---|---|---|
| `none` (default) | — | — | ≤ 1 s of acknowledged intents on a crash, plus replication lag on a failover |
| `aof_local` | `WAITAOF 1 0` after the function | one local fsync (group-committed) per acknowledged call | a crash loses nothing acknowledged; a failover still loses the replication lag |
| `aof_replica` | `WAITAOF 0 1` | one replica round trip + fsync | an acknowledged intent survives the loss of the primary |

`Settlement=journal` removes the window entirely by making Postgres, not Redis, the first durable
home of every intent.

The opposite direction — Postgres has booked intents a rolled-back Redis no longer holds — loses
nothing: the regression is detected, the shard is frozen, and the rebuilt balance comes from the
books.

---

## 6. Idempotency: two guards, two lifetimes

| Operation class | Hot guard | Durable guard | Window |
|---|---|---|---|
| usage (`Record`) | `idem:{s}:<tag>:u:<key>`, `HotIdemTTL` (default 24 h) | `usage_idem (realm, scope, idem_key)`, daily partitions | `UsageIdemWindow` (default 72 h), then the partition is dropped |
| grants, transfers, expiry, corrections, adjustments | per scope, `HotIdemTTL` | `credit_idem (realm, op, idem_key)`, **permanent** | unbounded — these mint or move money and are low-volume |

- **Every guard stores a fingerprint** — SHA-256 over the canonical operation (op, scope, pool,
  amount or resource + rate key + quantities + subjects). A reused key with a different fingerprint
  is `IDEM_CONFLICT` (`409 idempotency_conflict`), counted as `metering_idem_conflict_total`, and
  never a silent `Applied:false`: a producer reusing keys would otherwise drop charges without a trace.
- **Usage keys are unique per scope**, so hot and durable agree exactly on what a duplicate is.
  **Minting keys are unique per realm and operation**, so one provider payment can never be granted to
  two scopes, and a host's usage key `x` never collides with a grant `x`.
- **A usage event older than its window is refused, not re-applied.** `Record` requires
  `OccurredAt` and rejects one older than `UsageIdemWindow − IdemSafetyMargin` with `422
  stale_event`, so a retry that outlives its durable guard fails loudly instead of charging twice.
  `UsageIdemWindow` MUST exceed every producer's retry horizon; `Validate` refuses a `HotIdemTTL`
  longer than it.
- **Between `HotIdemTTL` and `UsageIdemWindow`** a replay passes the hot guard, debits the hot
  balance, and is caught by the writer: `duplicate` suspense plus an automatic correction (§3). The
  ledger is never double-charged; the hot balance is for a few milliseconds.

---

## 7. When Redis is unavailable

`Admit` applies `AdmitFailPolicy`. `Record` **does not lose the charge**: it writes the intent to
`spend_journal` (Postgres, unique per realm + scope + key, fingerprinted) and returns
`Receipt{Applied:true, Deferred:true}` (`metering_record_deferred_total`). The
`billing.journal.tick` sweep replays pending journal rows through the normal hot function once the
shard is live — idempotent on the same key — and the writer marks each row settled when it books the
resulting intent.

`Settlement=journal` is the same mechanism used on every call rather than only during an outage:
journal first, then the hot function. So `fail_open` means *served before it is metered*, never
*served unmetered*: the only unrecorded usage is usage whose `Record` failed at both stores, which the
host sees as an error and retries.

A balance that goes negative because deferred usage lands after recovery is consumption overshoot,
exactly as with concurrent admits. `NegativeBalancePolicy` governs grants, not consumption.

---

## 8. Reconcile, audit and resharding

**Reconcile** (`billing.reconcile.tick`, per shard) checks scopes the writer booked since the last
run, plus a full sweep on `ReconcileFullSweepInterval`. Its cost scales with active scopes, not with
ledger history. For each scope it reads `acct` (one key, so `gen`, `seq` and the pool balances are one
consistent snapshot) and, from Postgres, the watermark, `booked` per pool, and open suspense per pool:

| Observation | Outcome | Action |
|---|---|---|
| `acct.gen ≠ meta.gen` | `cold` | skip: it will rebuild from the books |
| `acct.seq > applied_seq` | `deferred` — intents in flight | re-check briefly; if still behind, record `deferred`. Lag is the outbox-age alert's job, not drift |
| `acct.seq < applied_seq` | `regression` | freeze the shard (§5) and page |
| `acct.seq == applied_seq` | compare | `drift = hot − (booked + open suspense)`, per pool |

At equal sequence numbers, any non-zero drift is a defect by construction, so `ReconcileTolerance`
defaults to `0` and the in-flight window cannot trip it. A drifted scope is written to
`reconcile_findings` (one row per scope and pool — a +100 and a −100 on two scopes never net to
zero), healed **on the hot side** by `ip_heal`, which is a compare-and-set on `seq` so a concurrent
charge is never overwritten, and paged when `|drift| > ReconcileTolerance`. **Reconcile never writes a
ledger row.** The books are corrected only by an investigated, audited `admin_adjustment`.

**Deep audit** (`billing.audit.tick`, off-peak) verifies `booked == checkpoint + Σ ledger since the
checkpoint` per scope, then advances `ledger_checkpoints`. Checkpoints are what let old ledger
partitions be **detached and archived** without losing the ability to prove a balance, since
summing the whole history is the one thing a ledger that grows forever cannot keep doing.

**Resharding** is the freeze procedure applied to every shard: freeze all; drain all; write the new
`Shards` to `hot_config` and bump every generation; unfreeze. Scopes rebuild lazily under their new
shard tags; old keys are garbage-collected in the background. Two consequences, both deliberate:
window counters restart (a daily ceiling can admit up to one extra window's worth on reshard day),
and hot idempotency guards are lost — the durable guards cover them through the late-replay path
(§6). With 256 shards by default, resharding is a rare, planned operation.

---

## 9. Verification

[`scripts/verify-hot-path.sh`](../../../scripts/verify-hot-path.sh) starts Redis 7 in **cluster
mode**, loads [`reference/hot_path.lua`](./reference/hot_path.lua), and asserts, among others:

- the layout before D29 — the balance tagged by scope, the outbox tagged by shard — fails with
  `CROSSSLOT`, and the shard-tagged layout does not;
- a debit applies once; a replay returns `REPLAY` with the original `seq`; a reused key with a
  different fingerprint returns `IDEM_CONFLICT` and changes nothing;
- pools drain in priority order and the last eligible pool absorbs overshoot;
- a frozen shard and a stale generation refuse every mutation; rehydrate installs state once, and
  only while the account is stale;
- `ip_heal` refuses when `seq` moved since it was read;
- a transfer refuses to overdraw and never touches another slot;
- each mutation increments `seq` by exactly one and emits exactly one intent;
- `clamp_to_zero` floors the pool and reports the writeoff; `block_and_flag` also blocks the account.

The Postgres half — watermark cases, suspense, window-bounded usage dedup, transfer conservation — is
the `ConsistencyContract` suite ([tasks.md](../tasks.md) T034a), which runs against real Redis and
Postgres via Testcontainers, including a forced rollback (a Redis restart from a truncated AOF)
mid-traffic.
