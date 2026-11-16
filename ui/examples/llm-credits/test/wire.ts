// What the host's proxy returns, in the server's own shapes: GET /v1/credits is the UI contract's camelCase snapshot (its pool and limit
// "labels" are just their keys — naming them is the binding's job), and the payment routes are snake_case. The numbers and keys follow
// metering/adapters/driving/resthandler/credits_test.go and billing/adapters/driving/paymenthttp.
export const creditsWire = {
  scope: { realm: "acme", kind: "workspace", id: "w1" },
  balance: {
    remaining: 12_480,
    granted: 15_000,
    primaryLimitKey: "scope_balance",
    pools: [
      { key: "promo", label: "promo", remaining: 30, appliesTo: "llm.chat" },
      { key: "general", label: "general", remaining: 12_450 },
    ],
  },
  limits: [
    { key: "scope_balance", label: "scope_balance", used: 0, cap: 0, window: "none", warnAtPct: 80, denyCode: "payment_required" },
    { key: "org_daily", label: "org_daily", used: 870, cap: 5_000, window: "day", resetsAt: "2026-09-30T00:00:00Z", warnAtPct: 80, scopeHint: "all members" },
  ],
  breakdown: [],
  ledger: [
    { id: "id-1", operationType: "query", delta: -660, at: "2026-09-29T11:59:59.5Z", actor: { label: "u1" }, attributes: { model: "gpt-4o", tokens: "1800" }, pool: "general", resource: "llm.chat", rateKey: "gpt", rateCardVersion: "v1", idemKey: "evt-1", eventCount: 1 },
    { id: "id-2", operationType: "purchase", delta: 15_000, at: "2026-09-28T09:00:00Z", attributes: {}, pool: "general", idemKey: "pay-1", eventCount: 1 },
  ],
  ledgerNext: "cur-2",
};

export const plansWire = {
  data: [
    { code: "free", name: "Free", kind: "subscription", credit_allotment: 1_000, pool: "general", interval: "month", sort_order: 1, prices: { USD: 0 } },
    // a field the server never sends today, to show that nothing the binding does not name reaches the package
    { code: "pro", name: "Pro", kind: "subscription", credit_allotment: 50_000, pool: "general", interval: "month", sort_order: 2, prices: { USD: 2_500 }, provider_price_id: "price_SHOULD_NEVER_ARRIVE" },
    { code: "enterprise", name: "Enterprise", kind: "subscription", credit_allotment: null, pool: "general", sort_order: 3, prices: {} },
    { code: "topup-10", name: "10k top-up", kind: "one_time", credit_allotment: 10_000, pool: "general", sort_order: 4, prices: { USD: 1_000 } },
  ],
};

export const subscriptionWire = { plan_code: "free", status: "active", current_period_end: "2026-10-28T00:00:00Z", cancel_at_period_end: false, grace_until: null };

/** A fetch that answers by path, records what it was asked, and never touches a network. */
export interface Call {
  readonly url: URL;
  readonly init: RequestInit;
}
export function fakeFetch(routes: Record<string, (call: Call) => unknown>) {
  const calls: Call[] = [];
  const fn = async (input: RequestInfo | URL, init: RequestInit = {}): Promise<Response> => {
    const url = new URL(String(input), "https://app.example");
    const call = { url, init };
    calls.push(call);
    const route = routes[`${init.method ?? "GET"} ${url.pathname}`];
    if (route === undefined) return new Response(JSON.stringify({ error: { code: "not_found" } }), { status: 404 });
    return new Response(JSON.stringify(route(call)), { status: 200, headers: { "content-type": "application/json" } });
  };
  return { fetch: fn as typeof fetch, calls };
}

export const BASE = "/api/payments";
export const standardRoutes = {
  "GET /api/payments/v1/credits": () => creditsWire,
  "GET /api/payments/v1/plans": () => plansWire,
  "GET /api/payments/v1/subscription": () => subscriptionWire,
  "POST /api/payments/v1/checkout": () => ({ redirect_url: "https://pay.example/session/abc" }),
  "GET /api/payments/v1/fulfilment": () => ({ kind: "none" }),
  "GET /api/payments/v1/portal": () => ({ portal_url: "https://portal.example/p/1" }),
};
