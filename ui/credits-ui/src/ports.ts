// The port types of the UI package — the browser-facing contract in specs/001-metering-billing-core/contracts/ (the UI ports).
//
// Framework-free, and nothing here imports anything: no React, no fetch, no store, no host. A ledger column renders to
// `unknown` — the host's own node type — so a Vue or Svelte port reuses this file unchanged.

declare const integerBrand: unique symbol;

/**
 * An integer, and only an integer. `number` cannot say so, and a float that reaches a balance, a delta or a total is how a
 * reconciliation discrepancy gets rounded out of sight of the person best placed to notice it (invariant 4). A value
 * becomes an `Int` where it enters the package — `quantity()` in model.ts — which refuses anything that is not a safe integer
 * instead of rounding it.
 */
export type Int = number & { readonly [integerBrand]: true };

/** Integer minor units of whatever the host meters. Never a float. */
export type Quantity = Int;

/**
 * Opaque to this package: the host's billing subject — workspace, org, user, account. `realm` mirrors the server's isolation
 * axis, so a shared deployment's snapshots can never be rendered under the wrong product's labels.
 */
export interface Scope {
  readonly realm?: string;
  readonly kind: string;
  readonly id: string;
}

/** What the host meters, and what it calls it. The package formats a Quantity with these and never names a unit itself. */
export interface UnitLabels {
  readonly unit: string; // "seats" | "GB-months" | …
  readonly one: string; // the singular
  readonly abbr?: string;
  /**
   * Internal units per displayed unit. The books keep a FINE unit (the reference is 1 unit = 1 µ$ of list price); a host
   * shows dollars with scale 1_000_000 and 2 decimals. Integer division plus a remainder — never a float.
   */
  readonly scale?: Int;
  readonly decimals?: Int;
  readonly format?: (q: Quantity) => string;
}

/**
 * One ceiling. Mirrors the kernel's `Limit` — same shape, so the wire is a pass-through and neither side owns a translation
 * table. There is deliberately no percentage here: the fill is derived from `used` and `cap`, and a pre-computed one is refused.
 */
export interface LimitView {
  readonly key: string;
  readonly label: string;
  readonly used: Quantity;
  readonly cap: Quantity | null; // null = uncapped: a figure, no meter
  /**
   * How a PERSON reads the ceiling — a view vocabulary, deliberately not the kernel's Window enum (the server maps one to the
   * other in one place). "job" drains and "call" is a wall, so a meter must not render them the same way; "none" is a balance,
   * whose `used` is the overdraft it is into and whose `cap` is the overdraft permitted.
   */
  readonly window: "period" | "day" | "hour" | "job" | "call" | "none";
  readonly resetsAt?: string; // ISO; absent when window is "call" or "none"
  readonly warnAtPct: Int; // the limit's own, a whole percent — never a constant in the view; 0 means no warning
  readonly denyCode?: string;
  readonly scopeHint?: string; // "all members" | "you" — set for a member's view
}

export interface BalanceView {
  readonly remaining: Quantity;
  readonly granted: Quantity;
  readonly primaryLimitKey: string; // which LimitView the hero mirrors; "" when the realm shows no limit
  readonly burnRatePerDay?: Quantity;
  /** Per pool, in draw order, when the realm defines pools beyond `general`. The parts sum to `remaining`, or a residual row says why. */
  readonly pools?: ReadonlyArray<{
    readonly key: string;
    readonly label: string;
    readonly remaining: Quantity;
    readonly appliesTo?: string;
  }>;
}

/** One member of a spend dimension. Colour is assigned by INDEX from the categorical ramp; the host never names a colour. */
export interface BreakdownSeries {
  readonly key: string;
  readonly label: string;
  readonly amount: Quantity;
}

/**
 * A ledger entry. `delta` is SIGNED — consumption negative, grants positive — so a subscription grant or a refund lands in the
 * same column with no schema-shaped change. `attributes` is opaque: the package hands it to registered columns and reads no key.
 */
export interface LedgerRow {
  readonly id: string;
  readonly operationType: string; // opaque, never an enum in this package
  readonly delta: Quantity;
  readonly at: string; // ISO
  readonly actor?: { readonly label: string; readonly kind?: string };
  readonly idempotent?: boolean; // renders the `deduped` tag
  readonly attributes: Readonly<Record<string, unknown>>;
}

export interface LedgerColumn {
  readonly key: string;
  readonly header: string;
  readonly align?: "start" | "end";
  readonly accepts: (row: LedgerRow) => boolean;
  readonly render: (row: LedgerRow) => unknown; // the host's framework node
}

/** Where the commercial relationship lives. Absent = this scope IS the billing entity and the panel shows the catalogue inline. */
export interface BillingAnchor {
  readonly label: string;
  readonly kind: string;
  readonly href: string;
  readonly facts: ReadonlyArray<{ readonly label: string; readonly value: string }>;
  readonly rule?: string; // the allocation rule, stated
}

export interface CreditsSnapshot {
  readonly scope: Scope;
  readonly balance: BalanceView;
  readonly limits: readonly LimitView[];
  readonly breakdown: readonly BreakdownSeries[];
  readonly series?: ReadonlyArray<{ readonly at: string; readonly amount: Quantity }>;
  readonly ledger: readonly LedgerRow[];
  readonly anchor?: BillingAnchor;
  readonly notices?: ReadonlyArray<{ readonly kind: string; readonly title: string; readonly body?: string }>;
}

export interface CreditsSource {
  load(scope: Scope): Promise<CreditsSnapshot>;
  subscribe?(scope: Scope, onSnapshot: (s: CreditsSnapshot) => void): () => void;
}

/**
 * One purchasable thing. `kind` is the ONLY branch the package makes on an offer, because the two kinds answer different
 * questions. There is no field a provider's identifier could ride in: `code` is a host slug (invariant 13).
 */
export interface PlanOffer {
  readonly code: string;
  readonly name: string;
  readonly kind: "subscription" | "one_time";
  readonly tagline?: string;
  /** null = not self-serve. Renders a contact action, NOT a disabled buy button: a greyed control implies a checkout that does not exist. */
  readonly price: { readonly minor: Int; readonly currency: string } | null;
  readonly interval?: "month" | "year";
  readonly creditAllotment: Quantity | null; // null = custom / negotiated
  readonly features?: ReadonlyArray<{ readonly label: string; readonly value: string }>;
  readonly current?: boolean; // renders inert, never a no-op button
  /** Set when this offer cannot be taken from here. The package renders `reason` and `remedy` and exposes no checkout action — there is no "proceed anyway". */
  readonly blocked?: {
    readonly reason: string;
    readonly remedy?: { readonly label: string; readonly href: string };
  };
}

export interface PlanCatalog {
  readonly offers: readonly PlanOffer[];
  /** false → the viewer may browse but not buy (the server would 403). The package renders the reason and `requestPath`, never a control that will fail. */
  readonly canPurchase: boolean;
  readonly cannotPurchaseReason?: string;
  readonly requestPath?: { readonly label: string; readonly href: string };
  /** Where an offer that is not self-serve sends the viewer. Absent: the offer says so in words and offers no control. */
  readonly contact?: { readonly label: string; readonly href: string };
}

/** Fulfilment is webhook-driven, so `granted` is a state the SERVER reports — never one the client infers from having returned from checkout. */
export type FulfilmentState =
  | { readonly kind: "none" }
  | { readonly kind: "processing"; readonly since: string }
  | { readonly kind: "granted"; readonly amount: Quantity; readonly receiptHref?: string }
  | { readonly kind: "failed"; readonly reason: string; readonly retryHref?: string };

export interface CheckoutSource {
  catalog(scope: Scope): Promise<PlanCatalog>;
  /** Returns a URL to hand off to. It MUST NOT resolve to a granted state — the caller navigates, and fulfilment arrives later via `fulfilment()`. */
  begin(scope: Scope, planCode: string): Promise<{ readonly redirectUrl: string }>;
  fulfilment(scope: Scope): Promise<FulfilmentState>;
  portalUrl?(scope: Scope): Promise<string>;
}
