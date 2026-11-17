import { formatQuantity, type SpendModel } from "../model.js";
import { resolveCopy, type Copy } from "../copy.js";
import type { UnitLabels } from "../ports.js";

const W = 320;
const H = 80;
// the line is drawn inside its box: a point on the edge would lose half its stroke to the clip
const PAD = 3;

/**
 * Spend over time as a line, drawn in the categorical ramp (green here would contradict the meters, where it means healthy). The line
 * is decoration over text: the summary and a table of every point are in the page for anyone who cannot see the line (invariant 7).
 * Pixel positions are geometry, not money — the figures themselves never pass through them.
 */
export function SpendChart({ spend, labels, copy }: { spend: SpendModel | null; labels: UnitLabels; copy?: Partial<Copy> | undefined }) {
  const c = resolveCopy(copy);
  // Two different silences. No series at all is a snapshot that knows nothing about spend, and says so. A series that adds up to nothing —
  // including one with no points — is a statement: nothing was spent, and it is not drawn as a line along the floor of a chart.
  if (spend === null || spend.total === 0) {
    return (
      <section className="cu-spend" aria-label={c.spendHeading}>
        <h2 className="cu-heading">{c.spendHeading}</h2>
        <p className="cu-muted">{spend === null ? c.spendUnavailable : c.noSpend}</p>
      </section>
    );
  }
  const last = spend.points.length - 1;
  const top = Math.max(spend.peak, 1);
  const x = (i: number) => (last === 0 ? W / 2 : PAD + (i * (W - 2 * PAD)) / last);
  const y = (amount: number) => PAD + (H - 2 * PAD) * (1 - amount / top);
  const pts = spend.points.map((p, i) => `${x(i)},${y(p.amount)}`).join(" ");
  const summary = c.spendSummary(formatQuantity(spend.total, labels), formatQuantity(spend.peak, labels), spend.points.length);
  return (
    <figure className="cu-spend" aria-label={c.spendHeading}>
      <h2 className="cu-heading">{c.spendHeading}</h2>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={summary} preserveAspectRatio="none">
        <polyline className="cu-spend-line" data-slot="0" points={pts} fill="none" />
      </svg>
      <figcaption>{summary}</figcaption>
      <table className="cu-sr-only">
        <tbody>
          {spend.points.map((p) => (
            <tr key={p.at}>
              <th scope="row">{p.at}</th>
              <td data-quantity={String(p.amount)}>{formatQuantity(p.amount, labels)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </figure>
  );
}
