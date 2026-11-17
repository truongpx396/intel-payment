import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';
import { expect, freshScope, grant, requireUiStack, screenUrl, settle, spend, test, type Scope } from './credits-support';

// The credits screen (ui/), in a real Chromium, over a real paymentd — the one thing the jsdom tests under ui/ cannot say. Each test takes its
// own scope (see support.ts), so they run in parallel and a re-run never sees the last run's state. The realm is the one `make e2e-seed` set up:
// a balance, and a daily cap of 1,000 credits, with one ordinary event (1,200 in, 800 out) costing 660.
test.beforeEach(() => requireUiStack());

const MINUS = '−';

/** A funded scope, in the three states the daily cap can be in, once the books hold every row (see `settle`). */
async function funded(request: Parameters<typeof grant>[0], scope: Scope, state: 'ok' | 'warn' | 'capped') {
  await grant(request, scope, 5_000_000);
  await spend(request, scope, { in: 1200, out: 800 }); // 660 of 1,000
  let rows = 2;
  if (state === 'warn') (await spend(request, scope, { in: 1200, out: 0 }), rows++); // +180 → 840: 84%
  if (state === 'capped') (await spend(request, scope, { in: 1200, out: 800 }), rows++); // +660 → 1,320: past the cap
  await settle(request, scope, rows);
}

/** A scope that spent without ever being funded: the books go negative. */
async function unfunded(request: Parameters<typeof grant>[0], scope: Scope) {
  await spend(request, scope, { in: 1200, out: 800 }); // −660
  await settle(request, scope, 1);
}

/** The browser's console errors and uncaught exceptions: a screen over a real server must not raise any. */
function trapErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on('console', (m) => m.type() === 'error' && errors.push(m.text()));
  page.on('pageerror', (e) => errors.push(`uncaught: ${e.message}`));
  return errors;
}

test.describe('the screen draws what the real server says', () => {
  test('a funded workspace: the balance, the daily ceiling and the ledger are the server\'s own figures', async ({ page, request, host }) => {
    const errors = trapErrors(page);
    const scope = freshScope();
    await funded(request, scope, 'ok');
    await page.goto(screenUrl(host, scope));

    await expect(page.getByText('4,999,340 credits left')).toBeVisible();
    await expect(page.locator('[data-quantity="4999340"]').first()).toBeVisible(); // the exact integer, beside the grouped text
    await expect(page.getByText('of 5,000,000 credits granted')).toBeVisible();

    const daily = page.getByRole('progressbar', { name: 'Daily cap' });
    await expect(daily).toHaveAttribute('aria-valuenow', '66');
    await expect(page.getByText('660 / 1,000 cr')).toBeVisible();
    await expect(page.getByRole('alert')).toHaveCount(0); // nothing is wrong, so nothing is raised
    await expect(page.getByRole('status')).toHaveCount(0);

    // the host's attributes, as the host recorded them, reach the host's columns
    const spent = page.getByRole('row', { name: /llm\.chat/ });
    await expect(spent).toContainText('gpt-5');
    await expect(spent).toContainText('2,000');
    await expect(spent).toContainText(`${MINUS}660`);
    await expect(page.getByRole('row', { name: /e2e/ })).toContainText('+5,000,000');

    // nothing real came back for these, and the screen says so instead of drawing a zero or a flat line
    await expect(page.getByText('No spend in this window yet.')).toBeVisible();
    await expect(page.getByText('No spend to break down yet.')).toBeVisible();
    expect(errors).toEqual([]);
  });

  test('near the daily cap: a warning, with the figure that earned it', async ({ page, request, host }) => {
    const scope = freshScope();
    await funded(request, scope, 'warn');
    await page.goto(screenUrl(host, scope));

    await expect(page.getByRole('progressbar', { name: 'Daily cap' })).toHaveAttribute('aria-valuenow', '84');
    await expect(page.getByRole('status')).toContainText('84% used');
    await expect(page.getByRole('alert')).toHaveCount(0); // a warning is not a block
  });

  test('past the daily cap: blocked, with the server\'s own deny code and no way to buy past it', async ({ page, request, host }) => {
    const scope = freshScope();
    await funded(request, scope, 'capped');
    await page.goto(screenUrl(host, scope));

    const alert = page.getByRole('alert');
    await expect(alert).toContainText('Daily cap reached');
    await expect(alert).toContainText('limit_reached');
    await expect(page.getByRole('progressbar', { name: 'Daily cap' })).toHaveAttribute('aria-valuenow', '100');
    await expect(page.getByText(`1,320 / 1,000 cr`)).toBeVisible(); // the real overshoot is shown, not clamped away
    await expect(alert.getByRole('link', { name: 'Top up' })).toHaveCount(0); // a daily cap resets by itself: selling credits would not lift it
  });

  test('an unfunded workspace: negative balance, blocked, and the one way out is the host\'s top-up', async ({ page, request, host }) => {
    const scope = freshScope();
    await unfunded(request, scope); // nothing was granted: the books go to −660
    await page.goto(screenUrl(host, scope));

    await expect(page.getByText(`${MINUS}660 credits left`)).toBeVisible();
    const alert = page.getByRole('alert');
    await expect(alert).toContainText('payment_required');
    await expect(alert.getByRole('link', { name: 'Top up' })).toHaveAttribute('href', '/billing');
  });
});

test.describe('a screen that cannot know does not guess', () => {
  test('a refusal by the server is an error with a retry, never an empty balance', async ({ page, host }) => {
    const scope = freshScope();
    await page.goto(screenUrl(host, scope, '&scope_realm=not-a-realm')); // the credential does not span this realm: the server refuses

    await expect(page.getByRole('alert')).toContainText('Could not load this balance');
    await expect(page.getByRole('button', { name: 'Try again' })).toBeVisible();
    await expect(page.locator('main')).not.toContainText('credits left'); // a refusal must not read as "0 credits left"
  });

  test('an unreachable server is the same error, and a retry reaches it once it is back', async ({ page, request, host }) => {
    const scope = freshScope();
    await funded(request, scope, 'ok');
    let down = true;
    await page.route('**/v1/credits**', (route) => (down ? route.abort('connectionrefused') : route.continue()));
    await page.goto(screenUrl(host, scope));

    await expect(page.getByRole('alert')).toContainText('Could not load this balance');
    await expect(page.locator('main')).not.toContainText('credits left');

    down = false;
    await page.getByRole('button', { name: 'Try again' }).click();
    await expect(page.getByText('4,999,340 credits left')).toBeVisible();
    await expect(page.getByRole('alert')).toHaveCount(0);
  });
});

test.describe('the deployment shape: the browser never holds the service credential', () => {
  test('no request the page makes carries it, and nothing the host serves contains it', async ({ page, request, host }) => {
    const scope = freshScope();
    await funded(request, scope, 'ok');
    const token = process.env.E2E_SERVICE_TOKEN ?? '';
    const sent: string[] = [];
    page.on('request', (r) => r.url().includes('/v1/') && sent.push(`${r.method()} ${new URL(r.url()).pathname} authorization=${r.headers()['authorization'] ?? 'none'}`));
    await page.goto(screenUrl(host, scope));
    await expect(page.getByText('4,999,340 credits left')).toBeVisible();

    expect(sent.length).toBeGreaterThan(0);
    for (const line of sent) expect(line, 'the browser must call its own backend, which holds the credential').toMatch(/authorization=none$/);

    const scripts = await page.locator('script[src]').evaluateAll((els) => els.map((e) => e.getAttribute('src') ?? ''));
    expect(scripts.length).toBeGreaterThan(0);
    for (const src of [...scripts, '/']) expect(await (await page.request.get(new URL(src, host.url).toString())).text()).not.toContain(token);
  });
});

test.describe('layout at real widths', () => {
  for (const width of [320, 390, 768, 1000]) {
    test(`${width}px: nothing makes the page wider than the screen`, async ({ page, request, host }) => {
      const scope = freshScope();
      await funded(request, scope, 'capped'); // the most content: every section, a banner, two ledger rows
      await page.setViewportSize({ width, height: 900 });
      await page.goto(screenUrl(host, scope));
      await expect(page.getByRole('row', { name: /llm\.chat/ }).first()).toBeVisible();

      const { scrollWidth, clientWidth } = await page.evaluate(() => ({ scrollWidth: document.documentElement.scrollWidth, clientWidth: document.documentElement.clientWidth }));
      expect(scrollWidth, `the page is ${scrollWidth}px wide in a ${clientWidth}px window`).toBeLessThanOrEqual(clientWidth);
    });
  }
});

test.describe('accessibility (axe-core, WCAG 2.2 AA and best practice)', () => {
  const audits: Array<[string, (r: Parameters<typeof grant>[0], s: Scope) => Promise<void>, string]> = [
    ['healthy', (r, s) => funded(r, s, 'ok'), ''],
    ['near the cap', (r, s) => funded(r, s, 'warn'), ''],
    ['blocked by the cap', (r, s) => funded(r, s, 'capped'), ''],
    ['blocked by the balance', unfunded, ''],
    ['refused', async () => {}, '&scope_realm=not-a-realm'],
  ];
  for (const [name, arrange, extra] of audits) {
    test(`${name}: no violations`, async ({ page, request, host }) => {
      const scope = freshScope();
      await arrange(request, scope);
      await page.goto(screenUrl(host, scope, extra));
      await expect(page.locator('.cu-panel')).toBeVisible();
      await expect(page.getByText('Loading')).toHaveCount(0);

      const { violations } = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice']).analyze();
      expect(violations.map((v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(' ')).join(' ; ')}`)).toEqual([]);
    });
  }
});
