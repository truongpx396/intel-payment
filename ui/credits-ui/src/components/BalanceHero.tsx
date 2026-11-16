import { formatQuantity, formatWithUnit, unitName, type HeroModel, type PoolsModel } from "../model.js";
import { resolveCopy, type Copy } from "../copy.js";
import type { UnitLabels } from "../ports.js";
import { Figure, Meter, Notice, SafeLink, type Remedy } from "./shared.js";

interface Props {
  readonly hero: HeroModel;
  readonly pools: PoolsModel | null;
  readonly labels: UnitLabels;
  readonly copy?: Partial<Copy> | undefined;
  readonly remedy?: Remedy | undefined;
}

/**
 * What is left, how much of what was granted is gone, and how long it lasts — and, per pool, where it sits. The headline is a
 * remainder; the bar beside it is consumption. They read together only because the bar never means "what is left".
 */
export function BalanceHero({ hero, pools, labels, copy, remedy }: Props) {
  const c = resolveCopy(copy);
  const name = hero.meter === null ? "" : (labels.abbr ?? unitName(hero.meter.cap, labels));
  const valueText = hero.meter === null ? "" : c.meterText("balance", formatQuantity(hero.meter.used, labels), `${formatQuantity(hero.meter.cap, labels)} ${name}`, hero.meter.pct);
  return (
    <section className="cu-hero" aria-label={c.balance} data-tone={hero.tone}>
      <p className="cu-hero-figure">
        <Figure q={hero.remaining} labels={labels} unit /> <span className="cu-muted">{c.left}</span>
      </p>
      {hero.meter !== null ? (
        <>
          <Meter meter={hero.meter} tone={hero.tone} label={c.balanceMeterLabel} valueText={valueText} />
          <p className="cu-muted">{c.grantedOf(formatWithUnit(hero.granted, labels))}</p>
        </>
      ) : null}
      {hero.daysLeft !== null ? (
        <p className="cu-muted" data-cu="days-left">
          {c.daysLeft(hero.daysLeft)}
        </p>
      ) : null}
      {hero.tone === "warn" && hero.meter !== null ? (
        <Notice role="status" tone="warn" data-cu="near-limit">
          {c.nearLimit(c.balance, hero.meter.pct)}
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
      {pools !== null ? (
        <ul className="cu-pools" aria-label={c.poolsHeading}>
          {pools.rows.map((r) => (
            <li key={r.key} data-pool={r.key} data-residual={r.residual ? "true" : "false"}>
              <span>{r.residual ? c.residualLabel : r.label}</span> <Figure q={r.amount} labels={labels} />
              {r.appliesTo !== undefined && r.appliesTo !== "" ? <span className="cu-muted"> · {r.appliesTo}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}
