// The pure model of the UI package: a snapshot in, what a person is shown out.
//
// No React, no fetch, no store, no clock, no DOM — a .ts file in this package is framework-free by rule (eslint.config.js), and
// the tests run it in plain `node`. It NEVER interprets `unit`, `operationType` or an `attributes` key: a ledger row's type is a
// string it hands on, and its only reading of a delta is the sign.
//
// Arithmetic is BigInt wherever a product or a sum could leave the 53 bits a double keeps exactly: `used * 100` at the top of the
// range is where a float lands a threshold on the wrong side. Anything that would leave the safe-integer range is REFUSED, not
// wrapped or rounded — the same rule the books follow.
import type { BalanceView, BillingAnchor, CreditsSnapshot, FulfilmentState, Int, LedgerRow, LimitView, PlanCatalog, PlanOffer, Quantity, Scope, UnitLabels } from "./ports.js";

/** How many categorical colours the ramp has. Members past this fold into one Other row; the host supplies the tokens (--cu-series-1…). */
export const SERIES_SLOTS = 6;

export type SnapshotErrorCode =
  | "not_an_integer"
  | "out_of_range"
  | "precomputed_percentage"
  | "unknown_primary_limit"
  | "invalid_limit"
  | "invalid_labels"
  | "provider_identifier"
  | "invalid_offer";

/** A snapshot this package will not draw. Loud on purpose: a money screen that half-renders bad data is how a wrong figure is believed. */
export class SnapshotError extends Error {
  readonly code: SnapshotErrorCode;
  readonly path: string;
  constructor(code: SnapshotErrorCode, path: string, message: string) {
    super(`${path}: ${message}`);
    this.name = "SnapshotError";
    this.code = code;
    this.path = path;
  }
}

// ── integers ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────

const MAX = BigInt(Number.MAX_SAFE_INTEGER);

/** Makes a number a Quantity, or refuses it: a float, NaN, an infinity or an integer a double cannot hold exactly is never rounded into one. */
export function quantity(n: number, path = "quantity"): Quantity {
  if (!Number.isSafeInteger(n)) {
    throw new SnapshotError("not_an_integer", path, `${String(n)} is not a safe integer — money is integer end to end and is never rounded to fit`);
  }
  return (n === 0 ? 0 : n) as Quantity; // -0 is 0
}

function exact(v: bigint, path: string): Quantity {
  if (v > MAX || v < -MAX) throw new SnapshotError("out_of_range", path, `${v} does not fit in a safe integer — refused, not wrapped`);
  return Number(v) as Quantity;
}

const sum = (xs: readonly number[]) => xs.reduce((s, x) => s + BigInt(x), 0n);

// ── formatting ────────────────────────────────────────────────────────────────────────────────────────────────────────────────

function display(labels: UnitLabels): { scale: bigint; decimals: number } {
  const scale = labels.scale ?? 1;
  const decimals = labels.decimals ?? 0;
  if (!Number.isSafeInteger(scale) || scale < 1) throw new SnapshotError("invalid_labels", "labels.scale", `${String(scale)} is not a positive integer`);
  if (!Number.isSafeInteger(decimals) || decimals < 0 || decimals > 18) throw new SnapshotError("invalid_labels", "labels.decimals", `${String(decimals)} is not an integer from 0 to 18`);
  return { scale: BigInt(scale), decimals };
}

/**
 * A Quantity as the host shows it: the internal figure divided by `scale`, to `decimals` places, the whole part grouped. Integer
 * division and a remainder, rounding half away from zero — never a float. The value this is drawn from is untouched: the text is a
 * view of it, and every figure the components draw also carries the exact integer.
 */
export function formatQuantity(q: Quantity, labels: UnitLabels): string {
  if (labels.format) return labels.format(q);
  const { scale, decimals } = display(labels);
  const n = BigInt(quantity(q));
  const negative = n < 0n;
  const unit = 10n ** BigInt(decimals);
  const scaled = (negative ? -n : n) * unit;
  let whole = scaled / scale;
  if ((scaled % scale) * 2n >= scale) whole += 1n;
  const text = (whole / unit).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",") + (decimals > 0 ? `.${(whole % unit).toString().padStart(decimals, "0")}` : "");
  return negative && whole !== 0n ? `−${text}` : text; // U+2212, and never "−0.00"
}

/** A ledger delta: consumption reads "−42" and a grant "+50,000", so both land in one column. Zero is "0", never blank. */
export function formatSigned(q: Quantity, labels: UnitLabels): string {
  const text = formatQuantity(q, labels);
  return q > 0 ? `+${text}` : text;
}

/** "seat" for exactly one displayed unit, "seats" otherwise — the host's words, and the host's plural. */
export function unitName(q: Quantity, labels: UnitLabels): string {
  const { scale } = display(labels);
  const n = BigInt(quantity(q));
  return (n < 0n ? -n : n) === scale ? labels.one : labels.unit;
}

/** A figure with its unit: "12,480 seats", or "12,480 cr" when compact and the host gave an abbreviation. */
export function formatWithUnit(q: Quantity, labels: UnitLabels, options: { readonly compact?: boolean } = {}): string {
  const name = options.compact === true && labels.abbr !== undefined ? labels.abbr : unitName(q, labels);
  return `${formatQuantity(q, labels)} ${name}`;
}

/**
 * A price in minor units, in its currency's own exponent (USD 2, JPY 0), with integer arithmetic: 1999 is "$19.99", never 1999 / 100.
 * A currency Intl does not know is shown by its code rather than guessed at.
 */
export function formatMinor(minor: Int, currency: string): string {
  let exponent = 2;
  let symbol = `${currency} `;
  try {
    const f = new Intl.NumberFormat("en-US", { style: "currency", currency, currencyDisplay: "narrowSymbol" });
    exponent = f.resolvedOptions().maximumFractionDigits ?? 2;
    symbol = f.formatToParts(0).find((p) => p.type === "currency")?.value ?? symbol;
  } catch {
    // not a currency Intl knows: keep the code
  }
  return symbol + formatQuantity(quantity(minor, "price.minor"), { unit: "", one: "", scale: 10 ** exponent as Int, decimals: exponent as Int });
}

// ── the view ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────

export type Tone = "ok" | "warn" | "blocked";

/** What a meter is drawn from. `pct` is derived here and only here, and is a whole percent that is 100 exactly when the cap is reached. */
export interface MeterModel {
  readonly used: Quantity;
  readonly cap: Quantity;
  readonly pct: number;
}

export interface LimitModel {
  readonly key: string;
  readonly label: string;
  readonly window: LimitView["window"];
  /** How to read it: a window that resets, a job that drains, a per-call wall, or a balance with an optional overdraft. */
  readonly kind: "window" | "job" | "call" | "balance";
  readonly used: Quantity;
  readonly cap: Quantity | null;
  readonly tone: Tone;
  readonly meter: MeterModel | null;
  readonly warnAtPct: Int;
  readonly resetsAt?: string;
  readonly denyCode?: string;
  readonly scopeHint?: string;
  /** Present exactly when the limit is at its cap: an exhausted limit is never rendered as nothing. */
  readonly blocking: { readonly denyCode?: string } | null;
}

export interface HeroModel {
  readonly remaining: Quantity;
  readonly granted: Quantity;
  readonly tone: Tone;
  readonly meter: MeterModel | null;
  /** What the fill is of: the primary limit's own counter, or — for a balance, whose counter is its overdraft — the share of what was granted that is gone. */
  readonly basis: "limit" | "granted" | null;
  readonly primaryKey: string | null;
  readonly warnAtPct: Int | null;
  readonly daysLeft: number | null;
  readonly blocking: { readonly denyCode?: string } | null;
}

export interface PoolRow {
  readonly key: string;
  readonly label: string;
  readonly amount: Quantity;
  readonly appliesTo?: string;
  /** True for the explicit row that closes the sum when the pools do not add up to the balance. */
  readonly residual: boolean;
}
export interface PoolsModel {
  readonly rows: readonly PoolRow[];
  readonly total: Quantity;
}

export interface SpendModel {
  readonly points: ReadonlyArray<{ readonly at: string; readonly amount: Quantity }>;
  readonly total: Quantity;
  readonly peak: Quantity;
}

export interface BreakdownRow {
  readonly key: string;
  readonly label: string;
  readonly amount: Quantity;
  /** The ramp slot, by the member's index in the list the host supplied — not by rank or size. Null for Other and for the residual. */
  readonly slot: number | null;
  readonly kind: "series" | "other" | "residual";
}
export interface BreakdownModel {
  readonly total: Quantity;
  readonly rows: readonly BreakdownRow[];
}

export interface LedgerModel {
  readonly id: string;
  readonly operationType: string;
  readonly delta: Quantity;
  readonly direction: "in" | "out" | "none";
  readonly at: string;
  readonly actor?: LedgerRow["actor"];
  readonly idempotent: boolean;
  readonly attributes: LedgerRow["attributes"];
}

export interface PanelView {
  readonly scope: Scope;
  readonly hero: HeroModel;
  readonly limits: readonly LimitModel[];
  readonly pools: PoolsModel | null;
  readonly spend: SpendModel | null;
  readonly breakdown: BreakdownModel;
  readonly ledger: readonly LedgerModel[];
  readonly anchor: BillingAnchor | null;
  readonly notices: NonNullable<CreditsSnapshot["notices"]>;
}

// ── a ceiling ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────

// Any of these on a limit is a percentage computed somewhere else. The fill is this package's to derive from used / cap (invariant 1),
// and one that arrives pre-computed is how a bar and the figure beside it come to disagree.
const PERCENT_KEY = /^(pct|percent|percentage|fill|fillpct|fillpercent|usedpct|usedpercent|pctused|percentused)$/i;

const WINDOWS: readonly LimitView["window"][] = ["period", "day", "hour", "job", "call", "none"];

function meterOf(used: number, cap: number): MeterModel {
  const pct = used >= cap ? 100 : Number((BigInt(used) * 100n) / BigInt(cap));
  return { used: used as Quantity, cap: cap as Quantity, pct };
}

/**
 * One ceiling as a person reads it. `remaining` is the scope's balance: a balance limit's own `used` is the overdraft it is into, so
 * whether it blocks cannot be read from it — the engine refuses when what is left plus the permitted overdraft is nothing, and so
 * does this.
 */
export function deriveLimit(l: LimitView, remaining: Quantity): LimitModel {
  const path = `limits[${l.key}]`;
  for (const k of Object.keys(l)) {
    if (PERCENT_KEY.test(k)) throw new SnapshotError("precomputed_percentage", `${path}.${k}`, "a percentage was supplied — the fill is derived from used and cap, never accepted");
  }
  const used = quantity(l.used, `${path}.used`);
  const cap = l.cap === null ? null : quantity(l.cap, `${path}.cap`);
  const warn = quantity(l.warnAtPct, `${path}.warnAtPct`);
  const left = quantity(remaining, "balance.remaining");
  if (used < 0) throw new SnapshotError("invalid_limit", `${path}.used`, "a counter cannot be negative");
  if (cap !== null && cap < 0) throw new SnapshotError("invalid_limit", `${path}.cap`, "a ceiling cannot be negative");
  if (warn < 0 || warn > 100) throw new SnapshotError("invalid_limit", `${path}.warnAtPct`, "a threshold is a whole percent from 0 to 100");

  if (!WINDOWS.includes(l.window)) throw new SnapshotError("invalid_limit", `${path}.window`, `"${String(l.window)}" is not a window this view has a word for`);

  const balance = l.window === "none";
  const blocked = cap !== null && (balance ? BigInt(left) + BigInt(cap) <= 0n : used >= cap);
  const meter = cap === null || (balance && cap === 0) ? null : meterOf(used, cap);
  // warnAtPct is the limit's own; 0 means no warning (the engine warns only when WarnAt > 0 and Max > 0)
  const warns = cap !== null && cap > 0 && warn > 0 && BigInt(used) * 100n >= BigInt(warn) * BigInt(cap);
  const tone: Tone = blocked ? "blocked" : warns ? "warn" : "ok";

  return {
    key: l.key,
    label: l.label,
    window: l.window,
    kind: balance ? "balance" : l.window === "call" ? "call" : l.window === "job" ? "job" : "window",
    used,
    cap,
    tone,
    meter,
    warnAtPct: warn as Int,
    ...(l.resetsAt === undefined ? {} : { resetsAt: l.resetsAt }),
    ...(l.denyCode === undefined ? {} : { denyCode: l.denyCode }),
    ...(l.scopeHint === undefined ? {} : { scopeHint: l.scopeHint }),
    blocking: blocked ? (l.denyCode === undefined ? {} : { denyCode: l.denyCode }) : null,
  };
}

// ── the hero ──────────────────────────────────────────────────────────────────────────────────────────────────────────────────

function deriveHero(b: BalanceView, limits: readonly LimitModel[]): HeroModel {
  const remaining = quantity(b.remaining, "balance.remaining");
  const granted = quantity(b.granted, "balance.granted");
  const burn = b.burnRatePerDay === undefined ? null : quantity(b.burnRatePerDay, "balance.burnRatePerDay");
  const primary = b.primaryLimitKey === "" ? undefined : limits.find((l) => l.key === b.primaryLimitKey);
  if (b.primaryLimitKey !== "" && primary === undefined) {
    throw new SnapshotError("unknown_primary_limit", "balance.primaryLimitKey", `"${b.primaryLimitKey}" names no limit in the snapshot`);
  }
  const daysLeft = burn !== null && burn > 0 ? (remaining > 0 ? Number(BigInt(remaining) / BigInt(burn)) : 0) : null;
  const base = { remaining, granted, primaryKey: primary?.key ?? null, daysLeft };

  if (primary !== undefined && primary.kind !== "balance") {
    return { ...base, tone: primary.tone, meter: primary.meter, basis: "limit", warnAtPct: primary.warnAtPct, blocking: primary.blocking };
  }
  // A balance — or no limit at all. A balance's own counter is its overdraft, which is empty while the account is healthy, so the
  // hero's bar is the share of what was granted that is gone: the same reading ("consumed, never remainder") the other meters have.
  const consumed = BigInt(granted) > BigInt(remaining) ? BigInt(granted) - BigInt(remaining) : 0n;
  const meter: MeterModel | null = granted > 0 ? meterOf(exact(consumed, "balance.consumed"), granted) : null;
  const warn = primary?.warnAtPct ?? null;
  const blocked = primary?.tone === "blocked";
  const warns = meter !== null && warn !== null && warn > 0 && consumed * 100n >= BigInt(warn) * BigInt(granted);
  return {
    ...base,
    tone: blocked ? "blocked" : warns ? "warn" : "ok",
    meter,
    basis: meter === null ? null : "granted",
    warnAtPct: warn,
    blocking: primary?.blocking ?? null,
  };
}

// ── the parts that sit beside a total ─────────────────────────────────────────────────────────────────────────────────────────

const RESIDUAL_KEY = "__residual";
const OTHER_KEY = "__other";

function derivePools(b: BalanceView, total: Quantity): PoolsModel | null {
  const pools = b.pools ?? [];
  if (pools.length === 0) return null;
  const rows: PoolRow[] = pools.map((p, i) => ({
    key: p.key,
    label: p.label,
    amount: quantity(p.remaining, `balance.pools[${i}].remaining`),
    ...(p.appliesTo === undefined ? {} : { appliesTo: p.appliesTo }),
    residual: false,
  }));
  // The pools and the balance are two figures the server computes; if they ever disagree the screen says so instead of closing the gap quietly.
  const gap = BigInt(total) - sum(rows.map((r) => r.amount));
  if (gap !== 0n) rows.push({ key: RESIDUAL_KEY, label: "", amount: exact(gap, "balance.pools"), residual: true });
  return { rows, total };
}

function deriveBreakdown(s: CreditsSnapshot): { spend: SpendModel | null; breakdown: BreakdownModel } {
  const points = s.series?.map((p, i) => ({ at: p.at, amount: quantity(p.amount, `series[${i}].amount`) }));
  const amounts = s.breakdown.map((m, i) => quantity(m.amount, `breakdown[${i}].amount`));
  const spendTotal = points === undefined ? null : exact(sum(points.map((p) => p.amount)), "series");
  const parts = sum(amounts);
  const total = spendTotal ?? exact(parts, "breakdown");

  const rows: BreakdownRow[] = s.breakdown.slice(0, SERIES_SLOTS).map((m, i) => ({ key: m.key, label: m.label, amount: amounts[i]!, slot: i, kind: "series" }));
  if (s.breakdown.length > SERIES_SLOTS) {
    rows.push({ key: OTHER_KEY, label: "", amount: exact(sum(amounts.slice(SERIES_SLOTS)), "breakdown[other]"), slot: null, kind: "other" });
  }
  const gap = BigInt(total) - parts;
  if (gap !== 0n) rows.push({ key: RESIDUAL_KEY, label: "", amount: exact(gap, "breakdown"), slot: null, kind: "residual" });

  const peak = points === undefined ? 0 : points.reduce((m, p) => (p.amount > m ? p.amount : m), 0);
  return {
    spend: points === undefined || spendTotal === null ? null : { points, total: spendTotal, peak: peak as Quantity },
    breakdown: { total, rows },
  };
}

// ── the whole snapshot ────────────────────────────────────────────────────────────────────────────────────────────────────────

/** Everything a person is shown, from a snapshot and nothing else. Throws a SnapshotError for a snapshot that cannot be shown. */
export function derivePanel(s: CreditsSnapshot): PanelView {
  const remaining = quantity(s.balance.remaining, "balance.remaining");
  const seen = new Set<string>();
  for (const l of s.limits) {
    if (seen.has(l.key)) throw new SnapshotError("invalid_limit", `limits[${l.key}]`, "two limits share a key");
    seen.add(l.key);
  }
  const limits = s.limits.map((l) => deriveLimit(l, remaining));
  const { spend, breakdown } = deriveBreakdown(s);
  return {
    scope: s.scope,
    hero: deriveHero(s.balance, limits),
    limits,
    pools: derivePools(s.balance, remaining),
    spend,
    breakdown,
    ledger: s.ledger.map((r, i) => {
      const delta = quantity(r.delta, `ledger[${i}].delta`);
      return {
        id: r.id,
        operationType: r.operationType,
        delta,
        direction: delta > 0 ? "in" : delta < 0 ? "out" : "none",
        at: r.at,
        ...(r.actor === undefined ? {} : { actor: r.actor }),
        idempotent: r.idempotent === true,
        attributes: r.attributes,
      };
    }),
    anchor: s.anchor ?? null,
    notices: s.notices ?? [],
  };
}

// ── the plan catalogue ────────────────────────────────────────────────────────────────────────────────────────────────────────

export interface Link {
  readonly label: string;
  readonly href: string;
}

/**
 * What an offer gets, decided here and not in a template. `blocked` has no checkout and no way to ask for one: the block exists to
 * stop a change that would silently shrink someone else's budget (invariant 11). `checkout` with `inert` is a purchase that is
 * valid for someone with authority, shown disabled to a viewer without it (invariant 12). `contact` is an offer that is not
 * self-serve — a way to ask, never a disabled buy button.
 */
export type OfferAction =
  | { readonly kind: "checkout"; readonly inert: boolean }
  | { readonly kind: "contact" }
  | { readonly kind: "current" }
  | { readonly kind: "blocked"; readonly reason: string; readonly remedy: Link | null };

export interface OfferModel {
  readonly code: string;
  readonly name: string;
  readonly kind: PlanOffer["kind"];
  readonly tagline?: string;
  readonly price: PlanOffer["price"];
  readonly interval?: "month" | "year";
  readonly allotment: Quantity | null;
  readonly features: NonNullable<PlanOffer["features"]>;
  readonly action: OfferAction;
}

export interface CatalogModel {
  readonly subscriptions: readonly OfferModel[];
  readonly oneTime: readonly OfferModel[];
  readonly canPurchase: boolean;
  /** Present exactly when the viewer may browse but not buy: the reason, and where to ask. */
  readonly viewerNotice: { readonly reason: string | null; readonly request: Link | null } | null;
  readonly contact: Link | null;
}

// A provider's own vocabulary on an offer — a price id, a customer, a payment method, a vendor's name. The package has no use for any
// of it and a UI that holds one is a UI that can leak one (invariant 13), so one arriving is refused rather than ignored.
const PROVIDER_KEY = /stripe|polar|paypal|provider|price_?id|customer|payment_?method|card/i;
// and a `code` that is itself one: a Stripe object id, a UUID (Polar's), a PayPal plan id. A host slug is none of these.
const PROVIDER_ID = /^((price|prod|cus|pm|sub|cs|pi|ch)_[A-Za-z0-9]{14,}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|P-[0-9A-Z]{18,})$/;

function noProviderKeys(o: object, path: string): void {
  for (const k of Object.keys(o)) {
    if (PROVIDER_KEY.test(k)) throw new SnapshotError("provider_identifier", `${path}.${k}`, "a provider's identifier or card data reached the UI — it never belongs here");
  }
}

const link = (l: { readonly label: string; readonly href: string } | undefined): Link | null => (l === undefined ? null : { label: l.label, href: l.href });

/** The plan catalogue as a person is offered it. Refuses an offer the package must not draw (a provider's id, an unknown kind, a price that is not an integer). */
export function deriveCatalog(c: PlanCatalog): CatalogModel {
  const seen = new Set<string>();
  const offers: OfferModel[] = c.offers.map((o, i) => {
    const path = `offers[${i}]`;
    noProviderKeys(o, path);
    if (o.price !== null) noProviderKeys(o.price, `${path}.price`);
    if (o.blocked !== undefined) noProviderKeys(o.blocked, `${path}.blocked`);
    if (PROVIDER_ID.test(o.code)) throw new SnapshotError("provider_identifier", `${path}.code`, "a code is a host slug, and this one is a provider's identifier");
    if (o.kind !== "subscription" && o.kind !== "one_time") throw new SnapshotError("invalid_offer", `${path}.kind`, `"${String(o.kind)}" is neither a subscription nor a one-time purchase`);
    if (seen.has(o.code)) throw new SnapshotError("invalid_offer", `${path}.code`, `two offers share the code "${o.code}"`);
    seen.add(o.code);
    const allotment = o.creditAllotment === null ? null : quantity(o.creditAllotment, `${path}.creditAllotment`);
    if (allotment !== null && allotment < 0) throw new SnapshotError("invalid_offer", `${path}.creditAllotment`, "an allotment cannot be negative");
    if (o.price !== null) {
      const minor = quantity(o.price.minor, `${path}.price.minor`);
      if (minor < 0) throw new SnapshotError("invalid_offer", `${path}.price.minor`, "a price cannot be negative");
      if (!/^[A-Z]{3}$/.test(o.price.currency)) throw new SnapshotError("invalid_offer", `${path}.price.currency`, "a currency is an upper-case ISO 4217 code");
    }
    const action: OfferAction =
      o.blocked !== undefined
        ? { kind: "blocked", reason: o.blocked.reason, remedy: link(o.blocked.remedy) }
        : o.current === true
          ? { kind: "current" }
          : o.price === null
            ? { kind: "contact" }
            : { kind: "checkout", inert: !c.canPurchase };
    return {
      code: o.code,
      name: o.name,
      kind: o.kind,
      ...(o.tagline === undefined ? {} : { tagline: o.tagline }),
      price: o.price,
      ...(o.interval === undefined ? {} : { interval: o.interval }),
      allotment,
      features: o.features ?? [],
      action,
    };
  });
  return {
    subscriptions: offers.filter((o) => o.kind === "subscription"),
    oneTime: offers.filter((o) => o.kind === "one_time"),
    canPurchase: c.canPurchase,
    viewerNotice: c.canPurchase ? null : { reason: c.cannotPurchaseReason ?? null, request: link(c.requestPath) },
    contact: link(c.contact),
  };
}

// ── fulfilment ────────────────────────────────────────────────────────────────────────────────────────────────────────────────

/** What a person is told about a payment they just made. */
export type FulfilmentView =
  | { readonly kind: "processing" }
  | { readonly kind: "granted"; readonly amount: Quantity; readonly receiptHref?: string }
  | { readonly kind: "failed"; readonly reason: string; readonly retryHref?: string };

/**
 * Invariant 10, as a function. Fulfilment is webhook-driven, so a return from checkout proves only that the browser came back: the honest
 * state on return is "processing", and `granted` is something the SERVER reports. `granted` is the result exactly when the server said it
 * was — never because time passed, a poll was slow, or the redirect looked successful. A user told "added" while it is not, or "failed"
 * while it is in flight, is a user who pays twice. Not on return (`returned` false) nothing is claimed at all.
 */
export function deriveFulfilment(returned: boolean, reported: FulfilmentState | null): FulfilmentView | null {
  if (!returned) return null;
  if (reported?.kind === "granted") {
    return { kind: "granted", amount: quantity(reported.amount, "fulfilment.amount"), ...(reported.receiptHref === undefined ? {} : { receiptHref: reported.receiptHref }) };
  }
  if (reported?.kind === "failed") {
    return { kind: "failed", reason: reported.reason, ...(reported.retryHref === undefined ? {} : { retryHref: reported.retryHref }) };
  }
  return { kind: "processing" }; // not heard yet, nothing, or processing: all the same to the person — it is not done
}
