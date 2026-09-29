import { expect, test } from '@playwright/test';
import { requireStack } from './support';

test.beforeEach(() => requireStack());

test.describe('platform endpoints', () => {
  test('/healthz answers while the process is up', async ({ request }) => {
    const res = await request.get('/healthz');
    expect(res.status()).toBe(200);
  });

  // FR-042: readiness is the migration gate. The stack under test was brought up by `make up`,
  // whose `migrate` service must have reached head before paymentd started.
  test('/readyz reports the schema at head and the configured shard count', async ({ request }) => {
    const res = await request.get('/readyz');
    expect(res.status(), await res.text()).toBe(200);
    expect(await res.json()).toMatchObject({
      status: 'ready',
      migrations: 'head',
      shards: expect.any(Number),
    });
  });

  test('/metrics is served', async ({ request }) => {
    const res = await request.get('/metrics', { headers: { Accept: 'text/plain' } });
    expect(res.status()).toBe(200);
  });
});
