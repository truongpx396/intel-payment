import { defineConfig, devices } from '@playwright/test';

// End-to-end tests run against a REAL stack (`make up`), never a mock: what they prove is that the
// deployed pieces — image, migrations, Redis ACL, transports — work together.
//
//   E2E_BASE_URL=http://localhost:8080 npx playwright test
//
// Everything is parallel by default. A test that mutates shared state must not: it creates its own
// realm/scope (see tests/support.ts `uniqueScope`) so tests never contend, or it is marked
// `test.describe.configure({ mode: 'serial' })` with a comment saying what it shares.
const baseURL = process.env.E2E_BASE_URL;

export default defineConfig({
  testDir: './tests',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,          // a stray test.only must fail CI, not silently shrink it
  retries: process.env.CI ? 1 : 0,       // a retry hides flakiness on a money path; one, and it is reported
  workers: process.env.CI ? 4 : undefined,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL,
    extraHTTPHeaders: { Accept: 'application/json' },
    trace: 'retain-on-failure',
  },
  projects: [
    // The REST surface. Playwright's APIRequestContext — no browser is launched or installed.
    { name: 'api', testMatch: /.*\.api\.spec\.ts/ },
    // The credits screen (ui/), in a real Chromium, over this stack: tests/ui-host.ts serves the built reference page and forwards /v1 to
    // E2E_BASE_URL with the service credential, as a product's backend would. `npm run build:host -w @intel-payment/example-llm-credits`
    // in ui/ builds the page; `make e2e-seed` gives the realm its rate card and ceilings. The page is a different origin from the stack, so
    // the JSON `Accept` header the api project sends by default is cleared here: a document request does not ask for JSON.
    { name: 'ui', testMatch: /.*\.ui\.spec\.ts/, use: { ...devices['Desktop Chrome'], extraHTTPHeaders: {} } },
  ],
});
