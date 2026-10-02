---
paths:
  - "**/*_test.go"
  - "**/testdata/**"
  - "e2e/**"
  - "integration/**"
  - "internal/pgtest/**"
  - "internal/chaos/**"
  - "metering/contracts/**"
  - "**/redistest/**"
  - "**/natstest/**"
  - "**/pricingtest/**"
---

# Writing tests here

Constitution X; the practice is [docs/testing.md](../../docs/testing.md). A test that has never failed
has not been shown to test anything.

- **Red first.** The failing test is committed alone as `test(red): <the invariant, in a sentence>`, and
  it must fail on an *assertion* that names the invariant. A compile error is a weaker red: add the
  smallest stub so it runs and fails on what it claims. Only then the `fix(green):`/`feat(green):` commit.
- **Added after the code?** It is not a red commit. Commit it as `test(mutation): …` after the green
  commit (a test written to kill a mutation survivor), and never describe it as test-first.
- **Every test calls `t.Parallel()`** (`paralleltest`/`tparallel` enforce it). One that truly cannot says
  why: `//nolint:paralleltest // <reason>`. Tests run `-race -shuffle=on`, so none may depend on another's
  side effects.
- **A package that starts goroutines or connections has a goleak `TestMain`.** Integration packages add
  the Testcontainers ignores — copy them from `integration/stack_test.go` (`Reaper`, `persistConn`).
- **Integration tests** carry `//go:build integration`, use Testcontainers (real Redis, Postgres, NATS),
  and run with `make test-integration` (Docker). Prefer the lowest tier that can express the invariant.
- **Name the test for the invariant**, and spend the effort on failure paths: replay, concurrency,
  cancellation, tampering, outage, drift, refund past spend. For any credit-affecting operation, a test
  delivers the same operation twice *and* once concurrently (constitution II).
- **Properties for arithmetic, encodings and time.** A `Fuzz…` target held to an *independent*
  reference (`math/big`, a 128-bit `math/bits` computation, a hand-built canonical string), with the
  known boundary cases as seeds. A failing input from `testdata/fuzz/` is committed with its fix.
- **Conformance suites** (`metering/contracts`) run unchanged against every implementation of a port and
  import no adapter. A new or changed suite is shown to fail against a deliberately broken
  implementation before it is claimed to verify anything.
- **No float in a money fixture.** Amounts are integers; fiat is integer minor units plus a currency.
- A new `.mutation-equivalents` entry claims no test could tell the code from its mutant. It needs the
  exact line and a reason; if a test could tell them apart, write the test instead.
