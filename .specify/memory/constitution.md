# intel-payment Constitution

**Version**: 1.2.0 | **Ratified**: 2026-09-27 | **Amended**: 2026-09-30

This project moves money. A bug here is not a degraded experience — it is a customer charged
twice, a customer charged for nothing, or revenue quietly lost. These principles exist because
each one, violated, produces one of those outcomes.

## Core Principles

### I. Money Integrity (NON-NEGOTIABLE)

No floating-point value may touch a balance, a price, a ledger row, a log line about money, or a
test fixture for one. Internal amounts are signed 64-bit integers; fiat is integer minor units
plus an ISO-4217 currency. Rounding, where unavoidable, rounds **up** so it never silently
under-bills. Money types are distinct from each other — credits are not money and the compiler
must know it.

### II. Exactly-Once by Construction (NON-NEGOTIABLE)

Every credit-affecting operation carries an idempotency key and is guarded by a **durable unique
constraint**. A cache, a lock, or a distributed-lock library is a performance optimization and is
never the guarantee. Retries, double-clicks, redelivered messages and replayed webhooks must
converge on one effect, and every such path has a test that delivers the same operation twice —
and once concurrently.

A key reused for a **different** request is an error, never a silent replay. Where a durable guard
is bounded in time (metered usage), every operation older than the bound is **refused**, so the
guarantee holds for all accepted operations rather than for most of them. Operations that mint or
move money are guarded without a bound.

### III. One Durable Writer

Exactly one component writes the account of record. Everything else publishes intents. A second
money-writer is not a design variation; it is the bug this principle exists to prevent, and it
usually arrives disguised as a convenience — a transfer "settled directly for atomicity", a
reconcile that "books a correcting row", an admin script that "just fixes the balance".

### IV. Fail Closed on Money, Fail Loud on Drift

Unknown rate key, unknown unit, unknown capability, unreachable store: **deny**. Never price at
zero, never grant by default, never assume. When the system detects it disagrees with itself
beyond tolerance, it **alarms** — it does not silently heal. A discrepancy absorbed without a
signal is a discrepancy nobody will ever investigate.

### V. Refuse Before You Spend

The admission gate runs before expensive work, not after. A system that discovers it cannot afford
something once the money is gone has no gate — it has a receipt.

### VI. Clean Architecture, Machine-Enforced

`domain → ports → app → adapters`, one direction. `domain` imports nothing. The engine never
depends on the fiat or entitlement layers. This is enforced by `go-arch-lint` and `depguard` in
CI, because a boundary maintained by discipline is a boundary that erodes at the first deadline.

### VII. Contract-First

Every surface is specified in `specs/*/contracts/` before it is built, with its invariants and a
conformance suite. The suite runs against **every** implementation of a port, so "this is generic"
is a test result rather than an assertion. A contract change and its code land in the same commit.

### VIII. Configuration Over Code

Anything a host might reasonably want to change without a deploy — prices, ceilings and how many
there are, plans, entitlements, policies — is data. A host that must fork or wait on a release to
change a price has not adopted a reusable system.

### IX. Host-Agnostic by Default

No host's tenancy model, unit, vocabulary or UI framework may leak into a core. The billing
subject is opaque; the unit is opaque; the ceiling count is data. Any behaviour conditioned on a
specific host's string is a defect.

### X. Test-Driven, Especially for Failure

A test drives the code, in this order: a failing test, the least code that passes it, then
cleanup. The conformance suite precedes the implementation; for every other change a test that has
been **seen to fail** precedes the code it tests, because a test that has never failed has not been
shown to test anything.

- **Red is evidence, and it is checked.** A change to production code arrives as a tests-only commit
  that fails for the reason it states, followed by the change that makes it pass
  (`scripts/verify-red-green.sh`, CI job `red-green`). A change that genuinely cannot be test-driven
  says so, with a reason, as `TDD-Exempt:`. "Hard to test" is not a reason; it is a finding about
  the design.
- **Every invariant that can be tested is, in a test named for it.** The failure paths — replay,
  concurrency, cancellation, tampering, outage, drift, refund past spend — get more attention than
  the happy path, because the happy path is the one that gets exercised in development anyway.
- **Where examples are not enough, state a property.** Arithmetic that must never wrap, canonical
  encodings that decide whether a retry is a replay or a conflict, and time logic that decides when a
  limit's counter disappears each have a fuzz target, held to an independent reference. Its seed
  corpus is a regression test, and a failing input is committed with its fix.
- **Tests are themselves tested.** Mutation testing asks whether a test fails when the code is
  wrong. A survivor that is not an equivalent mutant is a missing test, and the thresholds only rise.

The practice, the commit shape and the tiers are in [docs/testing.md](../../docs/testing.md).

### XI. Verification Before Completion (NON-NEGOTIABLE)

No one — human or agent — claims work complete, fixed or passing without running the verification
and showing its actual output. "Should work" is not a status. A schema is verified by applying it,
not by reading it; a webhook flow is verified by delivering one, not by describing it.

### XII. Observable or It Did Not Happen

Every terminal state emits a named metric and lands in durable storage: drift, poison messages,
verification failures, fail-open events, uncapped admissions, pricing errors. Metric names are
part of the contract, because an operator writes alerts against them before ever reading the code.
"Logged and moved on" is not a permitted outcome for anything that touched money.

### XIII. Security Posture for a Money Service

Webhook signatures are verified on the raw body, in constant time, within a timestamp tolerance,
before any parse or side effect. Credit-minting is privilege-separated from credit-spending at
every boundary. Realms are authenticated, never asserted. No card data is ever received. Provider
secrets are server-side only.

## Quality Gates

Before merge: `make ci` green (build, tests, conformance suites, fuzz, lint, architecture graph,
portability check, vulnerability scan, the reference hot path on Redis in cluster mode); a failing
test-only commit ahead of every production change (`red-green`) and the lines the change touched
surviving mutation (`mutation-diff`); the schema verified on a real PostgreSQL by
`scripts/verify-schema.sh`; no document describing a retired mechanism
(`scripts/check-spec-drift.sh`); contracts updated in the same commit; the verification output
pasted, not summarized.

Before release: load test against the scale envelope; chaos exercises (hot-store loss, failover to
a lagging replica, truncated AOF, webhook storm, a processor killed mid-event, injected drift, poison
intent); both security checklists; a rehearsed recovery with a measured time.

## Amendments

A change to these principles requires a documented rationale, a version bump, and a pass over the
contracts for anything it invalidates. Principles marked NON-NEGOTIABLE may be clarified but not
relaxed.

| Version | Change | Rationale |
|---|---|---|
| 1.1.0 | II clarified: key reuse with a different request is an error; a time-bounded guard requires refusing operations older than its bound. III gained examples. Quality gates gained the design verifications | A permanent per-event guard cannot scale ([D32](../../specs/001-metering-billing-core/design-decisions.md)); bounding it without refusing stale operations would have *relaxed* II, so the refusal is part of the principle. The examples in III are the three second-writer bugs the second design review found or pre-empted |
| 1.2.0 | X expanded from "the suite precedes the implementation" to a checked practice: red before green with evidence, properties for arithmetic/encodings/time, and mutation testing of the tests. Quality gates gained `red-green`, fuzz and `mutation-diff` | A review of the task list found the order was asserted, not shown: suites and the code they verify landed in single commits, nothing recorded a red run, and the first fuzz and mutation runs found two defects in reviewed code (a reordered duplicate unit changed a request fingerprint; a daily counter in a zone that skipped a calendar date was given a negative lifetime) and a dozen money-path boundaries no test pinned. A rule only a reviewer can see is followed until the first deadline |
