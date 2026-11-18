# Hardening (Phase 7)

Phase 7 of [tasks.md](../specs/001-metering-billing-core/tasks.md) is **measurement, not construction**: load, chaos,
security, recovery, adoption, and mutation over the integration tier. Nothing here adds a feature; each result is a number
or an outcome set against something the design or a success criterion already states, and **a miss is a design finding,
not a tuning task**.

This document records what was measured, how, on what, and — as importantly — what the measurement does *not* show. It is
written so a result can be reproduced and a claim can be checked: every figure below was copied from output of a run
made for this document, and the harness prints the same `MEASURED` and `VERDICT` lines the figures come from.

```bash
make hardening                                  # every measurement below, in order (Docker; ~30+ minutes)
make hardening HARDENING_RUN='^TestLoad'        # one group: Load, Chaos, Security, Recovery
```

**Run one group at a time, on an otherwise idle machine.** Two sessions running these suites at once froze the machine they were developed on, and even the repository's own integration tier (`-race`, two packages at a time) drove a 12-core laptop's load average to 21. `make hardening HARDENING_RUN=…` takes any `go test -run` pattern; confirm each expected test prints a `--- PASS` line (a combined pattern once skipped a group without saying so).

The harness is under the `hardening` build tag, so `make test-integration` and `make ci` do not run it: it takes minutes,
wants an otherwise idle machine, and two measurements running together measure each other. Its tests are deliberately
**not** `t.Parallel()` for the same reason.

## How to read a verdict

A measurement ends in one of three states, and the harness is built so they cannot be confused:

| State | Meaning |
|---|---|
| **MET** / **MISSED** | The number was measured and compared with the target. A miss fails the test and is the finding. |
| **SKIPPED — not assessable / inconclusive** | The machine could not show the target. The test is *skipped*, with the numbers, rather than passed or failed: "met" would be false and "missed" would blame the design for the machine. |
| **no row at all** | Not measured. See each section's *What this does not show*. |

A verdict is never computed over no data: a percentile over an empty histogram fails the test instead of passing every
threshold.

---

## Where each task stands

| Task | State | In one line |
|---|---|---|
| **T130** load | measured, **open** | every primary-side target and the writer's rate met; exactly-once held across ~1.1 million records; SC-001 at the target load **not shown** on this machine |
| **T131** chaos | six of seven run, **open** | each ends exact (nothing twice, nothing lost after retries) and pages where the contract says; one real defect found and fixed (a gauge that never cleared), and one query plan whose worst case is now bounded; a lagging-replica failover was **not** re-run under load |
| **T132** security | executed, **open** | what the repository can execute was executed and refused every attack; two findings (one fixed, one open); the checklist items a deployment owns are listed, not claimed |
| **T133** recovery | partly, **open** | an operator's shard recovery under traffic and an empty hot tier rebuilt from the books are timed; the Postgres half of a region failover was **not** rehearsed |
| **T134** adoption | run, guide corrected | both walkthroughs ran from an empty realm; the guide was wrong in four places and is fixed; the non-Go host ran against the built binaries, **not** the container image |
| **T135** mutation, integration tier | **not done** | a cost model, with measured inputs, says why it cannot be a nightly job as specified |

"Open" means the task stays unchecked in [tasks.md](../specs/001-metering-billing-core/tasks.md): a `[x]` there means done and verified, and each of these has a part that is not.

---

## T130 — Load, against the scale envelope and SC-001

**Run:** 2026-11-17, Apple-silicon laptop (12 logical CPUs), Docker Desktop 29.5.2 with 14 GB, `redis:7-alpine` and
`postgres:16-alpine` in containers, Go 1.27.1, load generator, Redis, Postgres, `paymentd` and `payment-worker` all on the
one machine. Two complete runs of the final harness are quoted (run 1 and run 2); the development runs that preceded them
agree with them within the ranges noted.

### The method, and why it is built this way

* **Open-loop pacing.** At a target rate, each operation falls due on a fixed schedule and its latency is measured from the
  instant it was *due*, not from when a worker got to it. A stall therefore charges everything that queued behind it. (Measuring
  from the send instant lets a slow server slow the generator down and report a flattering p99.) A closed-loop run finds the
  ceiling; its latencies are queueing time and are never quoted as the latency of a call.
* **The primary, measured on its own.** Redis's own `LATENCY HISTOGRAM` for `FCALL` and `FCALL_RO` is the time the primary
  spent executing, which is what "p99 at the primary" means. The latency monitor (20 ms) says what the server itself stalled
  on, so a spike in a client's p99 is either attributed to the server or shown to have come from outside it.
* **Controls, because a bare number cannot separate the design from the box.** The same load is driven at a `PING` through the
  same client and the same Docker port; at a TCP echo inside the test process over `127.0.0.1` (no Docker, no Redis); and
  `redis-benchmark` runs the **exact `ip_admit` command** the adapter sends *inside the Redis container over loopback*, with no VM
  network and no Go client in the path. The generator's and Redis's CPU are reported for every step.
* **A hard verdict only where the control can meet the bound.** A latency bound is judged only where a no-op over the same path
  meets it. Where the no-op misses the bound too, the test is skipped as inconclusive.
* **An oracle under load.** Each `Record` carries a unique key; about 2% are resent exactly (same key, same `occurred_at`).
  A resend that charged again fails the run, and at the end every scope's hot balance must equal its grant plus exactly the
  receipts its callers were told (SC-002, measured under load rather than argued).

### Results

Targets are from the [scale envelope](../specs/001-metering-billing-core/plan.md#scale-envelope) and SC-001.

| Claim | Target | Run 1 | Run 2 | Verdict |
|---|---|---|---|---|
| One primary executes the `Admit` command (loopback, 256 clients) | ≥ 50 000/s | 133 138/s | 137 760/s | **MET** (2.7×) |
| The primary's own p99 for `Admit` at 50 000/s offered | ≤ 1 ms | ≤ 33 µs (mean 6.0 µs) | ≤ 66 µs (mean 6.7 µs) | **MET** |
| One primary sustains mutations (`Record`), Redis's thread saturated | ≥ 20 000/s | 39 505/s (0.97 core) | 33 642/s (0.94 core) | **MET** — measured at saturation, not extrapolated |
| The primary's own p99 for the mutating `FCALL` at 20 000/s | ≤ 1 ms | ≤ 132 µs (mean 25.4 µs) | ≤ 132 µs (mean 25.6 µs) | **MET** |
| One scope (one slot) | ≥ 2 000/s | held 2 000/s; peaked 25 814/s | held 2 000/s; peaked 28 791/s | **MET** |
| A `Record` is one hot-tier call | — | 1.00 `FCALL` per record | 1.00 | — |
| **No operation charged twice** under load (SC-002) | 0 | 601 933 applied, 11 540 resent, **0 charged again** | 544 473 applied, 10 455 resent, **0** | **MET** — every scope's hot balance equals its grant plus its receipts |
| Durable writer, event granularity, batch 500, steady (10%–90% booked) | ≥ 5 000/s | 7 369/s (296 403 events) | 7 157/s (219 389 events) | **MET** |
| …the whole drain, including the worker's start-up | — | 6 233/s | 5 797/s | disclosed: 5.8 s of start-up is in it; a smaller 164 708-event drain averaged **4 986/s** whole |
| Books equal the hot tier after the drain; no key booked twice | — | all 1 024 scopes | all 1 024 scopes | **MET** |
| SC-001 **unloaded**, embedded (one caller): `Admit` p99 | ≤ 1 ms | 260 µs (p50 131 µs) | 246 µs (p50 132 µs) | **MET** — a *lower bound*; see below |
| SC-001 **unloaded**, co-located via `paymentd` gRPC: p99 | ≤ 5 ms | 1.02 ms (p50 209 µs) | 668 µs (p50 217 µs) | **MET** — a *lower bound* |
| SC-001 **at the target load**, embedded and co-located | ≤ 1 ms / ≤ 5 ms at 50 000/s | inconclusive / target not held | inconclusive / target not held | **NOT ASSESSED** — see below |

### What the machine showed, and why SC-001 at load is not claimed

Through this machine's Docker path the `Meter`'s `Admit` tops out at 46 169/s (run 1) and 58 734/s (run 2), against a **bare
`PING`** through the same client and port at 62 456/s and 66 270/s: the `Meter` reaches 74–89% of what a call that does nothing
can. The Redis thread was at 0.7–0.8 of one core and the generator at 1.2–2.1 of twelve, so **neither end was saturated — the
path between them was**, and `redis-benchmark` inside the container shows the primary itself does 2.7× the target.

At the 50 000/s rung the client-observed `Admit` p99 was 162 ms (run 1) and 373 ms (run 2), while a **bare `PING` over the same
path at the same rate** was 81 ms and 80 ms and the in-process loopback echo — no Docker, no Redis — was 0.09–0.24 ms at 10–40 k/s. A
no-op cannot meet 1 ms through this path at that load, so the criterion cannot be judged here: the harness skips it as
*inconclusive* and prints the numbers (`T130/SC-001-embedded INCONCLUSIVE`). For the co-located case, `paymentd`'s gRPC `Admit`
held 40 843/s of the 50 000/s asked in both runs (*not assessable*).

The unloaded rows are real but weaker than SC-001: they show what admission adds with no queue to hide behind (the `Meter` adds
roughly 0.1–0.3 ms over a bare round trip) and would catch a gross design miss; they say nothing about p99 at 50 000/s.

**To close SC-001:** run the same harness where the path is not a macOS Docker port-forward — Linux, Redis and the
load generator on one network — and the verdict logic will either pass or fail it. That has not been done, so SC-001 at the
target load is **unproven**, not met.

### Findings

1. **Ledger storage is about 569 bytes per event row, not 400.** The plan's rule of thumb said "≈ 400 bytes per event row
   including indexes — to be measured". Measured: 124 985 344 bytes across the ledger's partitions, indexes and TOAST for 219 389
   rows = **569 bytes/row**. At 1 000 events/s that is ≈ 49 GB/day, not "< 35 GB/day"; at 5 000/s ≈ 246 GB/day, not 170. The
   plan's storage table has been corrected to the measured figure.
2. **Redis forks and stalls do occur and are visible.** Redis's latency monitor attributed two forks (21 ms and 27 ms) in a
   development run, and in run 2 one 100 ms stall labelled `command` / `fast-command` (not a fork, not an AOF write) during a
   2 000/s step; the p99 of that step was 194 ms. It was not reproduced in run 1 or in the development runs, and the cause is not
   established (a VM pause on this machine is the likeliest). It is recorded as an unexplained observation, not as a finding
   about the design; the alert catalogue already pages on outbox age and on a shard frozen for more than a minute.
3. **The writer's whole-drain average is below its steady rate** by the worker's 5.8 s start-up (64 shards' ownership locks and
   connections). Harmless for a long-running worker; relevant to how long a restarted worker takes to catch up (see T133).

### What this does not show

* **Redis Cluster.** One standalone primary. "Scales by Redis Cluster primaries" is the design's claim and is not load-tested.
* **A production network or production hardware.** Redis and Postgres are single containers on a laptop; the writer's figure is a
  floor for that Postgres, not a statement about a dedicated primary.
* **`rollup` granularity at ≥ 50 000 events/s** and a columnar `UsageArchive`; **reconcile's cost** proportional to active scopes;
  **several workers** sharing a Postgres; the REST path (only gRPC was loaded through `paymentd`).
* **SC-001 at the target load** (above).

### Reproduce

```bash
make hardening HARDENING_RUN='^TestLoad'
```

The four tests (`TestLoadAdmitAgainstTheEnvelope`, `TestLoadRecordAgainstTheEnvelope`, `TestLoadTheWriterDrainsAgainstTheEnvelope`,
`TestLoadAdmitCoLocatedThroughPaymentd`) take ≈ 5 minutes together. Close other work first: a busy machine moves every number here.

---

## T131 — Chaos

Each scenario breaks something the design claims to survive **while callers are running**, and ends in the two checks the task
names. The shape is the same every time: a real Redis in a container (SIGKILLed, rolled back), the real `payment-worker` binary
booking into a real Postgres, and an embedded `Meter` driven by concurrent callers **that remember every key they ever sent**.
That memory is the oracle. It makes "no double charge" mean *the books hold exactly the keys that were sent, each once, and every
scope is its grant less exactly those charges*, instead of "the numbers look plausible". Where the contract allows a window of
loss (a rolled-back AOF) the test says so, measures it, and then has the callers retry, which is what the contract says converges
it. The world is small (48 scopes, 16 shards, a few hundred calls a second): these are correctness scenarios, not load.

| # | Scenario | What happened (final run) | The page the contract names |
|---|---|---|---|
| 1 | **Kill Redis** (SIGKILL, AOF fsynced every command) | 3 036 `Record` calls; 1 763 deferred to the journal; **0 errors** (invariant 16); every acknowledged key booked *without* any retry; retrying all 3 036: **0 applied, 3 036 replayed**; books exact | `metering_journal_pending > 0` true 6.4 s after the kill; the 1 157-entry backlog then drained in 5.4 s, and the gauge read 0 |
| 2 | **Restart from a truncated AOF** | 9 312 sent; **94 acknowledged-but-not-booked** (the loss window, 1.0%); all 16 shards recovered by the worker; after retry-everything 3 124 applied and 6 188 replayed; books exact; no key twice at any point | `metering_hot_regression_total` 0 → **40** |
| 3 | **Inject drift** (+100 on one scope, −250 on another, straight in Redis) | steady traffic: **zero findings** over 6 shards reconciled twice (SC-009); the injection: **exactly two findings** (a +100 and a −250 never net), healed on the hot side, **0 ledger rows written** | `metering_drift_findings_total{outcome="drift"}` = 2 |
| 4 | **Poison an intent** (the real hot function's real intent, refused by a trigger in the test's own database) | parked in `credit_outbox_dead` with a `poison` suspense entry; the 5 records sent *behind* it on the same scope booked; a malformed outbox entry parked whole (1) and blocked nothing; **hot = booked + open suspense** (−660) | `metering_suspense_open` = 1 |
| 5 | **Replay webhooks at volume** and **kill the processor mid-event**, together | 120 payments × 4 copies + 16 tampered = 496 deliveries from 24 concurrent senders, shuffled; the processor held *after minting and before acknowledging* (5 000 credits minted), SIGKILLed there; a replacement finished: **120 grants, 600 000 credits, exactly**; the inbox deduplicated to 120; 0 dead | `billing_webhook_verify_failed_total` counted the 16 tampered |
| 6 | **Fail over to a lagging replica** | **not re-run here.** The contract suite ([`ConsistencyContract`](../metering/contracts/consistency.go)) runs it at the writer level over a real replica; it has not been run against the binaries under load | — |

How scenario 5 reaches "mid-event" deterministically: a trigger in the test's database makes the write that marks an event processed
wait on an advisory lock the test owns, so the worker is *provably* between minting and acknowledging when it is killed. (Its
lease would lapse in two minutes; the test moves that clock with one `UPDATE`.) Scenario 4 uses a trigger for the same reason: forging
an intent would need a forged sequence number, which corrupts the scope it is meant to test.

### Findings

1. **FIXED — a page that never clears.** `metering_journal_pending` was refreshed only by a replay call that *claimed* entries.
   The writer settles an entry after the replay applied it, so the last count read was still above zero, and with nothing due
   afterwards it was never refreshed: after an outage in which everything drained the worker's gauge read **12 over a journal of
   9 474 settled entries**, and `MeteringDeferredUsageNotDraining` (`> 0` for 15 minutes) would page, and keep paging, while the
   runbook told the operator `billing.journal.tick` was not running. No money was affected. Found by scenario 1; fixed with a
   red/green pair on the commit history (`9ee3c58`, `cb72eac`) and pinned for concurrency by `5c399a1`.
2. **FIXED, BUT THE PREMISE WAS OVERSTATED — the journal claim read every due row to return 200, and that was never what made the drain
   slow.** `Journal.Due` asks for 200 rows `ORDER BY next_attempt_at, created_at`. A backlog forms in seconds and autovacuum's statistics lag
   it by up to a minute, so the planner believes the table nearly empty and chooses a Bitmap Heap Scan of every due row, a Sort, then a
   Limit: at a 12 776-entry backlog `Bitmap Heap Scan … actual rows=12581` (3 828 buffer hits to claim 200). With fresh statistics the
   existing index already gives an ordered scan, which is why a first version of the red test, written with an `ANALYZE`, *passed* on
   the unfixed code and was not committed. The claim now sends `set_config('enable_bitmapscan', 'off', true)` with its statement as one
   pgx batch (one round trip, one implicit transaction, so the setting ends with the claim): the same rows, order and lease, and at a
   comparable backlog the plan is `Index Scan … actual rows=226`. Two things I tried first did not survive measurement: a composite
   `(next_attempt_at, created_at)` index **does not fix it by itself** (with no statistics the planner still picks the bitmap plan), so no
   migration was made; and wrapping the claim in an explicit transaction made each claim *slower* (four round trips: 2.49 → 3.67 ms at a
   50 000-entry backlog), which the batch form removes.

   **What it did not do.** The cost it removes is small: claiming an entire backlog in isolation took **2–3.5 ms per claim at 5 000, 20 000
   and 50 000 entries, before and after**, within this machine's noise (`TestJournalClaimCostAgainstTheBacklog`). The 12.5k-backlog drain is
   **unchanged by the fix** (runs of the same test, before and after, ranged 22–41 s and 307–575 entries/s; the noise is larger than any
   difference), so it is limited by the per-entry replay and booking, **not by the claim**, and the `journal due: context deadline exceeded`
   of a heavier first run (≈ 48 000 deferred entries behind a 23 s outage) is *not* explained by the claim's cost: finding 3 is the better
   suspect, and was not re-run at that size. I had suggested the remedy was a migration and that the claim was the drain's cost; both were wrong.
   What remains is a plan shape whose worst case is now bounded. Red `cd3a2c8`, green `670d641`, `test(mutation)` `bf71b12` (the setting must
   not leak onto the pooled connection), refactor `7ddced8`.
3. **OBSERVED — a replay tick that begins during an outage can run for its whole budget.** With ≈ 2 000 deferred calls a second
   the tick that started during the outage ended 30 s later with `journal due: context deadline exceeded`, and a SIGTERM then waited
   for it (a running tick deliberately ignores cancellation and is bounded by `PAYMENT_SHUTDOWN_TIMEOUT`). I believe it is each failed
   entry being rescheduled in its own commit, but I did not measure that. It delays a graceful stop by up to the shutdown timeout and
   never loses anything.
4. **BY DESIGN, WORTH KNOWING — every Redis restart is a failover.** The writer compares the node's replication id on every batch, and a
   restarted Redis has a new one, so scenario 1 (nothing lost, AOF fsynced) still froze, drained, bumped and rebuilt **all 16 shards**.
   Safe, and invisible when idle, but during the freeze `Record` defers to the journal; how long that usage stays off the hot balance is the drain rate in [T133](#t133--recovery-rehearsals).

### What this does not show

* **A lagging-replica failover under load** (row 6) and **Redis Cluster**; one standalone Redis was killed and restarted.
* Load: these are small worlds. The load numbers are [T130](#t130--load-against-the-scale-envelope-and-sc-001).
* The **alerts themselves**. Each scenario checks the *condition* the alert's expression watches (a counter, a gauge, a suspense
  count) on the worker's `/metrics`; that the alert reaches a human is the operator's, and `deploy/alerts/alerts_test.go` holds the
  catalogue equal to the runbook.

---

## T132 — Security review

`docs/security.md` and the provider contract each end in a checklist. Most of their items are claims about a **deployment** — a secret
manager, TLS on Postgres, a backup restored for real — that nothing in a repository can verify. What can be tried was tried here,
against the real binaries and a real Redis, the way an attacker or a careless host would, and each attempt had to be **refused**,
not merely absent, with a positive control beside it so that a refusal means the guard worked and not that the route was missing.

### Executed

| Item | Tried | Result |
|---|---|---|
| **Redis ACL from outside** (`TestSecurityRedis…`) | 41 attempts over TCP from a `go-redis` client as six identities: no credential, the `default` user, a wrong password, the health probe, the deploy role, and the service role — reads, writes, `FCALL`, `XADD`, `FLUSHALL`, `KEYS`, `CONFIG`, `ACL`, `EVAL`, `FUNCTION LOAD/DELETE/FLUSH`, `REPLICAOF`, `SHUTDOWN`… | every one refused with the named error (`NOAUTH` / `WRONGPASS` / `NOPERM`); controls: the service role can `PING` and call the hot path, the probe can `PING` |
| A **record-only caller** attempting Grant, Transfer, an admin write, an admin operation, an admin read | with the credential, and with none | `403` with the credential, `401` without; the hot balance afterwards is the one legitimate grant |
| A **cross-realm scope** on admit, record, grant, balance | the full credential naming another realm | `403 realm_mismatch` each |
| **Webhook door**: tampered body, unsigned, garbage signature, signed an hour ago, signed with the wrong secret, unknown account, unknown provider, an 8 MiB body | one genuine delivery as control | all `4xx` (unknown account `404`); **the inbox holds exactly 1 row afterwards** — nothing persisted before verification |
| **No CORS** on the door | `OPTIONS` and `POST` with a foreign `Origin` | no `Access-Control-Allow-Origin` |
| A **direct SQL** change to configuration | `UPDATE limits …` as the database role | a `config.update` row in `audit_log`, actor `db:<role>` |
| `PAYMENT_SERVICE_AUTH=none` off loopback | start `paymentd` on `0.0.0.0` | refuses to start, naming the setting; control: starts on `127.0.0.1` |
| **Secrets** | `gitleaks detect` over this branch's own commits (`origin/main..HEAD`), and over all 698 commits of the clone | **none in this branch.** The clone reports 10: **7 `curl` auth-header lines in `README.md` and `quickstart.md` on `main`** (documentation examples), and **3 `generic-api-key` hits in test fixtures on other local branches**, none of them on `main`. (The working directory also holds a real, git-ignored, untracked `.env`; it was scanned by path and its contents were not read or printed.) |
| **Vulnerabilities** | `govulncheck ./...` | 14 affecting the code at the committed `go.mod`. **1 was a dependency**, `klauspost/compress v1.20.1` (`GO-2026-6865`, fixed in v1.20.2): **bumped to v1.20.2**, after which `govulncheck` reports none in any dependency. **The other 13 are the standard library of the local Go 1.27.1** (fixed in 1.27.2–1.27.4); CI resolves `go-version: '1.26'` to the latest 1.26.x patch, so they describe this machine's toolchain, not the repository |

### The checklists, item by item

| Checklist item | State |
|---|---|
| `PAYMENT_SERVICE_AUTH` is mtls/bearer; `none` only on loopback | executed above |
| Every credential realm-bound and privilege-scoped | executed above |
| Webhook routes: signature first, constant time, tolerance, size limit, no CORS, no auth middleware, no body rewrite | executed above; constant-time compare read at `events/sign.go`, `polar/webhook.go`, `auth/credentials.go` |
| Provider secrets in a secret manager; `gitleaks` green | `gitleaks` executed; the secret manager is the operator's |
| Balance Redis: `noeviction`, AOF, network-isolated, ACLs | ACLs and flags executed (`verify-deploy.sh` checks argv); **isolation was a finding, below**; "not shared with a cache" is the operator's |
| Outbound endpoints `https`; the SSRF guard refuses a private address | existing tests (`httpsend/sender_test.go`: loopback refused before any request, and a hostname resolving to loopback refused at connect) |
| Each provider account's `realms` narrow; secrets are references | the operator's; `billing/app/admin_test.go` holds that an account is accepted only if its secrets resolve |
| Postgres TLS, least-privilege role, **backups tested by an actual restore** | the operator's — **not verifiable here and not claimed** |
| `/metrics` and admin endpoints not publicly reachable | admin refused above; **`/metrics` is a finding, below** |
| Verification-failure and drift alerts reach a human | the conditions are asserted by T131; reaching a human is the operator's |
| No browser holds a `/v1/*` credential | by design (the UI takes a host-supplied transport); not testable in a repository |
| A tampered webhook, an unknown account, a cross-realm scope, a record-only caller attempting Grant/Transfer/admin write, a direct SQL config change | **each tried, above** |

### Findings

1. **FIXED — the compose stack published its data stores on every host interface.** `deploy/docker-compose.yml` mapped Postgres
   (`5432`), Redis (`6379`) and NATS (`4222`) as `"5432:5432"` and so on, i.e. `0.0.0.0`, beside credentials this repository
   documents (`dev-payment`, …). The checklist asks for a network-isolated Redis. They are now bound to `127.0.0.1`
   (`make verify-deploy` after the change: 36 passed, 0 failed; its first attempt failed at "image builds" while the machine was loaded, and the identical build succeeded by hand and on the re-run). A host process on another machine reaches them by its own network path, not through this stack.
2. **OPEN — `paymentd` serves `/metrics` on the same listener as the API, with no credential** (`200` with none). The checklist says
   "not publicly reachable", and the engine does not enforce it: keeping the listener off the public network, or fronting `/metrics`
   separately, is entirely the operator's. A second, internal listener would make it a property of the binary.
3. **OBSERVED — a realm with a rate card and no balance ceiling admits an unfunded scope** (see [T134](#t134--adoption-walkthroughs)).
   Not a vulnerability, but a new host that follows the guide as it stood was unmetered without knowing.

### What this does not show

A human reviewer's reading of the design; anything about a real deployment (TLS, secret storage, backup restores, network policy,
who holds which credential); and the SSRF guard and constant-time comparisons are cited, not re-executed here.

---

## T133 — Recovery rehearsals

SC-008 says shards are rebuilt from the books before serving resumes "within the host's stated RTO". **The repository states no
RTO — it is the host's number** — so these rehearsals *time* recovery and assert that it is *correct*; whether the time fits is for the
host to read off the lines below.

| Rehearsal | Result (final run) |
|---|---|
| **An operator's `recover` of the busiest shard, under traffic** (`POST /v1/admin/shards/{n}/recover`) | asked → succeeded in **1.29 s** (shard 2, 4 of the 48 scopes on it), generation 1 → 2, **0 errors** over 1 661 calls, **3 of them landed in the freeze and were deferred** (invariant 16), retry-everything **0 applied / 1 661 replayed**, books exact. *(In an earlier run of the same test none landed in the freeze, so whether a call lands there depends on timing.)* |
| **Total loss of the hot tier** — `FLUSHALL` and `FUNCTION FLUSH`, then the functions reinstalled as `payment-migrate functions` does | functions installed in 1 ms; **every shard open 55 ms** after the new Redis answered; **all 256 scopes serving 436 ms** after (first call p50 1.4 ms, p99 2.3 ms, max 2.4 ms), each rebuilt lazily from the books; every rebuilt balance equals the books. The rebuild is per scope and lazy, so it grows with the number of scopes; only 256 were timed |
| …and the callers who then retry everything | **all 1 456 keys were charged again in the hot tier** (it lost its idempotency guards), told `Applied`, refused by the books on their durable key, and put right by the writer's corrections — the documented late replay. The end state is exact once the corrections drain; until then the hot balance is *lower* than the truth by those duplicates, which is the safe direction |
| **A 12 556-entry journal backlog drained after a 13.9 s outage** | **33 s to empty**: 378/s overall, **328/s in the first half, 446/s in the second**; 14 477 keys retried, **0 applied**; the gauge cleared. Runs of this test on this machine ranged 22–41 s (307–575/s). This is the host's *exposure window*: usage still in the journal is not on the hot balance, so the hot tier over-admits by that much until it drains. The rate is limited by per-entry replay and booking, **not by the journal claim** ([finding 2](#findings)) |

Two things the rehearsal corrected in the runbook: a total loss does **not** open shards at "a new generation" — a shard whose
`meta` key is missing is reopened at the generation the books record, and nothing is bumped (an empty Redis holds no account or intent
of an older generation to tell apart); and the late-replay figure to watch after a rebuild is `metering_late_replay_total`.
`docs/operations.md` now says so.

### What this does not show

* **The Postgres half of a region failover**: promoting a standby, repointing `PAYMENT_LEDGER_DSN`, re-pointing provider webhooks. It
  needs a streaming replica and a second failure domain; it was **not** rehearsed, so the DR runbook's RTO is still "written from
  reasoning" for that half.
* Recovery of a shard **under production-sized traffic or a production-sized ledger**: the rebuilds here are over hundreds of scopes
  and tens of thousands of rows.

---

## T134 — Adoption walkthroughs

[integration-guide.md](./integration-guide.md) says a host adopts the engine with "a scope binding, a rate card and a realm's
configuration rows" and nothing under `metering/` (SC-005). A guide is only as true as the last time someone did what it says, so
each walkthrough below is a *new product doing exactly that, from an empty realm*, written as a test that CI compiles and runs
(`integration/adoption_test.go`). The second one reads the guide's own `curl` body out of the guide, so if the guide is wrong again
that test is what fails.

* **Embedded Go host** (`acme-crm`): rows for a rate card and a balance ceiling; `payment-worker` as the durable writer; the wiring
  from the guide; then an unfunded scope refused, a grant, an exactly-priced record (3 enrichments + 2 000 calls = **6 300** credits),
  a replay that does not apply twice, a changed request under the same key refused, and the worker booking the grant and the charge
  under two keys. **Passes.**
* **Non-Go host** (`adoption_host.py`, Python standard library only, a bearer credential and REST): the realm starts migrated and
  *empty*; the host's first act is the guide's `POST /v1/admin/rate-cards`; then nine checks (an unfunded scope refused with `402`,
  a grant, an admit, a record priced exactly, the same charge replayed not applied twice, the balance moved once, the same key with a
  different request `409`, an unpriced unit `422` fail-closed, an unaffordable `MaxCost` `402`). **All nine pass** once the guide is right.

What running the guide revealed, and what was changed:

1. **The guide's own rate-card `curl`, sent exactly as written, was refused: `400 invalid_request: actor is required`.** Every admin
   write names a person and a reason; the guide (and the runbook's repricing and ceiling examples) omitted both. Fixed in both.
2. **A realm with a rate card and no balance ceiling admits an unfunded scope** — measured: `200 {"allowed":true,"headroom":0}`. The
   guide never said a host must configure the `scope_balance` ceiling, so a host that followed it was **unmetered without knowing**.
   The guide now says so and shows the `PUT /v1/admin/limits/scope_balance` call. (Whether an engine should fail closed on a realm
   with no balance ceiling at all is a design question this finding raises and does not answer.)
3. **The guide's "Library" snippet named constructors that do not exist** (`redisstore.New`, `pgstore.NewLimitStore`,
   `table.Pricer{}`, `pricing.Registry`…). Replaced by the wiring the embedded test compiles and runs, with the two things the snippet
   hid: `Shards` must equal the worker's `PAYMENT_SHARDS`, and the durable writer is its own process either way.
4. The runbook's DR step claimed a new generation on total loss; corrected ([T133](#t133--recovery-rehearsals)).

### What this does not show

* **"Containerised" is not literal.** The non-Go host ran against the **built binaries** (`paymentd`, `payment-worker`) started as
  processes, the same code the image builds, not against the container image from `make up`. The image path is what
  `make verify-deploy` exercises (migrate, ACL, non-root) without a host on the other end.
* Provider checkout, webhooks and a live card are not part of either walkthrough ([sandbox.md](./sandbox.md)).
* One product and one language each; the guide's section 7 checklist beyond what the Python host checks is not re-run.

---

## T135 — Mutation over the integration tier

**Not done.** The task asks for a nightly, Docker-backed mutation run over the integration tier, survivors classified, and the `metering/app`
mutant-coverage floor in `scripts/mutation.sh` raised to what that run measures. Two inputs were measured to decide whether that is possible
as specified; the answer is no, and nothing below is a mutation result.

**Measured inputs**

| Input | Value | How |
|---|---|---|
| Mutants in `metering/app` | **674**, of which **423 are reachable by the unit tier and 251 are not** | `gremlins unleash --dry-run ./metering/app` (v0.6.0, the operators `scripts/mutation.sh` uses): *Runnable 423, Not covered 251* |
| The integration tier, whole | **139–177 s** wall time over two runs (the `integration` package alone: **76–104 s**) | `go test -race -count=1 -shuffle=on -p 2 -tags=integration ./...` with `GOMAXPROCS=4`, 54 packages `ok` both times, on one laptop, load average peaking at 21 |

The 251 uncovered mutants are the point: booking, reconcile, recover and the writer are exercised only by real Redis and Postgres, which is
why the app floor is 30 (`scripts/mutation.sh`).

**What that implies**, as arithmetic on those two numbers and gremlins' documented `--integration` behaviour ("run the complete test suite for
each mutation"), not as a run: 251 mutants × 139–177 s is **≈ 10–12 hours** sequentially, and ≈ 5–7 hours even if only the `integration` package were
run each time. More workers do not rescue it: a single suite at `-p 2` already took the machine to a load of 21, and every worker needs its own
containers. GitHub's hosted-runner job limit is six hours. So a nightly job as specified cannot be expected to finish (the lower bound, 5 hours,
is for running only one package and leaves no margin).

**What could work, not built**: choose, for each mutant, only the tests that execute its line (a coverage profile per test with
`-coverpkg=./metering/app`), so a mutant costs one or two tests (1–30 s each in the runs above, container start included) and not the suite.
That is hours, not minutes, and an estimate; it needs a tool that does not exist here. A sampled weekly run is the cheaper alternative.
Either is a decision for the maintainers, and until one is made the floor stays at 30 and no survivor has been classified.

### What this does not show

Any mutation result for the integration tier: no mutant was run against it, no survivor classified, no floor raised.
