// The wire, mapped once, at the edge. A browser holds no /v1 credential (rest-api.md): the host proxies these routes behind its own session,
// one to one, under `baseUrl`, and the browser calls the proxy with its cookie. Everything that arrives is decoded here into the package's
// types — integers branded by the package's own quantity(), which refuses a fraction rather than rounding it — and nothing the binding does
// not name travels any further, so a field the server grows tomorrow cannot reach the screen by accident.
import {
  quantity,
  type CheckoutSource,
  type CreditsSnapshot,
  type CreditsSource,
  type FulfilmentState,
  type LedgerPage,
  type LedgerRow,
  type LimitView,
  type PlanCatalog,
  type PlanOffer,
  type Scope,
} from "@intel-payment/credits-ui";
import { humanize, limitLabels, poolLabels } from "./labels.js";

export class WireError extends Error {
  readonly path: string;
  constructor(path: string, message: string) {
    super(`${path}: ${message}`);
    this.name = "WireError";
    this.path = path;
  }
}

type Rec = Record<string, unknown>;
const rec = (v: unknown, path: string): Rec => {
  if (typeof v !== "object" || v === null || Array.isArray(v)) throw new WireError(path, "expected an object");
  return v as Rec;
};
const arr = (v: unknown, path: string): unknown[] => {
  if (!Array.isArray(v)) throw new WireError(path, "expected a list");
  return v;
};
const str = (v: unknown, path: string): string => {
  if (typeof v !== "string") throw new WireError(path, "expected a string");
  return v;
};
const optStr = (v: unknown, path: string): string | undefined => (v === undefined || v === null || v === "" ? undefined : str(v, path));
const int = (v: unknown, path: string) => quantity(v as number, path);

const WINDOWS: readonly LimitView["window"][] = ["period", "day", "hour", "job", "call", "none"];

function toLimit(v: unknown, path: string): LimitView {
  const o = rec(v, path);
  const key = str(o.key, `${path}.key`);
  const window = str(o.window, `${path}.window`) as LimitView["window"];
  if (!WINDOWS.includes(window)) throw new WireError(`${path}.window`, `"${window}" is not a window this view has a word for`);
  const serverLabel = optStr(o.label, `${path}.label`);
  const resetsAt = optStr(o.resetsAt, `${path}.resetsAt`);
  const denyCode = optStr(o.denyCode, `${path}.denyCode`);
  const scopeHint = optStr(o.scopeHint, `${path}.scopeHint`);
  return {
    key,
    label: limitLabels[key] ?? (serverLabel !== undefined && serverLabel !== key ? serverLabel : humanize(key)),
    used: int(o.used, `${path}.used`),
    cap: o.cap === null ? null : int(o.cap, `${path}.cap`),
    window,
    ...(resetsAt === undefined ? {} : { resetsAt }),
    warnAtPct: int(o.warnAtPct, `${path}.warnAtPct`),
    ...(denyCode === undefined ? {} : { denyCode }),
    ...(scopeHint === undefined ? {} : { scopeHint }),
  };
}

/** GET /v1/credits — the contract's own shape — as a CreditsSnapshot. */
export function toSnapshot(wire: unknown): CreditsSnapshot {
  const w = rec(wire, "snapshot");
  const scope = rec(w.scope, "snapshot.scope");
  const realm = optStr(scope.realm, "snapshot.scope.realm");
  const balance = rec(w.balance, "snapshot.balance");
  const ledgerNext = optStr(w.ledgerNext, "snapshot.ledgerNext"); // absent, null and empty are all the end: a cursor is a non-empty string or nothing
  const burn = balance.burnRatePerDay === undefined || balance.burnRatePerDay === null ? undefined : int(balance.burnRatePerDay, "snapshot.balance.burnRatePerDay");
  const pools = balance.pools === undefined ? undefined : arr(balance.pools, "snapshot.balance.pools").map((p, i) => {
    const path = `snapshot.balance.pools[${i}]`;
    const o = rec(p, path);
    const key = str(o.key, `${path}.key`);
    const appliesTo = optStr(o.appliesTo, `${path}.appliesTo`);
    return { key, label: poolLabels[key] ?? humanize(key), remaining: int(o.remaining, `${path}.remaining`), ...(appliesTo === undefined ? {} : { appliesTo }) };
  });
  return {
    scope: { kind: str(scope.kind, "snapshot.scope.kind"), id: str(scope.id, "snapshot.scope.id"), ...(realm === undefined ? {} : { realm }) },
    balance: {
      remaining: int(balance.remaining, "snapshot.balance.remaining"),
      granted: int(balance.granted, "snapshot.balance.granted"),
      primaryLimitKey: str(balance.primaryLimitKey, "snapshot.balance.primaryLimitKey"),
      ...(burn === undefined ? {} : { burnRatePerDay: burn }),
      ...(pools === undefined ? {} : { pools }),
    },
    limits: arr(w.limits, "snapshot.limits").map((l, i) => toLimit(l, `snapshot.limits[${i}]`)),
    breakdown: arr(w.breakdown ?? [], "snapshot.breakdown").map((b, i) => {
      const o = rec(b, `snapshot.breakdown[${i}]`);
      return { key: str(o.key, "key"), label: str(o.label, "label"), amount: int(o.amount, `snapshot.breakdown[${i}].amount`) };
    }),
    ...(w.series === undefined ? {} : { series: arr(w.series, "snapshot.series").map((p, i) => ({ at: str(rec(p, "point").at, "at"), amount: int(rec(p, "point").amount, `snapshot.series[${i}].amount`) })) }),
    ledger: arr(w.ledger, "snapshot.ledger").map((r, i): LedgerRow => {
      const path = `snapshot.ledger[${i}]`;
      const o = rec(r, path);
      const actor = o.actor === undefined || o.actor === null ? undefined : str(rec(o.actor, `${path}.actor`).label, `${path}.actor.label`);
      return {
        id: str(o.id, `${path}.id`),
        operationType: str(o.operationType, `${path}.operationType`),
        delta: int(o.delta, `${path}.delta`),
        at: str(o.at, `${path}.at`),
        ...(actor === undefined ? {} : { actor: { label: actor } }),
        attributes: o.attributes === undefined ? {} : rec(o.attributes, `${path}.attributes`),
      };
    }),
    // null is the server's end of the ledger, not a cursor: the package reads absence as the end, so null is not carried
    ...(ledgerNext === undefined ? {} : { ledgerNext }),
  };
}

/** GET /v1/plans — snake_case, priced per currency — as offers in one currency, the current plan marked. Names every field it keeps. */
export function toOffers(wire: unknown, currency: string, currentPlan: string | null): PlanOffer[] {
  return arr(rec(wire, "plans").data, "plans.data").map((p, i) => {
    const path = `plans.data[${i}]`;
    const o = rec(p, path);
    const code = str(o.code, `${path}.code`);
    const kind = str(o.kind, `${path}.kind`);
    if (kind !== "subscription" && kind !== "one_time") throw new WireError(`${path}.kind`, `"${kind}" is neither a subscription nor a one-time purchase`);
    const interval = optStr(o.interval, `${path}.interval`);
    if (interval !== undefined && interval !== "month" && interval !== "year") throw new WireError(`${path}.interval`, `"${interval}" is not month or year`);
    const minor = rec(o.prices ?? {}, `${path}.prices`)[currency];
    return {
      code,
      name: str(o.name, `${path}.name`),
      kind,
      price: minor === undefined ? null : { minor: int(minor, `${path}.prices.${currency}`), currency },
      ...(interval === undefined ? {} : { interval }),
      creditAllotment: o.credit_allotment === null || o.credit_allotment === undefined ? null : int(o.credit_allotment, `${path}.credit_allotment`),
      ...(currentPlan === code ? { current: true } : {}),
    };
  });
}

/** GET /v1/fulfilment. The server has no retry address; a host that has one passes it. */
export function toFulfilment(wire: unknown, retryHref?: string): FulfilmentState {
  const o = rec(wire, "fulfilment");
  const kind = str(o.kind, "fulfilment.kind");
  switch (kind) {
    case "none":
      return { kind: "none" };
    case "processing":
      return { kind: "processing", since: optStr(o.since, "fulfilment.since") ?? "" };
    case "granted": {
      if (o.amount === undefined || o.amount === null) throw new WireError("fulfilment.amount", "granted without an amount");
      const receiptHref = optStr(o.receipt_url, "fulfilment.receipt_url");
      return { kind: "granted", amount: int(o.amount, "fulfilment.amount"), ...(receiptHref === undefined ? {} : { receiptHref }) };
    }
    case "failed":
      return { kind: "failed", reason: optStr(o.reason, "fulfilment.reason") ?? "", ...(retryHref === undefined ? {} : { retryHref }) };
    default:
      throw new WireError("fulfilment.kind", `"${kind}" is not a state the server reports`);
  }
}

// ── transport ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────

export interface RestOptions {
  /** Where the host's proxy is mounted; it forwards /v1/… to the service, one to one, behind the host's session. */
  readonly baseUrl: string;
  readonly fetch?: typeof fetch;
}

class Http {
  readonly #base: string;
  readonly #fetch: typeof fetch;
  constructor(o: RestOptions) {
    this.#base = o.baseUrl.replace(/\/+$/, "");
    this.#fetch = o.fetch ?? ((...a) => globalThis.fetch(...a));
  }
  async get(path: string, params: Record<string, string | undefined>): Promise<unknown> {
    return this.#read(await this.#fetch(`${this.#base}${path}?${query(params)}`, { method: "GET", credentials: "include", headers: { accept: "application/json" } }));
  }
  async post(path: string, body: unknown, idempotencyKey: string): Promise<unknown> {
    return this.#read(await this.#fetch(`${this.#base}${path}`, { method: "POST", credentials: "include", headers: { accept: "application/json", "content-type": "application/json", "idempotency-key": idempotencyKey }, body: JSON.stringify(body) }));
  }
  async #read(res: Response): Promise<unknown> {
    if (!res.ok) throw new WireError(res.url || "request", `HTTP ${res.status}`);
    return res.json();
  }
}

function query(params: Record<string, string | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined) q.set(k, v);
  return q.toString();
}

const scopeParams = (s: Scope) => ({ scope_kind: s.kind, scope_id: s.id, scope_realm: s.realm });

export interface RestCreditsOptions extends RestOptions {
  /**
   * A member's view: the scope-wide ceilings and that member's own, with only that member's ledger rows, and no spend section (the server's
   * rollup cannot tell a member's days from a colleague's). It is named on the snapshot and on EVERY older page — a page the member scrolls to
   * must still be theirs, or paging would be the way to read a colleague's usage.
   */
  readonly member?: { readonly kind: string; readonly id: string };
  /** The length of the spend series, in days. Not sent unless set: the server's default is the server's to choose. */
  readonly days?: number;
  /** The ledger page size. Not sent unless set, for the same reason. */
  readonly pageSize?: number;
}

export class RestCreditsSource implements CreditsSource {
  readonly #http: Http;
  readonly #o: RestCreditsOptions;
  constructor(options: RestCreditsOptions) {
    this.#http = new Http(options);
    this.#o = options;
  }
  #params(scope: Scope, cursor?: string): Record<string, string | undefined> {
    const { member, days, pageSize } = this.#o;
    return {
      ...scopeParams(scope),
      subject_kind: member?.kind,
      subject_id: member?.id,
      days: days === undefined ? undefined : String(days),
      limit: pageSize === undefined ? undefined : String(pageSize),
      cursor,
    };
  }
  async load(scope: Scope): Promise<CreditsSnapshot> {
    return toSnapshot(await this.#http.get("/v1/credits", this.#params(scope)));
  }
  /** An older page is the same endpoint with the cursor: the contract's snapshot, of which the page is the ledger. */
  async moreLedger(scope: Scope, cursor: string): Promise<LedgerPage> {
    const s = toSnapshot(await this.#http.get("/v1/credits", this.#params(scope, cursor)));
    return { rows: s.ledger, ...(s.ledgerNext === undefined ? {} : { next: s.ledgerNext }) };
  }
}

export interface RestCheckoutOptions extends RestOptions {
  readonly currency: string;
  /** Where the provider sends the buyer back to; the server only accepts https on hosts it was configured with. */
  readonly successUrl: string;
  readonly cancelUrl: string;
  readonly returnUrl: string;
  /** The host's own permission test, supplied as data (invariant 12): the package embeds no role model. */
  readonly canPurchase?: boolean;
  readonly cannotPurchaseReason?: string;
  readonly requestPath?: { readonly label: string; readonly href: string };
  readonly contact?: { readonly label: string; readonly href: string };
  readonly retryHref?: string;
}

export class RestCheckoutSource implements CheckoutSource {
  readonly #http: Http;
  readonly #o: RestCheckoutOptions;
  readonly #kinds = new Map<string, PlanOffer["kind"]>();
  constructor(options: RestCheckoutOptions) {
    this.#http = new Http(options);
    this.#o = options;
  }

  async catalog(scope: Scope): Promise<PlanCatalog> {
    const [plans, subscription] = await Promise.all([this.#http.get("/v1/plans", { currency: this.#o.currency, realm: scope.realm }), this.#http.get("/v1/subscription", scopeParams(scope))]);
    // a scope that has never subscribed has none, and the server says null
    const current = subscription === null ? null : str(rec(subscription, "subscription").plan_code, "subscription.plan_code");
    const offers = toOffers(plans, this.#o.currency, current);
    this.#kinds.clear();
    for (const o of offers) this.#kinds.set(o.code, o.kind);
    const o = this.#o;
    return {
      offers,
      canPurchase: o.canPurchase ?? true,
      ...(o.cannotPurchaseReason === undefined ? {} : { cannotPurchaseReason: o.cannotPurchaseReason }),
      ...(o.requestPath === undefined ? {} : { requestPath: o.requestPath }),
      ...(o.contact === undefined ? {} : { contact: o.contact }),
    };
  }

  /** A checkout names a plan and never a price: the catalogue says what a plan costs, and the server rejects a body that tries. */
  async begin(scope: Scope, planCode: string): Promise<{ readonly redirectUrl: string }> {
    const kind = this.#kinds.get(planCode);
    if (kind === undefined) throw new WireError("plan", `"${planCode}" is not in the catalogue this screen showed`);
    const out = rec(
      await this.#http.post(
        "/v1/checkout",
        {
          scope: { kind: scope.kind, id: scope.id, ...(scope.realm === undefined ? {} : { realm: scope.realm }) },
          plan_code: planCode,
          currency: this.#o.currency,
          mode: kind === "subscription" ? "subscription" : "payment",
          success_url: this.#o.successUrl,
          cancel_url: this.#o.cancelUrl,
        },
        globalThis.crypto.randomUUID(), // a press is a checkout; the panel does not let one press become two
      ),
      "checkout",
    );
    return { redirectUrl: str(out.redirect_url, "checkout.redirect_url") };
  }

  async fulfilment(scope: Scope): Promise<FulfilmentState> {
    return toFulfilment(await this.#http.get("/v1/fulfilment", scopeParams(scope)), this.#o.retryHref);
  }

  async portalUrl(scope: Scope): Promise<string> {
    return str(rec(await this.#http.get("/v1/portal", { ...scopeParams(scope), return_url: this.#o.returnUrl }), "portal").portal_url, "portal.portal_url");
  }
}
