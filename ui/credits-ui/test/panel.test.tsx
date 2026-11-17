// @vitest-environment jsdom
// R123 — the composition root. The one thing it must never do is say a payment succeeded because the browser came back from checkout
// (invariant 10): fulfilment is webhook-driven, so `granted` is something the SERVER reports. A user who is told "credits added" while
// they are not, or "failed" while it is in flight, is a user who pays twice.
import fc from "fast-check";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { CreditsPanel } from "../src/index.js";
import { deriveFulfilment } from "../src/model.js";
import type { CheckoutSource, CreditsSnapshot, CreditsSource, FulfilmentState, Scope } from "../src/ports.js";
import { catalog, exhausted, llm, panelSnapshot } from "./fixtures.js";
import { q } from "./support.js";

const scope: Scope = { kind: "workspace", id: "w1" };
const never = <T,>() => new Promise<T>(() => undefined);

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

const advance = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));
const flush = () => advance(0);

function fakes(over: { fulfilment?: () => Promise<FulfilmentState>; snapshot?: CreditsSnapshot } = {}) {
  const source = { load: vi.fn<CreditsSource["load"]>(async () => over.snapshot ?? panelSnapshot(3)) };
  const checkout = {
    catalog: vi.fn<CheckoutSource["catalog"]>(async () => catalog()),
    begin: vi.fn<CheckoutSource["begin"]>(async () => ({ redirectUrl: "https://pay.example/session/1" })),
    fulfilment: vi.fn<CheckoutSource["fulfilment"]>(over.fulfilment ?? (async () => ({ kind: "none" }))),
    portalUrl: vi.fn<NonNullable<CheckoutSource["portalUrl"]>>(async () => "https://portal.example/p/1"),
  };
  const navigate = vi.fn();
  return { source, checkout, navigate };
}

type Fakes = ReturnType<typeof fakes>;
const panel = (f: Fakes, props: Record<string, unknown> = {}) => (
  <CreditsPanel source={f.source} scope={scope} labels={llm} checkout={f.checkout} navigate={f.navigate} fulfilmentPollMs={1_000} {...props} />
);
const fulfilmentNotice = () => document.querySelector<HTMLElement>('[data-cu="fulfilment"]');

describe("invariant 10 — granted is never inferred from a checkout return", () => {
  test("on return the panel renders processing at once, before the server has said anything", async () => {
    const f = fakes({ fulfilment: () => never() });
    render(panel(f, { returnedFromCheckout: true }));
    await flush();
    expect(fulfilmentNotice()!.getAttribute("data-state")).toBe("processing");
    expect(fulfilmentNotice()!.textContent).toMatch(/processing/i);
  });

  test("a source that never reports leaves it processing forever — ten minutes of polling, and never granted", async () => {
    const f = fakes();
    render(panel(f, { returnedFromCheckout: true }));
    await flush();
    for (let i = 0; i < 20; i++) await advance(30_000);
    expect(f.checkout.fulfilment.mock.calls.length).toBeGreaterThan(5);
    expect(fulfilmentNotice()!.getAttribute("data-state")).toBe("processing");
    expect(document.querySelector('[data-state="granted"]')).toBeNull();
  });

  test("a server that says it is processing is believed, and a server that cannot be reached is not taken for a failure", async () => {
    let n = 0;
    const f = fakes({ fulfilment: async () => (++n % 2 === 0 ? ({ kind: "processing", since: "2026-02-01T00:00:00Z" } as const) : Promise.reject(new Error("network"))) });
    render(panel(f, { returnedFromCheckout: true }));
    for (let i = 0; i < 6; i++) await advance(30_000);
    expect(n).toBeGreaterThan(3);
    expect(fulfilmentNotice()!.getAttribute("data-state")).toBe("processing");
  });

  test("granted appears only when fulfilment() reports it, with the amount in the host's unit and the receipt, and then it stops asking", async () => {
    let n = 0;
    const f = fakes({ fulfilment: async () => (++n < 3 ? { kind: "none" } : { kind: "granted", amount: q(50_000), receiptHref: "https://pay.example/receipt/1" }) });
    render(panel(f, { returnedFromCheckout: true }));
    await flush();
    expect(fulfilmentNotice()!.getAttribute("data-state")).toBe("processing");
    await advance(1_000);
    expect(fulfilmentNotice()!.getAttribute("data-state")).toBe("processing");
    await advance(2_000);
    const granted = fulfilmentNotice()!;
    expect(granted.getAttribute("data-state")).toBe("granted");
    expect(granted.textContent).toContain("50,000 credits");
    expect(within(granted).getByRole("link", { name: "View receipt" }).getAttribute("href")).toBe("https://pay.example/receipt/1");
    const calls = f.checkout.fulfilment.mock.calls.length;
    await advance(120_000);
    expect(f.checkout.fulfilment.mock.calls.length).toBe(calls);
  });

  test("a granted purchase refreshes the balance once, so the figure beside it catches up", async () => {
    const before = panelSnapshot(3);
    const after = panelSnapshot(3, { balance: { ...before.balance, remaining: q(62_480), granted: q(65_000) } });
    let loads = 0;
    const f = fakes({ fulfilment: async () => ({ kind: "granted", amount: q(50_000) }) });
    f.source.load.mockImplementation(async () => (++loads === 1 ? before : after));
    render(panel(f, { returnedFromCheckout: true }));
    await flush();
    await flush();
    expect(loads).toBe(2);
    expect(document.querySelector(".cu-hero-figure")!.textContent).toContain("62,480");
  });

  test("a failure is shown with its reason and a way to retry, and then it stops asking", async () => {
    const f = fakes({ fulfilment: async () => ({ kind: "failed", reason: "The card was declined.", retryHref: "/retry" }) });
    render(panel(f, { returnedFromCheckout: true }));
    await flush();
    const failed = fulfilmentNotice()!;
    expect(failed.getAttribute("data-state")).toBe("failed");
    expect(failed.textContent).toContain("The card was declined.");
    expect(within(failed).getByRole("link", { name: "Try again" }).getAttribute("href")).toBe("/retry");
    const calls = f.checkout.fulfilment.mock.calls.length;
    await advance(60_000);
    expect(f.checkout.fulfilment.mock.calls.length).toBe(calls);
  });

  test("without a return the server is not asked and nothing is claimed — an old purchase is not news on every page load", async () => {
    const f = fakes({ fulfilment: async () => ({ kind: "granted", amount: q(1) }) });
    render(panel(f));
    await advance(60_000);
    expect(f.checkout.fulfilment).not.toHaveBeenCalled();
    expect(fulfilmentNotice()).toBeNull();
  });

  test("by construction: the decision is granted exactly when the server said granted, whatever else is true", () => {
    const reported = fc.oneof(
      fc.constant(null),
      fc.constant({ kind: "none" } as const),
      fc.record({ kind: fc.constant("processing" as const), since: fc.constant("2026-01-01T00:00:00Z") }),
      fc.record({ kind: fc.constant("granted" as const), amount: fc.integer({ min: 0, max: 1_000_000 }).map(q) }),
      fc.record({ kind: fc.constant("failed" as const), reason: fc.string() }),
    );
    fc.assert(
      fc.property(fc.boolean(), reported, (returned, state) => {
        const view = deriveFulfilment(returned, state);
        expect(view?.kind === "granted").toBe(returned && state?.kind === "granted");
        expect(view === null).toBe(!returned);
        if (returned && (state === null || state.kind === "none" || state.kind === "processing")) expect(view?.kind).toBe("processing");
      }),
      { numRuns: 300 },
    );
  });

  test("beginning a checkout hands off and cannot grant: begin() resolves to a URL, the panel navigates, and claims nothing", async () => {
    const f = fakes();
    render(panel(f));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Choose Pro" }));
    await flush();
    expect(f.checkout.begin).toHaveBeenCalledExactlyOnceWith(scope, "pro");
    expect(f.navigate).toHaveBeenCalledExactlyOnceWith("https://pay.example/session/1");
    expect(fulfilmentNotice()).toBeNull();
    expect(document.querySelector('[data-state="granted"]')).toBeNull();
    expect(f.checkout.fulfilment).not.toHaveBeenCalled();
  });

  test("a second press while a checkout is starting is not a second session", async () => {
    const f = fakes();
    f.checkout.begin.mockImplementation(() => never());
    render(panel(f));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Choose Pro" }));
    await flush();
    for (const b of document.querySelectorAll<HTMLButtonElement>('[data-action="checkout"]')) {
      expect(b.disabled).toBe(true);
      fireEvent.click(b);
    }
    expect(f.checkout.begin).toHaveBeenCalledTimes(1);
  });

  test("a hand-off to a URL that could run script is refused and the person is told", async () => {
    const f = fakes();
    f.checkout.begin.mockResolvedValue({ redirectUrl: "javascript:alert(1)" });
    render(panel(f));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Choose Pro" }));
    await flush();
    expect(f.navigate).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toMatch(/could not start/i);
    expect((screen.getByRole("button", { name: "Choose Pro" }) as HTMLButtonElement).disabled).toBe(false);
  });

  test("a checkout that fails to start says so and frees the control", async () => {
    const f = fakes();
    f.checkout.begin.mockRejectedValue(new Error("boom"));
    render(panel(f));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Choose Pro" }));
    await flush();
    expect(f.navigate).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toMatch(/could not start/i);
    expect((screen.getByRole("button", { name: "Choose Pro" }) as HTMLButtonElement).disabled).toBe(false);
  });
});

describe("a panel is never silently empty", () => {
  test("while loading it says so; then it draws the hero, the ceilings, the ledger and the plans", async () => {
    const f = fakes();
    f.source.load.mockImplementation(() => never());
    render(panel(f));
    expect(screen.getByRole("status").textContent).toBe("Loading…");
  });

  test("a snapshot that loads is drawn whole", async () => {
    const f = fakes();
    render(panel(f));
    await flush();
    expect(screen.getByRole("region", { name: "Balance" })).toBeTruthy();
    expect(screen.getByRole("list", { name: "Limits" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Activity" })).toBeTruthy();
    expect(screen.getByRole("region", { name: "Plans" })).toBeTruthy();
    expect(f.source.load).toHaveBeenCalledExactlyOnceWith(scope);
  });

  test("a failed load is an alert with a retry, and retrying loads again", async () => {
    const f = fakes();
    f.source.load.mockRejectedValueOnce(new Error("503"));
    render(panel(f));
    await flush();
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Could not load");
    expect(screen.queryByRole("region", { name: "Balance" })).toBeNull();
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }));
    await flush();
    expect(f.source.load).toHaveBeenCalledTimes(2);
    expect(screen.getByRole("region", { name: "Balance" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  test("a snapshot the package will not draw is refused whole — nothing half-drawn, and the reason is shown", async () => {
    const bad = panelSnapshot(3);
    const f = fakes({ snapshot: { ...bad, limits: [{ ...bad.limits[0]!, pct: 40 } as never] } });
    render(panel(f));
    await flush();
    expect(screen.getByRole("alert").textContent).toMatch(/cannot be shown/i);
    expect(screen.getByRole("alert").textContent).toMatch(/pre-computed|percentage/i);
    expect(screen.queryByRole("region", { name: "Balance" })).toBeNull();
    expect(screen.queryByRole("list", { name: "Limits" })).toBeNull();
  });

  test("an exhausted limit reaches the screen with its remedy through the panel", async () => {
    const f = fakes({ snapshot: exhausted() });
    render(panel(f, { remedyFor: (l: { denyCode?: string }) => (l.denyCode === "daily_cap" ? { label: "Upgrade your plan", href: "/plans" } : undefined) }));
    await flush();
    const alert = screen.getByRole("alert");
    expect(alert.querySelector('[data-cu="deny-code"]')!.textContent).toBe("daily_cap");
    expect(within(alert).getByRole("link", { name: "Upgrade your plan" })).toBeTruthy();
  });

  test("notices the host attaches to the snapshot are shown", async () => {
    const f = fakes({ snapshot: panelSnapshot(3, { notices: [{ kind: "trial", title: "Trial limits apply", body: "Daily limit rises in 3 days." }] }) });
    render(panel(f));
    await flush();
    const note = document.querySelector('[data-cu="notice"]')!;
    expect(note.textContent).toContain("Trial limits apply");
    expect(note.textContent).toContain("Daily limit rises in 3 days.");
  });
});

describe("a person can tell what they are looking at", () => {
  const headings = () => screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);

  test("every section has a visible title, so an empty one still says what it is", async () => {
    const f = fakes();
    render(panel(f));
    await flush();
    expect(headings()).toEqual(["Limits", "Spend over time", "Spend breakdown", "Activity", "Billing"]);
  });

  test("and the titles are there when there is nothing to show", async () => {
    const f = fakes({ snapshot: panelSnapshot(1, { series: undefined as never, breakdown: [], ledger: [] }) });
    render(panel(f));
    await flush();
    expect(headings()).toEqual(["Limits", "Spend over time", "Spend breakdown", "Activity", "Billing"]);
    // no series was sent, so what the screen knows about spend is nothing: it says that, not that nothing was spent
    expect(document.body.textContent).toContain("Spend over time is not available here.");
    expect(document.body.textContent).not.toContain("No spend in this window yet.");
  });

  test("a screen with nothing to sell has no Billing title either", async () => {
    render(<CreditsPanel source={fakes().source} scope={scope} labels={llm} />);
    await flush();
    expect(headings()).toEqual(["Limits", "Spend over time", "Spend breakdown", "Activity"]);
  });
});

describe("the seams — what the host may leave out, or swap", () => {
  test("a product with no self-serve purchase passes no CheckoutSource: no plan affordance and no error", async () => {
    const f = fakes();
    render(<CreditsPanel source={f.source} scope={scope} labels={llm} />);
    await flush();
    expect(screen.queryByRole("region", { name: "Plans" })).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("region", { name: "Balance" })).toBeTruthy();
  });

  test("a billing anchor replaces the catalogue with a pointer, and the catalogue is not even asked for", async () => {
    const f = fakes({ snapshot: panelSnapshot(3, { anchor: { label: "Acme Corp", kind: "organization", href: "/orgs/acme", facts: [{ label: "Plan", value: "Team" }, { label: "Allocation", value: "10,000" }, { label: "Renews", value: "2026-03-01" }], rule: "Running out here pauses work here only." } }) });
    render(panel(f));
    await flush();
    const pointer = document.querySelector('[data-cu="anchor"]')!;
    expect(pointer.textContent).toContain("Acme Corp");
    expect(pointer.textContent).toContain("Team");
    expect(pointer.textContent).toContain("Running out here pauses work here only.");
    expect(within(pointer as HTMLElement).getByRole("link").getAttribute("href")).toBe("/orgs/acme");
    expect(screen.queryByRole("region", { name: "Plans" })).toBeNull();
    expect(f.checkout.catalog).not.toHaveBeenCalled();
  });

  test("a host's own plan card IS the billing section: drawn in place of the catalogue, which is not asked for", async () => {
    const f = fakes();
    render(panel(f, { billing: { planCard: <p data-cu="host-card">Managed by Acme</p> } }));
    await flush();
    expect(document.querySelector('[data-cu="host-card"]')!.textContent).toBe("Managed by Acme");
    expect(screen.queryByRole("region", { name: "Plans" })).toBeNull();
    expect(f.checkout.catalog).not.toHaveBeenCalled();
  });

  test("a catalogue that cannot be drawn is an alert in its own place — the balance above it still stands", async () => {
    const f = fakes();
    f.checkout.catalog.mockResolvedValue({ canPurchase: true, offers: [{ code: "pro", name: "Pro", kind: "subscription", price: null, creditAllotment: null, provider_price_id: "price_1Nabc" } as never] });
    render(panel(f));
    await flush();
    expect(screen.getByRole("region", { name: "Balance" })).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toMatch(/plans cannot be shown/i);
    expect(document.body.innerHTML).not.toContain("price_1Nabc");
  });

  test("the card on file stays with the provider: the portal link hands off and claims nothing", async () => {
    const f = fakes();
    render(panel(f));
    await flush();
    fireEvent.click(screen.getByRole("button", { name: "Manage billing" }));
    await flush();
    expect(f.navigate).toHaveBeenCalledExactlyOnceWith("https://portal.example/p/1");
  });

  test("a host that can push gets live updates, and its subscription ends with the panel", async () => {
    const f = fakes();
    let push: (s: CreditsSnapshot) => void = () => undefined;
    const unsubscribe = vi.fn();
    const source: CreditsSource = { ...f.source, subscribe: vi.fn((_s, on) => ((push = on), unsubscribe)) };
    const { unmount } = render(<CreditsPanel source={source} scope={scope} labels={llm} />);
    await flush();
    const next = panelSnapshot(3);
    act(() => push({ ...next, balance: { ...next.balance, remaining: q(777), pools: [] } }));
    expect(document.querySelector(".cu-hero-figure")!.textContent).toContain("777");
    unmount();
    expect(unsubscribe).toHaveBeenCalledTimes(1);
  });

  test("switching scope never shows the old scope's late answer under the new one", async () => {
    const answers = new Map<string, (s: CreditsSnapshot) => void>();
    const source: CreditsSource = { load: (s) => new Promise((res) => answers.set(s.id, res)) };
    const snap = (n: number) => panelSnapshot(1, { balance: { ...panelSnapshot(1).balance, remaining: q(n), pools: [] } });
    const { rerender } = render(<CreditsPanel source={source} scope={{ kind: "workspace", id: "A" }} labels={llm} />);
    rerender(<CreditsPanel source={source} scope={{ kind: "workspace", id: "B" }} labels={llm} />);
    await act(async () => answers.get("B")!(snap(2_222)));
    expect(document.querySelector(".cu-hero-figure")!.textContent).toContain("2,222");
    await act(async () => answers.get("A")!(snap(1_111)));
    expect(document.querySelector(".cu-hero-figure")!.textContent).toContain("2,222");
    expect(document.body.textContent).not.toContain("1,111");
  });
});
