import type { BreakdownModel } from "../model.js";
import { resolveCopy, type Copy } from "../copy.js";
import type { UnitLabels } from "../ports.js";
import { Figure } from "./shared.js";

/**
 * Spend by member. Each row is its label and its amount as text; the swatch is a colour from the ramp by the member's index, and it
 * is aria-hidden because colour never carries identity alone (invariant 7). Other and the residual row are labelled, never blank.
 */
export function Breakdown({ breakdown, labels, copy }: { breakdown: BreakdownModel; labels: UnitLabels; copy?: Partial<Copy> | undefined }) {
  const c = resolveCopy(copy);
  if (breakdown.rows.length === 0) {
    return (
      <section className="cu-breakdown" aria-label={c.breakdownHeading}>
        <h2 className="cu-heading">{c.breakdownHeading}</h2>
        <p className="cu-muted">{c.noBreakdown}</p>
      </section>
    );
  }
  return (
    <section className="cu-breakdown" aria-label={c.breakdownHeading}>
      <h2 className="cu-heading">{c.breakdownHeading}</h2>
      <ul>
        {breakdown.rows.map((r) => (
          <li key={r.key} data-kind={r.kind} data-slot={r.slot === null ? r.kind : String(r.slot)}>
            <span className="cu-swatch" aria-hidden="true" data-slot={r.slot === null ? r.kind : String(r.slot)} />
            <span className="cu-breakdown-label">{r.kind === "other" ? c.otherLabel : r.kind === "residual" ? c.residualLabel : r.label}</span> <Figure q={r.amount} labels={labels} />
          </li>
        ))}
      </ul>
      <p data-cu="total">
        <span>{c.totalLabel}</span> <Figure q={breakdown.total} labels={labels} unit />
      </p>
    </section>
  );
}
