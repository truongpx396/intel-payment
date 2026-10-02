---
paths:
  - "metering/domain/**"
  - "metering/ports/**"
  - "metering/app/**"
  - "metering/config.go"
---

# The metering core moves money

A bug here is a customer charged twice, charged for nothing, or revenue quietly lost. These are
[constitution](../../.specify/memory/constitution.md) principles I–VI and IX, restated where the code is.

**Money (I)**
- Amounts are signed `int64`; fiat is integer minor units plus an ISO-4217 currency. No `float` touches
  a balance, a price, a ledger row, or a log line about money. (The few existing floats are metric
  durations and the near-limit `WarnAt` fraction — do not add another.)
- Credits and money are distinct types; do not convert between them implicitly.
- Rounding rounds **up**, once per event, so the system never silently under-bills. Arithmetic that could
  wrap is refused, not wrapped.

**Exactly-once (II) and one writer (III)**
- Every credit-affecting operation carries an idempotency key guarded by a **durable unique
  constraint**. A cache or lock is an optimisation, never the guarantee.
- A key reused for a *different* request is an error, never a silent replay. A time-bounded guard refuses
  every operation older than its bound; operations that mint or move money are guarded without a bound.
- Only the durable writer writes the account of record; everything else publishes intents. Refuse the
  disguises: a transfer "settled directly", a reconcile that "books a correcting row", a script that
  "just fixes the balance".

**Fail closed, fail loud, refuse first (IV, V, XII)**
- Unknown rate key, unit, capability, or an unreachable store: **deny**. Never price at zero, never grant
  by default. Drift beyond tolerance **alarms** — it is never healed silently.
- The admission gate runs before the expensive work. Every terminal state emits a named metric
  (`metering/ports/metrics.go`; the names are part of the contract) and lands in durable storage.

**Boundary (VI, IX) — enforced by `go-arch-lint` and `depguard`**
- `domain` imports nothing. `ports` → `domain`; `app` → `domain`, `ports`, config. No go-redis, pgx, NATS,
  gRPC, `net/http` or provider SDK in the core. `metering/` never imports `billing`, `entitlement`,
  `events` or `cmd` (`make verify-portability`).
- No `time.Now` in the core (`forbidigo`): take the injected `Clock` port, so the core stays replayable.
- No host vocabulary: the subject, the unit and the ceiling count are opaque data. Behaviour conditioned
  on a specific host's string is a defect.
- `os.Getenv` is only called in `cmd/`; configuration arrives as `metering.Config`, checked by
  `Config.Validate()`.

**Contract-first (VII).** A port or invariant change updates `specs/001-metering-billing-core/contracts/`
and its conformance suite **in the same commit**, then `scripts/check-spec-drift.sh`.
