import { formatMinor, formatWithUnit, type CatalogModel, type OfferModel } from "../model.js";
import { resolveCopy, type Copy } from "../copy.js";
import type { UnitLabels } from "../ports.js";
import { SafeLink } from "./shared.js";

interface Props {
  readonly catalog: CatalogModel;
  readonly labels: UnitLabels;
  readonly copy?: Partial<Copy> | undefined;
  /** Called with a plan code when a live purchase control is pressed. Never called for an inert, blocked, current or contact offer. */
  readonly onSelect: (code: string) => void;
  /** The code of a checkout being started, so a second press is not a second session. */
  readonly pending?: string | null | undefined;
}

function Offer({ offer, catalog, labels, copy, onSelect, pending }: Props & { offer: OfferModel }) {
  const c = resolveCopy(copy);
  const a = offer.action;
  return (
    <li className="cu-offer" data-offer={offer.code} data-kind={offer.kind} data-state={a.kind}>
      <h4>{offer.name}</h4>
      {offer.tagline !== undefined ? <p className="cu-muted">{offer.tagline}</p> : null}
      {offer.price !== null ? (
        <p data-cu="price">
          {formatMinor(offer.price.minor, offer.price.currency)}
          {offer.interval !== undefined ? ` / ${c.interval[offer.interval]}` : ""}
        </p>
      ) : null}
      <p className="cu-muted" data-cu="allotment">
        {offer.allotment === null ? c.allotmentCustom : c.allotment(formatWithUnit(offer.allotment, labels))}
      </p>
      {offer.features.length > 0 ? (
        <ul className="cu-features">
          {offer.features.map((f) => (
            <li key={f.label}>
              <span>{f.label}</span>: <span>{f.value}</span>
            </li>
          ))}
        </ul>
      ) : null}

      {a.kind === "checkout" ? (
        <button type="button" data-action="checkout" disabled={a.inert || pending !== null && pending !== undefined} aria-disabled={a.inert || (pending !== null && pending !== undefined) ? "true" : undefined} onClick={() => onSelect(offer.code)}>
          {pending === offer.code ? c.redirecting : c.buy(offer.name)}
        </button>
      ) : a.kind === "contact" ? (
        catalog.contact !== null ? (
          <SafeLink to={catalog.contact} data-action="contact">
            {catalog.contact.label}
          </SafeLink>
        ) : (
          <p className="cu-muted" data-cu="not-self-serve">
            {c.notSelfServe}
          </p>
        )
      ) : a.kind === "current" ? (
        <p data-cu="current">{c.currentPlan}</p>
      ) : (
        <div data-cu="blocked">
          <p>{a.reason}</p>
          {a.remedy !== null ? (
            <SafeLink to={a.remedy} data-cu="remedy">
              {a.remedy.label}
            </SafeLink>
          ) : null}
        </div>
      )}
    </li>
  );
}

function Group({ title, offers, ...props }: Props & { title: string; offers: readonly OfferModel[] }) {
  if (offers.length === 0) return null;
  return (
    <section aria-label={title}>
      <h3>{title}</h3>
      <ul className="cu-offers">
        {offers.map((o) => (
          <Offer key={o.code} offer={o} {...props} />
        ))}
      </ul>
    </section>
  );
}

/**
 * Subscriptions and one-time purchases are different questions, so they are different lists. What an offer's control is — live, inert,
 * a way to ask, the current plan, or nothing at all because it is blocked — was decided in the model; this only draws it.
 */
export function PlanCatalogView(props: Props) {
  const c = resolveCopy(props.copy);
  const { catalog } = props;
  const empty = catalog.subscriptions.length + catalog.oneTime.length === 0;
  return (
    <section className="cu-plans" aria-label={c.plansHeading}>
      {catalog.viewerNotice !== null ? (
        <div className="cu-notice" role="note" data-cu="cannot-purchase">
          <p>{catalog.viewerNotice.reason ?? c.cannotPurchaseDefault}</p>
          {catalog.viewerNotice.request !== null ? (
            <SafeLink to={catalog.viewerNotice.request} data-cu="request-path">
              {catalog.viewerNotice.request.label}
            </SafeLink>
          ) : null}
        </div>
      ) : null}
      {empty ? <p className="cu-muted">{c.noPlans}</p> : null}
      <Group {...props} title={c.subscriptions} offers={catalog.subscriptions} />
      <Group {...props} title={c.oneTime} offers={catalog.oneTime} />
    </section>
  );
}
