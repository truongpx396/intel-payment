import { test } from '@playwright/test';
import { randomUUID } from 'node:crypto';

/** Skip (loudly, with the reason) unless a stack is pointed at. CI always sets E2E_BASE_URL. */
export function requireStack() {
  test.skip(!process.env.E2E_BASE_URL, 'set E2E_BASE_URL to a running stack (make up) to run e2e tests');
}

/**
 * A scope no other test will ever touch, so parallel tests never contend on a balance and a re-run
 * never sees the previous run's state.
 */
export function uniqueScope(kind = 'e2e') {
  return { scope_kind: kind, scope_id: `${kind}-${randomUUID()}` };
}

/** The service credential the stack was started with (PAYMENT_SERVICE_TOKENS), if any. */
export function bearer(): Record<string, string> {
  const t = process.env.E2E_SERVICE_TOKEN;
  return t ? { Authorization: `Bearer ${t}` } : {};
}
