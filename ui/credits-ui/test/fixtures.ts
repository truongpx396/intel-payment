// Fixtures for the render tests. Three units on purpose: the package must draw "credits", "seats" and "GB-months" with no edit.
import type { CreditsSnapshot, LedgerColumn, PlanCatalog, PlanOffer } from "../src/ports.js";
import { int, labels, limit, q, row, series, snapshot } from "./support.js";

export const llm = labels({ unit: "credits", one: "credit", abbr: "cr" });
export const seats = labels({ unit: "seats", one: "seat", abbr: "st" });
// 1,000 internal units to the displayed GB-month, one decimal place: 12,480 is "12.5" and is still 12,480
export const storage = labels({ unit: "GB-months", one: "GB-month", abbr: "GBm", scale: int(1_000), decimals: int(1) });

export const L = {
  balance: limit({ key: "scope_balance", label: "Workspace balance", window: "none", used: q(0), cap: q(0), denyCode: "insufficient_balance" }),
  daily: limit({ key: "user_daily", label: "Per-user daily", window: "day", used: q(1_200), cap: q(5_000), resetsAt: "2026-02-01T00:00:00Z", scopeHint: "you", denyCode: "daily_cap" }),
  call: limit({ key: "run_cap", label: "Per-call output cap", window: "call", used: q(120), cap: q(500), denyCode: "call_cap" }),
  hourly: limit({ key: "burst", label: "Hourly burst", window: "hour", used: q(9), cap: q(10), resetsAt: "2026-01-31T11:00:00Z", denyCode: "burst" }),
  job: limit({ key: "job", label: "This job", window: "job", used: q(40), cap: q(100) }),
  uncapped: limit({ key: "period", label: "Billing period", window: "period", used: q(7), cap: null }),
} as const;

export const ceilings = (n: 1 | 3 | 5) => ({ 1: [L.balance], 3: [L.balance, L.daily, L.call], 5: [L.balance, L.daily, L.call, L.hourly, L.job] })[n];

// 2,000 + 10,480 = 12,480, so the pools close; 2,520 of the 15,000 granted is gone (16%); 12,480 at 1,040 a day is 12 days
export function panelSnapshot(n: 1 | 3 | 5 = 3, over: Partial<CreditsSnapshot> = {}): CreditsSnapshot {
  return snapshot({
    balance: {
      remaining: q(12_480),
      granted: q(15_000),
      primaryLimitKey: "scope_balance",
      burnRatePerDay: q(1_040),
      pools: [
        { key: "promo", label: "Promotional", remaining: q(2_000), appliesTo: "chat" },
        { key: "general", label: "General", remaining: q(10_480) },
      ],
    },
    limits: ceilings(n),
    breakdown: [series("chat", 500), series("search", 400), series("code", 300), series("image", 200), series("audio", 150), series("video", 100), series("docs", 60), series("misc", 40)],
    series: Array.from({ length: 14 }, (_, i) => ({ at: `2026-01-${String(i + 1).padStart(2, "0")}T00:00:00Z`, amount: q(125) })),
    ledger: ledgerRows(),
    ...over,
  });
}

export const exhausted = () => panelSnapshot(3, { limits: [L.balance, { ...L.daily, used: q(5_000) }, L.call] });

export function ledgerRows() {
  return [
    row({ id: "r1", operationType: "purchase", delta: q(50_000), at: "2026-01-30T10:00:00Z", actor: { label: "Billing" }, attributes: { plan: "pro" } }),
    row({ id: "r2", operationType: "usage", delta: q(-42), at: "2026-01-30T11:00:00Z", idempotent: true, attributes: { model: "m-large", tokens: 1800 } }),
    row({ id: "r3", operationType: "usage", delta: q(-7), at: "2026-01-30T12:00:00Z", attributes: { egress_gb: "3" } }),
    row({ id: "r4", operationType: "refused", delta: q(0), at: "2026-01-30T13:00:00Z", attributes: {} }),
  ];
}

// A product registers its own columns; the package knows no key. "model" claims a row that has one.
export const modelColumn: LedgerColumn = { key: "model", header: "Model", accepts: (r) => "model" in r.attributes, render: (r) => `model:${String(r.attributes.model)}` };

export const offers: PlanOffer[] = [
  { code: "free", name: "Free", kind: "subscription", price: { minor: int(0), currency: "USD" }, interval: "month", creditAllotment: q(1_000), current: true },
  { code: "pro", name: "Pro", kind: "subscription", price: { minor: int(2_500), currency: "USD" }, interval: "month", creditAllotment: q(50_000), features: [{ label: "Seats", value: "5" }] },
  { code: "enterprise", name: "Enterprise", kind: "subscription", price: null, creditAllotment: null },
  {
    code: "lite",
    name: "Lite",
    kind: "subscription",
    price: { minor: int(500), currency: "USD" },
    creditAllotment: q(5_000),
    blocked: { reason: "Lite has fewer seats than you use.", remedy: { label: "Remove 2 seats", href: "/members" } },
  },
  { code: "topup-10", name: "10k top-up", kind: "one_time", price: { minor: int(1_000), currency: "USD" }, creditAllotment: q(10_000) },
];

export const catalog = (over: Partial<PlanCatalog> = {}): PlanCatalog => ({ offers, canPurchase: true, contact: { label: "Talk to sales", href: "/contact" }, ...over });
