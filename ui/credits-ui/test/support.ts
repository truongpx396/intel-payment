// Test support: builders and generators for snapshots. Not shipped, and not part of what a test asserts — the brands here are
// plain casts so that generating a value never depends on the `quantity()` under test.
import fc from "fast-check";
import type { BreakdownSeries, CreditsSnapshot, Int, LedgerRow, LimitView, Quantity, UnitLabels } from "../src/ports.js";

export const q = (n: number): Quantity => n as Quantity;
export const int = (n: number): Int => n as Int;

export const labels = (over: Partial<UnitLabels> = {}): UnitLabels => ({ unit: "units", one: "unit", ...over });

export function limit(over: Partial<LimitView> = {}): LimitView {
  return { key: "k", label: "A ceiling", used: q(0), cap: q(100), window: "day", warnAtPct: int(80), ...over };
}

export function snapshot(over: Partial<CreditsSnapshot> = {}): CreditsSnapshot {
  return {
    scope: { kind: "workspace", id: "w1" },
    balance: { remaining: q(100), granted: q(200), primaryLimitKey: "" },
    limits: [],
    breakdown: [],
    ledger: [],
    ...over,
  };
}

export function row(over: Partial<LedgerRow> = {}): LedgerRow {
  return { id: "r1", operationType: "usage", delta: q(-1), at: "2026-01-01T00:00:00Z", attributes: {}, ...over };
}

export const series = (key: string, amount: number): BreakdownSeries => ({ key, label: `Label ${key}`, amount: q(amount) });

// Integers as large as the type allows: the arithmetic that decides a threshold must not lose a digit at the top of the range.
export const anyQuantity = fc.oneof(fc.integer({ min: -1_000_000, max: 1_000_000 }), fc.integer({ min: -Number.MAX_SAFE_INTEGER, max: Number.MAX_SAFE_INTEGER }));
export const anyAmount = fc.oneof(fc.integer({ min: 0, max: 1_000_000 }), fc.integer({ min: 0, max: Number.MAX_SAFE_INTEGER }));
export const anyPct = fc.integer({ min: 0, max: 100 });
export const windowedWindow = fc.constantFrom("period", "day", "hour", "job", "call") as fc.Arbitrary<Exclude<LimitView["window"], "none">>;

// A cap and a use that are often close, because that is where a threshold is decided.
export const usedAndCap = fc.oneof(
  fc.tuple(anyAmount, anyAmount),
  fc.integer({ min: 0, max: Number.MAX_SAFE_INTEGER }).chain((cap) =>
    fc.tuple(fc.integer({ min: Math.max(0, cap - 3), max: Math.min(Number.MAX_SAFE_INTEGER, cap + 3) }), fc.constant(cap)),
  ),
  fc.integer({ min: 1, max: 1000 }).chain((cap) => fc.tuple(fc.integer({ min: 0, max: cap + 2 }), fc.constant(cap))),
);
