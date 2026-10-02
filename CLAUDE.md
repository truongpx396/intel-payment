# intel-payment

A reusable metering, credits and payments engine: Go 1.26+, Redis for the hot path, Postgres for the books.
**It moves money.** A bug here is a customer charged twice, charged for nothing, or revenue quietly lost.
The authority is the [constitution](.specify/memory/constitution.md) (13 principles) — read it before
changing anything on the money path. Principle XI applies to you: **never say work is done, fixed or
passing without running the check and showing its real output.**

Status is derived, not asserted: [ROADMAP.md](ROADMAP.md) states task counts that CI checks against
`specs/*/tasks.md`. Read it rather than trusting this file. As of writing, the metering core is built and
verified, and there is **no gRPC/REST transport and no `paymentd`/`payment-worker` binary** — do not write
code, docs or comments that imply otherwise.

## Layout

| Path | What it is |
|---|---|
| `metering/` | The engine, standalone and portable. `domain` → `ports` → `app` → `adapters/{driven,driving}`; `contracts/` holds the conformance suites |
| `billing/` `entitlement/` `events/` `api/` | Designed, still `doc.go` stubs. They may depend on metering's *ports* only |
| `migrations/` `migrate/` `cmd/payment-migrate` | Forward-only schema and its migrator |
| `specs/001-metering-billing-core/` | `spec`, `plan`, `tasks`, `design-decisions`, and the normative `contracts/` |
| `specs/002-postpaid-invoicing/` | Phase 2 — designed, not built; its schema is a draft that never ships from `migrations/` |
| `integration/` `internal/{pgtest,chaos}` `e2e/` | Testcontainers stack tests, fixtures and fault injection, Playwright |
| `scripts/` `deploy/` | Every verification gate; compose stack, Dockerfile, Redis ACL |

## Commands (`make help` lists them all)

| Run | When |
|---|---|
| `make test` | Always, while working: unit tests and every conformance suite, `-race -shuffle=on` |
| `make test-integration` | Anything touching an adapter, the writer, recovery, migrations (Docker) |
| `make lint` | `golangci-lint` (depguard boundary rules) + `go-arch-lint` |
| `make fuzz` `FUZZTIME=20s` | After touching arithmetic, an encoding, or time logic |
| `make mutation-diff` `REF=origin/main` | Before a PR: do the tests fail when the lines you changed are wrong? |
| `make verify-red-green` | Before a PR: a failing tests-only commit precedes the change (run on a branch) |
| `make ci` | Before a PR: build, tests, fuzz, integration, lint, portability, boundary, vuln, hot path, deploy. Not `verify-schema`, e2e, or the two branch-history gates above |
| `make verify-schema` / `verify-hot-path` / `verify-deploy` | Changed a migration / the Redis Lua / the image or ACL |
| `scripts/check-spec-drift.sh` | Changed any document or contract |

## How work is done here

1. **Test-first, and the evidence is checked.** A production change arrives as a tests-only
   `test(red): <invariant>` commit that *fails on an assertion*, then `fix(green):`/`feat(green):`, then
   optionally `refactor:`. `scripts/verify-red-green.sh` (CI job `red-green`) enforces it. A change that
   cannot be test-driven (rename, dependency bump, formatting, doc comment) says
   `TDD-Exempt: <reason>` on its own line — "hard to test" is never a reason. Write the failing test
   first and commit it alone; a test added afterwards is `test(mutation): …`, never a red commit.
   Detail: [docs/testing.md](docs/testing.md); test rules load from `.claude/rules/go-tests.md`.
2. **Do not weaken a gate to go green.** Mutation thresholds only rise; an `.mutation-equivalents`
   entry is a claim that no test could tell the code from its mutant, not a way to skip writing one;
   no `//nolint` without a reason; never lower a lint or architecture rule. Edits to those files prompt.
3. **The boundary is machine-enforced** (`go-arch-lint`, `depguard`, `make verify-portability`).
   `os.Getenv` only in `cmd/`; no `time.Now` in the core; no float for money; no provider SDK outside
   its adapter; metering never imports billing, entitlement, events or cmd.
4. **Contract and code land in the same commit** (`specs/001-metering-billing-core/contracts/`), and
   `scripts/check-spec-drift.sh` must pass.
5. **Report honestly.** Failing output is pasted verbatim. Mutation numbers come only from a run you
   made; a `TIMED OUT` mutant is never "killed". If you skipped a check, say so.

## Git and PRs

- Work on a branch (`feat/…`, `fix/…`, `chore/…`); **never commit to or push `main`** — a hook refuses it.
  History is PR merges, and the red-green gate can only judge a branch.
- Commit subjects: `test(red): …`, `fix(green): …`, `feat(green): …`, `refactor: …`, `test(mutation): …`,
  `docs: …`, `chore: …`. Open PRs from `.github/pull_request_template.md`: red/green evidence plus the
  pasted output of `make ci` and `make mutation-diff`.
- **Ask first** before opening, merging or closing a PR, or force-pushing anything — settings prompt for
  these. For a stack of PRs, merge **bottom-up** (lowest into `main`, retarget the next) or squash;
  merging the top one first turns every PR page into the compounded diff.

## Gotchas

- `go.mod` floors Go at 1.26.0; CI runs the latest 1.26.x, local may be newer. Keep Go, actions and
  dependencies current (golangci-lint v2, Node 24 for e2e).
- **gremlins is pinned to v0.6.0**: `scripts/mutation.sh` parses its summary, and gremlins' own `--diff`
  matches nothing for a sub-package, so use the script's scoping rather than the flag.
- Docker is required for integration tests, `verify-hot-path`, `verify-deploy` and `make ci`. A
  Testcontainers Ryuk start-up race ("unexpected container status created") appears when many package
  binaries start at once — rerun before debugging.
- Migrations are forward-only. No release tag exists yet, so `0001`–`0005` are still a baseline corrected
  in place; after the first tag, only additive `NNNN+1`. [migrations/README.md](migrations/README.md).
- Real `.env` files are denied to you; `.env.example` is the configuration reference. Never put a secret
  in a file, a log line, a fixture or a commit.
- `.claude/worktrees/` is git-ignored scratch space for agent worktrees; don't commit it or edit in it
  from the main checkout.

## This directory's Claude configuration

`.claude/settings.json` holds permissions and four hooks (scripts in `.claude/hooks/`): gofmt on every
Go edit; a Bash guard that refuses commits/pushes on `main`; a Stop gate that runs `go build`, `go vet
-tags=integration` and `check-spec-drift.sh` when the relevant files changed, and blocks once if they
fail; and a SessionStart note on branch and toolchain. `.claude/rules/` holds path-scoped rules
(tests, money core, Redis hot path, migrations, specs and docs, config and secrets). `/pre-pr` runs the
pre-PR checklist; the `constitution-reviewer` agent audits a diff against the principles CI cannot
check. If a hook denies something, fix the cause; do not route around it.
