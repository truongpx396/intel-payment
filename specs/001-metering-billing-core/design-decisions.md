# Design Decisions

Every decision the originating design left open, plus every refinement made during the lift.
Each entry states the decision, the alternative rejected, and why — so a future reader can
re-open one with the original reasoning in hand rather than guessing at it.

---

## Resolved from the originating design

### D1 — Ledger sign convention: one signed column
**Decided**: `credit_ledger.delta BIGINT`, signed. Negative is consumption, positive is a grant.
**Rejected**: separate `debit`/`credit` columns; a `credits_used` column with an implicit sign.
**Why**: the originating design listed this as open decision #1 while every part of the system
had already assumed signed — the internal type was a signed integer, the UI rendered `−42`
beside `+50,000`, and the balance was defined as `SUM(delta)`. A debit/credit pair would need
a new column for each new operation type direction and makes `SUM` a `CASE`. One signed column
means `purchase`, `refund`, `chargeback`, `reconcile` and `writeoff` all land in the same
column with no schema change. Renamed from `credits_used` because a column named "used" holding
a positive grant is a trap for the next person to read it.

### D2 — Credit expiry: configurable, off by default
**Decided**: `CreditExpiry` ∈ `off` | `lots_fifo`. When on, `credit_lots` tracks per-grant lots
and consumption draws oldest-first.
**Rejected**: picking one and shipping it.
**Why**: open decision #2. Hosts differ on this for commercial reasons, not technical ones —
a prepaid-pack product usually expires, a platform-credit product usually does not. Off by
default because expiry is the surprising behaviour, and a system that silently destroys
something a customer paid for should have been asked for explicitly.

### D3 — Billing anchor: any scope, no built-in hierarchy
**Decided**: the host picks the `Scope` it bills. The UI's `BillingAnchor` is *described* to the
component or absent.
**Rejected**: the originating resolution, which anchored billing to an organization above a
workspace.
**Why**: that resolution was correct **for that product** and is exactly the coupling this
system exists to remove — and the evidence was in the originating design itself, which had to
re-anchor four money tables from one tenant column to another and called it a migration. A
single-tenant host has no level above its scope; a reseller has two. So the anchor is data.

---

> **Note on attribution.** The originating design listed **five** open decisions. D1–D3 below are
> the first three. D9 and D11 resolve #4 (tax/invoicing) and #5 (proration & mid-cycle plan
> changes) — they are grouped with the refinements because of what they add (explicit config), but
> the *questions* were the originating design's, not new findings here.

## Refinements made during the lift

### D4 — `Realm` as the outermost isolation axis
**Added**: `Scope{Realm, Kind, ID}`; realm inside `Tag()`; `UNIQUE(realm, idem_key)`.
**Why**: the originating design hinted at multi-product deployments ("the service holds each
product's rate card keyed by `rate_key`") without an isolation axis to make it safe. Without
`Realm`, two hosts sharing a deployment share a plan catalogue, a limit set, and — the real
bug — an idempotency key space, so host B's `invoice-1` is silently swallowed as a replay of
host A's. A single-product deployment leaves it at `default` and pays nothing.
**Cost**: one field on the scope; one column on the money tables.

### D5 — `AdmitRequest.MaxCost`
**Added**: an optional cost upper bound at the gate; `Admission.Headroom` in the reply.
**Why**: the strongest correctness refinement here. The original `Admit(scope, limits...)`
could only answer "are you already at a ceiling?" — so a scope with 1 credit left admitted an
arbitrarily expensive call. The design meanwhile asserted a bound of "in-flight concurrency ×
per-call ceiling", a quantity the gate never received. `MaxCost` supplies it, making the stated
bound enforced rather than aspirational. `Headroom` lets a caller resize work instead of
starting work it cannot finish.
**Compatibility**: optional. `RequireMaxCost` turns it into a precondition once every caller
supplies it, and `metering_admit_uncapped_total` shows who has not.

### D6 — `rate_card_version` on every ledger row
**Added**: `Price.RateCardVersion`, persisted.
**Why**: the design required pricing to be pure and replayable, then stored no reference to
*which* card priced a row. A dispute months later would re-derive with today's rates and
disagree with the charge it was meant to verify — so "auditable" did not hold. One column
closes it.

### D7 — `NegativeBalancePolicy`
**Added**: `allow_debt` | `clamp_to_zero` (default) | `block_and_flag`, with a mandatory
compensating `writeoff` row when flooring.
**Why**: a refund for credits already consumed drives the balance negative. There is no
arithmetic that avoids it, and the original design had no answer, which in practice means each
implementer invents one. The compensating row matters as much as the policy: flooring silently
breaks `balance == SUM(ledger)`, and the next reconcile then pages for drift the system created
itself.

### D8 — `Entitler` port
**Added**: a whole contract ([entitlement-ports.md](./contracts/entitlement-ports.md)).
**Why**: the biggest functional gap for "support any other project". A plan's value is rarely
only credits — it is seats, SSO, retention, support tier. With no port, every host re-derives
them from plan codes at call sites (`if plan == "pro"`), which scatters the rule, makes
launching a plan a code change in every branch, and lets the UI's idea of a plan drift from the
server's.

### D9 — `TaxStrategy` named explicitly *(resolves originating open decision #4)*
**Decided**: `provider_managed` (default) | `external_hook` | `none`. No compute-it-here mode.
**Rejected**: issuing our own invoices and computing our own tax.
**Why**: the originating design's open decision #4 framed this exactly — *"rely on
provider-hosted invoices/tax (Stripe Tax / Polar Merchant-of-Record / PayPal) vs. issuing own
invoices; MoR (Polar) materially reduces tax-compliance scope."* That framing is right, and
`provider_managed` is its first option. What this adds is making the choice **explicit
configuration** rather than a deployment-time assumption: an unnamed tax strategy produces
systems where the answer to "who computes tax?" is nobody. Omitting a compute mode is
deliberate — rate tables and filing obligations are a specialist product, and a half-built one
is a liability rather than a feature. A merchant-of-record provider remains the lowest-scope
option and `provider_managed` covers it.

### D10 — Dunning grace window
**Added**: `Sub.GraceUntil`, `DunningGraceDays`, `DunningSuspendAfterDays`, a sweep tick, and
entitlements that hold through grace.
**Why**: the original notified on a failed payment but never said what happened to access. The
default behaviour of "revoke on decline" cuts off a paying customer over a transient card
failure, and they cannot diagnose it.

### D11 — `PlanChangePolicy` *(resolves originating open decision #5)*
**Decided**: `at_period_end` (default) | `immediate_prorate` | `immediate_no_prorate`.
**Why**: the originating design's open decision #5 was *"defer to provider proration, or block
plan changes to period boundaries."* Both are legitimate and hosts differ, so this is config
rather than a single answer: `at_period_end` is the "block to boundaries" option and the default
because it needs no proration math at all; `immediate_prorate` is the "defer to provider" option.
Proration is delegated to the provider either way, since it already owns the invoice.

### D12 — Multi-currency prices per plan
**Added**: `plan_prices(plan_id, currency, minor_units)`; `plan_provider_prices` keyed by
currency too.
**Why**: one price per plan forces a host selling in three currencies to maintain three plans
and three credit allotments, which then drift.

### D13 — Rate cards and limits as configuration data
**Changed**: from injected Go values to per-realm `rate_cards` / `rate_card_entries` / `limits`
rows, loaded and cached.
**Why**: the original injected a rate card at wire time, which makes a price change a redeploy.
A host that does not control this service's release cycle cannot reprice at all. Cards stay
immutable and versioned; a change inserts a new version.

### D14 — `409` for a spent window, `402` for an exhausted balance
**Changed**: from `429` to `409 limit_reached` for windowed ceilings, with a compatibility flag.
**Why**: `429` means "you are calling too fast" and invites retry-with-backoff. A spent daily
budget is not a rate problem, and retrying will not fix it until the window rolls. The split
lets a client act correctly without inspecting a body: back off on `429`, show the reset time on
`409`, offer an upgrade on `402`.

### D15 — Observability as contract
**Added**: named metrics, `/healthz`, `/readyz`, and a `/readyz` that fails on a behind-head
schema.
**Why**: the original required alarms on drift without naming what to alert on, so every
operator invents metric names and none of them match the runbook. `/readyz` gating on migrations
prevents a rollout serving money against a half-migrated schema.

### D16 — Subscription drift sweep
**Added**: `billing.subdrift.tick` calling `FetchSubscription`.
**Why**: webhooks are the source of truth and webhooks get missed. Without a sweep, a dropped
`subscription.deleted` leaves a cancelled customer entitled indefinitely, and nothing detects it.

### D17 — `Ignored` as a first-class event outcome
**Added**: `EventType = "ignored"` plus an unknown-customer path that returns `200`.
**Why**: providers deliver dozens of event types nobody subscribed to, and provider accounts
carry customers a given deployment never created. Treating either as an error turns normal
traffic into alert noise, and alert noise is how a real webhook failure gets missed.

### D18 — Normalized `EventObject`
**Added**: adapters normalize into a provider-neutral object before dispatch.
**Why**: the original dispatched on provider-shaped payloads, so the dispatch logic would be
written three times and diverge. Normalizing makes one dispatcher serve every provider and makes
the ingress conformance suite provider-independent.

### D19 — Framework-free UI ports
**Changed**: `ports.ts` and `model.ts` carry no framework import; only components bind to React.
**Why**: the original assumed the host's React app. A payments service whose only UI is React
excludes hosts that are not, and the derivation logic is the reusable part.

### D20 — Member-scoped spend breakdown
**Added**: the breakdown is self-only unless the caller holds an admin entitlement.
**Why**: the original noted the data was admin-only today and a member view was needed, without
saying the member view must be *narrower*. A credits screen that shows colleagues' usage is a
privacy leak wearing a billing label.

### D21 — The idempotency guard is its own non-partitioned table
> **Refined by [D32](#d32--two-idempotency-guards-two-lifetimes-and-fingerprints):** `credit_idem` is now keyed `(realm, op, idem_key)` and guards minting operations; metered usage has its own window-bounded `usage_idem`.

**Changed**: from `credit_ledger UNIQUE (idem_key)` on a table partitioned by `created_at`, to a
dedicated `credit_idem PRIMARY KEY (realm, idem_key)` written in the same transaction as the
ledger row.
**Why**: the inherited design specified a ledger that was both **partitioned by `created_at`**
and carried a **global `UNIQUE (idem_key)`**. PostgreSQL forbids that combination — a unique
constraint on a partitioned table must include every partition key column — so the schema as
written does not create:

```
ERROR:  unique constraint on partitioned table must include all partitioning columns
DETAIL:  UNIQUE constraint on table "credit_ledger" lacks column "created_at"
         which is part of the partition key.
```

This was found by applying the migrations rather than by reading them, and it mattered because
that one index is the backstop for every exactly-once guarantee in the system (SC-002). The two
obvious compromises are both bad: dropping the partitioning loses time-based retention on the
largest table, and scoping uniqueness per partition means the same `idem_key` can be applied once
per month.

Splitting them keeps both properties and is better than either:

- the guard is a **narrow, hot, fully-cached** table — cheaper to check than a wide partitioned index;
- it **outlives ledger partitions**, so dropping an aged partition cannot resurrect the ability to double-charge an old key;
- `delta` on the guard row lets a replay report the original outcome with no partition scan.

The write becomes: insert the guard row `ON CONFLICT DO NOTHING`; no row returned *is* the replay
signal; otherwise insert the ledger row — one transaction, so a crash between them is impossible.


### D22 — `Window: Job` — a cumulative budget for one unit of work
> **Amended by [D30](#d30--subjects-and-ceilings-sized-by-the-plan):** the job is identified by `Subjects["job"]`, not `Limit.Scope`. The claim below that the UI and engine "enumerate the same five" windows was wrong: the UI's is a view vocabulary with an explicit mapping, documented in credits-ui-ports.md.

**Added**: a fifth `Window` value, keyed by an ephemeral `Limit.Scope` (e.g. `{Kind:"run", ID:…}`)
and expiring after `Limit.Dur`. Mirrored in the UI as `window: "job"`.
**Why**: the originating product had this and the extraction initially dropped it. Its agent-run
table carried `credits_cap INT NOT NULL -- hard per-run ceiling (NOT just the daily budget)`, with
the loop checking spend after each step, and the stated reason was exact: *"this bounds a runaway
agent loop independently of the daily budget — the daily budget alone would still permit one task
to burn a large bill."*

None of the four original windows express that. `Balance` is the account, the calendar windows
reset on a clock, and `AdmitRequest.MaxCost` ([D5](#d5--admitrequestmaxcost)) bounds **one call**
rather than a run's accumulated spend across many. So a long-horizon job had no engine-level
ceiling, and the single most expensive failure mode in an agentic product — a loop that does not
terminate — was left to each host to re-solve.

It is a **counter, not a balance**: no credits are granted to the job, no ledger rows are written
for it, leftover budget needs no return, and an abandoned job leaks only a TTL'd key. That is why
it is a `Window` and not a `Scope` with its own balance, which would have doubled the ledger
volume for every multi-step job.

This also fixes an asymmetry carried over from the originating design: its UI already had a
`"call"` window that its engine's `Window` enum did not, so the two halves disagreed about what a
ceiling could be. Both now enumerate the same five.

### D23 — `Transfer` — the pool → allocation primitive
**Added**: `Transfer{From, To, Amount, IdemKey, Reason, MaxDestBalance}` on the `Ledger`/`Meter`
ports, the paired `allocation_out`/`allocation_in` operation types, `counter_scope_*` columns, and
invariant 14 (a transfer conserves the realm).
**Why**: also present in the originating design and initially dropped. Its tenancy section
specified *"`organization_credits` becomes the purchased pool; `workspace_credits` becomes an
**allocation drawn from it** with an optional per-workspace cap"* — a parent scope buys once, child
scopes draw from it.

The extraction had modelled only the *presentation* of this (the UI's `BillingAnchor`, which
describes a parent and links to it) and left the mechanism to the host. A host would then implement
it as two independent `Grant`s — which is **not equivalent**, because a crash between them either
destroys credits at the source or mints them at the destination, and no reconcile can tell which
happened. Money that exists in two places needs one atomic operation, not two.

`MaxDestBalance` covers the "optional per-workspace cap" from the same sentence: a ceiling on what
a destination may *hold*, which is distinct from a `Limit`'s ceiling on what it may *spend*, and is
what stops an errant admin action handing one team the entire pool.

Invariant 14 is what makes it auditable: the pair shares one `IdemKey` and sums to zero, so a
half-applied allocation is detectable rather than silent. Verified against PostgreSQL 16.

---

## Decisions from the design review

Prompted by a direct question — *is this genuinely production-grade and general-purpose?* — and the
honest answer was partly. These record what that review changed.

### D24 — `Transfer` settles in Postgres, not on the Redis fast path
> **Superseded by [D33](#d33--transfer-is-two-phase-through-the-hot-tier-supersedes-d24).** Settling in Postgres from the request tier made a request handler a ledger writer, and checked sufficiency against a balance that lags the hot one. The lesson below still stands.

**Changed**: from "one Lua script over both scopes' keys" to a Postgres transaction writing the
paired rows and the guard, followed by a best-effort hot-balance refresh.
**Why**: [D23](#d23--transfer--the-pool--allocation-primitive) as first written was **not
implementable on Redis Cluster**. A Lua script may not touch keys in different hash slots, and two
scopes' keys are in different slots by construction. The escape hatch I reached for — force them
together with a shared `{realm}` hash tag — would put every scope in a realm on one slot, destroying
the sharding the entire design depends on. So the original wording papered over a contradiction
rather than resolving it.

Postgres-first resolves it properly, and the trade is favourable: a transfer is an administrative
operation measured per-day, not per-request, so one round trip buys real atomicity on the path where
atomicity is the whole point. The hot-balance refresh afterwards is advisory — the ledger is
authoritative, so a lost refresh costs a reconcile rather than money.

The general lesson, worth keeping: **the fast path is for operations that are frequent, not for
operations that are important.**

### D25 — Redis Streams is the default bus; NATS JetStream is the swap
> **Made true by [D29](#d29--keys-are-hash-tagged-by-shard-not-by-scope):** the argument below — XADD inside the same atomic script as the balance change — holds on Redis Cluster only once the outbox stream shares the account's hash slot. JetStream, when chosen, sits behind the Redis outbox via a relay (bus-subjects.md).

**Changed**: from JetStream-as-reference to Redis Streams as the default adapter, both behind the
same `Bus` port with one conformance suite.
**Why**: two reasons, and the second is the stronger one.

1. **Required infrastructure drops to Redis + Postgres.** A broker was the third system you had to
   stand up before taking a single payment, and "quick setup" was not true with it in the list.
2. **It removes a failure class, not just a dependency.** With a broker, the hot path writes an
   outbox entry and *something else* publishes it later — so there is a window in which an intent
   exists in the outbox but not on the bus, and a crash inside that window is a lost charge. With a
   stream outbox, `XADD` runs **inside the same atomic script as `DECRBY`**, so the intent is durable
   on the bus the instant the balance moves. The old "drain the outbox, then publish" two-step
   collapses into one, and there is no translation layer between them to get wrong.

Streams provide everything the design needed from a broker: durable entries, consumer groups with
one-delivery-per-group, `XAUTOCLAIM` redelivery for a dead consumer, and `XPENDING` for the depth and
age metrics the runbook depends on.

**What JetStream still wins:** quorum (R3/R5) replication and cross-region mirroring, both
synchronous. Redis replication is asynchronous, so for `journal`-settlement deployments that want the
bus as durable as the journal, JetStream is the right answer. It stays a one-line swap.

**The cost, stated plainly:** Redis Streams have no automatic retention. Nothing reclaims stream
memory, and this instance must be `noeviction`, so an untrimmed stream fills memory and then **stops
the hot path** — `XADD` fails inside the script that moves the balance. Hence FR-047, the
`billing.trim.tick`, a stream-length metric, and a runbook entry. Trim never removes an un-acked
entry, so the floor can be generous.

### D26 — Post-paid invoicing is a separate feature, not a `Pricer` variant
**Decided**: [feature 002](../002-postpaid-invoicing/spec.md), additive, with its own `Rater` port.
**Rejected**: extending `Pricer` to handle tiers; bolting invoices onto the credit ledger.
**Why**: post-paid is the settlement model most B2B contracts use, and its absence was the largest
gap between what "billing service" implies and what feature 001 delivers. But it cannot be retrofitted
into `Pricer`, and the reason is structural rather than a matter of effort:

`Pricer` is pure over **one event**, and that purity is what makes reconciliation and dispute replay
possible. Tiered, graduated and volume pricing depend on **period-to-date volume** — pricing request
4,312,905 requires knowing it is the 4,312,905th. A `Pricer` that reads cumulative state is no longer
pure, and the moment it is not pure, every replay guarantee in feature 001 dies with it.

So `Rater` is a second port that is pure over **the set**: it runs once at period close over the
complete immutable event list, stays replayable for the same reason `Pricer` is, and never touches a
hot path. Both can serve one scope — `hybrid` settlement meters credits for real-time enforcement and
rates the period for an overage invoice.

The two also differ in what they produce. `Pricer` produces a number. `Rater` produces a **document**
a human reads and disputes — which is why 002 carries invariants `Pricer` never needed: immutability
after finalize, gapless numbering, line-level traceability back to ledger rows, and corrections only
by credit note.

### D27 — Forward-only migrations, by policy
**Decided**: no down migrations. Development resets the database; production fixes forward.
**Rejected**: shipping a `.down.sql` per migration.
**Why**: I had listed "no down migrations" as a gap, then, writing them, noticed every one began by
dropping a table that holds an account of record — `credit_ledger`, `credit_idem`, `payments`,
`audit_log`. Shipping those files asserts that dropping the ledger is a supported operation.

It is not, and a rollback that destroys the ledger is worse than the bad migration it undoes: the
migration is fixable, the ledger is not reconstructible. A backup restores a *copy*, not the rows a
live system wrote since.

Two conventions make forward-only safe rather than merely stubborn: forward migrations are
**additive** (new tables, nullable columns, new indexes — never a drop or a re-key of a money table),
which means the **previous binary still runs against the new schema**. So a deploy stays reversible
even when the schema is not: roll back the code, leave the schema, fix forward.
[002's schema](../002-postpaid-invoicing/data-model.md) follows this and drops nothing.

### D28 — The README states its own status and scope, prominently
**Changed**: a status banner above everything; Quick Start marked as the intended interface;
[ROADMAP.md](../../ROADMAP.md) naming the competing tools and the honest gap list.
**Why**: the README described a service — features, quick start, deployment shapes — for a repository
containing zero lines of Go, where `make up` cannot work because `cmd/` does not exist. That is an
overclaim regardless of intent, and a billing system is the worst category of software to overstate,
because the cost of adopting one for a job it cannot do is measured in someone's revenue.

ROADMAP.md also names **when to use something else** (Stripe Billing, Lago, Orb, OpenMeter, Kill
Bill). A reusable component that cannot say who should not use it is not being honest about its own
boundaries.

---

## Decisions from the second design review

Prompted by the same question a second time — *is this genuinely production-grade?* — asked of the
design after D24–D28. The answer was that the principles held but the **mechanism** did not: the
hot path could not run on Redis Cluster, reconcile could not tell in-flight charges from drift, the
webhook path could lose a paid grant, and the price format could not represent real prices. Each
entry below records one fix. Two of them supersede earlier decisions, and say so.

The fixes were verified the way D21 was found — by running them: the key layout and the reference
hot functions on a cluster-mode Redis ([`scripts/verify-hot-path.sh`](../../scripts/verify-hot-path.sh)),
and the schema, seed and Phase 2 draft on PostgreSQL 16 with behavioural assertions
([`scripts/verify-schema.sh`](../../scripts/verify-schema.sh)). Both run in CI.

### D29 — Keys are hash-tagged by shard, not by scope
**Changed**: every hot key of a scope — account, idempotency guard, window counters — **and the
outbox stream** carry the shard tag `{s<n>}`, where `shard = fnv1a64(Scope.Tag()) mod Shards`.
`Shards` defaults to 256 and is recorded in `hot_config`.
**Why**: the hot-path script touched `credit:{<tag>}:balance` and `billing:outbox:{<shard>}` — two
hash slots. Redis Cluster refuses that (`CROSSSLOT`, reproduced), so the design either did not run on
a cluster or capped the whole system at one Redis primary. D24 found this exact constraint for
`Transfer` and missed that the core debit, the `Job` counter and every per-subject counter had it
too. D25's central argument — the intent is written by the same atomic script that moves the balance
— is only true once all of those share a slot.
**Rejected**: one stream per scope (the writer cannot consume millions of streams); moving the
outbox XADD out of the script (reopens the window D25 closed).
**Cost**: a shard, not a scope, is the unit of placement, so one shard's scopes share a node's CPU;
256 shards spread over ~64 primaries before that matters. A subject ceiling is evaluated within its
charged scope — which is what every reference binding meant by it.

### D30 — Subjects, and ceilings sized by the plan
**Changed**: calls carry `Subjects` (`user`, `api_key`, `project`, `job`…); a `Limit` names the
`Subject` whose counter it is; a call missing a configured subject is refused (`missing_subject`).
A `limits` row may name a `max_entitlement` quota key, resolved through a `QuotaSource` port that the
entitlement module implements. `Charge.Job` became `Subjects["job"]`. A `Balance` limit's `Max` is
the permitted overdraft. Daily and hourly windows reset in the limit's `tz`.
**Why**: the seed defined `user_daily` keyed by `user`, but `AdmitRequest` and `Charge` carried one
`Scope` — there was no way to tell the engine which member a call belonged to, so the counter could
be neither keyed nor incremented. And one `max` per realm could not express "Free 100 a day, Pro
10 000", the first knob a customizable billing service is asked for.
**Rejected**: a parallel `plan_limits` + `limit_overrides` system — entitlements already have
per-plan values, a free tier, audited overrides, precedence, provenance and event-driven cache
invalidation. Metering reuses them through a port it owns, so it still imports nothing.

### D31 — Prices are rationals; the accounting unit is fine; `table` is the default pricer
**Changed**: `rate_card_entries` carries `credits_per_block` / `block_size`; the reference accounting
unit is 1 credit = 1 µ$ of list price; pricing is exact and rounds up **once per event**. A card
names its pricer; every deployment registers the data-driven `table` pricer; compiled pricers are
added to a `PricerRegistry` by name. `Pricer.Price` takes the card as an argument.
**Why**: an integer "µ$ per unit" cannot express any price below one µ$ per unit — $0.15 per million
tokens is 0.15 µ$ per token. The seed showed it: gpt-4o input at 2 instead of 2.5 (20 % under list),
gpt-4o-mini input at 1 instead of 0.15 (6.7× over). Rounding each event up to a whole 1 000-µ$ credit
then billed a ~15 µ$ call at 1 000 µ$: "never under-bill" had become "systematically over-bill small
events". Separately, a non-Go host — the container's whole audience — could not supply a compiled
`Pricer`, and one `Pricer` in `Deps` could not serve several realms.
**Rejected**: floating point (constitution I); carrying a sub-credit remainder per scope (makes a
row's delta depend on history, which kills replay).
**Cost**: `Credits` values are large numbers; the UI shows them through `UnitLabels.scale`.

### D32 — Two idempotency guards, two lifetimes, and fingerprints
**Changed**: `credit_idem (realm, op, idem_key)` — permanent — guards everything that mints or moves
money; `usage_idem (realm, scope, idem_key)` — daily partitions, `UsageIdemWindow` default 72 h —
guards consumption. Every guard stores a request fingerprint; a reused key with a different request
is `409 idempotency_conflict`. `Record` requires `OccurredAt` and refuses one older than the window.
A replay that outlives its hot guard is caught by the writer and reversed through suspense.
**Why**: `credit_idem` held one row per metered event forever, in one non-partitioned table — at
10 000 events/s that is 864 M rows a day, so "narrow, hot, fully cached" could not survive. A reused
key with a different payload returned `Applied:false`, turning a producer bug into silent
under-billing. And a usage key and a grant key shared one space.
**Rejected**: a time-bounded window with no stale-event refusal — a retry after the window would
charge twice, which is a relaxation of constitution II rather than a clarification of it.

### D33 — `Transfer` is two-phase through the hot tier *(supersedes D24)*
**Changed**: phase 1 is an atomic check-and-debit of the source's **hot** pool in its own slot,
emitting `transfer_out`; the writer books `allocation_out` and an `in_transit` record, credits the
destination through a writer-issued hot function, and books `allocation_in`. Conservation is
`Σ ledger + Σ in-transit`, invariant at every step.
**Why**: D24 settled transfers in Postgres from the request tier — a request handler writing the
ledger, which invariant 1 names explicitly as forbidden — and checked sufficiency against Postgres,
which lags the hot balance by the undrained outbox, so it could overdraw.
**Cost**: the destination is credited milliseconds later; the receipt says `in_transit`.

### D34 — The webhook inbox: persist, acknowledge, then process
**Changed**: ingress verifies, persists the normalized facts to `payment_events`, and answers `200`;
a processor leases events from the table, dispatches idempotently, and retries with backoff into a
`dead` state that pages.
**Why**: the old flow claimed the event (atomic insert) *before* granting, and answered a conflict
with `200`. A crash between the claim and the grant turned the provider's retry into a no-op — the
customer paid and received nothing — and nothing swept payments.
**Rejected**: a single transaction around claim + grant — the grant goes to Redis, not Postgres, so
no transaction spans both.

### D35 — Hosts hear about events through signed webhooks and a feed, never the bus
**Changed**: `outbound-events.md` — signed, retried webhooks per endpoint plus `GET /v1/events`, over
one event log. The internal bus is internal.
**Why**: the integration guide told hosts to subscribe to `billing.*` on the bus — which by default
is the balance Redis, where grant intents travel unsigned. Read access for a host usually means
write access, and write access there is the ability to mint credits.

### D36 — Provider accounts, and the account on the webhook route
**Changed**: `provider_accounts` rows (secret *references*, the realms each serves, live/test);
webhooks at `/webhooks/{provider}/{account}`; payments, customers and price mappings keyed by account;
no `CHECK provider IN (…)`.
**Why**: one set of provider secrets per deployment could not serve products on different provider
accounts, the route carried no realm, and adding a provider required a migration.

### D37 — Configuration is audited and signalled by the database
**Changed**: a trigger on every configuration table writes `audit_log` (actor from `intelpay.actor`,
or `db:<role>`) and `NOTIFY intelpay_config` in the same transaction; rate cards are insert-only by
trigger; an admin API is the supported write path.
**Why**: the runbook repriced with raw SQL, which bypassed the audit invariant and left every
in-process cache stale. An invariant that holds only when people use the right tool is a convention.

### D38 — A feature that is not built ships no schema; the pre-release baseline is editable
**Changed**: the Phase 2 migration moved to `specs/002-postpaid-invoicing/draft-migrations/`, verified
in CI on top of the baseline but never applied by `make migrate`. Migrations `0001`–`0005` were
corrected in place, and forward-only applies from the first release tag.
**Why**: under a forward-only policy, a table shipped before its feature exists can never be removed.
Nothing had ever been deployed — the implementation had not started — so stacking fix-up migrations
onto a schema no database has run would have added history without protecting anyone. The draft's
comment also promised an invoice immutability trigger that did not exist; it exists now and is tested.

### D39 — A per-scope sequence watermark; reconcile compares at equal sequence numbers *(supersedes FR-009's "compensating row")*
**Changed**: every hot mutation advances the scope's `seq` and stamps its intent; the writer books in
sequence order and records `applied_seq`; hot movements the books do not accept are **suspense**
entries. Reconcile compares `hot` with `booked + open suspense` only when the sequence numbers are
equal, heals the **hot** side by compare-and-set, and never books a ledger row. `ReconcileTolerance`
stays 0 — now meaningfully.
**Why**: reconcile compared the live hot balance with `SUM(ledger)` while charges sat in the outbox.
With the default tolerance of 0 every active scope paged every run, and the heal was specified two
contradictory ways: a compensating ledger row (which double-charges when the queued intent lands) or
healing Redis (which forgives the queued charges). The runbook read the drift direction backwards,
and `reconcile_runs` netted drift per shard so offsetting errors cancelled.
**Also corrected**: invariant 7. Redis persists and replicates a function's effects as one unit, so a
lost charge vanishes from the balance *and* the stream together — reconcile cannot see it, rather
than "healing upward". `HotAckWait` (`WAITAOF`) narrows that window; `journal` removes it.

### D40 — Recovery by shard generation: freeze, drain, bump, unfreeze
**Changed**: `hot_shards.gen`, mirrored in `meta:{s<n>}`; every account records the generation it was
built in; a restart or failover (detected by the node's replication id) or a sequence regression
freezes the shard, drains it, bumps the generation, and unfreezes; accounts rebuild lazily from the
books. Resharding is the same procedure over every shard.
**Why**: rehydrating from the ledger while intents were still queued dropped them; a rollback reissued
sequence numbers; and changing the shard count "orphaned queued entries" with no procedure at all.

### D41 — An outage defers usage to the journal; `fail_open` means served before metered
**Changed**: when the hot tier is unavailable, `Record` writes the event to `spend_journal` and
returns `Deferred`; a tick replays it through the hot function on recovery.
**Why**: `fail_open` promised "bounded overspend, healed by reconcile", but a charge that was never
recorded anywhere cannot be reconciled. `Record` during an outage was unspecified.

### D42 — Credit pools
**Changed**: `credit_pools` with a priority and an optional resource filter; balances, lots, grants,
refunds and transfers are per pool; `general` is implicit and drawn last.
**Why**: "promotional credits are spent before paid ones" and "these credits only work on GPU jobs"
are the first two requests any credit product receives, and neither is expressible by lot order.

### D43 — The payment lifecycle beyond "succeeded"
**Changed**: subscription state comes from `FetchSubscription` guarded by the provider timestamp
(`last_event_at`); disputes are a lifecycle (opened → funds withdrawn → reinstated/closed) with a
reversible chargeback; partial refunds revoke `ceil(granted × refunded / amount)` cumulatively; auto
top-up charges a saved method off-session, bounded per day, fulfilled only by the webhook.
**Why**: providers do not order deliveries, so an older event could overwrite a newer state; a won
dispute had no path back; the refund test conflated 500 cents with 500 credits; and auto top-up is the
feature that keeps credit products from interrupting paying customers.

### D44 — A stated scale envelope, and storage that stays bounded
**Changed**: [plan.md § Scale envelope](./plan.md#scale-envelope) states targets per Redis primary and
per Postgres primary; `LedgerGranularity=rollup` books one row per batch with per-event detail in a
`UsageArchive`; ledger checkpoints make partition archival safe; partitions are created ahead of
need; the `Rater` rates per-(unit, version) aggregates rather than loading a period's events.
**Why**: one Postgres row per event plus one permanent guard row, kept forever, is not a scaling
story, and "highly scalable" was asserted without a number.

### D45 — One home region, and a written disaster-recovery posture
**Changed**: a deployment has one home region; DR is Postgres replication to a standby region plus a
cold Redis rebuilt from the books; the RPO and RTO are stated in [operations.md](../../docs/operations.md#disaster-recovery-region-loss).
**Why**: a single scope's balance needs a single writer, so active-active across regions is a
different design (per-scope home regions), not a setting. Saying so beats leaving it implied.

### D46 — Contract drift fails CI; the design verifies itself
**Changed**: `scripts/check-spec-drift.sh` fails the build when a document describes a retired
mechanism as current; the schema and hot-path verifications run in CI without any Go code.
**Why**: the review found a dozen normative documents contradicting each other — list-based outbox
commands in the runbook, a quickstart whose SQL named a column that does not exist, tasks that
reintroduced constraints D21 had proved impossible. CI checked that links resolved, not that the
contracts agreed.
