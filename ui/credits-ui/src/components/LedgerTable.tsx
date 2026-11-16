import type { ReactNode } from "react";
import type { LedgerModel } from "../model.js";
import { resolveCopy, type Copy } from "../copy.js";
import type { LedgerColumn, UnitLabels } from "../ports.js";
import { defaultFormatTime } from "./LimitMeter.js";
import { Figure } from "./shared.js";

// What a row says when no registered column claimed it: every attribute, as text, in the order it came. The package reads no key — it
// lists them — so a product whose columns are not registered still sees what happened instead of a blank.
function Generic({ attributes }: { attributes: LedgerModel["attributes"] }) {
  const entries = Object.entries(attributes);
  if (entries.length === 0) return null;
  return (
    <dl className="cu-attributes" data-cu="generic">
      {entries.map(([k, v]) => (
        <div key={k}>
          <dt>{k}</dt> <dd>{typeof v === "object" && v !== null ? JSON.stringify(v) : String(v)}</dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * The durable record, newest first as the server sends it. One signed column — "−42" for consumption, "+50,000" for a grant — so a
 * purchase, a refund and a chargeback land in the same place with no schema-shaped change. The product's own columns come from the
 * host; a replayed operation carries a `deduped` tag, which is how "charged at most once" becomes visible to the person it protects.
 */
export function LedgerTable({ rows, columns = [], labels, copy, formatTime = defaultFormatTime }: { rows: readonly LedgerModel[]; columns?: readonly LedgerColumn[] | undefined; labels: UnitLabels; copy?: Partial<Copy> | undefined; formatTime?: ((iso: string) => string) | undefined }) {
  const c = resolveCopy(copy);
  if (rows.length === 0) {
    return (
      <section className="cu-ledger" aria-label={c.ledgerHeading}>
        <h2 className="cu-heading">{c.ledgerHeading}</h2>
        <p className="cu-muted">{c.noLedger}</p>
      </section>
    );
  }
  return (
    // tabIndex: the box scrolls sideways on a narrow screen, and a scroll box a keyboard cannot focus cannot be scrolled by one
    <section className="cu-ledger" aria-label={c.ledgerHeading} tabIndex={0}>
      <h2 className="cu-heading">{c.ledgerHeading}</h2>
      <table>
        <thead>
          <tr>
            <th scope="col">{c.colOperation}</th>
            <th scope="col">{c.colActor}</th>
            {columns.map((col) => (
              <th key={col.key} scope="col" data-align={col.align ?? "start"}>
                {col.header}
              </th>
            ))}
            <th scope="col">{c.colDetails}</th>
            <th scope="col" data-align="end">
              {c.colChange}
            </th>
            <th scope="col">{c.colWhen}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const original = { id: r.id, operationType: r.operationType, delta: r.delta, at: r.at, attributes: r.attributes, ...(r.actor === undefined ? {} : { actor: r.actor }), ...(r.idempotent ? { idempotent: true } : {}) };
            const claimed = columns.some((col) => col.accepts(original));
            return (
              <tr key={r.id} data-row={r.id}>
                <td>
                  <span data-cu="operation">{r.operationType}</span>
                  {r.idempotent ? <span className="cu-tag" data-cu="deduped">{c.deduped}</span> : null}
                </td>
                <td>{r.actor?.label ?? ""}</td>
                {columns.map((col) => (
                  <td key={col.key} data-align={col.align ?? "start"}>
                    {col.accepts(original) ? (col.render(original) as ReactNode) : null}
                  </td>
                ))}
                <td>{claimed ? null : <Generic attributes={r.attributes} />}</td>
                <td data-align="end" data-direction={r.direction}>
                  <Figure q={r.delta} labels={labels} signed />
                </td>
                <td>
                  <time dateTime={r.at}>{formatTime(r.at)}</time>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}
