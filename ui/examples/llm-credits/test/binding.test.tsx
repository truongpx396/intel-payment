// @vitest-environment jsdom
// R124 — the reference binding builds and renders against fixtures, and imports nothing outside its own package. It is the one place a
// product's vocabulary may live ("credits", the model and tokens columns, what a pool is called), and the proof that the generic package
// is enough: it talks to the package only through the package's entry point, as a host would.
import { fireEvent, render, screen, within } from "@testing-library/react";
import { SnapshotError, CreditsPanel, deriveCatalog, type CheckoutSource, type CreditsSnapshot, type LedgerColumn, type UnitLabels } from "@intel-payment/credits-ui";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { describe, expect, test } from "vitest";
import { LlmCreditsScreen, RestCheckoutSource, RestCreditsSource, WireError, llmLabels, modelColumn, tokensColumn, toFulfilment, toOffers, toSnapshot } from "../src/index.js";
import { BASE, creditsWire, fakeFetch, plansWire, standardRoutes } from "./wire.js";

const root = resolve(import.meta.dirname, "..");
const sources = (dir: string): string[] => readdirSync(dir).flatMap((f) => (statSync(join(dir, f)).isDirectory() ? sources(join(dir, f)) : /\.(ts|tsx)$/.test(f) ? [join(dir, f)] : []));
const specifiers = (file: string) => [...readFileSync(file, "utf8").matchAll(/(?:from|import)\s*\(?\s*["']([^"']+)["']/g)].map((m) => m[1]!);

describe("no import outside its own package", () => {
  const allowed = (spec: string, file: string): boolean => {
    if (spec.startsWith(".")) return !relative(root, resolve(dirname(file), spec)).startsWith("..");
    return ["@intel-payment/credits-ui", "@intel-payment/credits-ui/theme.css", "react", "react/jsx-runtime"].includes(spec);
  };

  test("every import is its own file, the package's entry point, or React — never the package's source, dist or a sibling", () => {
    const all = sources(join(root, "src")).flatMap((f) => specifiers(f).map((s) => ({ f: relative(root, f), s, ok: allowed(s, f) })));
    expect(all.length).toBeGreaterThan(5);
    expect(all.filter((x) => !x.ok)).toEqual([]);
    expect(all.filter((x) => /credits-ui\/(src|dist)|\.\.\/\.\.\//.test(x.s))).toEqual([]);
  });

  test("it declares only what it imports", () => {
    const pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8")) as { dependencies: Record<string, string> };
    expect(Object.keys(pkg.dependencies).sort()).toEqual(["@intel-payment/credits-ui", "react", "react-dom"]);
  });
});

describe("the wire, mapped once, at the edge", () => {
  test("a snapshot from the real shape: integers branded, the host's own names for pools and limits, nothing invented", () => {
    const s = toSnapshot(creditsWire);
    expect([s.balance.remaining, s.balance.granted]).toEqual([12_480, 15_000]);
    expect(s.balance.pools!.map((p) => [p.key, p.label, p.remaining])).toEqual([["promo", "Promotional", 30], ["general", "General", 12_450]]);
    expect(s.balance.pools![0]!.appliesTo).toBe("llm.chat");
    expect(s.limits.map((l) => [l.key, l.label, l.window, l.used, l.cap, l.warnAtPct])).toEqual([["scope_balance", "Workspace balance", "none", 0, 0, 80], ["org_daily", "Organisation daily", "day", 870, 5_000, 80]]);
    expect(s.limits[0]!.denyCode).toBe("payment_required");
    expect(s.limits[1]!.scopeHint).toBe("all members");
    expect(s.ledger.map((r) => [r.id, r.operationType, r.delta])).toEqual([["id-1", "query", -660], ["id-2", "purchase", 15_000]]);
    expect(s.ledger[0]!.attributes).toEqual({ model: "gpt-4o", tokens: "1800" });
    expect(s.ledger[0]!.actor).toEqual({ label: "u1" });
    expect(s.ledger[1]!.actor).toBeUndefined();
    // the server does not say whether a row was a replay (a replay writes no row), so the binding does not either
    expect(s.ledger.every((r) => r.idempotent === undefined)).toBe(true);
    expect(s.series).toBeUndefined();
    expect(s.anchor).toBeUndefined();
    expect(s.breakdown).toEqual([]);
  });

  test("a limit with no cap stays uncapped — null is not a cap of zero, which would read as exhausted", () => {
    const s = toSnapshot({ ...creditsWire, limits: [{ ...creditsWire.limits[1]!, cap: null }] });
    expect(s.limits[0]!.cap).toBeNull();
  });

  test("a key it has no name for is shown as readable words, not hidden", () => {
    const s = toSnapshot({ ...creditsWire, limits: [{ ...creditsWire.limits[1]!, key: "burst_per_minute", label: "burst_per_minute" }], balance: { ...creditsWire.balance, primaryLimitKey: "burst_per_minute" } });
    expect(s.limits[0]!.label).toBe("Burst per minute");
  });

  test("a fraction anywhere is refused, as the package refuses it — the wire is not rounded to fit", () => {
    for (const bad of [{ ...creditsWire, balance: { ...creditsWire.balance, remaining: 12_480.5 } }, { ...creditsWire, ledger: [{ ...creditsWire.ledger[0]!, delta: -0.5 }] }, { ...creditsWire, limits: [{ ...creditsWire.limits[1]!, cap: 1e21 }] }]) {
      expect(() => toSnapshot(bad)).toThrow(SnapshotError);
    }
  });

  test("a window the view has no word for, or a shape that is not a snapshot, is refused by name", () => {
    expect(() => toSnapshot({ ...creditsWire, limits: [{ ...creditsWire.limits[1]!, window: "weekly" }] })).toThrow(WireError);
    expect(() => toSnapshot({ ...creditsWire, limits: "none" })).toThrow(WireError);
    expect(() => toSnapshot(null)).toThrow(WireError);
    expect(() => toSnapshot({ ...creditsWire, balance: { ...creditsWire.balance, primaryLimitKey: 7 } })).toThrow(WireError);
  });

  test("plans: a price in the asked currency, the current plan marked, and no field the binding does not name reaches the package", () => {
    const offers = toOffers(plansWire, "USD", "free");
    expect(offers.map((o) => [o.code, o.kind, o.price, o.creditAllotment, o.current ?? false])).toEqual([
      ["free", "subscription", { minor: 0, currency: "USD" }, 1_000, true],
      ["pro", "subscription", { minor: 2_500, currency: "USD" }, 50_000, false],
      ["enterprise", "subscription", null, null, false],
      ["topup-10", "one_time", { minor: 1_000, currency: "USD" }, 10_000, false],
    ]);
    expect(JSON.stringify(offers)).not.toContain("price_SHOULD_NEVER_ARRIVE");
    for (const o of offers) expect(Object.keys(o).filter((k) => !["code", "name", "kind", "price", "interval", "creditAllotment", "current"].includes(k))).toEqual([]);
    expect(() => deriveCatalog({ offers, canPurchase: true })).not.toThrow();
  });

  test("a plan kind it does not know is refused, not guessed", () => {
    expect(() => toOffers({ data: [{ ...plansWire.data[0]!, kind: "lifetime" }] }, "USD", null)).toThrow(WireError);
  });

  test("fulfilment: each kind the server reports, and only those", () => {
    expect(toFulfilment({ kind: "none" })).toEqual({ kind: "none" });
    expect(toFulfilment({ kind: "processing", since: "2026-02-01T00:00:00Z" })).toEqual({ kind: "processing", since: "2026-02-01T00:00:00Z" });
    expect(toFulfilment({ kind: "granted", amount: 50_000, receipt_url: "https://pay.example/r/1" })).toEqual({ kind: "granted", amount: 50_000, receiptHref: "https://pay.example/r/1" });
    expect(toFulfilment({ kind: "failed", reason: "card declined" }, "/billing")).toEqual({ kind: "failed", reason: "card declined", retryHref: "/billing" });
    expect(() => toFulfilment({ kind: "granted", amount: 5.5 })).toThrow(SnapshotError);
    expect(() => toFulfilment({ kind: "granted" })).toThrow(WireError);
    expect(() => toFulfilment({ kind: "settled" })).toThrow(WireError);
  });
});

describe("the sources, over a fetch that is never a network", () => {
  const scope = { realm: "acme", kind: "workspace", id: "w1" };

  test("the snapshot is asked for by scope, through the host's own session — the browser holds no service credential", async () => {
    const f = fakeFetch(standardRoutes);
    const s = await new RestCreditsSource({ baseUrl: BASE, fetch: f.fetch }).load(scope);
    expect(s.balance.remaining).toBe(12_480);
    const [call] = f.calls;
    expect(call!.url.pathname).toBe("/api/payments/v1/credits");
    expect(Object.fromEntries(call!.url.searchParams)).toEqual({ scope_kind: "workspace", scope_id: "w1", scope_realm: "acme" });
    expect(call!.init.credentials).toBe("include");
    expect(JSON.stringify(call!.init.headers ?? {})).not.toMatch(/authorization|bearer/i);
  });

  test("a failed request is an error with its status, never an empty snapshot", async () => {
    const f = fakeFetch({});
    await expect(new RestCreditsSource({ baseUrl: BASE, fetch: f.fetch }).load(scope)).rejects.toThrow(/404/);
  });

  const checkout = (over: Partial<ConstructorParameters<typeof RestCheckoutSource>[0]> = {}, routes = standardRoutes) => {
    const f = fakeFetch(routes);
    return { f, c: new RestCheckoutSource({ baseUrl: BASE, fetch: f.fetch, currency: "USD", successUrl: "https://app.example/billing?checkout=done", cancelUrl: "https://app.example/billing", returnUrl: "https://app.example/billing", ...over }) };
  };

  test("the catalogue is the realm's plans in the currency, with the current plan marked, and the host's word on who may buy", async () => {
    const { c, f } = checkout({ canPurchase: false, cannotPurchaseReason: "Only owners can buy.", requestPath: { label: "Ask an owner", href: "/request" }, contact: { label: "Talk to sales", href: "/sales" } });
    const cat = await c.catalog(scope);
    expect(cat.offers.map((o) => o.code)).toEqual(["free", "pro", "enterprise", "topup-10"]);
    expect(cat.offers[0]!.current).toBe(true);
    expect([cat.canPurchase, cat.cannotPurchaseReason, cat.requestPath, cat.contact]).toEqual([false, "Only owners can buy.", { label: "Ask an owner", href: "/request" }, { label: "Talk to sales", href: "/sales" }]);
    expect(f.calls.find((x) => x.url.pathname.endsWith("/plans"))!.url.searchParams.get("currency")).toBe("USD");
  });

  test("who may buy is the host's to say, and without a word the catalogue is open", async () => {
    const { c } = checkout();
    expect((await c.catalog(scope)).canPurchase).toBe(true);
  });

  test("a scope that has never subscribed has no current plan, and that is not an error", async () => {
    const { c } = checkout({}, { ...standardRoutes, "GET /api/payments/v1/subscription": () => null as never });
    const cat = await c.catalog(scope);
    expect(cat.offers.some((o) => o.current === true)).toBe(false);
  });

  test("a checkout names a plan and never a price: the body is exactly the fields the server accepts, with an idempotency key", async () => {
    const { c, f } = checkout();
    await c.catalog(scope);
    expect(await c.begin(scope, "pro")).toEqual({ redirectUrl: "https://pay.example/session/abc" });
    const post = f.calls.find((x) => x.init.method === "POST")!;
    const body = JSON.parse(String(post.init.body)) as Record<string, unknown>;
    expect(Object.keys(body).sort()).toEqual(["cancel_url", "currency", "mode", "plan_code", "scope", "success_url"]);
    expect(body).toMatchObject({ plan_code: "pro", currency: "USD", mode: "subscription", scope: { kind: "workspace", id: "w1", realm: "acme" } });
    expect(new Headers(post.init.headers).get("idempotency-key")).toMatch(/^[0-9a-f-]{36}$/);
    expect(new Headers(post.init.headers).get("content-type")).toBe("application/json");
    await c.begin(scope, "topup-10");
    expect(JSON.parse(String(f.calls.filter((x) => x.init.method === "POST")[1]!.init.body))).toMatchObject({ mode: "payment" });
  });

  test("each press is its own checkout: two presses do not share a key", async () => {
    const { c, f } = checkout();
    await c.catalog(scope);
    await c.begin(scope, "pro");
    await c.begin(scope, "pro");
    const keys = f.calls.filter((x) => x.init.method === "POST").map((x) => new Headers(x.init.headers).get("idempotency-key"));
    expect(new Set(keys).size).toBe(2);
  });

  test("a plan it was never shown is not started", async () => {
    const { c } = checkout();
    await c.catalog(scope);
    await expect(c.begin(scope, "made-up")).rejects.toThrow(/made-up/);
  });

  test("fulfilment and the portal are the server's, asked for and mapped", async () => {
    const { c, f } = checkout({}, { ...standardRoutes, "GET /api/payments/v1/fulfilment": () => ({ kind: "granted", amount: 10_000 }) });
    expect(await c.fulfilment(scope)).toEqual({ kind: "granted", amount: 10_000 });
    expect(await c.portalUrl(scope)).toBe("https://portal.example/p/1");
    expect(f.calls.find((x) => x.url.pathname.endsWith("/portal"))!.url.searchParams.get("return_url")).toBe("https://app.example/billing");
  });
});

describe("the screen, rendered", () => {
  const scope = { realm: "acme", kind: "workspace", id: "w1" };
  const flush = () => screen.findByRole("region", { name: "Balance" });

  test("it draws the LLM product: credits, the model and tokens columns, the host's names, and the plans", async () => {
    const f = fakeFetch(standardRoutes);
    render(<LlmCreditsScreen baseUrl={BASE} scope={scope} currency="USD" fetch={f.fetch} />);
    const hero = await flush();
    expect(hero.textContent).toContain("12,480 credits");
    expect(hero.textContent).toMatch(/of 15,000 credits granted/);
    expect(within(hero).getByText("Promotional")).toBeTruthy();
    expect(screen.getByText("Organisation daily")).toBeTruthy();
    expect(screen.getByText("Workspace balance")).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Model" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Tokens" })).toBeTruthy();
    expect(screen.getByText("gpt-4o")).toBeTruthy();
    expect(screen.getByText("1,800")).toBeTruthy();
    const plans = await screen.findByRole("region", { name: "Plans" });
    expect(within(plans).getByRole("heading", { name: "Pro" })).toBeTruthy();
    expect(plans.textContent).toContain("$25.00 / month");
    expect(plans.textContent).toContain("Current plan");
  });

  test("an exhausted balance is an alert with the host's remedy, and a daily cap that resets says to wait", async () => {
    const spent = { ...creditsWire, balance: { ...creditsWire.balance, remaining: 0, pools: [{ key: "general", label: "general", remaining: 0 }] } };
    const f = fakeFetch({ ...standardRoutes, "GET /api/payments/v1/credits": () => spent });
    render(<LlmCreditsScreen baseUrl={BASE} scope={scope} currency="USD" fetch={f.fetch} topUpHref="/billing/top-up" />);
    await flush();
    const alert = screen.getByRole("alert");
    expect(alert.querySelector('[data-cu="deny-code"]')!.textContent).toBe("payment_required");
    expect(within(alert).getByRole("link", { name: "Top up" }).getAttribute("href")).toBe("/billing/top-up");
  });

  test("under an organisation it points at the organisation instead of selling a plan: the catalogue is not even asked for", async () => {
    const f = fakeFetch(standardRoutes);
    render(<LlmCreditsScreen baseUrl={BASE} scope={scope} currency="USD" fetch={f.fetch} organisation={{ name: "Acme Corp", href: "/orgs/acme", plan: "Team", allocation: "10,000 credits", renews: "2026-03-01" }} />);
    await flush();
    const card = document.querySelector('[data-example="org-plan"]')!;
    expect(card.textContent).toContain("Acme Corp");
    expect(card.textContent).toContain("Team");
    expect(within(card as HTMLElement).getByRole("link").getAttribute("href")).toBe("/orgs/acme");
    expect(screen.queryByRole("region", { name: "Plans" })).toBeNull();
    expect(f.calls.some((c) => c.url.pathname.endsWith("/plans"))).toBe(false);
  });

  test("pressing a plan hands the browser to the provider, and the screen claims nothing", async () => {
    const f = fakeFetch(standardRoutes);
    const go: string[] = [];
    render(<LlmCreditsScreen baseUrl={BASE} scope={scope} currency="USD" fetch={f.fetch} navigate={(u) => go.push(u)} />);
    await flush();
    fireEvent.click(await screen.findByRole("button", { name: "Choose Pro" }));
    await screen.findByRole("region", { name: "Plans" });
    await new Promise((r) => setTimeout(r, 20));
    expect(go).toEqual(["https://pay.example/session/abc"]);
    expect(document.querySelector('[data-state="granted"]')).toBeNull();
  });
});

describe("a different product is a different binding, and the package is not edited", () => {
  test("the same panel draws GB-months and an egress column, with no credits in sight", async () => {
    const storage: UnitLabels = { unit: "GB-months", one: "GB-month", abbr: "GBm" };
    const egress: LedgerColumn = { key: "egress", header: "Egress", accepts: (r) => "egress_gb" in r.attributes, render: (r) => `${String(r.attributes.egress_gb)} GB` };
    const snap = toSnapshot({ ...creditsWire, ledger: [{ ...creditsWire.ledger[0]!, attributes: { egress_gb: "3" } }] });
    const none = { catalog: async () => ({ offers: [], canPurchase: true }), begin: async () => ({ redirectUrl: "/" }), fulfilment: async () => ({ kind: "none" as const }) } satisfies CheckoutSource;
    const { container } = render(<CreditsPanel source={{ load: async (): Promise<CreditsSnapshot> => snap }} scope={{ kind: "bucket", id: "b1" }} labels={storage} columns={[egress]} checkout={none} />);
    await screen.findByRole("region", { name: "Balance" });
    expect(screen.getByRole("columnheader", { name: "Egress" })).toBeTruthy();
    expect(screen.getByText("3 GB")).toBeTruthy();
    expect(container.textContent).toContain("12,480 GB-months");
    expect(container.textContent).not.toMatch(/credit/i);
  });

  test("the LLM columns claim only the rows that carry their key, and a token count is grouped", () => {
    const row = (attributes: Record<string, unknown>) => ({ id: "x", operationType: "query", delta: -1 as never, at: "2026-01-01T00:00:00Z", attributes });
    expect([modelColumn.accepts(row({ model: "m" })), modelColumn.accepts(row({})), tokensColumn.accepts(row({ tokens: "1800" })), tokensColumn.accepts(row({ model: "m" }))]).toEqual([true, false, true, false]);
    const { container } = render(<>{tokensColumn.render(row({ tokens: "1234567" })) as never}</>);
    expect(container.textContent).toBe("1,234,567");
    const odd = render(<>{tokensColumn.render(row({ tokens: "n/a" })) as never}</>);
    expect(odd.container.textContent).toBe("n/a");
    expect(llmLabels).toMatchObject({ unit: "credits", one: "credit", abbr: "cr" });
  });
});
