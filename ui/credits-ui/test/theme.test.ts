// Invariant 8 — status hues and categorical hues are disjoint sets — and the stylesheet's promise that it defines no value of its own.
import { readFileSync } from "node:fs";
import { describe, expect, test } from "vitest";

const css = readFileSync(new URL("../src/theme.css", import.meta.url), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
const rules = [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({ selector: m[1]!.trim(), body: m[2]! }));
const tokens = (body: string) => [...body.matchAll(/var\((--cu-[a-z0-9-]+)\)/g)].map((m) => m[1]!);
const NEUTRAL = ["--cu-surface", "--cu-text", "--cu-muted", "--cu-border", "--cu-radius", "--cu-font-mono", "--cu-meter-pct"];

describe("the stylesheet reads tokens and sets none", () => {
  test("it declares no custom property: the host supplies every value", () => {
    expect(css).not.toMatch(/(^|[;{\s])--cu-[a-z0-9-]+\s*:/);
  });
});

describe("the layout the screenshots asked for", () => {
  const rule = (sel: string) => rules.find((r) => r.selector === sel)?.body ?? "";

  test("the panel is a grid with a gap, so sections are not stacked flush", () => {
    expect(rule(".cu-panel")).toMatch(/display:\s*grid/);
    expect(rule(".cu-panel")).toMatch(/gap:\s*\d+px/);
  });

  test("the chart's stroke does not stretch with the box, and the box has a height", () => {
    expect(rule(".cu-spend-line")).toMatch(/vector-effect:\s*non-scaling-stroke/);
    expect(rule(".cu-spend svg")).toMatch(/width:\s*100%/);
    expect(rule(".cu-spend svg")).toMatch(/height:\s*\d+px/);
  });

  test("a wide child cannot widen the page: the grid column may shrink, and the ledger scrolls inside its own box", () => {
    // found at 390px: a grid column sizes to its widest child, so the ledger table made the whole page 525px wide
    expect(rule(".cu-panel")).toMatch(/grid-template-columns:\s*minmax\(0,\s*1fr\)/);
    expect(rule(".cu-ledger")).toMatch(/overflow-x:\s*auto/);
  });

  test("a group title inside the plans is no larger than the section title above it", () => {
    expect(rule(".cu-plans h3")).toMatch(/font-size:\s*1rem/);
    expect(rule(".cu-heading")).toMatch(/font-size:\s*1rem/);
  });

  test("a section title is set apart from the text around it", () => {
    expect(rule(".cu-heading")).toMatch(/margin:/);
    expect(rule(".cu-heading")).toMatch(/font-size:/);
  });
});

describe("invariant 8 — a tone comes from a threshold state, a series colour from an index, and neither borrows from the other", () => {
  const byTone = rules.filter((r) => r.selector.includes("[data-tone"));
  const bySlot = rules.filter((r) => r.selector.includes("[data-slot") || r.selector.includes(".cu-spend-line"));

  test("there are rules of both kinds, or the checks below prove nothing", () => {
    expect(byTone.length).toBeGreaterThanOrEqual(5);
    expect(bySlot.length).toBeGreaterThanOrEqual(7);
  });

  test("a tone rule reads status tokens and neutrals only", () => {
    for (const r of byTone) for (const t of tokens(r.body)) expect(t.startsWith("--cu-status-") || NEUTRAL.includes(t), `${r.selector} reads ${t}`).toBe(true);
  });

  test("a slot rule reads series tokens only", () => {
    for (const r of bySlot) for (const t of tokens(r.body)) expect(t.startsWith("--cu-series-") || t === "--cu-radius", `${r.selector} reads ${t}`).toBe(true);
  });

  test("each tone maps to its own status token", () => {
    for (const tone of ["ok", "warn", "blocked"]) {
      const mine = byTone.filter((r) => r.selector.includes(`[data-tone="${tone}"]`));
      expect(mine.length).toBeGreaterThan(0);
      for (const r of mine) for (const t of tokens(r.body).filter((x) => x.startsWith("--cu-status-"))) expect(t).toBe(`--cu-status-${tone}`);
    }
  });

  test("the ramp is six slots and an Other, each distinct", () => {
    const slotTokens = bySlot.flatMap((r) => tokens(r.body).filter((t) => t.startsWith("--cu-series-")));
    expect(new Set(slotTokens)).toEqual(new Set(["--cu-series-1", "--cu-series-2", "--cu-series-3", "--cu-series-4", "--cu-series-5", "--cu-series-6", "--cu-series-other"]));
  });
});
