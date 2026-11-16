// @vitest-environment jsdom
// R122 — the components, drawn from fixtures: one, three and five ceilings, pools, and three units, with no component edit (SC-012), and
// one named test per invariant of credits-ui-ports.md that a render can break (3, 7, 9, 11, 12, 13).
import { fireEvent, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { BalanceHero, Breakdown, LedgerTable, LimitMeters, PlanCatalogView, SpendChart } from "../src/index.js";
import { SnapshotError, deriveCatalog, derivePanel, formatWithUnit } from "../src/model.js";
import type { PlanCatalog, PlanOffer, UnitLabels } from "../src/ports.js";
import { catalog, ceilings, exhausted, L, llm, ledgerRows, modelColumn, offers, panelSnapshot, seats, storage } from "./fixtures.js";
import { int, q } from "./support.js";

const remedy = (code: string | undefined) => (code === undefined ? undefined : { label: "Upgrade your plan", href: "/plans" });

function Screen({ snap = panelSnapshot(5), labels = llm, plans = catalog() }: { snap?: ReturnType<typeof panelSnapshot>; labels?: UnitLabels; plans?: PlanCatalog }): ReactNode {
  const view = derivePanel(snap);
  return (
    <>
      <BalanceHero hero={view.hero} pools={view.pools} labels={labels} />
      <LimitMeters limits={view.limits} labels={labels} remedyFor={(l) => remedy(l.denyCode)} />
      <SpendChart spend={view.spend} labels={labels} />
      <Breakdown breakdown={view.breakdown} labels={labels} />
      <LedgerTable rows={view.ledger} columns={[modelColumn]} labels={labels} />
      <PlanCatalogView catalog={deriveCatalog(plans)} labels={labels} onSelect={() => undefined} />
    </>
  );
}

describe("SC-012 — the same components draw any number of ceilings, any pools and any unit", () => {
  test.each([1, 3, 5] as const)("%i ceiling(s): one item each, in the realm's order, and a meter for each that has a cap", (n) => {
    const view = derivePanel(panelSnapshot(n));
    render(<LimitMeters limits={view.limits} labels={llm} />);
    const items = within(screen.getByRole("list", { name: "Limits" })).getAllByRole("listitem");
    expect(items.map((li) => li.getAttribute("data-limit"))).toEqual(ceilings(n).map((l) => l.key));
    // the balance limit has no overdraft, so it has nothing to meter; every other capped limit has a meter
    expect(screen.queryAllByRole("progressbar")).toHaveLength(ceilings(n).filter((l) => l.cap !== null && !(l.window === "none" && l.cap === 0)).length);
  });

  test("a limit with no cap is a figure and no meter, and says it is uncapped", () => {
    const view = derivePanel(panelSnapshot(1, { limits: [L.uncapped], balance: { remaining: q(1), granted: q(1), primaryLimitKey: "period" } }));
    render(<LimitMeters limits={view.limits} labels={llm} />);
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.getByText(/No cap/)).toBeTruthy();
  });

  test("each window says what it is — a reset time, a per-call wall, a job, a balance — rather than one look for all", () => {
    const view = derivePanel(panelSnapshot(5));
    render(<LimitMeters limits={view.limits} labels={llm} />);
    const text = (key: string) => within(screen.getByRole("list", { name: "Limits" }).querySelector(`[data-limit="${key}"]`) as HTMLElement).getByText(/.+/, { selector: ".cu-limit-window" }).textContent;
    expect(text("user_daily")).toBe("Resets 2026-02-01 00:00:00 UTC");
    expect(text("run_cap")).toBe("Last call against the cap");
    expect(text("job")).toBe("For this job");
    expect(text("scope_balance")).toBe("Balance");
  });

  test("pools are one row each, in draw order, and add up to the figure beside them", () => {
    const view = derivePanel(panelSnapshot());
    const { container } = render(<BalanceHero hero={view.hero} pools={view.pools} labels={llm} />);
    const rows = [...container.querySelectorAll("[data-pool]")];
    expect(rows.map((r) => r.getAttribute("data-pool"))).toEqual(["promo", "general"]);
    expect(rows[0]!.textContent).toContain("Promotional");
    expect(rows[0]!.textContent).toContain("chat");
    const parts = rows.reduce((s, r) => s + Number(r.querySelector("[data-quantity]")!.getAttribute("data-quantity")), 0);
    expect(parts).toBe(Number(container.querySelector(".cu-hero-figure [data-quantity]")!.getAttribute("data-quantity")));
  });

  test("pools that do not add up get an explicit, labelled residual row (invariant 5)", () => {
    const snap = panelSnapshot(3);
    const view = derivePanel({ ...snap, balance: { ...snap.balance, remaining: q(13_000) } });
    const { container } = render(<BalanceHero hero={view.hero} pools={view.pools} labels={llm} />);
    const residual = container.querySelector('[data-residual="true"]')!;
    expect(residual.textContent).toContain("Unallocated");
    expect(residual.querySelector("[data-quantity]")!.getAttribute("data-quantity")).toBe("520");
    expect(container.querySelectorAll('[data-residual="true"]')).toHaveLength(1);
  });

  test.each([
    ["credits", llm, /12,480 credits left/, /of 15,000 credits granted/],
    ["seats", seats, /12,480 seats left/, /of 15,000 seats granted/],
    ["GB-months", storage, /12\.5 GB-months left/, /of 15\.0 GB-months granted/],
  ] as const)("the unit is the host's: %s", (_name, labels, remaining, granted) => {
    const { container } = render(<BalanceHero hero={derivePanel(panelSnapshot()).hero} pools={null} labels={labels} />);
    expect(container.textContent).toMatch(remaining);
    expect(container.textContent).toMatch(granted);
  });

  test("a screen in a non-credit unit says nothing about credits anywhere, in any component", () => {
    for (const labels of [seats, storage]) {
      const { container, unmount } = render(<Screen labels={labels} />);
      expect(container.textContent).not.toMatch(/credit/i);
      expect(container.textContent).toContain(labels.unit);
      unmount();
    }
  });

  test("display rounding never changes a value: '12.5' is drawn, 12,480 is carried", () => {
    const { container } = render(<BalanceHero hero={derivePanel(panelSnapshot()).hero} pools={null} labels={storage} />);
    const figure = container.querySelector(".cu-hero-figure [data-quantity]")!;
    expect(figure.textContent).toBe("12.5 GB-months");
    expect(figure.getAttribute("data-quantity")).toBe("12480");
  });

  test("the unit's plural is the host's: exactly one displayed unit is singular", () => {
    expect(formatWithUnit(q(1), seats)).toBe("1 seat");
    expect(formatWithUnit(q(2), seats)).toBe("2 seats");
    expect(formatWithUnit(q(0), seats)).toBe("0 seats");
    expect(formatWithUnit(q(1_000), storage)).toBe("1.0 GB-month");
    expect(formatWithUnit(q(1_001), storage)).toBe("1.0 GB-months");
    expect(formatWithUnit(q(5), llm, { compact: true })).toBe("5 cr");
  });
});

describe("invariant 1 in the hero — a headline that is a remainder, beside a bar that is consumption", () => {
  test("'12,480 left' sits beside a bar that is 16% — what has gone, not what is left", () => {
    render(<BalanceHero hero={derivePanel(panelSnapshot()).hero} pools={null} labels={llm} />);
    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("16");
    expect(bar.getAttribute("aria-valuetext")).toBe("2,520 of 15,000 cr used (16%)");
  });

  test("how long it lasts is said, or not claimed at all without a burn rate", () => {
    const { container, rerender } = render(<BalanceHero hero={derivePanel(panelSnapshot()).hero} pools={null} labels={llm} />);
    expect(container.querySelector('[data-cu="days-left"]')!.textContent).toBe("Runs out in about 12 days");
    const snap = panelSnapshot();
    rerender(<BalanceHero hero={derivePanel({ ...snap, balance: { ...snap.balance, burnRatePerDay: undefined as never } }).hero} pools={null} labels={llm} />);
    expect(container.querySelector('[data-cu="days-left"]')).toBeNull();
    expect(container.textContent).not.toMatch(/runs out/i);
  });

  test("near the limit it says so, with a way out, at the limit's own threshold", () => {
    const snap = panelSnapshot(1);
    const near = derivePanel({ ...snap, balance: { ...snap.balance, remaining: q(150), granted: q(1_000), pools: [] } });
    render(<BalanceHero hero={near.hero} pools={null} labels={llm} remedy={{ label: "Top up", href: "/plans" }} />);
    const note = screen.getByRole("status");
    expect(note.textContent).toContain("85% used");
    expect(within(note).getByRole("link", { name: "Top up" }).getAttribute("href")).toBe("/plans");
  });
});

describe("invariant 3 — an exhausted limit is never rendered as nothing", () => {
  test("it renders an alert carrying the denyCode and a remedy the person can follow", () => {
    const view = derivePanel(exhausted());
    render(<LimitMeters limits={view.limits} labels={llm} remedyFor={(l) => remedy(l.denyCode)} />);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Per-user daily reached");
    expect(alert.querySelector('[data-cu="deny-code"]')!.textContent).toBe("daily_cap");
    expect(within(alert).getByRole("link", { name: "Upgrade your plan" }).getAttribute("href")).toBe("/plans");
    expect(alert.closest("li")!.getAttribute("data-tone")).toBe("blocked");
  });

  test("with no code and no remedy from the host it still says so, and says to ask someone", () => {
    const snap = exhausted();
    const view = derivePanel({ ...snap, limits: [{ ...L.daily, used: q(5_000), denyCode: undefined as never }] , balance: { ...snap.balance, primaryLimitKey: "user_daily" } });
    render(<LimitMeters limits={view.limits} labels={llm} />);
    const alert = screen.getByRole("alert");
    expect(alert.querySelector('[data-cu="deny-code"]')).toBeNull();
    expect(alert.querySelector('[data-cu="remedy"]')!.textContent).toBe("Contact your administrator to continue.");
  });

  test("a balance that is spent is exhausted, though its own counter reads 0 of 0", () => {
    const snap = panelSnapshot(1);
    const view = derivePanel({ ...snap, balance: { ...snap.balance, remaining: q(0), pools: [] } });
    render(<LimitMeters limits={view.limits} labels={llm} remedyFor={(l) => remedy(l.denyCode)} />);
    expect(screen.getByRole("alert").querySelector('[data-cu="deny-code"]')!.textContent).toBe("insufficient_balance");
  });

  test("a healthy panel raises no alert — an alarm that is always on is not read", () => {
    render(<LimitMeters limits={derivePanel(panelSnapshot(5)).limits.filter((l) => l.tone === "ok")} labels={llm} />);
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
  });

  test("a limit near its threshold warns without alarming", () => {
    render(<LimitMeters limits={derivePanel(panelSnapshot(5)).limits} labels={llm} remedyFor={(l) => remedy(l.denyCode)} />);
    expect(screen.queryAllByRole("alert")).toHaveLength(0);
    expect(screen.getByRole("status").textContent).toContain("Hourly burst: 90% used");
  });
});

describe("invariant 7 — every series shows its label and its value as text, and colour is decoration", () => {
  test("each member's row says who it is and how much, and its swatch is hidden from assistive technology", () => {
    const view = derivePanel(panelSnapshot());
    const { container } = render(<Breakdown breakdown={view.breakdown} labels={llm} />);
    const rows = [...container.querySelectorAll("li")];
    expect(rows).toHaveLength(7); // six ramp slots and one Other
    ["chat", "search", "code", "image", "audio", "video"].forEach((label, i) => {
      expect(rows[i]!.textContent).toContain(`Label ${label}`);
      expect(rows[i]!.textContent).toContain(String([500, 400, 300, 200, 150, 100][i]));
      expect(rows[i]!.querySelector(".cu-swatch")!.getAttribute("aria-hidden")).toBe("true");
      expect(rows[i]!.getAttribute("data-slot")).toBe(String(i));
    });
    expect(rows[6]!.textContent).toContain("Other");
    expect(rows[6]!.textContent).toContain("100"); // docs 60 + misc 40
    expect(container.querySelector('[data-cu="total"]')!.textContent).toBe("Total 1,750 credits");
  });

  test("the chart is a line over text: a summary, and every point as a row", () => {
    const view = derivePanel(panelSnapshot());
    const { container } = render(<SpendChart spend={view.spend} labels={llm} />);
    expect(screen.getByRole("img").getAttribute("aria-label")).toBe("1,750 over 14 days, at most 125 in one day");
    expect(container.querySelector("figcaption")!.textContent).toBe("1,750 over 14 days, at most 125 in one day");
    expect(container.querySelectorAll("tbody tr")).toHaveLength(14);
  });

  test("the line is drawn inside its own box: no point on the edge, where half its stroke would be cut off", () => {
    const view = derivePanel(panelSnapshot(1, { series: [90, 120, 80, 170, 130, 40, 0].map((a, i) => ({ at: `2026-01-0${i + 1}T00:00:00Z`, amount: q(a) })) }));
    const { container } = render(<SpendChart spend={view.spend} labels={llm} />);
    const [w, h] = container.querySelector("svg")!.getAttribute("viewBox")!.split(" ").slice(2).map(Number) as [number, number];
    const pts = container.querySelector("polyline")!.getAttribute("points")!.split(" ").map((p) => p.split(",").map(Number) as [number, number]);
    expect(pts).toHaveLength(7);
    for (const [x, y] of pts) {
      expect(x).toBeGreaterThanOrEqual(2);
      expect(x).toBeLessThanOrEqual(w - 2);
      expect(y).toBeGreaterThanOrEqual(2);
      expect(y).toBeLessThanOrEqual(h - 2);
    }
    // the largest day is the highest point and the smallest the lowest; nothing is reordered
    const ys = pts.map(([, y]) => y);
    expect(ys.indexOf(Math.min(...ys))).toBe(3);
    expect(ys.indexOf(Math.max(...ys))).toBe(6);
  });

  test("a single day is a point in the middle, not on an edge", () => {
    const view = derivePanel(panelSnapshot(1, { series: [{ at: "2026-01-01T00:00:00Z", amount: q(50) }] }));
    const { container } = render(<SpendChart spend={view.spend} labels={llm} />);
    const [x] = container.querySelector("polyline")!.getAttribute("points")!.split(",").map(Number) as [number];
    expect(x).toBe(160);
  });

  test("no data is said in words, never drawn as an empty box", () => {
    const view = derivePanel(panelSnapshot(1, { series: undefined as never, breakdown: [] }));
    const { container } = render(<><SpendChart spend={view.spend} labels={llm} /><Breakdown breakdown={view.breakdown} labels={llm} /><LedgerTable rows={[]} labels={llm} /></>);
    expect(container.textContent).toContain("No spend in this window yet.");
    expect(container.textContent).toContain("No spend to break down yet.");
    expect(container.textContent).toContain("No activity yet.");
  });
});

describe("invariant 9 — every meter is a progressbar, with a value and a value in the host's words", () => {
  test("each carries aria-valuenow, aria-valuetext in the host's unit, and the same number the bar is painted from", () => {
    const view = derivePanel(panelSnapshot(5));
    render(<><BalanceHero hero={view.hero} pools={null} labels={seats} /><LimitMeters limits={view.limits} labels={seats} /></>);
    const bars = screen.getAllByRole("progressbar");
    expect(bars).toHaveLength(5); // the hero's, and four of the five ceilings (the balance has no overdraft to meter)
    for (const bar of bars) {
      expect(bar.getAttribute("aria-valuemin")).toBe("0");
      expect(bar.getAttribute("aria-valuemax")).toBe("100");
      expect(bar.getAttribute("aria-valuenow")).toMatch(/^\d+$/);
      expect(bar.getAttribute("aria-valuetext")).toMatch(/\d+ of (a )?[\d,]+ st\b/);
      expect(bar.getAttribute("aria-label")).toBeTruthy();
      expect(bar.style.getPropertyValue("--cu-meter-pct")).toBe(`${bar.getAttribute("aria-valuenow")}%`);
    }
    expect(screen.getByRole("progressbar", { name: "Hourly burst" }).getAttribute("aria-valuetext")).toBe("9 of 10 st used (90%)");
    expect(screen.getByRole("progressbar", { name: "Per-call output cap" }).getAttribute("aria-valuetext")).toBe("120 of a 500 st per-call cap (24%)");
    // the colour of a bar is its threshold state, and the bar says which state it is in
    expect(screen.getByRole("progressbar", { name: "Hourly burst" }).getAttribute("data-tone")).toBe("warn");
    expect(screen.getByRole("progressbar", { name: "Per-call output cap" }).getAttribute("data-tone")).toBe("ok");
  });
});

describe("invariant 10 is the panel's (R123); invariants 11 and 12 are the catalogue's", () => {
  const card = (code: string) => document.querySelector(`[data-offer="${code}"]`) as HTMLElement;

  test("11: a blocked offer shows its reason and remedy and exposes no checkout action, however the viewer's authority is", () => {
    for (const canPurchase of [true, false]) {
      const onSelect = vi.fn();
      const { unmount } = render(<PlanCatalogView catalog={deriveCatalog(catalog({ canPurchase }))} labels={llm} onSelect={onSelect} />);
      const lite = card("lite");
      expect(lite.getAttribute("data-state")).toBe("blocked");
      expect(within(lite).queryByRole("button")).toBeNull();
      expect(lite.querySelector('[data-action="checkout"]')).toBeNull();
      expect(lite.textContent).toContain("Lite has fewer seats than you use.");
      expect(within(lite).getByRole("link", { name: "Remove 2 seats" }).getAttribute("href")).toBe("/members");
      expect(onSelect).not.toHaveBeenCalled();
      unmount();
    }
  });

  test("11: whatever else is true of an offer, blocked wins", () => {
    const base: PlanOffer = { code: "x", name: "X", kind: "subscription", price: { minor: int(100), currency: "USD" }, creditAllotment: q(1), blocked: { reason: "no" } };
    for (const canPurchase of [true, false])
      for (const current of [true, false, undefined])
        for (const price of [base.price, null]) {
          const [offer] = deriveCatalog({ offers: [{ ...base, price, ...(current === undefined ? {} : { current }) }], canPurchase }).subscriptions;
          expect(offer!.action.kind).toBe("blocked");
        }
  });

  test("12: a viewer who cannot purchase is told why and where to ask, and every purchase control is inert", () => {
    const onSelect = vi.fn();
    render(<PlanCatalogView catalog={deriveCatalog(catalog({ canPurchase: false, cannotPurchaseReason: "Only an owner can buy.", requestPath: { label: "Ask an owner", href: "/request" } }))} labels={llm} onSelect={onSelect} />);
    const note = document.querySelector('[data-cu="cannot-purchase"]') as HTMLElement;
    expect(note.textContent).toContain("Only an owner can buy.");
    expect(within(note).getByRole("link", { name: "Ask an owner" }).getAttribute("href")).toBe("/request");
    const buy = [...document.querySelectorAll<HTMLButtonElement>('[data-action="checkout"]')];
    expect(buy.map((b) => b.closest("[data-offer]")!.getAttribute("data-offer"))).toEqual(["pro", "topup-10"]);
    for (const b of buy) {
      expect(b.disabled).toBe(true);
      expect(b.getAttribute("aria-disabled")).toBe("true");
      fireEvent.click(b);
    }
    expect(onSelect).not.toHaveBeenCalled();
  });

  test("12: with authority the same controls are live, and pressing one selects that plan", () => {
    const onSelect = vi.fn();
    render(<PlanCatalogView catalog={deriveCatalog(catalog())} labels={llm} onSelect={onSelect} />);
    expect(document.querySelector('[data-cu="cannot-purchase"]')).toBeNull();
    const pro = within(card("pro")).getByRole("button", { name: "Choose Pro" });
    expect((pro as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(pro);
    expect(onSelect).toHaveBeenCalledExactlyOnceWith("pro");
  });

  test("a checkout being started disables every control, so a second press is not a second session", () => {
    render(<PlanCatalogView catalog={deriveCatalog(catalog())} labels={llm} onSelect={() => undefined} pending="pro" />);
    for (const b of document.querySelectorAll<HTMLButtonElement>('[data-action="checkout"]')) expect(b.disabled).toBe(true);
    expect(within(card("pro")).getByRole("button").textContent).toBe("Taking you to checkout…");
  });

  test("an offer that is not self-serve is a way to ask, never a disabled buy button", () => {
    render(<PlanCatalogView catalog={deriveCatalog(catalog())} labels={llm} onSelect={() => undefined} />);
    const ent = card("enterprise");
    expect(within(ent).queryByRole("button")).toBeNull();
    expect(within(ent).getByRole("link", { name: "Talk to sales" }).getAttribute("href")).toBe("/contact");
    expect(ent.textContent).toContain("Custom allotment");
  });

  test("with nowhere to send them it says so in words, and offers no control", () => {
    const noContact: PlanCatalog = { offers, canPurchase: true };
    render(<PlanCatalogView catalog={deriveCatalog(noContact)} labels={llm} onSelect={() => undefined} />);
    const ent = card("enterprise");
    expect(within(ent).queryByRole("button")).toBeNull();
    expect(within(ent).queryByRole("link")).toBeNull();
    expect(ent.querySelector('[data-cu="not-self-serve"]')!.textContent).toBe("Not available to buy online.");
  });

  test("the current plan is inert text, never a button that does nothing", () => {
    render(<PlanCatalogView catalog={deriveCatalog(catalog())} labels={llm} onSelect={() => undefined} />);
    const free = card("free");
    expect(within(free).queryByRole("button")).toBeNull();
    expect(free.querySelector('[data-cu="current"]')!.textContent).toBe("Current plan");
  });

  test("subscriptions and one-time purchases are different lists, and prices are drawn in minor units without a float", () => {
    render(<PlanCatalogView catalog={deriveCatalog(catalog())} labels={llm} onSelect={() => undefined} />);
    expect([...within(screen.getByRole("region", { name: "Subscriptions" })).getAllByRole("heading", { level: 4 })].map((h) => h.textContent)).toEqual(["Free", "Pro", "Enterprise", "Lite"]);
    expect([...within(screen.getByRole("region", { name: "One-time purchases" })).getAllByRole("heading", { level: 4 })].map((h) => h.textContent)).toEqual(["10k top-up"]);
    expect(card("pro").querySelector('[data-cu="price"]')!.textContent).toBe("$25.00 / month");
    expect(card("topup-10").querySelector('[data-cu="price"]')!.textContent).toBe("$10.00");
    expect(card("pro").querySelector('[data-cu="allotment"]')!.textContent).toBe("50,000 credits included");
  });
});

describe("invariant 13 — a provider's identifier or card data never reaches the package", () => {
  const offer = (over: Record<string, unknown>): PlanCatalog => ({ canPurchase: true, offers: [{ code: "pro", name: "Pro", kind: "subscription", price: null, creditAllotment: null, ...over } as PlanOffer] });
  const refused = (c: PlanCatalog) => {
    try {
      deriveCatalog(c);
    } catch (e) {
      expect(e).toBeInstanceOf(SnapshotError);
      expect((e as SnapshotError).code).toBe("provider_identifier");
      return;
    }
    expect.unreachable("a provider identifier was accepted");
  };

  test("a field that carries one is refused, on the offer or inside it", () => {
    refused(offer({ provider_price_id: "price_1Nabc" }));
    refused(offer({ providerPriceId: "x" }));
    refused(offer({ priceId: "x" }));
    refused(offer({ stripe_price: "x" }));
    refused(offer({ customer_id: "cus_1" }));
    refused(offer({ payment_method: "pm_1" }));
    refused(offer({ cardLast4: "4242" }));
    refused(offer({ price: { minor: 100, currency: "USD", provider_price_id: "x" } }));
    refused(offer({ blocked: { reason: "r", provider_ref: "x" } }));
  });

  test("a code that is itself a provider's id is refused: a code is a host slug", () => {
    refused(offer({ code: "price_1NxZ2mABCDEFGHIJKLMNOPQR" }));
    refused(offer({ code: "prod_ABCDEFGHIJKLMNOP" }));
    refused(offer({ code: "3f2b8c1e-9d4a-4c7b-8e21-5a6f7b8c9d0e" }));
    refused(offer({ code: "P-5ML4271244454362WXNWU5NQ" }));
  });

  test("an ordinary slug is not mistaken for one", () => {
    for (const code of ["pro", "team-annual", "plan_pro", "plan_enterprise", "topup_10k", "price_list"]) {
      expect(deriveCatalog(offer({ code })).subscriptions.map((o) => o.code)).toEqual([code]);
    }
  });
});

describe("a catalogue that cannot be drawn honestly is refused", () => {
  const one = (over: Record<string, unknown>): PlanCatalog => ({ canPurchase: true, offers: [{ code: "pro", name: "Pro", kind: "subscription", price: null, creditAllotment: null, ...over } as PlanOffer] });
  const refused = (c: PlanCatalog, code: string) => {
    try {
      deriveCatalog(c);
    } catch (e) {
      expect(e).toBeInstanceOf(SnapshotError);
      expect((e as SnapshotError).code).toBe(code);
      return;
    }
    expect.unreachable(`expected ${code}`);
  };

  test("two offers with one code", () => {
    const first = one({}).offers[0]!;
    refused({ canPurchase: true, offers: [first, first] }, "invalid_offer");
  });

  test("a kind it does not know is not guessed at: kind is the one branch the package makes", () => {
    refused(one({ kind: "lifetime" }), "invalid_offer");
  });

  test("a price or an allotment that is not an integer, or is negative, or a currency that is not ISO 4217 upper case", () => {
    refused(one({ price: { minor: 19.99, currency: "USD" } }), "not_an_integer");
    refused(one({ price: { minor: -1, currency: "USD" } }), "invalid_offer");
    refused(one({ price: { minor: 100, currency: "usd" } }), "invalid_offer");
    refused(one({ price: { minor: 100, currency: "US" } }), "invalid_offer");
    refused(one({ creditAllotment: 1.5 }), "not_an_integer");
    refused(one({ creditAllotment: -1 }), "invalid_offer");
  });

  test("an offer, and a catalogue, with nothing in it is drawn as an empty list in words", () => {
    render(<PlanCatalogView catalog={deriveCatalog({ offers: [], canPurchase: true })} labels={llm} onSelect={() => undefined} />);
    expect(screen.getByText("No plans are available.")).toBeTruthy();
  });
});

describe("the ledger", () => {
  const draw = () => {
    const view = derivePanel(panelSnapshot());
    return render(<LedgerTable rows={view.ledger} columns={[modelColumn]} labels={llm} />);
  };
  const row = (id: string) => document.querySelector(`[data-row="${id}"]`) as HTMLElement;

  test("one signed column: a grant reads +50,000 and a consumption −42, and zero is a 0, never blank", () => {
    draw();
    const change = (id: string) => row(id).querySelector("td[data-direction]")!;
    expect([change("r1").textContent, change("r2").textContent, change("r3").textContent, change("r4").textContent]).toEqual(["+50,000", "−42", "−7", "0"]);
    expect([change("r1"), change("r2"), change("r4")].map((c) => c.getAttribute("data-direction"))).toEqual(["in", "out", "none"]);
    expect(change("r2").querySelector("[data-quantity]")!.getAttribute("data-quantity")).toBe("-42");
  });

  test("a replayed operation carries a deduped tag — how 'charged at most once' becomes visible — and only it does", () => {
    draw();
    expect(row("r2").querySelector('[data-cu="deduped"]')!.textContent).toBe("deduped");
    for (const id of ["r1", "r3", "r4"]) expect(row(id).querySelector('[data-cu="deduped"]')).toBeNull();
  });

  test("the operation type is shown as the opaque string it is, whatever the host calls it", () => {
    draw();
    expect(["r1", "r2", "r4"].map((id) => row(id).querySelector('[data-cu="operation"]')!.textContent)).toEqual(["purchase", "usage", "refused"]);
  });

  test("a registered column draws the rows it accepts, and an unaccepted row's cell is empty rather than an error", () => {
    draw();
    expect(screen.getByRole("columnheader", { name: "Model" })).toBeTruthy();
    expect(row("r2").textContent).toContain("model:m-large");
    expect(row("r3").textContent).not.toContain("model:");
  });

  test("a row no column claims still shows what it carries, as text, instead of hiding it", () => {
    draw();
    expect(row("r3").querySelector('[data-cu="generic"]')!.textContent).toContain("egress_gb");
    expect(row("r3").querySelector('[data-cu="generic"]')!.textContent).toContain("3");
    expect(row("r1").querySelector('[data-cu="generic"]')!.textContent).toContain("plan");
    expect(row("r2").querySelector('[data-cu="generic"]')).toBeNull(); // claimed by the model column
  });

  test("the ledger scrolls sideways on a narrow screen, so a keyboard must be able to reach it", () => {
    draw();
    const region = screen.getByRole("region", { name: "Activity" });
    expect(region.getAttribute("tabindex")).toBe("0");
  });

  test("an empty ledger has nothing to scroll and is not a tab stop", () => {
    render(<LedgerTable rows={[]} labels={llm} />);
    expect(screen.getByRole("region", { name: "Activity" }).hasAttribute("tabindex")).toBe(false);
  });

  test("the rows keep the order the server sent", () => {
    draw();
    expect([...document.querySelectorAll("[data-row]")].map((r) => r.getAttribute("data-row"))).toEqual(ledgerRows().map((r) => r.id));
  });
});

describe("a destination that could run script is not drawn as a link", () => {
  test("javascript:, data: and protocol-relative URLs are shown as text; https, mailto and paths are links", () => {
    for (const [href, link] of [
      ["javascript:alert(1)", false],
      ["JaVaScRiPt:alert(1)", false],
      ["data:text/html,<script>1</script>", false],
      ["//evil.example/x", false],
      ["https://example.com/plans", true],
      ["mailto:sales@example.com", true],
      ["/plans", true],
      ["#plans", true],
    ] as const) {
      const view = derivePanel(exhausted());
      const { unmount } = render(<LimitMeters limits={view.limits} labels={llm} remedyFor={() => ({ label: "Fix it", href })} />);
      const el = screen.getByText("Fix it");
      expect(el.tagName === "A" && el.getAttribute("href") === href).toBe(link);
      if (!link) expect(el.closest("a")).toBeNull();
      unmount();
    }
  });
});
