# intel-payment Constitution

**Version**: 1.0.0 | **Ratified**: 2026-09-27

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

### III. One Durable Writer

Exactly one component writes the account of record. Everything else publishes intents. A second
money-writer is not a design variation; it is the bug this principle exists to prevent, and it
usually arrives disguised as a convenience.

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

The conformance suite precedes the implementation. Every invariant that can be tested is, in a
test named for it. The failure paths — replay, concurrency, cancellation, tampering, outage,
drift, refund past spend — get more attention than the happy path, because the happy path is the
one that gets exercised in development anyway.

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

Before merge: `make ci` green (build, tests, conformance suites, lint, architecture graph,
portability check, vulnerability scan); migrations apply forward on a real PostgreSQL; contracts
updated in the same commit; the verification output pasted, not summarized.

Before release: load test against the admission latency criterion; chaos exercises (hot-store
loss, webhook storm, injected drift, poison message); both security checklists; a rehearsed
recovery with a measured time.

## Amendments

A change to these principles requires a documented rationale, a version bump, and a pass over the
contracts for anything it invalidates. Principles marked NON-NEGOTIABLE may be clarified but not
relaxed.
