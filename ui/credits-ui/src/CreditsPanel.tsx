import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { resolveCopy, type Copy } from "./copy.js";
import { Breakdown } from "./components/Breakdown.js";
import { BalanceHero } from "./components/BalanceHero.js";
import { LedgerTable } from "./components/LedgerTable.js";
import { LimitMeters } from "./components/LimitMeter.js";
import { PlanCatalogView } from "./components/PlanCatalog.js";
import { SpendChart } from "./components/SpendChart.js";
import { Notice, SafeLink, type Remedy } from "./components/shared.js";
import { SnapshotError, deriveCatalog, deriveFulfilment, derivePanel, formatWithUnit, type CatalogModel, type FulfilmentView, type LimitModel, type PanelView } from "./model.js";
import type { CheckoutSource, CreditsSnapshot, CreditsSource, LedgerColumn, Scope, UnitLabels } from "./ports.js";

/** The host's own billing UI, where it has one. A `planCard` IS the billing section: the panel draws it instead of a catalogue or a pointer. */
export interface BillingSlots {
  readonly planCard?: ReactNode;
}

export interface CreditsPanelProps {
  readonly source: CreditsSource;
  readonly scope: Scope;
  readonly labels: UnitLabels;
  /** The product's ledger columns. A row no column claims still shows what it carries. */
  readonly columns?: readonly LedgerColumn[] | undefined;
  /** Absent: the panel draws no plan affordance at all, and no error. A product with no self-serve purchase passes none. */
  readonly checkout?: CheckoutSource | undefined;
  readonly billing?: BillingSlots | undefined;
  readonly copy?: Partial<Copy> | undefined;
  /** True when the browser has just come back from the provider's checkout. The panel then says "processing" and waits for the server. */
  readonly returnedFromCheckout?: boolean | undefined;
  /** A denyCode's way out, chosen by the host. Nothing here guesses one. */
  readonly remedyFor?: ((limit: LimitModel) => Remedy | undefined) | undefined;
  readonly navigate?: ((url: string) => void) | undefined;
  readonly fulfilmentPollMs?: number | undefined;
  readonly formatTime?: ((iso: string) => string) | undefined;
}

type Load = { readonly state: "loading" } | { readonly state: "error"; readonly message: string; readonly refused: boolean } | { readonly state: "ready"; readonly view: PanelView };
type Plans = { readonly state: "loading" } | { readonly state: "error" } | { readonly state: "ready"; readonly model: CatalogModel };

// A hand-off goes to the provider over https, or to a page of this site. The URL comes back from a call to the host's own server,
// and navigating to `javascript:` from it would be running whatever it says.
function safeRedirect(url: string): boolean {
  return /^https:\/\//i.test(url) || (url.startsWith("/") && !url.startsWith("//"));
}

const defaultNavigate = (url: string): void => window.location.assign(url);
const scopeKey = (s: Scope) => `${s.realm ?? ""}\u0000${s.kind}\u0000${s.id}`;

/**
 * The composition root: a source in, a screen out. It owns the three things that change over time — the snapshot, the catalogue and the
 * fulfilment of a payment — and nothing else; what is drawn, and every decision about it, lives in the model and the components.
 */
export function CreditsPanel({ source, scope, labels, columns, checkout, billing, copy, returnedFromCheckout = false, remedyFor, navigate = defaultNavigate, fulfilmentPollMs = 3_000, formatTime }: CreditsPanelProps) {
  const c = resolveCopy(copy);
  const key = scopeKey(scope);
  const [load, setLoad] = useState<Load>({ state: "loading" });
  const [attempt, setAttempt] = useState(0);
  const refresh = useRef<() => void>(() => undefined);

  useEffect(() => {
    let live = true;
    setLoad({ state: "loading" });
    const apply = (s: CreditsSnapshot) => {
      if (!live) return;
      try {
        setLoad({ state: "ready", view: derivePanel(s) });
      } catch (e) {
        setLoad({ state: "error", message: e instanceof Error ? e.message : String(e), refused: e instanceof SnapshotError });
      }
    };
    const fail = (e: unknown) => live && setLoad({ state: "error", message: e instanceof Error ? e.message : String(e), refused: false });
    source.load(scope).then(apply, fail);
    refresh.current = () => void source.load(scope).then(apply, () => undefined); // a quiet reload: the screen is already up
    const unsubscribe = source.subscribe?.(scope, apply);
    return () => {
      live = false;
      unsubscribe?.();
    };
    // `scope` is identified by its key: a new object for the same scope is not a new scope
  }, [source, key, attempt]);

  const view = load.state === "ready" ? load.view : null;
  const hasPlanCard = billing?.planCard !== undefined;
  const hasAnchor = view?.anchor !== null && view?.anchor !== undefined;

  const [plans, setPlans] = useState<Plans>({ state: "loading" });
  useEffect(() => {
    if (checkout === undefined || view === null || hasAnchor || hasPlanCard) return;
    let live = true;
    setPlans({ state: "loading" });
    checkout.catalog(scope).then(
      (cat) => {
        if (!live) return;
        try {
          setPlans({ state: "ready", model: deriveCatalog(cat) });
        } catch {
          setPlans({ state: "error" });
        }
      },
      () => live && setPlans({ state: "error" }),
    );
    return () => {
      live = false;
    };
  }, [checkout, key, view !== null, hasAnchor, hasPlanCard]);

  const [fulfilment, setFulfilment] = useState<FulfilmentView | null>(null);
  useEffect(() => {
    if (!returnedFromCheckout || checkout === undefined) {
      setFulfilment(null);
      return;
    }
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let tries = 0;
    setFulfilment(deriveFulfilment(true, null));
    const poll = async () => {
      try {
        const next = deriveFulfilment(true, await checkout.fulfilment(scope));
        if (!live) return;
        setFulfilment(next);
        if (next?.kind === "granted") return void refresh.current();
        if (next?.kind === "failed") return;
      } catch {
        // the server could not be reached, or said something unreadable: that is not a failure of the payment, so keep asking
      }
      if (!live) return;
      tries += 1;
      timer = setTimeout(() => void poll(), Math.min(fulfilmentPollMs * 2 ** Math.min(tries - 1, 4), 30_000));
    };
    void poll();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [checkout, key, returnedFromCheckout, fulfilmentPollMs]);

  const [pending, setPending] = useState<string | null>(null);
  const [handoffFailed, setHandoffFailed] = useState(false);
  const handOff = useCallback(
    (getUrl: () => Promise<string>, code: string | null) => {
      setHandoffFailed(false);
      setPending(code);
      getUrl()
        .then((url) => {
          if (!safeRedirect(url)) throw new Error("unsafe redirect");
          navigate(url);
        })
        .catch(() => {
          setPending(null);
          setHandoffFailed(true);
        });
    },
    [navigate],
  );

  if (load.state === "loading") {
    return (
      <div className="cu-panel" data-state="loading" role="status">
        {c.loading}
      </div>
    );
  }
  if (load.state === "error") {
    return (
      <div className="cu-panel" data-state="error" role="alert">
        <p>{load.refused ? c.cannotShow : c.loadFailed}</p>
        {load.refused ? <p><code>{load.message}</code></p> : null}
        <button type="button" onClick={() => setAttempt((n) => n + 1)}>
          {c.retry}
        </button>
      </div>
    );
  }

  const v = load.view;
  const planNode = (): ReactNode => {
    if (hasPlanCard) return billing?.planCard;
    if (v.anchor !== null) {
      const a = v.anchor;
      return (
        <div className="cu-anchor" data-cu="anchor">
          <p>{c.anchorHolds(a.label)}</p>
          <dl>
            {a.facts.map((f) => (
              <div key={f.label}>
                <dt>{f.label}</dt> <dd>{f.value}</dd>
              </div>
            ))}
          </dl>
          {a.rule !== undefined ? <p className="cu-muted">{a.rule}</p> : null}
          <SafeLink to={a}>{c.anchorOpen(a.label)}</SafeLink>
        </div>
      );
    }
    if (checkout === undefined) return null;
    return (
      <>
        {plans.state === "error" ? (
          <Notice role="alert" tone="blocked">
            {c.plansCannotShow}
          </Notice>
        ) : plans.state === "ready" ? (
          <PlanCatalogView catalog={plans.model} labels={labels} copy={copy} pending={pending} onSelect={(code) => pending === null && handOff(async () => (await checkout.begin(scope, code)).redirectUrl, code)} />
        ) : null}
        {checkout.portalUrl !== undefined ? (
          <button type="button" onClick={() => handOff(() => checkout.portalUrl!(scope), null)}>
            {c.manageBilling}
          </button>
        ) : null}
      </>
    );
  }
  const billingNode = planNode();
  const primary = v.limits.find((l) => l.key === v.hero.primaryKey);

  return (
    <div className="cu-panel" data-state="ready">
      {fulfilment !== null ? (
        <Notice role={fulfilment.kind === "failed" ? "alert" : "status"} tone={fulfilment.kind === "failed" ? "blocked" : "ok"} data-cu="fulfilment" data-state={fulfilment.kind}>
          {fulfilment.kind === "processing" ? (
            c.processing
          ) : fulfilment.kind === "granted" ? (
            <>
              {c.granted(formatWithUnit(fulfilment.amount, labels))}
              {fulfilment.receiptHref !== undefined ? (
                <>
                  {" "}
                  <SafeLink to={{ href: fulfilment.receiptHref }}>{c.viewReceipt}</SafeLink>
                </>
              ) : null}
            </>
          ) : (
            <>
              {c.failed(fulfilment.reason)}
              {fulfilment.retryHref !== undefined ? (
                <>
                  {" "}
                  <SafeLink to={{ href: fulfilment.retryHref }}>{c.tryAgain}</SafeLink>
                </>
              ) : null}
            </>
          )}
        </Notice>
      ) : null}
      {handoffFailed ? (
        <Notice role="alert" tone="blocked">
          {c.checkoutFailed}
        </Notice>
      ) : null}
      {v.notices.map((n, i) => (
        <Notice key={`${n.kind}:${i}`} role="status" tone="warn" data-cu="notice" data-kind={n.kind}>
          <strong>{n.title}</strong>
          {n.body !== undefined ? <p>{n.body}</p> : null}
        </Notice>
      ))}
      <BalanceHero hero={v.hero} pools={v.pools} labels={labels} copy={copy} remedy={primary === undefined || remedyFor === undefined ? undefined : remedyFor(primary)} />
      <section aria-label={c.limitsHeading}>
        <h2 className="cu-heading">{c.limitsHeading}</h2>
        <LimitMeters limits={v.limits} labels={labels} copy={copy} remedyFor={remedyFor} formatTime={formatTime} />
      </section>
      <SpendChart spend={v.spend} labels={labels} copy={copy} />
      <Breakdown breakdown={v.breakdown} labels={labels} copy={copy} />
      <LedgerTable rows={v.ledger} columns={columns} labels={labels} copy={copy} formatTime={formatTime} />
      {billingNode !== null ? (
        <section aria-label={c.billingHeading}>
          <h2 className="cu-heading">{c.billingHeading}</h2>
          {billingNode}
        </section>
      ) : null}
    </div>
  );
}
