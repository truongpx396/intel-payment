// R121 — the pure model: what a person is shown, derived from a snapshot and nothing else (credits-ui-ports.md, invariants 1, 2, 4, 5).
//
// Run in `node`, not a browser: if the model needed a DOM it would not be pure. The references below are written independently, in
// BigInt, so they do not share the arithmetic of the thing they check — which matters at the top of the integer range, where a
// float `used * 100` silently loses its last digits and a threshold lands on the wrong side.
import fc from "fast-check";
import { describe, expect, test } from "vitest";
import type { CreditsSnapshot, LimitView } from "../src/ports.js";
import { SERIES_SLOTS, SnapshotError, derivePanel, deriveLedgerRows, deriveLimit, formatQuantity, formatSigned, quantity } from "../src/model.js";
import { anyAmount, anyPct, anyQuantity, int, labels, limit, q, row, series, snapshot, usedAndCap, windowedWindow } from "./support.js";

const big = (n: number) => BigInt(n);
const refPct = (used: number, cap: number) => (used >= cap ? 100 : Number((big(used) * 100n) / big(cap)));
const refTone = (used: number, cap: number, warn: number) =>
  used >= cap ? "blocked" : cap > 0 && warn > 0 && big(used) * 100n >= big(warn) * big(cap) ? "warn" : "ok";

function refuses(fn: () => unknown, code: string) {
  try {
    fn();
  } catch (e) {
    expect(e).toBeInstanceOf(SnapshotError);
    expect((e as SnapshotError).code).toBe(code);
    return;
  }
  expect.unreachable(`expected a SnapshotError(${code}), got a value`);
}

describe("invariant 1 — the fill encodes consumption, derived from used / cap", () => {
  test("for any limit the fill is floor(used / cap), and it is full exactly when the cap is reached", () => {
    fc.assert(
      fc.property(usedAndCap, windowedWindow, ([used, cap], window) => {
        const m = deriveLimit(limit({ used: q(used), cap: q(cap), window }), q(0));
        expect(m.meter?.pct).toBe(refPct(used, cap));
        expect(m.meter?.pct === 100).toBe(used >= cap);
      }),
      { numRuns: 400 },
    );
  });

  test("more consumption never lowers the fill, and no consumption is an empty meter", () => {
    fc.assert(
      fc.property(fc.integer({ min: 1, max: Number.MAX_SAFE_INTEGER }), anyAmount, anyAmount, (cap, a, b) => {
        const [lo, hi] = a <= b ? [a, b] : [b, a];
        const fill = (used: number) => deriveLimit(limit({ used: q(used), cap: q(cap) }), q(0)).meter?.pct ?? -1;
        expect(fill(lo)).toBeLessThanOrEqual(fill(hi));
        expect(fill(0)).toBe(0);
      }),
    );
  });

  test("the fill is what has been used, never what is left: 18 of 100 is 18, not 82", () => {
    expect(deriveLimit(limit({ used: q(18), cap: q(100) }), q(0)).meter?.pct).toBe(18);
    // the hero beside "12,480 left": 2,520 of the 15,000 granted is gone, so the bar is 16% — not the 83% that is left
    const view = derivePanel(snapshot({ balance: { remaining: q(12_480), granted: q(15_000), primaryLimitKey: "bal" }, limits: [limit({ key: "bal", window: "none", used: q(0), cap: q(0) })] }));
    expect(view.hero.meter).toEqual({ used: 2_520, cap: 15_000, pct: 16 });
    expect(view.hero.basis).toBe("granted");
  });

  test("a limit with no cap is a figure and has no meter", () => {
    const m = deriveLimit(limit({ used: q(7), cap: null }), q(0));
    expect(m.meter).toBeNull();
    expect(m.tone).toBe("ok");
    expect(m.used).toBe(7);
  });

  test("a pre-computed percentage is refused, whatever it is called, rather than trusted or quietly dropped", () => {
    for (const key of ["pct", "percent", "percentage", "fill", "fillPct", "usedPct"]) {
      const poisoned = { ...limit({ key: "x" }), [key]: 40 } as unknown as LimitView;
      refuses(() => derivePanel(snapshot({ limits: [poisoned] })), "precomputed_percentage");
    }
  });
});

describe("invariant 2 — warnAtPct comes from the limit, never from a constant", () => {
  test("a windowed limit is ok below the threshold, warn from it, blocked at the cap — for any threshold", () => {
    fc.assert(
      fc.property(usedAndCap, anyPct, windowedWindow, ([used, cap], warn, window) => {
        const m = deriveLimit(limit({ used: q(used), cap: q(cap), warnAtPct: int(warn), window }), q(0));
        expect(m.tone).toBe(refTone(used, cap, warn));
        expect(m.warnAtPct).toBe(warn);
      }),
      { numRuns: 400 },
    );
  });

  test("at the exact boundary — the last use below the threshold and the first at it — however large the cap", () => {
    // t = ceil(warn * cap / 100) is the first use that warns. Near 2^53 a double cannot tell t * 100 from (t - 1) * 100, so a threshold
    // decided in floating point puts one of these two on the wrong side.
    fc.assert(
      fc.property(fc.integer({ min: 1_000, max: Number.MAX_SAFE_INTEGER }), fc.integer({ min: 1, max: 99 }), windowedWindow, (cap, warn, window) => {
        const t = Number((big(warn) * big(cap) + 99n) / 100n);
        const tone = (used: number) => deriveLimit(limit({ used: q(used), cap: q(cap), warnAtPct: int(warn), window }), q(0)).tone;
        expect(tone(t - 1)).toBe("ok");
        expect(tone(t)).toBe("warn");
      }),
      { numRuns: 500 },
    );
  });

  test("pinned at the boundary, so a constant 80 cannot pass: 75 warns at 750 of 1000, 90 at 900", () => {
    const tone = (used: number, warn: number) => deriveLimit(limit({ used: q(used), cap: q(1000), warnAtPct: int(warn) }), q(0)).tone;
    expect([tone(749, 75), tone(750, 75), tone(999, 75), tone(1000, 75)]).toEqual(["ok", "warn", "warn", "blocked"]);
    expect([tone(799, 90), tone(800, 90), tone(899, 90), tone(900, 90)]).toEqual(["ok", "ok", "ok", "warn"]);
    expect([tone(799, 80), tone(800, 80)]).toEqual(["ok", "warn"]);
  });

  test("a threshold of 0 means no warning (the engine's rule), so the limit is ok until it is blocked", () => {
    fc.assert(
      fc.property(usedAndCap, ([used, cap]) => {
        expect(deriveLimit(limit({ used: q(used), cap: q(cap), warnAtPct: int(0) }), q(0)).tone).not.toBe("warn");
      }),
    );
  });

  test("the hero's own threshold is the primary limit's", () => {
    const view = (warn: number) =>
      derivePanel(snapshot({ balance: { remaining: q(250), granted: q(1000), primaryLimitKey: "bal" }, limits: [limit({ key: "bal", window: "none", cap: q(0), warnAtPct: int(warn) })] })).hero;
    // 750 of 1000 granted is gone
    expect([view(74).tone, view(75).tone, view(76).tone]).toEqual(["warn", "warn", "ok"]);
    expect(view(76).warnAtPct).toBe(76);
  });
});

describe("a balance is not a counter — its `used` is the overdraft it is into", () => {
  const refBalanceBlocked = (remaining: number, cap: number) => big(remaining) + big(cap) <= 0n;

  test("blocked exactly when what is left plus the permitted overdraft is nothing, as the engine decides it", () => {
    fc.assert(
      fc.property(anyQuantity, anyAmount, (remaining, cap) => {
        const used = remaining < 0 ? -remaining : 0;
        const m = deriveLimit(limit({ window: "none", used: q(used), cap: q(cap) }), q(remaining));
        expect(m.tone === "blocked").toBe(refBalanceBlocked(remaining, cap));
      }),
      { numRuns: 400 },
    );
  });

  test("a healthy balance with no overdraft is not blocked, though used (0) has reached cap (0)", () => {
    const healthy = deriveLimit(limit({ window: "none", used: q(0), cap: q(0) }), q(12_480));
    expect(healthy.tone).toBe("ok");
    expect(healthy.meter).toBeNull();
    expect(deriveLimit(limit({ window: "none", used: q(0), cap: q(0) }), q(0)).tone).toBe("blocked");
    expect(deriveLimit(limit({ window: "none", used: q(100), cap: q(100) }), q(-100)).tone).toBe("blocked");
    expect(deriveLimit(limit({ window: "none", used: q(79), cap: q(100) }), q(-79)).tone).toBe("ok");
    expect(deriveLimit(limit({ window: "none", used: q(80), cap: q(100) }), q(-80)).tone).toBe("warn"); // 80 of the overdraft's 100, at the limit's own 80
  });
});

describe("invariant 4 — money is integer end to end, and display rounding never changes a value", () => {
  test("a float, NaN, an infinity or an unsafe integer is refused where it enters, never rounded", () => {
    for (const bad of [1.5, -0.25, Number.NaN, Number.POSITIVE_INFINITY, 2 ** 53, -(2 ** 53), 1e21]) refuses(() => quantity(bad), "not_an_integer");
    expect(quantity(12)).toBe(12);
    expect(Object.is(quantity(-0), 0)).toBe(true);
    fc.assert(
      fc.property(anyQuantity, (n) => {
        expect(quantity(n)).toBe(n);
      }),
    );
  });

  const paths: Array<[string, (v: number) => CreditsSnapshot]> = [
    ["balance.remaining", (v) => snapshot({ balance: { remaining: v as never, granted: q(1), primaryLimitKey: "" } })],
    ["balance.granted", (v) => snapshot({ balance: { remaining: q(1), granted: v as never, primaryLimitKey: "" } })],
    ["balance.burnRatePerDay", (v) => snapshot({ balance: { remaining: q(1), granted: q(1), primaryLimitKey: "", burnRatePerDay: v as never } })],
    ["balance.pools[0].remaining", (v) => snapshot({ balance: { remaining: q(1), granted: q(1), primaryLimitKey: "", pools: [{ key: "g", label: "G", remaining: v as never }] } })],
    ["limits[0].used", (v) => snapshot({ limits: [limit({ used: v as never })] })],
    ["limits[0].cap", (v) => snapshot({ limits: [limit({ cap: v as never })] })],
    ["limits[0].warnAtPct", (v) => snapshot({ limits: [limit({ warnAtPct: v as never })] })],
    ["breakdown[0].amount", (v) => snapshot({ breakdown: [{ key: "a", label: "A", amount: v as never }] })],
    ["series[0].amount", (v) => snapshot({ series: [{ at: "2026-01-01T00:00:00Z", amount: v as never }] })],
    ["ledger[0].delta", (v) => snapshot({ ledger: [row({ delta: v as never })] })],
  ];
  test.each(paths)("a float in %s reaches no view", (_path, build) => {
    for (const bad of [0.5, 1.0000001, Number.NaN, 2 ** 53]) refuses(() => derivePanel(build(bad)), "not_an_integer");
  });

  test("every figure in a derived view is a safe integer, and equal to the figure it was given", () => {
    fc.assert(
      fc.property(anyAmount, anyAmount, anyQuantity, anyAmount, anyPct, (used, cap, delta, amount, warn) => {
        const snap = snapshot({
          balance: { remaining: q(delta), granted: q(amount), primaryLimitKey: "k", burnRatePerDay: q(Math.max(1, used)) },
          limits: [limit({ key: "k", used: q(used), cap: q(cap), warnAtPct: int(warn) })],
          breakdown: [series("a", amount)],
          ledger: [row({ delta: q(delta) })],
        });
        const v = derivePanel(snap);
        const figures = [v.hero.remaining, v.hero.granted, v.limits[0]!.used, v.limits[0]!.cap!, v.ledger[0]!.delta, v.breakdown.total, ...v.breakdown.rows.map((r) => r.amount)];
        for (const f of figures) expect(Number.isSafeInteger(f)).toBe(true);
        expect([v.hero.remaining, v.hero.granted, v.limits[0]!.used, v.limits[0]!.cap, v.ledger[0]!.delta]).toEqual([delta, amount, used, cap, delta]);
        if (v.hero.daysLeft !== null) expect(Number.isSafeInteger(v.hero.daysLeft)).toBe(true);
      }),
    );
  });

  test("runs out in ~N days is whole days of what is left at the burn rate, and no claim without a rate", () => {
    fc.assert(
      fc.property(anyAmount, fc.integer({ min: 1, max: Number.MAX_SAFE_INTEGER }), (remaining, burn) => {
        const hero = derivePanel(snapshot({ balance: { remaining: q(remaining), granted: q(remaining), primaryLimitKey: "", burnRatePerDay: q(burn) } })).hero;
        expect(hero.daysLeft).toBe(Number(big(remaining) / big(burn)));
      }),
    );
    expect(derivePanel(snapshot()).hero.daysLeft).toBeNull();
    expect(derivePanel(snapshot({ balance: { remaining: q(5), granted: q(5), primaryLimitKey: "", burnRatePerDay: q(0) } })).hero.daysLeft).toBeNull();
    expect(derivePanel(snapshot({ balance: { remaining: q(-5), granted: q(5), primaryLimitKey: "", burnRatePerDay: q(3) } })).hero.daysLeft).toBe(0);
  });

  describe("display", () => {
    const usd = labels({ unit: "dollars", one: "dollar", scale: int(1_000_000), decimals: int(2) });

    test("rounds half away from zero to the stated places, and groups the whole part", () => {
      expect(formatQuantity(q(12_345_678), usd)).toBe("12.35");
      expect(formatQuantity(q(12_344_999), usd)).toBe("12.34");
      expect(formatQuantity(q(5_000), usd)).toBe("0.01");
      expect(formatQuantity(q(4_999), usd)).toBe("0.00");
      expect(formatQuantity(q(1_234_567_890_123), usd)).toBe("1,234,567.89");
      expect(formatQuantity(q(-5_000), usd)).toBe("−0.01");
      expect(formatQuantity(q(-1), usd)).toBe("0.00");
      expect(formatQuantity(q(12_480), labels())).toBe("12,480");
      expect(formatQuantity(q(Number.MAX_SAFE_INTEGER), labels())).toBe("9,007,199,254,740,991");
    });

    test("with one internal unit per displayed unit the text is the value, exactly", () => {
      fc.assert(
        fc.property(anyQuantity, (n) => {
          expect(Number(formatQuantity(q(n), labels()).replaceAll(",", "").replace("−", "-"))).toBe(n);
        }),
      );
    });

    test("with room for every digit the scale has, the text is the value, exactly", () => {
      fc.assert(
        fc.property(anyQuantity, fc.integer({ min: 0, max: 9 }), fc.integer({ min: 0, max: 3 }), (n, k, extra) => {
          const l = labels({ scale: int(10 ** k), decimals: int(k + extra) });
          const text = formatQuantity(q(n), l).replaceAll(",", "").replace("−", "-");
          const [whole = "", frac = ""] = text.split(".");
          const exact = big(whole === "-0" ? 0 : Number(whole)) * 10n ** big(k + extra) + (whole.startsWith("-") ? -1n : 1n) * big(Number(frac || "0"));
          expect(exact).toBe(big(n) * 10n ** big(extra));
        }),
      );
    });

    test("with fewer places than the scale needs, the text is within half a displayed place of the value, never further", () => {
      fc.assert(
        fc.property(anyQuantity, (n) => {
          const text = formatQuantity(q(n), usd).replaceAll(",", "").replace("−", "-");
          const cents = BigInt(text.replace(".", ""));
          // |cents/100 - n/1e6| <= 0.005  <=>  |cents*1e4 - n| <= 5000
          const diff = cents * 10_000n - big(n);
          expect(diff < 0n ? -diff : diff).toBeLessThanOrEqual(5_000n);
        }),
      );
    });

    test("a signed delta carries its sign, so a grant and a consumption read at a glance, and zero is not blank", () => {
      expect(formatSigned(q(50_000), labels())).toBe("+50,000");
      expect(formatSigned(q(-42), labels())).toBe("−42");
      expect(formatSigned(q(0), labels())).toBe("0");
    });

    test("a host's own formatter wins", () => {
      expect(formatQuantity(q(3), labels({ format: (n) => `<${n}>` }))).toBe("<3>");
    });
  });

  test("a snapshot's limits are not reordered, renamed or dropped: the count is the realm's", () => {
    fc.assert(
      fc.property(fc.integer({ min: 0, max: 9 }), (n) => {
        const limits = Array.from({ length: n }, (_, i) => limit({ key: `k${i}`, label: `L${i}`, used: q(i), cap: q(100) }));
        const v = derivePanel(snapshot({ limits }));
        expect(v.limits.map((l) => [l.key, l.label])).toEqual(limits.map((l) => [l.key, l.label]));
      }),
    );
  });
});

// Bounded so that a derived total or residual stays representable; what happens when one would not is its own test below.
const mid = fc.integer({ min: -100_000_000_000_000, max: 100_000_000_000_000 });
const midPos = fc.integer({ min: 0, max: 100_000_000_000_000 });

describe("invariant 5 — parts shown beside a total sum to it, or an explicit residual row says why", () => {
  test("pools: the rows always add up to the balance, with a residual row exactly when the pools do not", () => {
    fc.assert(
      fc.property(mid, fc.array(mid, { maxLength: 6 }), (remaining, parts) => {
        const pools = parts.map((p, i) => ({ key: `p${i}`, label: `P${i}`, remaining: q(p) }));
        const v = derivePanel(snapshot({ balance: { remaining: q(remaining), granted: q(0), primaryLimitKey: "", pools } }));
        if (pools.length === 0) {
          expect(v.pools).toBeNull();
          return;
        }
        const sum = parts.reduce((s, p) => s + big(p), 0n);
        const rows = v.pools!.rows;
        expect(rows.reduce((s, r) => s + big(r.amount), 0n)).toBe(big(remaining));
        expect(v.pools!.total).toBe(remaining);
        const residual = rows.filter((r) => r.residual);
        if (sum === big(remaining)) expect(residual).toHaveLength(0);
        else {
          expect(residual).toHaveLength(1);
          expect(big(residual[0]!.amount)).toBe(big(remaining) - sum);
          expect(rows.at(-1)).toBe(residual[0]);
        }
        expect(rows.filter((r) => !r.residual).map((r) => r.key)).toEqual(pools.map((p) => p.key));
      }),
      { numRuns: 300 },
    );
  });

  test("pools that add up to the balance get no residual row — a zero row would be noise that looks like a finding", () => {
    fc.assert(
      fc.property(fc.array(mid, { minLength: 1, maxLength: 6 }), (parts) => {
        const remaining = parts.reduce((a, b) => a + b, 0);
        const v = derivePanel(snapshot({ balance: { remaining: q(remaining), granted: q(0), primaryLimitKey: "", pools: parts.map((p, i) => ({ key: `p${i}`, label: `P${i}`, remaining: q(p) })) } }));
        expect(v.pools!.rows.map((r) => r.key)).toEqual(parts.map((_, i) => `p${i}`));
        expect(v.pools!.rows.some((r) => r.residual)).toBe(false);
      }),
    );
  });

  test("breakdown: members take ramp slots by index, the overflow folds into one Other, and the rows add up to the total", () => {
    fc.assert(
      fc.property(fc.array(midPos, { maxLength: 14 }), fc.option(fc.array(midPos, { maxLength: 14 }), { nil: undefined }), (amounts, daily) => {
        const bd = amounts.map((a, i) => series(`m${i}`, a));
        const v = derivePanel(snapshot({ breakdown: bd, ...(daily ? { series: daily.map((a, i) => ({ at: `2026-01-${String(i + 1).padStart(2, "0")}T00:00:00Z`, amount: q(a) })) } : {}) }));
        const partsSum = amounts.reduce((s, a) => s + big(a), 0n);
        const total = daily ? daily.reduce((s, a) => s + big(a), 0n) : partsSum;
        expect(big(v.breakdown.total)).toBe(total);
        expect(v.breakdown.rows.reduce((s, r) => s + big(r.amount), 0n)).toBe(total);

        const members = v.breakdown.rows.filter((r) => r.kind === "series");
        expect(members).toHaveLength(Math.min(amounts.length, SERIES_SLOTS));
        members.forEach((m, i) => {
          expect(m.slot).toBe(i);
          expect([m.key, m.amount]).toEqual([`m${i}`, amounts[i]]);
        });
        const other = v.breakdown.rows.filter((r) => r.kind === "other");
        expect(other).toHaveLength(amounts.length > SERIES_SLOTS ? 1 : 0);
        if (other[0]) expect(big(other[0].amount)).toBe(amounts.slice(SERIES_SLOTS).reduce((s, a) => s + big(a), 0n));
        const residual = v.breakdown.rows.filter((r) => r.kind === "residual");
        expect(v.breakdown.rows.map((r) => r.kind)).toEqual([...members, ...other, ...residual].map((r) => r.kind));
        expect(residual).toHaveLength(total === partsSum ? 0 : 1);
        if (residual[0]) expect(big(residual[0].amount)).toBe(total - partsSum);
      }),
      { numRuns: 300 },
    );
  });

  test("a total that would leave the safe-integer range is refused, never wrapped or rounded", () => {
    const huge = 2 ** 52;
    // the residual would be 0 − 2^53
    refuses(() => derivePanel(snapshot({ balance: { remaining: q(0), granted: q(0), primaryLimitKey: "", pools: [{ key: "a", label: "A", remaining: q(huge) }, { key: "b", label: "B", remaining: q(huge) }] } })), "out_of_range");
    // the total would be 2^53
    refuses(() => derivePanel(snapshot({ breakdown: [series("a", huge), series("b", huge)] })), "out_of_range");
    // the Other row would be 2^53
    refuses(() => derivePanel(snapshot({ breakdown: [...Array.from({ length: SERIES_SLOTS }, (_, i) => series(`m${i}`, 0)), series("x", huge), series("y", huge)], series: [{ at: "2026-01-01T00:00:00Z", amount: q(0) }] })), "out_of_range");
  });

  test("a member's slot follows their place in the list the host supplies, not their size", () => {
    const v = derivePanel(snapshot({ breakdown: [series("small", 1), series("large", 1_000_000)] }));
    expect(v.breakdown.rows.map((r) => [r.key, r.slot])).toEqual([["small", 0], ["large", 1]]);
  });

  test("a ledger is shown as it was given, newest first as the server sends it, with its signed deltas and its dedupe tag", () => {
    const rows = [row({ id: "a", delta: q(50_000), operationType: "purchase" }), row({ id: "b", delta: q(-42), idempotent: true }), row({ id: "c", delta: q(0), operationType: "refused" })];
    const v = derivePanel(snapshot({ ledger: rows }));
    expect(v.ledger.map((r) => [r.id, r.delta, r.direction, r.idempotent])).toEqual([["a", 50_000, "in", false], ["b", -42, "out", true], ["c", 0, "none", false]]);
    expect(v.ledger.map((r) => r.operationType)).toEqual(["purchase", "usage", "refused"]);
  });

  test("the model's only reading of a row is the sign of its delta — never its operation type, whatever the host calls it", () => {
    fc.assert(
      fc.property(fc.string(), fc.constantFrom("purchase", "refund", "usage", "chargeback", "writeoff", "credits", "query"), anyQuantity, (own, known, delta) => {
        for (const operationType of [own, known]) {
          const [m] = derivePanel(snapshot({ ledger: [row({ operationType, delta: q(delta) })] })).ledger;
          expect(m!.direction).toBe(delta > 0 ? "in" : delta < 0 ? "out" : "none");
          expect(m!.operationType).toBe(operationType);
        }
      }),
    );
  });
});

describe("a snapshot that cannot be shown is refused loudly, not half-drawn", () => {
  test("the hero's limit must exist unless the realm shows none", () => {
    refuses(() => derivePanel(snapshot({ balance: { remaining: q(1), granted: q(1), primaryLimitKey: "nope" } })), "unknown_primary_limit");
    expect(() => derivePanel(snapshot())).not.toThrow();
  });

  test("two limits cannot share a key", () => {
    refuses(() => derivePanel(snapshot({ limits: [limit({ key: "same" }), limit({ key: "same" })] })), "invalid_limit");
  });

  test("a window the view has no word for is refused — its window text would be blank, and a blank is a silent state", () => {
    for (const window of ["weekly", "", "Day", "balance"]) refuses(() => derivePanel(snapshot({ limits: [limit({ window: window as never })] })), "invalid_limit");
    for (const window of ["period", "day", "hour", "job", "call", "none"] as const) expect(() => derivePanel(snapshot({ limits: [limit({ window })] }))).not.toThrow();
  });

  test("a negative use or cap, and a threshold outside 0–100, are not ceilings", () => {
    refuses(() => derivePanel(snapshot({ limits: [limit({ used: q(-1) })] })), "invalid_limit");
    refuses(() => derivePanel(snapshot({ limits: [limit({ cap: q(-1) })] })), "invalid_limit");
    refuses(() => derivePanel(snapshot({ limits: [limit({ warnAtPct: int(101) })] })), "invalid_limit");
    refuses(() => derivePanel(snapshot({ limits: [limit({ warnAtPct: int(-1) })] })), "invalid_limit");
  });
});

describe("an absent figure is not a zero", () => {
  test("the breakdown is known when a series came with it, and not known when none did — an empty list means two different things", () => {
    expect(derivePanel(snapshot({ series: [{ at: "2026-01-01T00:00:00Z", amount: q(0) }], breakdown: [] })).breakdown.known).toBe(true);
    expect(derivePanel(snapshot({ breakdown: [] })).breakdown.known).toBe(false);
    expect(derivePanel(snapshot({ breakdown: [series("a", 5)] })).breakdown.known).toBe(false); // rows with no series is still not a statement about the window
  });

  test("a series of zeros is a spend of nothing and is kept, so the panel can say so; no series is no spend model at all", () => {
    expect(derivePanel(snapshot({ series: [{ at: "2026-01-01T00:00:00Z", amount: q(0) }] })).spend).toMatchObject({ total: 0, peak: 0 });
    expect(derivePanel(snapshot({})).spend).toBeNull();
  });
});

describe("the ledger's cursor", () => {
  test("a cursor in the snapshot is the panel's next page, and none, or an empty one, is the end", () => {
    expect(derivePanel(snapshot({ ledgerNext: "c1" })).ledgerNext).toBe("c1");
    expect(derivePanel(snapshot({})).ledgerNext).toBeNull();
    expect(derivePanel(snapshot({ ledgerNext: "" })).ledgerNext).toBeNull(); // an empty cursor asks for the first page again: it is not "more"
  });

  test("a page of rows is derived the way the first page is, and refused the same way", () => {
    const rows = deriveLedgerRows([row({ id: "a", delta: q(-5) }), row({ id: "b", delta: q(7) }), row({ id: "c", delta: q(0) })]);
    expect(rows.map((r) => [r.id, r.direction])).toEqual([["a", "out"], ["b", "in"], ["c", "none"]]);
    refuses(() => deriveLedgerRows([row({ delta: 0.5 as never })]), "not_an_integer");
  });
});
