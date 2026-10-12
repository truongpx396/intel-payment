---
paths:
  - "specs/**"
  - "docs/**"
  - "README.md"
  - "ROADMAP.md"
  - "PROVENANCE.md"
  - ".specify/**"
---

# Specs, contracts and documents

The documents here are checked by CI, and a stale one has already been a defect.

- **The constitution is the authority** (`.specify/memory/constitution.md`). An amendment needs a
  documented rationale, a version bump, and a pass over the contracts for anything it invalidates.
  NON-NEGOTIABLE principles (I, II, XI) may be clarified, never relaxed.
- **Contracts are normative.** Change a contract and the code that implements it in the **same commit**.
  Contract text and a conformance suite move together.
- **`scripts/check-spec-drift.sh` must pass** (CI job `drift`; the Stop hook runs it too). It fails when a
  document describes a retired mechanism as current — to retire another term, add a line to the script.
  `design-decisions.md` and `PROVENANCE.md` are exempt: recording what was retired is their job.
- **Status is derived, not asserted.** ROADMAP.md must state each spec's task count exactly as its
  `tasks.md` has it; marking a task `[x]` means editing ROADMAP.md in the same change. `[x]` means done
  **and verified**, with the verification named. Never write "implemented" for something CI does not
  check — the README and ROADMAP say plainly what is not built.
- **Task pairs.** From Phase 2 on, a behavioural task is **R###** (red, a failing tests-only commit)
  then **T###** (green). IDs are never reused for a different task. `scripts/check-task-pairs.sh` (inside
  the drift script) holds the list to it between the `pairs` markers: a green with no red above it, a red
  nothing makes pass, or a bold ID used to *refer* to a task (bold defines) is refused. A red is one
  invariant or a few that move together; one that would need a dozen failing tests and one big change
  is split.
- **No mutation run for docs, specs or scripts.** `make mutation-diff` mutates Go in three packages; for
  a change that touches none of them it prints "no changed lines" and exits 0, which proves nothing.
  Skip it, write `N/A` in the PR, and run what proves the change: `check-spec-drift.sh` for a document or
  task list, `make verify-tdd-gates` for a gate script ([when it applies](../../docs/testing.md#when-mutation-testing-applies)).
- **No dangling relative links** (CI job `docs`): every relative markdown link to a `.md`, `.sql` or
  `.excalidraw` file must resolve — including from `CLAUDE.md` and `.claude/`, which that job scans too.
- A change to the Phase 2 draft lives in `specs/002-postpaid-invoicing/`; it must stay constructible on
  top of the Phase 1 baseline (`make verify-schema`).
