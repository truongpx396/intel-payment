import { formatQuantity, unitName, type LimitModel } from "../model.js";
import { resolveCopy, type Copy } from "../copy.js";
import type { UnitLabels } from "../ports.js";
import { Figure, Meter, Notice, SafeLink, type Remedy } from "./shared.js";

interface Props {
  readonly limit: LimitModel;
  readonly labels: UnitLabels;
  readonly copy?: Partial<Copy> | undefined;
  /** Where to send someone whose limit is reached or near. The host maps a denyCode to a way out; nothing here guesses one. */
  readonly remedy?: Remedy | undefined;
  readonly formatTime?: ((iso: string) => string) | undefined;
}

export const defaultFormatTime = (iso: string): string => iso.replace("T", " ").replace(/(\.\d+)?Z$/, " UTC");

/** One ceiling: its label, the window it counts over, a meter if it has a cap, and — when it is reached — a notice that cannot be missed. */
export function LimitMeter({ limit, labels, copy, remedy, formatTime = defaultFormatTime }: Props) {
  const c = resolveCopy(copy);
  const cap = limit.cap;
  const capName = cap === null ? "" : (labels.abbr ?? unitName(cap, labels));
  const valueText = limit.meter === null ? "" : c.meterText(limit.kind, formatQuantity(limit.meter.used, labels), `${formatQuantity(limit.meter.cap, labels)} ${capName}`, limit.meter.pct);
  const windowText =
    limit.resetsAt !== undefined && limit.kind === "window" ? c.resets(formatTime(limit.resetsAt)) : limit.kind === "call" ? c.lastCall : c.windowText[limit.window];

  return (
    <li className="cu-limit" data-limit={limit.key} data-tone={limit.tone} data-kind={limit.kind}>
      <div className="cu-limit-head">
        <span className="cu-limit-label">{limit.label}</span>
        {limit.scopeHint !== undefined ? <span className="cu-tag">{limit.scopeHint}</span> : null}
      </div>
      <div className="cu-limit-figure">
        {cap === null ? (
          <>
            <Figure q={limit.used} labels={labels} unit />
            <span className="cu-muted"> · {c.uncapped}</span>
          </>
        ) : limit.kind === "balance" && cap === 0 ? (
          <span className="cu-muted">{c.noOverdraft}</span>
        ) : (
          <>
            {limit.kind === "balance" ? <span className="cu-muted">{c.overdraft}: </span> : null}
            <Figure q={limit.used} labels={labels} /> <span className="cu-muted">/</span> <Figure q={cap} labels={labels} /> <span className="cu-muted">{capName}</span>
          </>
        )}
      </div>
      {limit.meter !== null ? <Meter meter={limit.meter} tone={limit.tone} label={limit.label} valueText={valueText} /> : null}
      <p className="cu-limit-window cu-muted">{windowText}</p>
      {limit.blocking !== null ? (
        <Notice role="alert" tone="blocked" data-cu="blocking">
          <strong>{c.blockedTitle(limit.label)}</strong>
          <p>{c.blockedBody}</p>
          {limit.blocking.denyCode !== undefined ? (
            <p>
              {c.denyCode}: <code data-cu="deny-code">{limit.blocking.denyCode}</code>
            </p>
          ) : null}
          {remedy !== undefined ? (
            <SafeLink to={remedy} data-cu="remedy">
              {remedy.label}
            </SafeLink>
          ) : (
            <p data-cu="remedy">{c.noRemedy}</p>
          )}
        </Notice>
      ) : limit.tone === "warn" && limit.meter !== null ? (
        <Notice role="status" tone="warn" data-cu="near-limit">
          {c.nearLimit(limit.label, limit.meter.pct)}
          {remedy !== undefined ? (
            <>
              {" "}
              <SafeLink to={remedy} data-cu="remedy">
                {remedy.label}
              </SafeLink>
            </>
          ) : null}
        </Notice>
      ) : null}
    </li>
  );
}

/** The realm's ceilings, in its order, however many there are: one list, one component, no card per limit. */
export function LimitMeters({ limits, labels, copy, remedyFor, formatTime }: { limits: readonly LimitModel[]; labels: UnitLabels; copy?: Partial<Copy> | undefined; remedyFor?: ((limit: LimitModel) => Remedy | undefined) | undefined; formatTime?: ((iso: string) => string) | undefined }) {
  const c = resolveCopy(copy);
  if (limits.length === 0) return <p className="cu-muted">{c.noLimits}</p>;
  return (
    <ol className="cu-limits" aria-label={c.limitsHeading}>
      {limits.map((l) => (
        <LimitMeter key={l.key} limit={l} labels={labels} copy={copy} remedy={remedyFor?.(l)} formatTime={formatTime} />
      ))}
    </ol>
  );
}

