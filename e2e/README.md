# End-to-end tests (Playwright)

These tests drive a **running stack** — the real image, migrations, Redis ACL and transports — never
a mock. Unit and conformance tests prove the parts; this proves they are deployed together correctly.

```bash
make up                 # postgres + redis + migrate + paymentd + worker
make e2e-install        # once
make e2e                # E2E_BASE_URL defaults to http://localhost:8080
```

The `api` project needs nothing more. The `ui` project drives the credits screen in Chromium and needs a credential and a seeded realm:

```bash
npx playwright install chromium                                              # once, in e2e/
(cd ui && npm ci && npm run build -w @intel-payment/credits-ui && npm run build:host -w @intel-payment/example-llm-credits)

TOKEN=$(openssl rand -hex 24)                                                # a throwaway; the stack's config holds only its digest
PAYMENT_SERVICE_TOKENS="e2e:$(printf %s "$TOKEN" | shasum -a 256 | cut -d' ' -f1):default:record+grant+admin" make up
make e2e-seed                                                                # a rate card and two ceilings (e2e/seed.sql), idempotent
E2E_SERVICE_TOKEN=$TOKEN make e2e
```

| Variable | Meaning |
|---|---|
| `E2E_BASE_URL` | the stack under test. **Unset → every test is skipped with that reason** (CI always sets it) |
| `E2E_SERVICE_TOKEN` | a bearer token whose digest is in `PAYMENT_SERVICE_TOKENS`. The `ui` project needs it (locally an unset one skips with the reason; in CI it fails, so a stack started without one cannot turn the project into silent skips) |

## Conventions

- **Parallel by default** (`fullyParallel`). Tests never share state: each takes its own scope from
  `uniqueScope()`, so a balance is never contended and a re-run never sees the last run's data. A
  test that must share state is `serial` with a comment saying what it shares.
- **`*.api.spec.ts`** use Playwright's `request` (no browser, nothing to install). **`*.ui.spec.ts`** use a real
  Chromium (the `ui` project): the credits screen of `ui/examples/llm-credits`, over this stack. `tests/ui-host.ts` is the product's
  "backend" in miniature — it serves the built page and forwards `/v1/*` with the service credential, because `/v1` is for a host's backend
  and a browser never holds the token (a test asserts that). The jsdom tests under `ui/` stay the place for a component's rules; these say
  what only a browser and a real server can: that the shapes the server sends are the shapes the screen reads, how it lays out at real
  widths, and what an accessibility audit (axe-core, WCAG 2.2 AA and best practice) finds.
- **The books lag the balance.** A balance is the hot copy and moves at once; the ledger is what the durable writer has booked, a few
  milliseconds later. A test that reads the ledger first calls `settle()` (it polls the real `/v1/ledger`), or it passes by luck.
- **Failure paths first**: what a credential-less caller gets, a replay, an outage. The happy path is
  the one that is exercised in development anyway (constitution X).
- **No retry-until-green.** One CI retry, reported; a test that needs more is a bug in the system or
  the test.
- A test asserts the contract in `specs/001-metering-billing-core/contracts/`, and names the
  requirement it covers in a comment.

The suite is type-checked, listed and run against a freshly started stack in CI on every push (the `e2e` job).
