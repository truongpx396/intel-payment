// The pure model of the UI package: a snapshot in, what a person is shown out.
//
// No React, no fetch, no store, no clock, no DOM — a .ts file in this package is framework-free by rule (eslint.config.js), and
// the tests run it in plain `node`. It NEVER interprets `unit`, `operationType` or an `attributes` key: a ledger row's type is a
// string it hands on, and its only reading of a delta is the sign.
//
// Arithmetic is BigInt wherever a product or a sum could leave the 53 bits a double keeps exactly: `used * 100` at the top of the
// range is where a float lands a threshold on the wrong side. Anything that would leave the safe-integer range is REFUSED, not
// wrapped or rounded — the same rule the books follow.
import type { BalanceView, BillingAnchor, CreditsSnapshot, Int, LedgerRow, LimitView, Quantity, Scope, UnitLabels } from "./ports.js";

/** How many categorical colours the ramp has. Members past this fold into one Other row; the host supplies the tokens (--cu-series-1…). */
export const SERIES_SLOTS = 6;

export type SnapshotErrorCode =
  | "not_an_integer"
  | "out_of_range"
  | "precomputed_percentage"
  | "unknown_primary_limit"
  | "invalid_limit"
  | "invalid_labels";

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
