import { test as base, expect, type APIRequestContext } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { bearer, uniqueScope } from './support';
import { startHost, type Host } from './ui-host';

/** The built reference page: `npm run build:host -w @intel-payment/example-llm-credits` in ui/. */
const PAGE_DIR = resolve(__dirname, '../../ui/examples/llm-credits/dist-host');

/**
 * These tests need a stack AND the credential its host would hold. Locally an unset variable skips, with the reason; in CI it fails, so a
 * stack started without a token cannot turn the whole project into silent skips.
 */
export function requireUiStack() {
  const missing = ['E2E_BASE_URL', 'E2E_SERVICE_TOKEN'].filter((v) => !process.env[v]);
  if (missing.length === 0) return;
  const why = `set ${missing.join(' and ')} (make up with PAYMENT_SERVICE_TOKENS holding the token's digest, then make e2e-seed)`;
  if (process.env.CI) throw new Error(`the credits UI tests ran without a stack: ${why}`);
  base.skip(true, why);
}

/** One host (page + forwarding proxy) per worker, on a free port, closed when the worker ends. */
export const test = base.extend<object, { host: Host }>({
  host: [
    async ({}, use) => {
      const host = await startHost({ upstream: process.env.E2E_BASE_URL ?? '', token: process.env.E2E_SERVICE_TOKEN ?? '', dir: PAGE_DIR });
      await use(host);
      await host.close();
    },
    { scope: 'worker' },
  ],
});
export { expect };

export interface Scope {
  readonly kind: string;
  readonly id: string;
}

export function freshScope(): Scope {
  const s = uniqueScope('workspace');
  return { kind: s.scope_kind, id: s.scope_id };
}

const headers = () => ({ ...bearer(), 'Content-Type': 'application/json', 'Idempotency-Key': `e2e-${randomUUID()}` });

/** Credits a scope, through the real API. */
export async function grant(request: APIRequestContext, scope: Scope, amount: number): Promise<void> {
  const res = await request.post('/v1/grant', { headers: headers(), data: { scope, amount, reason: 'e2e' } });
  expect(res.status(), await res.text()).toBe(200);
}

/** Spends from a scope through the real API, at the rate card `make e2e-seed` installs: 1,200 in + 800 out is 660 credits. */
export async function spend(request: APIRequestContext, scope: Scope, tokens: { in: number; out: number }, resource = 'llm.chat'): Promise<void> {
  const quantities = [
    ...(tokens.in > 0 ? [{ unit: 'in', amount: tokens.in }] : []),
    ...(tokens.out > 0 ? [{ unit: 'out', amount: tokens.out }] : []),
  ];
  const res = await request.post('/v1/record', {
    headers: headers(),
    // `attributes` is the host's own bag: the reference screen's Model and Tokens columns read these two keys, and the server returns them as sent
    data: { scope, resource, rate_key: 'gpt', quantities, attributes: { model: 'gpt-5', tokens: String(tokens.in + tokens.out) }, occurred_at: new Date().toISOString() },
  });
  expect(res.status(), await res.text()).toBe(200);
}

/**
 * Waits until the books hold `rows` rows for the scope. The balance is the hot copy and moves at once; the ledger is what the durable writer has
 * booked, a few milliseconds behind (constitution III), so a screen read straight after a write can show a balance that has moved beside a
 * ledger that has not. A test that asserts on the ledger waits for the books, through the same API the screen's ledger is read from.
 */
export async function settle(request: APIRequestContext, scope: Scope, rows: number): Promise<void> {
  await expect
    .poll(
      async () => {
        const res = await request.get('/v1/ledger', { headers: bearer(), params: { scope_kind: scope.kind, scope_id: scope.id } });
        return res.ok() ? ((await res.json()) as { data: unknown[] }).data.length : -1;
      },
      { message: `the writer to book ${rows} rows for ${scope.id}`, timeout: 20_000 },
    )
    .toBe(rows);
}

/**
 * Runs the usage rollup now. The worker does it every five minutes (the spend figures trail the ledger by that much), and a test that wants
 * them cannot wait; it asks the stack's own worker for the tick it would have run, the way an operator would: `payment-worker tick usage.rollup`
 * publishes it and the running worker does it under its claim. The stack is the compose one (`make up`).
 */
export function publishRollupTick(): void {
  const root = resolve(__dirname, '../..');
  const env = resolve(root, '.env');
  execFileSync(
    'docker',
    ['compose', '-f', resolve(root, 'deploy/docker-compose.yml'), ...(existsSync(env) ? ['--env-file', env] : []), 'exec', '-T', 'payment-worker', 'payment-worker', 'tick', 'usage.rollup'],
    { stdio: 'pipe', cwd: root },
  );
}

/** Waits until the rollup holds `total` credits of spend for the scope, as the snapshot the screen reads reports it. */
export async function rolledUp(request: APIRequestContext, scope: Scope, total: number): Promise<void> {
  publishRollupTick();
  await expect
    .poll(
      async () => {
        const res = await request.get('/v1/credits', { headers: bearer(), params: { scope_kind: scope.kind, scope_id: scope.id } });
        if (!res.ok()) return -1;
        const body = (await res.json()) as { series?: Array<{ amount: number }> };
        return (body.series ?? []).reduce((n, p) => n + p.amount, 0);
      },
      { message: `the rollup to hold ${total} credits of spend for ${scope.id}`, timeout: 30_000 },
    )
    .toBe(total);
}

/** The address of the screen for a scope on a host. */
export const screenUrl = (host: Host, scope: Scope, extra = '') => `${host.url}/?scope_kind=${encodeURIComponent(scope.kind)}&scope_id=${encodeURIComponent(scope.id)}${extra}`;
