# Go engineering review — 2026-11-21

A senior-level review of the whole Go codebase at `main` @ `74372b0`: idiom and style, implementation patterns, scalability,
reliability and security. It is a **backlog, not a change**: nothing here is fixed yet. Each finding is written so that an
engineer or a model picking it up can fix it the way this repository requires (red test first, contract in the same commit,
real verification output) without re-deriving the analysis.

The prior review ([production review of 2026-11-18](../production.md), fixed in PR #45) is not repeated here. Its open items
(unpinned action and image digests, no release tag, the unfunded-scope admit, webhook rate limiting at the edge, untried
PgBouncer/TLS Postgres/Redis Cluster/region failover) still stand and are tracked in [production.md](../production.md).

## Verdict

The codebase is well above the usual bar. The money path is careful and adversarially tested: one Redis Function per
mutation in one hash slot, a durable guard behind every effect, a scope advisory lock around every booking, fail-closed
pricing, raw-body webhook verification before any parse, constant-time credential checks, an SSRF guard enforced at dial
time, panic recovery on the REST surface, body caps and request deadlines. The boundary rules are machine-enforced and
hold.

The problems are mostly at the **edges of the process**, where library defaults and operational behaviour meet the design.
The configured hot-path deadlines don't bind on the Redis socket. One process-global gauge can page forever. Entitlement
eviction never reaches `paymentd` on the JetStream bus. Unbounded identifiers can turn an accepted charge into one that can
never be booked. Leases can be overrun and completions aren't fenced. A worker doesn't share shards and polls them
expensively when idle. None of these loses money silently, because the durable guards hold, but several would cause pages,
stale answers or outages in production.

## How to use this document (read before fixing anything)

1. **One finding per branch** (`fix/<id>-<slug>`), following [CLAUDE.md](../../CLAUDE.md): a `test(red): …` commit that
   fails on an assertion, then `fix(green): …`, then optionally `refactor:`. A change to a contract under
   `specs/001-metering-billing-core/contracts/` lands **in the same commit** as the code. A behavioural fix that becomes a
   task gets the next free `R###`/`T###` pair in `specs/001-metering-billing-core/tasks.md` (the highest ID at review time
   was 149; IDs are never reused), and ROADMAP.md's count is updated in the same change.
2. **Status words.** **CONFIRMED** means the defect was traced in this repository's code and, where a library is involved,
   in that library's source at the pinned version. **PLAUSIBLE** means the reasoning is sound but no test or measurement was
   run, either because Docker's image store was failing during this review or because a measurement is needed. For a
   PLAUSIBLE finding, the red test is the experiment: **if it passes on unmodified code, the finding is wrong.** Say so in the
   PR, and correct or delete the entry here.
3. **Measure before optimising.** The performance findings (F07, F10, F17, F18, F20) need a before/after number: a
   benchmark, `EXPLAIN`, or `make hardening`. A perf change with no measured benefit is not shipped. That lesson cost a PR
   in Phase 7.
4. **Do not weaken a gate to make a fix pass.** That covers mutation thresholds, lint rules, the architecture graph and
   `//nolint` without a reason.
5. When a finding is fixed, change its row in the index to `FIXED in #<PR>` and keep the entry. When a finding is
   rejected, write why.

## Index

Severity: **P1**: fix before real money is served. **P2**: fix before scaling out or running unattended. **P3**:
refinement.

| ID | Sev | Area | Finding | Status |
|---|---|---|---|---|
| [F01](#f01) | P1 | reliability | Redis client ignores context deadlines: `AdmitTimeout` 250 ms can take up to the 5 s socket timeout | CONFIRMED |
| [F02](#f02) | P1 | reliability, observability | A caller's cancellation is classified as a hot-store outage (fail-open counted, journal path taken) | CONFIRMED |
| [F03](#f03) | P1 | observability | `metering_writer_seq_gap_seconds` is process-global under a per-shard label and is never cleared on shard release, so it pages forever | CONFIRMED |
| [F04](#f04) | P1 | correctness | With `PAYMENT_BUS=nats_jetstream`, entitlement evictions never reach `paymentd` (stale for the cache TTL) | CONFIRMED |
| [F05](#f05) | P1 | correctness, security | No length or charset bounds on `idem_key`, `rate_key`, subjects or attributes: a hot charge can become unbookable | PLAUSIBLE |
| [F06](#f06) | P2 | scalability | The first worker takes every shard; replicas don't divide them (the docs say they do) | CONFIRMED |
| [F07](#f07) | P2 | scalability | Each idle shard poll costs 4 Postgres statements and about 7 Redis commands (~4k + ~7k/s at 256 shards) | CONFIRMED (cost not measured) |
| [F08](#f08) | P2 | correctness | Event dispatcher: a batch outlives its lease (duplicate sends), and completion is not fenced | CONFIRMED |
| [F09](#f09) | P2 | correctness | Billing inbox: `MarkProcessed`/`MarkFailed` are not fenced by the lease | CONFIRMED |
| [F10](#f10) | P2 | performance | `WHERE id::text = $1` cannot use the uuid primary key index | PLAUSIBLE (EXPLAIN not run) |
| [F11](#f11) | P2 | observability | `metering_price_error_total` labels a caller-supplied `rate_key` (unbounded cardinality) | CONFIRMED |
| [F12](#f12) | P2 | reliability, security | gRPC: entitlement handlers have no panic recovery; no keepalive or stream limits | CONFIRMED |
| [F13](#f13) | P2 | reliability | Shutdown closes listeners at once (no readiness drain); no gRPC health service | CONFIRMED |
| [F14](#f14) | P2 | reliability | NATS dialled with defaults: gives up for good after about 2 minutes of outage, silently | CONFIRMED |
| [F15](#f15) | P2 | security | PayPal: any well-formed forgery costs an outbound API call, and a PayPal 429 parks the raw body | CONFIRMED |
| [F16](#f16) | P3 | reliability | Postgres session defaults: no `application_name`, no writer `lock_timeout`; PgBouncer constraints undocumented | CONFIRMED |
| [F17](#f17) | P3 | performance | The warn/blocked coalescer sweeps O(n) under a global lock once past 4096 live keys, with no hard cap | CONFIRMED |
| [F18](#f18) | P3 | performance, idiom | Hand-rolled REST router: linear scan plus a map allocation per candidate route per request | CONFIRMED (cost not measured) |
| [F19](#f19) | P3 | idiom | Errors are matched by message text where typed helpers exist | CONFIRMED |
| [F20](#f20) | P3 | performance | Entitlement cache: one global epoch, so any eviction discards every concurrent load | CONFIRMED (cost not measured) |
| [F21](#f21) | P3 | security | SSRF guard: Teredo and local-use NAT64 ranges are not refused | CONFIRMED |
| [F22](#f22) | P3 | observability, idiom | `http.Server.ErrorLog` unset; panic recovery swallows `http.ErrAbortHandler` | CONFIRMED |
| [F23](#f23) | P3 | operations | TLS certificates are loaded once; rotation needs a restart | CONFIRMED |
| [F24](#f24) | P3 | observability | No tracing, no request or trace id in logs; `ports.Metrics` has no context, so no exemplars | CONFIRMED |
| [F25](#f25) | P3 | maintainability | Cognitive-complexity hot spots worth splitting behind existing tests | CONFIRMED |

Suggested order: F01+F02 together (they share code and F01 makes F02 frequent), then F03, F05, F04, F08, F09, F12,
F13, F14, then the rest.

---

## P1

<a id="f01"></a>
### F01: Redis client ignores context deadlines, so hot-path timeouts don't bind

**Where.** `metering/adapters/driven/redis/connect.go:17` (`Connect`). Every hot call wraps its context with
`AdmitTimeout` (250 ms) or `RecordTimeout` (500 ms) (`admit.go`, `debit.go`, `grant.go`, `lifecycle.go`).

**What is wrong.** go-redis v9 applies a context's deadline to socket reads and writes **only** when
`Options.ContextTimeoutEnabled` is true. Otherwise it uses `context.Background()` for the I/O and the client's
`ReadTimeout` (default 5 s) and `WriteTimeout`. See `github.com/redis/go-redis/v9@v9.22.0/redis.go:1468`
(`baseClient.context`), and `options.go:520-551` for the defaults (`ReadTimeout = 5s`, `MaxRetries = 3`). `Connect`
builds clients with `ParseURL`/`ParseClusterURL` and never sets the flag, and the URL cannot set it.

**Failure scenario.** A Redis primary that stops answering without resetting connections: a black-holed network, a long
`BUSY` script, swap, or a stalled failover. Every `Admit` then waits up to 5 s instead of 250 ms before `AdmitFailPolicy`
applies. At the design rate (tens of thousands of admits per second) that is a goroutine and connection pile-up and an
outage, exactly what the fail policy exists to prevent. The chaos proxy (`internal/chaos/proxy.go`) only *cuts*
connections (RST), which fails fast, so no test sees a stall.

**Fix.**
1. In `Connect`, set `opts.ContextTimeoutEnabled = true` on both the `*goredis.Options` and the `*goredis.ClusterOptions`.
   Consider explicit `ReadTimeout`/`WriteTimeout` ceilings (around 1 s) for calls made without a deadline, such as bus
   reads with `Block`. Check that `XREADGROUP … BLOCK` still works: go-redis adds the block time to the read timeout.
2. Do F02 in the same branch. Once deadlines bind, cancellations surface as errors mid-read far more often.
3. A timed-out read closes the connection in go-redis. Under a latency spike, expect connection churn and keep `PoolSize`
   in mind (default 10 × GOMAXPROCS).

**Red test first.** Add a `Stall()` mode to `internal/chaos.Proxy` that keeps connections open but stops forwarding server
→ client bytes. Under `-tags=integration` (`metering/adapters/driven/redis/integration_test.go`), point a `Store` at the
proxy, stall it, and assert that `Admit` returns `domain.ErrHotStoreUnavailable` within `AdmitTimeout + 100ms`. It fails
today, taking about 5 s.

**Verify.** `make test-integration` (the new test and the redis package), `make verify-hot-path`, `make hardening` with
the hot-store-loss run if available. Paste the output.

**Don't.** Don't lower `ReadTimeout` alone as "the fix": it bounds every call by one global number and ignores the
per-operation budgets the contract defines.

<a id="f02"></a>
### F02: A caller's cancellation is treated as a hot-store outage

**Where.** `metering/adapters/driven/redis/errors.go:42` (`wrapErr`). Any non-`goredis.Error`, including
`context.Canceled` from the caller, becomes `ErrHotStoreUnavailable`. That is consumed by
`metering/app/admit.go:67` → `admitOutage` (`admit.go:99`) and by `metering/app/record.go:53`.

**Failure scenario.** A client disconnects, or its own deadline passes, while `Admit` waits on Redis. Under
`fail_open`, the engine counts `metering_admit_failopen_total` and records `outcome="failopen"` on the latency histogram,
as if Redis were down. Under `fail_closed` it reports `unavailable`. `Record` tries to journal the charge with the
already-cancelled context and returns "the journal is unavailable too". Alerts on fail-open and hot unavailability fire on
ordinary client churn, and they would fire more after F01.

**Fix.** Tell **the caller's** cancellation apart from **the adapter's own** deadline:
- In `Store.call`/`Store.mutate` (or a helper they share), when the *parent* context (before `withTimeout`) is done,
  return `ctx.Err()` as-is (`context.Canceled`/`DeadlineExceeded`), not wrapped as `ErrHotStoreUnavailable`. When only
  the child deadline expired, the hot tier was too slow, and `ErrHotStoreUnavailable` is correct.
- In `meter.admit` and `meter.Record`, check `ctx.Err()` before taking the outage branch and return it without counting
  fail-open or deferral. The gRPC door already prefers `ctx.Err()` (`grpcserver/server.go`, `fail`); check that the REST
  door does the same.
- Idempotency is unaffected: a `Record` cancelled after the function applied is retried by the caller with the same key
  and gets `REPLAY`.

**Red test first.** Pure unit tests in `metering/app` with the existing fakes: a `Balance` fake that returns
`fmt.Errorf("%w: …: %w", domain.ErrHotStoreUnavailable, context.Canceled)` while the passed context is cancelled. Assert
that `Admit` under `FailOpen` returns `context.Canceled` and that the fake metrics saw no `metering_admit_failopen_total`.
Add an adapter-level table test for the classification helper.

**Contract.** `specs/001-metering-billing-core/contracts/metering-ports.md`: state that a caller's cancellation is
reported as cancellation and is not a fail-open event. `make mutation-diff` applies (`metering/app`).

<a id="f03"></a>
### F03: The sequence-gap gauge is process-global, per-shard labelled, and never cleared

**Where.** `metering/app/writer.go:189` (`observe`) and `writer.go:209` (`noteGap`). The map `Writer.gaps` is keyed by
scope tag only, written from `metering/app/booking.go:73`. The alert is `deploy/alerts.yml`
(`max by (shard) (metering_writer_seq_gap_seconds) > 120`).

**What is wrong.**
1. `observe(shard)` exports the oldest gap across **all** scopes of **all** shards the process owns, under the label of
   the shard it is draining. Every shard reports the same number, and the alert names the wrong shard.
2. An entry is removed only when *this process* books that scope again. When a shard is released (`runner.reap`,
   shutdown) or its scope's gap is resolved by another worker after a takeover, the entry stays. The gauge then grows
   without bound and the alert pages until the process restarts.

**Fix.** Key gaps by shard: `map[domain.Shard]map[string]time.Time`. `observe(shard)` reads only its own shard. Add
`Writer.ReleaseShard(sh)`, which drops the shard's entries and exports `0` for its label. Call it from
`cmd/payment-worker/runner.go` when a shard loop ends (the `defer` in `shardLoop`, `runner.go:157`). `noteGap`
needs the shard, so `bookRun` passes it (it already has it).

**Red test first.** In `metering/app` (`writer_internal_test.go`, which can reach `noteGap`/`observe`): note a gap for a
scope on shard 1, call `observe` for shard 2, and assert the fake gauge for `shard="2"` is 0. It fails today. A second
test: after `ReleaseShard(1)`, the gauge for shard 1 is 0.

**Contract.** None: `metering-ports.md` already defines the gauge per `shard`, so this makes the code match it.
`make mutation-diff` applies.

<a id="f04"></a>
### F04: Entitlement evictions never reach `paymentd` on the JetStream bus

**Where.** `paymentd` builds its bus from `PAYMENT_BUS` (`cmd/paymentd/serve.go:192`), and its entitlement cache
subscribes to `<prefix>.entitlement.>` and `<prefix>.config.>` on that bus (`entitlement/app/invalidate.go:68`). Every
publisher of those subjects runs in **`payment-worker`**, which always uses Redis streams (`cmd/payment-worker/serve.go:81`).
They are the processor (`billing/app/processor.go:705`) and the config relay (`cmd/internal/entitlements/stack.go:146`,
started only by the worker).

**Failure scenario.** Set `PAYMENT_BUS=nats_jetstream` (documented as supported for `paymentd` in
`docs/configuration.md:47`). A subscription is cancelled or upgraded, or a plan's entitlements change: `paymentd` keeps
serving the old answer, including `max_entitlement`-sized limits used by `Admit`, for up to
`PAYMENT_ENTITLEMENT_CACHE_TTL` (default 30 s, maximum 10 m). The contract says a change is visible at once. The
`.webhook.received` hint `paymentd` publishes is also lost to the worker. That only costs latency, because the inbox sweep
is the guarantee.

**Fix: needs a maintainer decision.**
- (a, recommended) `paymentd` subscribes its *internal* listeners (the entitlement watcher) on the Redis-streams bus it
  shares with the worker, whatever `PAYMENT_BUS` says, and `PAYMENT_BUS` selects only where host-facing notifications go.
  This keeps one control plane.
- (b, smallest) `paymentd` refuses `PAYMENT_BUS=nats_jetstream` at start-up, as the worker already does, until a relay
  exists. This fails loud.
- (c) The worker publishes to both buses. That's more moving parts, so it is not recommended.

**Red test first.** For (b), a settings test in `cmd/paymentd/settings_test.go` asserting the refusal. For (a), a wiring
test in `cmd/internal/entitlements` showing that a message published on the Redis bus evicts a `paymentd`-built cache when
`Bus=nats_jetstream` (use `natstest` for NATS and `redistest` for Redis, under `-tags=integration`).

**Docs.** `docs/configuration.md` (`PAYMENT_BUS`), `bus-subjects.md` in the contracts if the subject ownership changes.

<a id="f05"></a>
### F05: Unbounded identifiers can make an accepted charge unbookable

**Where.** `metering/domain/event.go:37` (`Event.Validate`) checks that `idem_key`, `rate_key`, subject kinds and values
are **non-empty** and nothing more. The same goes for grants, transfers and `AdmitRequest`. The REST body cap is 1 MiB
(`resthandler/registry.go:211`), and gRPC allows 4 MiB. Only admin operations bound their key (`metering/app/adminops.go:25`,
`MaxAdminKey = 200`, no control characters).

**Failure scenario (PLAUSIBLE).** A host bug sends a 3 KB+ idempotency key (or one built from a user-supplied string).
The hot function accepts it, because Redis keys can be large: the balance is debited and the intent is XADDed. The writer
then fails to book it, because `credit_ledger_idem_idx (realm, idem_key)` and the guard table's primary key
`(realm, op, idem_key)` exceed PostgreSQL's btree tuple limit (about 2704 bytes for incompressible data). After
`MaxAttempts` the intent is parked as poison with an open suspense entry (`writer.go`, `parkPoison`). `credit_outbox_dead`
and `credit_suspense` don't index the key, so parking succeeds. Replaying the parked intent fails the same way, forever.
Every such event needs an operator. The same unbounded strings also bloat Redis keys and counters (`ctr:` keys embed the
subject value) and flow into logs.

**Fix.** Add bounds in the **domain**, so every transport refuses the same way with `ErrInvalid` → 400 or
`INVALID_ARGUMENT`. Agree the numbers with the maintainers. They must exceed any key a known host sends: provider ids are
around 30 characters and UUIDs are 36. A starting point: `idem_key` ≤ 255 bytes, `rate_key` ≤ 128, subject kind ≤ 64 and
value ≤ 255, ≤ 16 subjects, ≤ 32 quantities, attributes ≤ 32 entries (key ≤ 64, value ≤ 1024). Refuse control
characters, mirroring `MaxAdminKey`. Put the limits in one place (`metering/domain`), used by `Event`, `Grant`, `Transfer`,
`AdmitRequest` and the job budget subject.

**Red test first.** `metering/domain`: `TestAnIdemKeyOverTheLimitIsRefused` and similar for the other fields. These are
pure unit tests and fail today. Optionally, an integration test proving the poison path with a 4 KB key before the fix, to
convert this entry to CONFIRMED.

**Contract.** `metering-ports.md` (validation rules) and a conformance case in `metering/contracts` that every
`Meter` refuses an over-long key. The OpenAPI and protobuf docs should state the limits. `make mutation-diff` and
`make fuzz` apply (the bounds are an encoding-adjacent boundary).

---

## P2

<a id="f06"></a>
### F06: The first worker takes every shard; replicas don't divide them

**Where.** `cmd/payment-worker/runner.go:116` (`acquire`) tries every shard on every `OwnershipRetry` and takes each lock
it can get. Nothing caps how many one process holds, and nothing gives shards back. The pool is sized for all of them
(`serve.go:76`, `Shards+16`). `docs/operations.md:38` ("Run replicas to divide the shards") and
`docs/configuration.md:108` ("Replicas divide the shards, so each needs only its share") describe behaviour the code
doesn't have.

**Impact.** A second replica started after the first is a hot standby that owns nothing. Throughput is bounded by one
process. One process holds 256 session-level connections. Session advisory locks also rule out transaction-mode
PgBouncer for the worker.

**Fix: needs a decision.** Minimum: correct both docs, saying that one worker owns all shards it can open and others are
standbys. Better: add `PAYMENT_WORKER_MAX_SHARDS`, or derive a fair share from a heartbeat table of live workers
(`target = ceil(shards / live)`). A worker over target releases its excess after the batch in hand (`lease.Release`), and
one under target acquires. Correctness doesn't depend on exclusivity (the writer's comment and the scope lock), so a
handoff overlap is safe.

**Red test first.** `cmd/payment-worker` runner test with a fake `Books.Acquire`: two runners and a cap, each owns ≤ cap
and together they own all.

<a id="f07"></a>
### F07: Idle shard polling is expensive

**Where.** `metering/app/writer.go:80` (`drain`) runs on every loop of every owned shard (`runner.go:157`, every
`PAYMENT_DRAIN_INTERVAL` = 250 ms when idle). Each call does:
- `syncShard` → `Books.EnsureShard`: `INSERT … ON CONFLICT DO NOTHING` + `SELECT` (`shards.go:60`); then Redis
  `HGETALL meta` (`lifecycle.go:125`).
- `failedOver` → `EnsureShard` **again** (2 more statements), and `INFO replication` on the primary (`lifecycle.go:178`).
- `Stream.Read`: `XAUTOCLAIM`, a non-blocking `XREADGROUP` (`redisstreams/intents.go:131`, `Block: -1`), and delivery
  counts when there are messages.
- `observe` → stream stats: `XLEN`, `XPENDING`, `XINFO GROUPS`, and an `XRANGE` when a group lags (`redisstreams/streamutil.go`).

At the default 256 shards that is about 1,024 polls/s: **~4,000 Postgres statements/s (half of them `INSERT … ON CONFLICT`) and
~7,000 Redis commands/s with no traffic at all**, including about 1,000 `INFO` calls/s on the primary.

**Fix (measure first with `make hardening` or a pg_stat_statements snapshot).** Cache the `ShardRecord` per owned shard
in the writer, and refresh it on a slower cadence (a few seconds) or after `Recover`/a bump. Call `EnsureShard` once at
acquire. Run the replication-id check on a cadence of its own (the sequence-regression check stays the per-intent
backstop; confirm against `hot-path-consistency.md` that the detection latency is acceptable). Export stream stats on the
gauge cadence (15 s), not per drain. Back off an idle shard adaptively (250 ms → 2 s) and reset on work.

**Red test first.** A writer test with counting fakes: `Drain` on an idle shard calls `EnsureShard` at most once per
refresh period, not twice per call.

<a id="f08"></a>
### F08: The event dispatcher's batch outlives its lease, and completion is not fenced

**Where.** `events/app/dispatcher.go:19-21` (`DefaultLease = 30s`, `DefaultBatch = 50`, `sendWorkers = 8`) and `Tick`
(`dispatcher.go:79`). A send times out after 10 s (`httpsend.DefaultTimeout`). Completion is written by
`events/adapters/driven/postgres/store.go:188-197` with `WHERE id::text = $1` only.

**Failure scenario.** Several endpoints are slow or dead. The claimed batch of 50 runs 8 at a time, so the last items
start after 6 rounds × 10 s = 60 s, past their 30 s lease. The next tick re-claims and sends them again, concurrently.
`attempts` is incremented twice, which skews the retry schedule and the dead-after window, and the two completions race:
last writer wins. A delivered row can be overwritten by a stale failure. Receivers dedupe by event id, so this is
duplicates and wrong bookkeeping, not data loss. The comment on `DefaultLease` reasons per send, not per batch.

**Fix.** Claim only what can be sent inside the lease (`Batch ≤ sendWorkers × floor(Lease / Timeout)`, with a margin),
or claim just in time per worker. Fence completion the way `admin_operations` already does
(`metering/adapters/driven/postgres/adminops.go:124`: `AND status = 'running' AND attempts = $2`). Return the claim token
from `Claim` and make `Delivered`/`Failed` refuse a stale one (rows affected 0 → `ErrLeaseLost`, counted, not retried).
Consider a per-endpoint concurrency cap so one dead endpoint cannot occupy all 8 workers.

**Red test first.** An `events/contracts` conformance case, run against the Postgres store: "a completion from an expired
claim changes nothing". Plus an `events/app` test with a fake clock and slow sender showing that no delivery is attempted
after its lease.

<a id="f09"></a>
### F09: Billing inbox completion is not fenced by the lease

**Where.** `billing/adapters/driven/postgres/inbox.go:257` (`MarkProcessed`) and `:265` (`MarkFailed`) update by `id`
alone. `Lease` (`:157`) enforces "never one customer's two events at once" only while leases are live.

**Failure scenario.** Processor A leases an event and stalls past the TTL (GC pause, slow provider call). B re-leases it
(attempts +1) and processes it. A then calls `MarkFailed`: the row becomes `failed`, or **`dead`** if attempts reached the
maximum, after it was processed. A dead-letter page fires for a processed event. Meanwhile, once A's lease has expired, B
can also lease the *next* event of the same customer while A still works on the previous one. Grants stay exactly-once
(durable guards) and subscription state is order-independent (`ApplyIfNewer`), so money is safe. Status and the ordering
promise are not.

**Fix.** Use `attempts` as the fencing token. `Lease` already returns it, so `Mark*` add
`AND status = 'processing' AND attempts = $n`. Rows affected 0 → a typed `ErrLeaseLost` that the processor logs and
counts without retrying. For long work, add lease extension (heartbeat) or size the TTL from the measured p99.

**Red test first.** A conformance case in `billing/contracts` (the `Inbox` suite): "a completion after the lease was
taken over is refused and leaves the newer outcome". **Contract:** the payments contract's inbox section.

<a id="f10"></a>
### F10: `WHERE id::text = $1` defeats the primary-key index

**Where.** `events/adapters/driven/postgres/store.go:189,197,224,234` (`event_deliveries`, the busiest of these tables),
`metering/adapters/driven/postgres/deadletters.go:83,97,111,123`, `adminops.go:84,124`, and `drift.go:89`
(`run_id::text = ANY(...)`). No expression index exists on `(id::text)`.

**Why (PLAUSIBLE).** An expression on the indexed column hides it from the btree on `id`, so each lookup is a sequential
scan. That is harmless on small tables and grows with `event_deliveries`. The cast probably exists so that a malformed id
answers "not found" instead of raising `22P02`.

**Fix.** Parse the id in Go. On failure, return the adapter's not-found error. Otherwise query `WHERE id = $1` with a
`uuid`/`pgtype.UUID` argument.

**Red test first, and it is the measurement.** Under `-tags=integration`, run
`EXPLAIN (FORMAT JSON) UPDATE event_deliveries SET attempts = attempts WHERE id::text = $1` (the current statement) against
the migrated schema with enough rows that the planner prefers an index (or `SET enable_seqscan = off`), and assert the plan
contains `Index Scan`. It fails today. Confirm manually with:

```sql
EXPLAIN ANALYZE UPDATE event_deliveries SET attempts = attempts + 1 WHERE id::text = '<uuid>';  -- expect Seq Scan
EXPLAIN ANALYZE UPDATE event_deliveries SET attempts = attempts + 1 WHERE id = '<uuid>'::uuid;  -- expect Index Scan
```

This could not be run during the review: Docker Desktop's image store returned `input/output error` for every image.

<a id="f11"></a>
### F11: `metering_price_error_total` labels a caller-supplied value

**Where.** `metering/app/record.go:86` labels `rate_key` with `e.RateKey` as sent. The label is part of the contract
(`metering-ports.md`, the metrics table; `metering/ports/metrics.go`).

**Failure scenario.** A host passes an end-user string through as the rate key (a model name, a SKU from a form). Every
distinct unknown key creates a new Prometheus series until the scrape target falls over. The other labels in the catalogue
are bounded (realm, a configured limit name, a registered provider).

**Fix.** Label the rate key only when it names an entry on the realm's active card, which is bounded by configuration.
Otherwise label it `(unknown)` and log the raw key at debug level, rate-limited (the coalescer can do this).
**Contract:** amend the metric's row in `metering-ports.md` in the same commit.

**Red test first.** An `metering/app` test with fake metrics: two different unknown rate keys produce one label value.
`make mutation-diff` applies.

<a id="f12"></a>
### F12: gRPC server hardening

**Where.** `cmd/paymentd/serve.go:316` builds `grpc.NewServer` with one unary auth interceptor. Metering's handlers each
`defer s.guard(...)` (`grpcserver/server.go`), but **`entitlement/adapters/driving/entitlementgrpc/server.go:79` (`Check`)
and `Resolve` do not**. gRPC does not recover panics, so a panic there kills `paymentd`. No keepalive enforcement,
`MaxConcurrentStreams` or connection age is set.

**Fix.** Add a recovery unary interceptor, chained **first** with `grpc.ChainUnaryInterceptor(recover, auth)`, so it also
covers the auth interceptor and any future service. It logs the stack and returns `codes.Internal`. Keep or remove the
per-handler guards, but don't leave two behaviours. Set `keepalive.EnforcementPolicy{MinTime: …}`,
`keepalive.ServerParameters{MaxConnectionIdle, MaxConnectionAge, MaxConnectionAgeGrace}` (connection age also lets L4
load balancers rebalance long-lived clients) and `grpc.MaxConcurrentStreams`. If a streaming RPC is ever added, it needs a
stream auth interceptor too. Today there are none.

**Red test first.** `entitlementgrpc` test: a `Checker` that panics → the call returns `codes.Internal` and a second call
succeeds.

<a id="f13"></a>
### F13: Shutdown has no readiness drain, and there is no gRPC health service

**Where.** `cmd/paymentd/serve.go:133`. On a stop signal both listeners stop at once (`drain`). Behind Kubernetes
Services or most load balancers, endpoint removal lags the signal by seconds, so new connections in that window are
refused.

**Fix.** On stop: make `/readyz` answer 503 and set a gRPC health server (`google.golang.org/grpc/health`) to
`NOT_SERVING`, wait `PAYMENT_SHUTDOWN_DELAY` (new; default `0s` keeps today's behaviour, and the docs recommend about 5 s
under an orchestrator), then drain as today. Register `grpc_health_v1` regardless, so orchestrators can probe gRPC
natively.

**Red test first.** `cmd/paymentd` test: after cancelling the run context with a delay configured, `/readyz` answers 503
while `/v1/...` still serves. **Docs:** `docs/configuration.md`, `docs/operations.md` (stop section).

<a id="f14"></a>
### F14: NATS dialled with defaults gives up silently

**Where.** `cmd/internal/wiring/wiring.go:139`: `nats.Connect(url, nats.Name(...), nats.Timeout(5s))`. nats.go v1.54
defaults to `MaxReconnect = 60` and `ReconnectWait = 2s` (`nats.go:56-57`). After about 2 minutes of outage the
connection is **closed for good**. Nothing logs it and readiness doesn't check the bus.

**Fix.** Add `nats.MaxReconnects(-1)`, a bounded `ReconnectWait` with jitter, and `DisconnectErrHandler`,
`ReconnectHandler` and `ClosedHandler` that log through slog and count a metric. Expose the bus state to `/readyz`, where a
closed connection means not ready.

**Red test first.** A wiring test asserting the built connection's options (`nc.Opts.MaxReconnect == -1`), plus an
integration test that stops `natstest` for longer than the old window and sees the bus recover.

<a id="f15"></a>
### F15: PayPal verification can be used for amplification and storage growth

**Where.** `billing/adapters/driven/paypal/webhook.go:31` (`VerifyWebhook`) refuses missing headers, an unparseable or
out-of-tolerance timestamp and a body that isn't a single JSON value, which are all good. Anything that passes costs an
outbound call to PayPal's verify API. A PayPal error such as a 429 is "unavailable", and `billing/app/ingress.go:219`
(`park`) stores the raw body (up to 1 MiB) and headers in Postgres for retry.

**Failure scenario.** An unauthenticated sender who knows a PayPal account's route posts well-formed forgeries with a
fresh timestamp. Each costs a PayPal API call on this deployment's credentials. Once PayPal rate-limits them, every
forgery, and every genuine delivery, is parked. Genuine fulfilment is delayed and the parked table grows with the
attacker's input. Edge rate limiting is the deployer's job (production.md), but the application amplifies.

**Fix.** Add cheap pre-checks before the remote call: an allowlist for `PAYPAL-AUTH-ALGO`, and an `https` scheme plus a
PayPal-owned host for `PAYPAL-CERT-URL`. Check PayPal's documentation for the exact hosts and don't guess. Add a
per-account token bucket on remote verifications (over budget → 503, so PayPal retries genuine deliveries), a cap on
parked rows per account (over cap → 503), and dedupe of parked deliveries by `PAYPAL-TRANSMISSION-ID` as well as body
hash.

**Red test first.** `billing/app` ingress test with a provider fake that counts remote verifications: N forgeries
beyond the budget make at most budget calls, and park nothing beyond the cap.

---

## P3

<a id="f16"></a>
### F16: Postgres session defaults

`metering/adapters/driven/postgres/pool.go:13` sets only `MaxConnLifetime` and `HealthCheckPeriod`.
- Set `RuntimeParams["application_name"]` (`paymentd`, `payment-worker`, `payment-migrate`) unless the DSN names one, so
  `pg_stat_activity` and lock graphs are readable.
- `Books.InTx` (`books.go:33`) waits on `pg_advisory_xact_lock` bounded only by the caller's context (the worker's
  drain context is `PAYMENT_SHUTDOWN_TIMEOUT`, 30 s). A `SET LOCAL lock_timeout` (as `maintenance.go` already does) gives a
  clearer failure.
- Document in `docs/production.md` that the worker cannot run behind transaction-mode PgBouncer (session advisory locks),
  and that pgx's statement cache needs `default_query_exec_mode=exec` or PgBouncer ≥ 1.21 with prepared statements for
  `paymentd`.
- The `statement_timeout` and `idle_in_transaction_session_timeout` guidance already in production.md stands.

<a id="f17"></a>
### F17: The coalescer degrades under a denial storm

`metering/app/coalesce.go:29`: past 4096 entries, every `allow` call iterates the whole map under one mutex, on the
`Admit` path for every warning and denial. If all entries are fresh (a burst across many scopes within the minute), the map
keeps growing and every call pays O(n). Sweep at most once per window (track `lastSweep`), and enforce a hard cap
(drop-oldest via a ring of time buckets). Red test with a fake clock: 20k distinct keys inside one window keep
`len(last)` ≤ cap. Add a benchmark.

<a id="f18"></a>
### F18: The hand-rolled REST router allocates per request

`metering/adapters/driving/resthandler/registry.go:302` (`entry.match`) allocates a `map[string]string` for every
registered route with the same segment count, before comparing literals, on every request including `Admit` and `Record`.
Since Go 1.22, `net/http.ServeMux` matches method + path patterns with `{wildcards}`, picks the most specific pattern,
answers 405 with `Allow`, and panics at registration on ambiguous patterns. **Before switching, prove with a test that it
keeps an escaped `%2F` inside one segment**, which the router's comment requires for idempotency keys in paths. If it
doesn't, keep the router and allocate `vals` only after all literals match. Measure with a `BenchmarkRouter` before and
after.

<a id="f19"></a>
### F19: Typed error matching

- Redis replies are matched by message text in several places with mixed `Contains`/`HasPrefix`
  (`redisstreams/intents.go:67,115,136,190`, `streamutil.go:58`, `bus.go:131,278`, `redis/errors.go:54`). go-redis
  exports `HasErrorPrefix` (`error.go:37` at v9.22.0). Use it from one helper.
- `natsjetstream/intents.go:424` matches `"no message found"`. Use the nats.go sentinel via `errors.Is` (check its name in
  v1.54's `jetstream` package).
- `metering/adapters/driven/postgres/config_writer.go:94` tells a constraint apart by `strings.Contains(err.Error(),
  "one_active")`. Inspect `*pgconn.PgError.ConstraintName` where the error is mapped.

<a id="f20"></a>
### F20: Entitlement cache: one global epoch

`entitlement/app/cache.go:54`: `put` refuses any load that began before *any* eviction anywhere. Correct, but under
eviction churn (many scopes changing per second) the hit rate collapses for unrelated scopes. Use per-tag, per-realm and
global epochs, so a load is refused only by an eviction that could have covered it. Measure the hit rate under a
synthetic eviction rate first.

<a id="f21"></a>
### F21: SSRF guard gaps

`events/guard.go:52` refuses the ranges that matter. Add Teredo `2001::/32`, which embeds an obfuscated IPv4 address, and
local-use NAT64 `64:ff9b:1::/48` (RFC 8215), with cases in `guard_test.go`. Optionally, a destination-port policy
(443 by default, configurable).

<a id="f22"></a>
### F22: Server error log and panic handling details

- `cmd/paymentd/serve.go:303`: `http.Server.ErrorLog` is unset, so TLS handshake and connection errors go to stderr in the
  standard log format, bypassing the JSON slog handler. Set `ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)`.
  Do the same for the metrics server and the worker's server.
- `resthandler/registry.go:368` (`guard`) recovers everything, including `http.ErrAbortHandler`, which must be re-panicked.
  It also writes a 500 even when the handler already wrote a header. Track whether the header was written.

<a id="f23"></a>
### F23: TLS certificate rotation

`cmd/paymentd/serve.go` (`serverTLS`) loads the key pair once. With short-lived certificates (cert-manager, SPIFFE),
rotation needs a restart. Use `tls.Config.GetCertificate` backed by a reloader (on SIGHUP or a periodic mtime check). The
client CA pool for mTLS needs the same treatment.

<a id="f24"></a>
### F24: Tracing and correlation

There is no distributed tracing and no request or trace id in log lines. `ports.Metrics` takes no context, so exemplars
can't link a latency spike to a trace. Suggested shape, behind configuration and off by default: `otelhttp`/`otelgrpc` at
the driving adapters; spans in the Redis adapter (go-redis hooks) and Postgres (pgx's `QueryTracer`); a slog handler that
adds `trace_id`/`span_id` from the context. Keep the core free of tracing imports: the adapters own it. Changing
`ports.Metrics` to take a context is a port change (contract plus every implementation), so weigh it separately.

<a id="f25"></a>
### F25: Complexity hot spots

Cognitive complexity ≥ 35 (gocognit): `paypal/webhook.go` `normalize` (62), `payment-worker/ticks.go` `handlers` (48),
`metering/domain/limit.go` `Evaluate` (41), `payment-worker/adminops.go` `adminExecutors` (40), `billing/.../inbox.go`
`Lease` (40), `metering/.../ledger.go` `LedgerRows` (39), `resthandler/registry.go` `Validate` (38), `billing/app/processor.go`
`dispute` and `paymentSucceeded` (37 each), `stripe/webhook.go` `normalize` (37), `metering/app/reconcile.go` `Reconcile`
(35). Split them only behind the existing tests, as `refactor:` commits after a green. For `metering/domain` and
`metering/app`, run `make mutation-diff`. Table-driven dispatch suits `handlers`/`adminExecutors`, and per-event-type
functions suit the `normalize` switches. Don't refactor these for their own sake: do it when a finding above already
touches the function.

---

## Reviewed and sound: do not "fix"

These were examined and are correct. A linter or a reviewer may flag them, so they are listed to save churn.

- **Hot path (Lua).** One slot per call; the guard before backpressure; backpressure refuses and never trims; `seq` +1
  and exactly one XADD per mutation; CAS on `seq` for heal and unblock; `%d` formatting. `MaxOperationAmount` keeps
  values inside 2^53.
- **Durability ack.** `WAITAOF` on the pinned connection that ran the function, with the timeout in the right unit (go-redis
  converts it to ms). An unconfirmed wait is reported as unavailable *after* apply, so the replay is a `REPLAY`.
- **Rehydrate.** A per-process single flight; installs only while stale; retries on a generation bump.
- **Booking.** The scope advisory lock in every transaction; rollback with `context.WithoutCancel`; effects after commit,
  idempotent, swept on failure; poison parked with suspense, never dropped.
- **Webhooks.** The order is resolve account → verify the raw body under a deadline → persist → acknowledge. Metric labels
  use the registered provider, never the route's text. The PayPal splice guard (`json.Valid`) blocks webhook-id injection.
- **Auth.** SHA-256 token digests compared in constant time; `none` refused on reachable addresses; `/metrics` refused on
  a public API listener.
- **SSRF.** The URL is checked, then the address is checked in `Dialer.Control` after resolution (beats DNS rebinding);
  no proxy, no redirects, no keep-alive, capped response.
- **REST.** Body cap via `MaxBytesReader`, per-request deadline, strict JSON (unknown fields and trailing data refused),
  panic recovery.
- **Fail-closed switches.** `exhaustive` flags `Sub.Live`, `HoldsEntitlements`, the refund and dispute status switches,
  and similar. Their fall-through is `false` or "no effect", which is the fail-closed answer. Don't add `default: panic`.
- **`errchkjson` reports** on `json.Marshal` of `map[string]string` (`config_writer.go`, `paypal/webhook.go`,
  `ledger.go`, `relay.go`) are false positives: such a map always marshals.
- **Dockerfile.** Distroless static, non-root, `-trimpath`, `CGO_ENABLED=0`. CI runs with `permissions: contents: read`
  (digest pinning is tracked in production.md).

## What was run for this review

All on `main` @ `74372b0`, Go 1.27.1 locally (the module floor is 1.26.0):

| Check | Result |
|---|---|
| `go build ./... && go vet ./...` | clean |
| `GOMAXPROCS=4 go test -race -shuffle=on -count=1 ./...` | 49 packages `ok`, exit 0 |
| Extended `golangci-lint` sweep (config below; not a gate) | 634 reports, mostly `goconst`, `gocritic rangeValCopy` and `intrange` style. Each signal-bearing linter was read: the real findings are above, and the false positives are listed under "Reviewed and sound" |
| go-redis v9.22.0 and nats.go v1.54.0 sources | read to confirm F01 and F14 |
| `EXPLAIN` for F10, integration tier, `verify-hot-path` | **not run**: Docker Desktop's containerd store returned `input/output error` on every image pull or run |
| `make mutation-diff`, fuzz | not applicable: this change is documentation only |

The extended lint configuration, to reproduce the sweep (`golangci-lint run -c <file> ./...`). It is not a repository gate:
adopting any linter from it into `.golangci.yml` is a separate decision.

```yaml
version: "2"
run: { timeout: 15m, tests: false }
linters:
  default: none
  enable: [contextcheck, noctx, nilerr, nilnil, gocritic, unparam, sqlclosecheck, exhaustive, gocognit, dupl, durationcheck,
           errchkjson, forcetypeassert, containedctx, fatcontext, spancheck, musttag, makezero, nestif, errname, copyloopvar,
           intrange, perfsprint, usestdlibvars, wastedassign, prealloc, goconst, gosec, recvcheck, iface, canonicalheader, mirror]
  settings:
    gocognit: { min-complexity: 30 }
    nestif: { min-complexity: 6 }
    exhaustive: { default-signifies-exhaustive: true }
    gocritic: { enabled-tags: [diagnostic, performance, style], disabled-checks: [whyNoLint, commentedOutCode, unnamedResult, hugeParam, paramTypeCombine] }
  exclusions: { paths: ['.*\.pb\.go$', 'ui/', '\.claude/'] }
issues: { max-issues-per-linter: 0, max-same-issues: 0 }
```
