# Testing — how this repository does TDD

This service moves money, so a test that cannot fail is worse than no test: it is a false
statement that something is checked. Test-driven development here is not "we write tests"; it is a
loop with evidence at each step, and machinery that makes the evidence checkable.
[Constitution principle X](../.specify/memory/constitution.md) is the rule; this is the practice.

## The loop

1. **Red.** Write the smallest test that states the next invariant, and watch it fail *for the
   reason you expect*. A test that has never failed has not been shown to test anything.
2. **Green.** Write the least production code that makes it pass. Nothing the test does not ask for.
3. **Refactor.** With the suite green, improve the structure. The tests do not change in this step;
   if they must, the step was a behaviour change and goes back to red.

A failing test is red **for the right reason** when an *assertion* fails and its message names the
invariant. A compile error is a weaker red: add the smallest stub (`return 0`, `panic("unimplemented")`)
so the test runs and fails on what it claims. `scripts/verify-red-green.sh` accepts a build failure but
says so.

## Commit shape

One branch, three kinds of commit, in this order:

| Commit | Touches | The suite |
|---|---|---|
| `test(red): <the invariant, in a sentence>` | tests only | **fails**, for the reason the message gives |
| `fix(green): …` / `feat(green): …` | production code | passes |
| `refactor: …` | production code | passes before and after, tests untouched |

`scripts/verify-red-green.sh` (CI job `red-green`, pull requests) checks the shape mechanically: for
a branch that changes production code there must be a tests-only commit **before** the first
production change, the tests it adds must **fail** when that commit is checked out, and the branch
tip must pass them. A test written after the code cannot satisfy this — at the commit where it
appears the code is already right, the test passes, and the script says so.

**Production code** is Go that ships, `migrations/*.sql` and `*.lua`. **Test code** is `_test.go`,
`testdata/`, `e2e/`, the conformance suites under `contracts/`, and the fixture packages
(`pricingtest`, `redistest`, `natstest`, `pgtest`, `chaos`).

**`TDD-Exempt: <reason>`** — on its own line in a commit message or the PR description — excuses a
change that genuinely cannot be test-driven: a rename, a dependency bump, a formatting pass, a doc
comment. The reason is required and is read in review like any other line. "Hard to test" is not a
reason; it is a finding about the design.

A test added *afterwards* to kill a mutation survivor (below) is test-after by definition. Commit it
after the green commit, as `test(mutation): …`, so it is not mistaken for a red commit.

## Which tier an invariant belongs to

Write the test at the lowest tier that can express the invariant; move up only when it cannot.

| Tier | Runs against | Put here |
|---|---|---|
| **Unit** | pure code or programmable fakes | arithmetic, encodings, validation, every branch of a decision |
| **Conformance suite** (`metering/contracts`) | *every* implementation of a port | what any `Pricer`, `Ledger`, `Bus`… must do — the suite is written before the second implementation |
| **Integration** (`-tags=integration`) | real Redis, Postgres, NATS (Testcontainers) | the seams: guards, locks, the hot path, the writer, recovery |
| **Consistency / chaos** | a hot tier that goes back in time | failover, truncated AOF, poison, drift |
| **End-to-end** (`e2e/`, Playwright) | the deployed stack | the wiring: image, migrations, ACLs, transports |

Failure paths get the most attention — replay, concurrency, cancellation, tampering, outage, drift, a
refund past the spend. The happy path is the one development exercises anyway.

Every test is `t.Parallel()` (the `paralleltest` linter enforces it), runs shuffled under `-race`,
and any package that starts goroutines has a goleak `TestMain`.

## Properties and fuzzing

Where "it works on the examples I thought of" is not enough — canonical encodings, arithmetic that
must never wrap, time logic — state a **property** and let the fuzzer search for the input that
breaks it:

```go
func FuzzCreditsAddIsExactOrRefused(f *testing.F) {
    f.Add(int64(math.MaxInt64), int64(1))            // seeds are the boundary cases you know
    f.Fuzz(func(t *testing.T, a, b int64) { /* compare against math/big */ })
}
```

- The **seed corpus runs as an ordinary test** on every `go test`. Put every known-bad input in it.
- `make fuzz` (`scripts/fuzz.sh`) explores beyond the seeds; CI runs it for 20 s per target on every
  change and for 10 minutes nightly.
- A failure writes the input to `<package>/testdata/fuzz/<Target>/`. **Commit it with the fix**; it is
  a regression test from then on.
- Prefer an **independent reference** over restating the code: `math/big` for `int64` arithmetic, a
  128-bit `math/bits` computation for the pricer, a hand-built canonical string for a fingerprint.

The first fuzz run of this repository found two defects in code that had passed review: a retry that
reordered a twice-named unit got an idempotency conflict, and a daily counter in a zone that skipped a
calendar date got a negative lifetime and was deleted at once. Both are seeds now.

## Mutation testing

Coverage says a line *ran*. It cannot say a test would notice the line being wrong. Mutation testing
([gremlins](https://github.com/go-gremlins/gremlins)) changes the code in small ways — `<` to `<=`,
`+` to `-` — and requires a test to fail. A **survivor** is behaviour nothing pins down.

```sh
make mutation-diff     # only the lines changed since REF (committed or not) — what a PR is held to (REF=origin/main)
make mutation          # every target, against its thresholds — nightly, and before a release
```

Reading a survivor:

- **Equivalent mutant** — `a > b` → `a >= b` where `a == b` cannot change the result. No test can
  kill it; the thresholds leave room for these. Say so in the PR rather than forcing a test.
- **Anything else is a missing test.** Write the test that passes on the real code and fails on the
  mutant (usually the exact boundary value, on both sides).

Limits, stated so the numbers are not over-read:

- gremlins reports a mutant on a `case` expression as *not covered* whatever the tests do: Go's
  coverage profile has no block for a case condition.
- `metering/app` is measured at the unit tier only. Booking, reconcile, recover and the writer are
  verified by the integration tier, which a unit-tier run cannot see, so its mutant coverage is low and
  is reported as such rather than claimed.
- A `TIMED OUT` mutant is never counted as killed, and a run where more than 5% time out fails —
  gremlins sizes the timeout from one cached coverage run, and a short one makes everything "time out"
  and leaves a flattering denominator (`.gremlins.yaml`).

Thresholds live in `scripts/mutation.sh`. Raise them as the suite improves; never lower one to go green.

## What CI holds, and where

| Gate | Job | On |
|---|---|---|
| build, vet, tests (`-race -shuffle`), every conformance suite | `go` | every change |
| integration (real Redis, Postgres) | `go` | every change |
| a failing test-only commit precedes the change | `red-green` | pull requests |
| properties hold on the seed corpus and a short search | `fuzz` | every change |
| the tests fail when the lines this PR changed are wrong | `mutation` | pull requests |
| deep fuzz (10 min/target); mutation over whole targets | `fuzz`, `mutation` | nightly |

`make ci` runs the local equivalents except the two that need a branch history (`red-green`,
`mutation-diff`): run them before opening a PR.
