## What and why

<!-- One paragraph. Name the task IDs from specs/001-metering-billing-core/tasks.md and any contract this touches. -->

Tasks: T…

## Red, then green

<!-- Test-first is checked by CI (`scripts/verify-red-green.sh`), but the evidence belongs here for the reviewer. -->

- **Red** — `<sha> test(red): …`. It fails because: <the invariant, in a sentence>

  ```text
  <paste the failing output — the assertion, not a compile error; 5–10 lines>
  ```

- **Green** — `<sha> fix(green): …` / `feat(green): …`

If this change cannot be test-driven (a rename, a dependency bump, a formatting pass, a doc comment), replace this
section with one line, which CI reads: `TDD-Exempt: <reason>`.

## Tests beyond the examples

- [ ] An invariant over arithmetic, an encoding or time has a **property / fuzz target** (seeds include the boundary cases)
- [ ] Failure paths are tested, not just the happy one: replay, concurrency, outage, tampering, refund past spend
- [ ] The branch changes production Go in `metering/domain`, `metering/app` or the table pricer: `scripts/mutation.sh --diff origin/main` output pasted; each survivor is fixed with a test, or is an **equivalent mutant** listed with a reason in `.mutation-equivalents`
- [ ] Otherwise: `N/A: <what this changes>` — and the check that proves it instead (`check-spec-drift.sh`, `make verify-tdd-gates`, `make verify-schema`, …). Do not paste a "no changed lines" pass; it measured nothing ([docs/testing.md](../docs/testing.md#when-mutation-testing-applies))

```text
<paste>
```

## Verification

- [ ] `make ci` passes — output pasted below, **not summarized**
- [ ] Any contract change is in `specs/001-metering-billing-core/contracts/` **in this PR**, and `scripts/check-spec-drift.sh` passes
- [ ] No `os.Getenv` outside `cmd/`; no float near money; no provider SDK outside its adapter; no host access to Redis

```text
<paste>
```
