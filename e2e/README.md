# End-to-end tests (Playwright)

These tests drive a **running stack** — the real image, migrations, Redis ACL and transports — never
a mock. Unit and conformance tests prove the parts; this proves they are deployed together correctly.

```bash
make up                 # postgres + redis + migrate + paymentd + worker
make e2e-install        # once
make e2e                # E2E_BASE_URL defaults to http://localhost:8080
```

| Variable | Meaning |
|---|---|
| `E2E_BASE_URL` | the stack under test. **Unset → every test is skipped with that reason** (CI always sets it) |
| `E2E_SERVICE_TOKEN` | a bearer token from `PAYMENT_SERVICE_TOKENS`, for tests that need an authenticated caller |

## Conventions

- **Parallel by default** (`fullyParallel`). Tests never share state: each takes its own scope from
  `uniqueScope()`, so a balance is never contended and a re-run never sees the last run's data. A
  test that must share state is `serial` with a comment saying what it shares.
- **`*.api.spec.ts`** use Playwright's `request` (no browser, nothing to install). **`*.ui.spec.ts`**
  arrive with the credits-ui package in Phase 6 and add a browser project in `playwright.config.ts`.
- **Failure paths first**: what a credential-less caller gets, a replay, an outage. The happy path is
  the one that is exercised in development anyway (constitution X).
- **No retry-until-green.** One CI retry, reported; a test that needs more is a bug in the system or
  the test.
- A test asserts the contract in `specs/001-metering-billing-core/contracts/`, and names the
  requirement it covers in a comment.

The suite is type-checked and listed in CI on every push; it *runs* once `cmd/paymentd` exists.
