// Invariant 8 in the reference values: a limit's tone and a series' colour are disjoint hue sets. The package's stylesheet keeps them in
// separate rules; this keeps the VALUES a host is likely to copy from reintroducing the confusion — an olive "Audio" next to the green
// "healthy" meter, or a brown "Video" next to the amber "near the limit" one, is the same mistake in a different place.
import { readFileSync } from "node:fs";
import { describe, expect, test } from "vitest";

const css = readFileSync(new URL("../src/tokens.css", import.meta.url), "utf8");
const token = (name: string): string => css.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`))?.[1] ?? "";

function hsl(hex: string): { h: number; s: number; l: number } {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255) as [number, number, number];
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  const d = max - min;
  if (d === 0) return { h: 0, s: 0, l };
  const s = d / (1 - Math.abs(2 * l - 1));
  const h = max === r ? ((g - b) / d) % 6 : max === g ? (b - r) / d + 2 : (r - g) / d + 4;
  return { h: (h * 60 + 360) % 360, s, l };
}
const apart = (a: number, b: number) => Math.min(Math.abs(a - b), 360 - Math.abs(a - b));

describe("the reference token values", () => {
  const status = ["ok", "warn", "blocked"].map((t) => ({ name: `status-${t}`, ...hsl(token(`cu-status-${t}`)) }));
  const series = [1, 2, 3, 4, 5, 6].map((n) => ({ name: `series-${n}`, ...hsl(token(`cu-series-${n}`)) }));

  test("every token the stylesheet reads has a value", () => {
    for (const t of ["surface", "text", "muted", "border", "status-ok", "status-warn", "status-blocked", "series-1", "series-2", "series-3", "series-4", "series-5", "series-6", "series-other"]) expect(token(`cu-${t}`), t).toMatch(/^#[0-9a-f]{6}$/i);
  });

  test("no series colour is within 40° of a status hue: nothing in the ramp can be read as healthy, near the limit or blocked", () => {
    for (const s of series) for (const t of status) expect(apart(s.h, t.h), `${s.name} (${Math.round(s.h)}°) against ${t.name} (${Math.round(t.h)}°)`).toBeGreaterThanOrEqual(40);
  });

  test("the series colours are chromatic, and the Other slot is a neutral", () => {
    for (const s of series) expect(s.s, s.name).toBeGreaterThan(0.2);
    expect(hsl(token("cu-series-other")).s).toBeLessThan(0.2);
  });

  test("the status hues are green, amber and red, so a tone reads as what it is", () => {
    const [ok, warn, blocked] = status as [typeof status[number], typeof status[number], typeof status[number]];
    expect(ok.h).toBeGreaterThan(100);
    expect(ok.h).toBeLessThan(180);
    expect(warn.h).toBeGreaterThan(20);
    expect(warn.h).toBeLessThan(55);
    expect(Math.min(blocked.h, 360 - blocked.h)).toBeLessThan(15);
  });
});
