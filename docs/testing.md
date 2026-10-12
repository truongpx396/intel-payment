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
invariant. A compile error is a weaker red, and `scripts/verify-red-green.sh` accepts it but says so —
a red commit touches tests only, so a stub of a *new production symbol* cannot ride in it. Where the
seam is a port, give the test a double of its own so it fails on what it claims: a conformance suite run
against a test-owned stub that answers `ErrNotImplemented` (R060's unbuilt transport, a stub rater that
rates everything at zero). Where the symbol itself is the new API, the build failure is the red; say so
in the commit message.

## Commit shape

A branch carries one or more cycles, each three kinds of commit in this order:

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

**Every cycle is held to this, not only the first** — the constitution asks for a failing test-only
commit "ahead of every production change". After the first production change, a test-only commit counts
as a red only if its subject says `test(red):` (a `test(mutation):` commit is test-after and is never
mistaken for one); each later production change needs a red of its own since the previous production
change, each such red is checked out and must fail, and the tip must pass them all. A `refactor:` commit
needs none — and does not buy the next change one. A branch can therefore carry a stack of task pairs
(R### → T###, R### → T###); an unredded second change is refused with the commit named.

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

### When mutation testing applies

gremlins mutates **Go**, and `scripts/mutation.sh` knows three packages: `metering/domain`,
`metering/adapters/driven/pricing/table` and `metering/app`. `make mutation-diff` judges the lines a
branch changed **in those packages** and nowhere else. For any other change it mutates nothing: it prints
`no changed lines since <ref>` for each target and exits 0 in about a second. That pass measured
nothing, so it is not evidence — do not run it for such a change, do not paste it into a pull request,
and do not describe it as "passed". Say `N/A` and name the check that does prove the change:

| The branch changes | Mutation | What proves it instead |
|---|---|---|
| docs, specs, `ROADMAP.md`, task lists, `.specify/` | no | `scripts/check-spec-drift.sh` (retired terms, status counts, task pairing) and the CI `docs` link check |
| a gate or helper script (`scripts/*.sh`), the `Makefile`, the CI workflow | no | `make verify-tdd-gates` — each gate shown to refuse what it should, by planting the defect |
| `CLAUDE.md`, `.claude/`, other configuration | no | review, and `check-spec-drift.sh` for dangling links |
| a migration | no | `make verify-schema` |
| the Redis Lua | no | `make verify-hot-path` |
| the `Dockerfile`, compose file or Redis ACL | no | `make verify-deploy` |
| lint or architecture rules | no | `make verify-boundary` |
| Go **outside** the three targets (adapters, `billing/`, …) | it would report "no changed lines" — not evidence | the unit and conformance suites and `make test-integration`; mutating those tiers is task T135 |
| production Go **inside** the three targets | **yes, always** — no tolerance on the lines changed | `make mutation-diff` |

To decide, ask the diff, not your sense of the change:

```sh
git diff --name-only origin/main...HEAD -- metering/domain metering/app metering/adapters/driven/pricing/table | grep -v '_test.go$'
```

No output means the branch touches no production line a mutant could sit on. The exemption is by what the
branch changes, never by effort: one changed line in a target package puts the whole branch under the
rule. `make verify-red-green` has the same shape — on a branch with no production Go, SQL or Lua it
answers `nothing to drive`, which is its result, not a skipped check. The constitution's "the lines the
change touched surviving mutation" is unchanged; a change that touches no such line has none to survive.

Reading a survivor:

- **Equivalent mutant** — `a > b` → `a >= b` where `a == b` cannot change the result. No test can
  kill it, so do not force one. List it in [`.mutation-equivalents`](../.mutation-equivalents) with a
  reason: the entry names the file, the operator, the column and the *exact text of the line*, so
  editing the line lapses the excuse, and an excuse for one operator never covers another's on the same
  line. An entry with no reason stops the run. Excused mutants are set aside from the efficacy figure
  and counted in the output (`excused 13 as equivalent`); a whole-package run notes any entry that no
  longer matches a survivor. The list is reviewed like code — each entry is a claim that no test could
  tell the code from its mutant, and a wrong claim hides a missing test.
- **Anything else is a missing test.** Write the test that passes on the real code and fails on the
  mutant (usually the exact boundary value, on both sides). A pull request has **no tolerance** here:
  on the lines it changed, `make mutation-diff` fails on any survivor that is not excused, however high
  the percentage around it. Whole-package runs keep a percentage floor, so the equivalents nobody has
  listed yet do not fail the nightly run.

Reaching more code exposes more mutants, so a run after adding tests can show survivors that were not
there before: that is the tool working, not the tests getting worse.

Limits, stated so the numbers are not over-read:

- gremlins reports a mutant on a `case` expression as *not covered* whatever the tests do: Go's
  coverage profile has no block for a case condition.
- `metering/app` is measured at the unit tier only. Booking, reconcile, recover and the writer are
  verified by the integration tier, which a unit-tier run cannot see, so its mutant coverage is low and
  is reported as such rather than claimed. A mutation run over that tier is task T135.
- A `TIMED OUT` mutant is never counted as killed, and a run where more than 5% time out fails —
  gremlins sizes the timeout from one cached coverage run, and a short one makes everything "time out"
  and leaves a flattering denominator (`.gremlins.yaml`).

Thresholds live in `scripts/mutation.sh`. Raise them as the suite improves; never lower one to go green.
`scripts/verify-tdd-gates.sh` proves the gate refuses what it should — a planted survivor, a survivor
one line touched, an excuse that outlives an edit to its line, one aimed at a neighbouring operator, one
with no reason — and accepts an exact one.

## What CI holds, and where

| Gate | Job | On |
|---|---|---|
| build, vet, tests (`-race -shuffle`), every conformance suite | `go` | every change |
| integration (real Redis, Postgres) | `go` | every change |
| a failing test-only commit precedes every production change | `red-green` | pull requests |
| the task lists are test-first: every green has a red above it, every red a green | `drift` | every change |
| properties hold on the seed corpus and a short search | `fuzz` | every change |
| the tests fail when the lines this PR changed are wrong | `mutation` | pull requests |
| deep fuzz (10 min/target); mutation over whole targets | `fuzz`, `mutation` | nightly |

`make ci` runs the local equivalents except the two that need a branch history (`red-green`,
`mutation-diff`): run them before opening a PR.

## Working through a long task list without losing hours

Phase 2 (about thirty red→green pairs) showed where the time goes. None of it is the discipline itself; all of
it is avoidable.

| Cost | Do this instead |
|---|---|
| `make mutation-diff` takes 4–7 minutes for `metering/app` | Run it **once per slice**, in the background, while writing the next red — not per cycle. It reads `git diff` of **committed** history, so a run on the working tree silently skips every new file: commit first |
| Gremlins cannot judge a `case` condition ("not covered") | Read those mutants by eye and pin the boundary with a plain test: an inclusive limit, exactly zero, an `int64` extreme. An equivalent mutant gets a reasoned `.mutation-equivalents` entry, never a contrived test |
| Planted-defect checks that go on and on | Three or four mutants on the decision lines (auth, privilege, realm, idempotency, money) per slice; the mutation run covers the rest. A mutant that does not compile proves nothing |
| A text-replace script that silently misses | Assert the pattern matched, and run `gofmt -w` first: it re-aligns lines |
| `git stash` with staged changes | Don't: it un-stages them, and a "complete" commit then lacks its `Makefile`/`go.mod`. Run `git show --stat HEAD` after each commit |
| A compile-failing red with mistakes of its own | Drop a throwaway stub in the package, `go vet` it so the only error left is the missing symbol, delete the stub, then commit |
| A test that hangs when the code under test is broken | Bound every wait (`select` with `time.After`); give each parallel subtest its own counters |
| Running everything while working | `go test -run <name> ./pkg/` while working; `-race -shuffle=on ./...` and the Docker `-tags=integration` suite at the end of a slice |
| Looping the integration suite after a flake | Rerun the one failing package. `TestTrimRemovesOnlyWhatEveryGroupHasAcknowledged/cluster` and `TestHotAckWaitAOFLocal` are known intermittent |
| `scripts/mutation.sh` dying with `No module named 'tomllib'` | `/opt/homebrew/bin` must come **before** `/usr/bin` on `PATH` (the script needs Python 3.11+); the tools live in `/opt/homebrew/bin` and `~/go/bin` |
| `go-arch-lint` failing on a clean tree | It scans `.claude/worktrees/*` (stale agent worktrees inside the repo). Check the graph on a copy that excludes `.claude/worktrees`, `.git` and `node_modules` |
| A branch that needs `TDD-Exempt:` for generated code | One `TDD-Exempt:` line in **any** commit exempts the **whole** branch for `verify-red-green.sh`: keep exempt work (a generated proto) on its own bottom branch of the stack |
