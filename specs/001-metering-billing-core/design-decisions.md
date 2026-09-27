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
