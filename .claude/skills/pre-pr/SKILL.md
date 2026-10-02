---
name: pre-pr
description: Run this repo's pre-pull-request checklist — red-before-green evidence, spec drift, make ci, mutation on the changed lines — and report each result with its real output, ready for the PR template. Use before opening a PR.
disable-model-invocation: true
---

# Pre-PR checklist

Constitution XI: every claim below is backed by output you actually ran and show — pasted, not
summarized. Run the checks in order and stop at the first failure unless the user says to continue. If
something cannot run (Docker down, no base ref), say so; that is a skipped check, not a passed one.

## 0. Context

`git branch --show-current` (refuse on `main`), `git fetch origin main`, then
`git log --oneline origin/main..HEAD` and `git diff --stat origin/main...HEAD`. Name the task IDs the
branch covers from `specs/001-metering-billing-core/tasks.md`.

## 1. Red, then green — `make verify-red-green`

A branch that changes production code needs a tests-only commit before it that **fails**, and a tip
that passes. If it refuses, say which rule failed. Do not rewrite history to satisfy it without asking.
A change that genuinely cannot be test-driven needs a `TDD-Exempt: <reason>` line in a commit message or
the PR body — and "hard to test" is not a reason.

## 2. Spec drift — `scripts/check-spec-drift.sh`

## 3. The gate — `make ci`

Long, and needs Docker. If it will exceed the shell timeout, run it in the background and wait for the
real result. On a Testcontainers Ryuk start-up error ("unexpected container status created"), rerun once
before treating it as a failure.

## 4. Mutation on the changed lines — `make mutation-diff`

A pull request has no tolerance on lines it changed. For each survivor, decide: **equivalent mutant**
(no test could tell the code from the mutant) or **missing test**. A missing test gets the test written
— usually the exact boundary on both sides, committed as `test(mutation): …`. Propose a
`.mutation-equivalents` entry only with the exact line and a reason, and show it to the user; never add
one to make the run pass.

## 5. Report

A table of check → result → the evidence (the command and the decisive lines of its output). Then the
filled-in sections of `.github/pull_request_template.md`: what and why with task IDs; red and green
commit SHAs with the failing assertion; the mutation output; the verification output.

**Do not open the pull request.** Opening, merging or closing one is the user's call — ask.
