import { expect, test } from '@playwright/test';
import { requireStack, uniqueScope } from './support';

test.beforeEach(() => requireStack());

// rest-api.md: /v1/* is for a host's BACKEND, behind a service credential. A metering API a browser
// (or anyone) can reach unauthenticated is an unmetered-spend API, so the refusal is a test, not a hope.
test.describe('/v1 requires a service credential', () => {
  const routes: Array<[method: 'GET' | 'POST', path: string]> = [
    ['POST', '/v1/admit'],
    ['POST', '/v1/record'],
    ['POST', '/v1/grant'],
    ['POST', '/v1/transfer'],
    ['GET', '/v1/balance'],
    ['GET', '/v1/ledger'],
  ];

  for (const [method, path] of routes) {
    test(`${method} ${path} without a credential is 401 unauthenticated`, async ({ request }) => {
      const res = await request.fetch(path, {
        method,
        headers: { 'Idempotency-Key': `e2e-${Date.now()}` },
        data: method === 'POST' ? { ...uniqueScope(), amount: 1 } : undefined,
      });
      expect(res.status()).toBe(401);
      expect(await res.json()).toMatchObject({ error: { code: 'unauthenticated' } });
    });
  }

  test('a wrong bearer token is refused, not treated as anonymous', async ({ request }) => {
    const res = await request.get('/v1/balance', {
      headers: { Authorization: 'Bearer not-a-real-token' },
      params: uniqueScope(),
    });
    expect(res.status()).toBe(401);
  });
});
