# Contract: Credits, Limits & Billing UI (reusable ports)

**Plan**: [../plan.md](../plan.md) | **Status**: Design addition — the reusability seam for the browser-facing half of metering (FR-016…FR-020, US4). It factors the balance/limits/ledger surface into ports so the *same* components render **any** metered product's position, and so this product's credit vocabulary plugs in as data rather than as component internals. **Changes no Phase 1 behavior** — the same screen shows the same numbers; this names the render seam before the screen is built, which is the cheapest moment to name it. Same treatment [metering-ports.md](./metering-ports.md), [stream-ui-ports.md](./stream-ui-ports.md), [audit-ports.md](./audit-ports.md), [notification-ports.md](./notification-ports.md) and [approval-ports.md](./approval-ports.md) gave their backbones.

> **Why this contract exists at all.** The backend half of metering is already exemplary: `kernel/metering` is an import-clean hexagon with a `Pricer`/`Ledger`/`Meter` port set, a generalization checklist, a `git mv` compile litmus, and a conformance suite (T091a) that *proves* reuse by running the same `PricerContract` against a second fixture `Pricer`. The UI half has none of that, and [stream-ui-ports.md](./stream-ui-ports.md) coupling #2 deliberately **ejects** credit UI from the reusable stream package via `ChromeSlots` — correctly, since a stream renderer must not know what a credit is. The consequence is that no contract owns the credit surface, so by default it would be built the way the mockups are drawn: hardcoded to three specific ceilings, to the word "credits", and to the feature names `Query`/`Ingestion`/`Captioning`/`Rerank`.
>
> **The asymmetry is the bug.** The kernel already models limits generically as `[]Limit` with `Window`/`WarnAt`/`DenyCode`, and spend generically as `Unit` + `Quantity`. Re-hardcoding that generality away in the view layer discards it at the last step.

---

## Why: the five couplings this removes

| # | Coupling if built from the mockup as drawn | Evidence it is a coupling | The port that removes it |
|---|---|---|---|
| 1 | **Three specific ceilings**, hardcoded as three cards | `credits.html` draws "Workspace pool", "Your daily allowance", "Your per-call output cap" as three bespoke blocks. A product with one ceiling renders two empty cards; one with five cannot render the other two | `LimitView[]` — the panel renders an **ordered list of limits** from the kernel's `[]Limit`, whatever its length |
| 2 | **"Credits" is baked into the copy** | Every label, the hero, the ledger column header and the empty states say *credits*. A product metering seats, GB-months or API calls inherits a screen that lies about its own unit | `UnitLabels` — unit name, symbol, precision and pluralization injected; the kernel formats a `Quantity`, never a "credit" |
| 3 | **This product's spend taxonomy is a component detail** | `Query`/`Ingestion`/`Captioning`/`Rerank` appear as a fixed four-item list with a fixed four-colour ramp, in two places, in a fixed order | `BreakdownSeries[]` — the host supplies dimension members; the ramp assigns slots by index, and > 4 folds to `Other` |
| 4 | **The ledger row is a fixed 7-column table** | Columns `operation/actor/feature/model/tokens/credits/when` are the *schema*, and `model`/`tokens` are LLM-specific. A storage product has no `model` | `LedgerColumn[]` + `LedgerRow` with an opaque `attributes` bag — the product's columns are registered, exactly as `TraceSection` registers run-detail |
| 5 | **Billing anchoring is assumed to be org-level** | Both mockups hardcode "the organization buys, workspaces are allocated". A single-tenant product has no such level, and a reseller has three | `BillingAnchor` — the anchor is described to the component (or absent), rather than assumed |

The rule, stated once: **the credits-UI kernel renders balances, limits, spend breakdowns and a ledger over an opaque `Unit` and an opaque `Scope`; only the `UnitLabels`, the registered `LedgerColumn`s, the `BreakdownSeries` and the `BillingAnchor` are product-specific.**

---

## Ports at a glance

```text
SOURCE (any) ── our BFF · a fixture · a billing vendor's API · an in-process meter
   │  CreditsSource.load(scope) → CreditsSnapshot        (poll or subscribe)
   ▼
┌── CreditsModel (pure · no React, no app deps) ────────────────────────────────┐
│   snapshot → BalanceView + LimitView[] + BreakdownView + LedgerPage    PURE   │
│   threshold state: ok | warn | blocked  (from Limit.warnAt/denyCode)   PURE   │
│   NEVER interprets `unit`, `attributes`, or a ledger `operationType`   PURE   │
└───────────────────────────────────┬───────────────────────────────────────────┘
                                     ▼
┌── <CreditsPanel> (generic view · props-only) ────────────────────────────────┐
│   BalanceHero · LimitMeters · SpendChart · Breakdown · LedgerTable            │
│   delegates row rendering to ───────────────────────────────────────────┐    │
└─────────────────────────────────────────────────────────────────────────┼─────┘
                                                                           ▼
┌── LedgerColumnRegistry (the pluggable detail renderer) ──────────────────────┐
│   ModelColumn · TokensColumn   (ours — LLM-specific, registered not built-in)│
│   (register another: SeatsColumn · EgressColumn — never edit the table;      │
│    an UNKNOWN column key falls back to GenericAttributeColumn)               │
└──────────────────────────────────────────────────────────────────────────────┘
        BillingSlots (optional) → plan card · anchor pointer · receipts link
```

Four seams a host swaps independently: the **`CreditsSource`** (its transport), the **`LedgerColumnRegistry`** (its spend vocabulary), the **`BillingSlots`** + `BillingAnchor` (its commercial model), and the **theme contract** (its design tokens). The reference implementation uses our BFF and an LLM column set, but nothing in the port signatures requires either.

---

## Domain types

```typescript
// frontend/src/credits-ui/ports.ts — ZERO imports from features/, lib/, or the app store.

/** Opaque to this package. The host's billing subject: workspace, org, user, account. */
export interface Scope { readonly kind: string; readonly id: string; }

/** Integer minor units. Never a float — the money rule from metering-ports.md holds
 *  identically in the view layer, where a rounded display is how a reconciliation
 *  discrepancy gets hidden from the person best placed to notice it. */
export type Quantity = number;

/** What the host meters. `credits` is OUR value, not the package's vocabulary. */
export interface UnitLabels {
  readonly unit: string;            // "credits" | "seats" | "GB-months"
  readonly one: string;             // "credit"
  readonly abbr?: string;           // "cr"
  readonly format?: (q: Quantity) => string;  // default: grouped integer
}

/** One ceiling. Mirrors kernel/metering `Limit` — same shape, so the wire is a
 *  pass-through and neither side owns a translation table. */
export interface LimitView {
  readonly key: string;             // stable id; also the a11y label key
  readonly label: string;
  readonly used: Quantity;
  readonly cap: Quantity | null;    // null = uncapped (render as a figure, no meter)
  readonly window: "period" | "day" | "hour" | "call" | "none";
  readonly resetsAt?: string;       // ISO; omitted when window === "call" | "none"
  readonly warnAtPct: number;       // from Limit.WarnAt — NOT a hardcoded 80
  readonly denyCode?: string;       // maps to the host's 402/429 (metering-ports.md)
  readonly scopeHint?: string;      // "all members" | "you"
}

export interface BalanceView {
  readonly remaining: Quantity;
  readonly granted: Quantity;
  readonly primaryLimitKey: string; // which LimitView the hero mirrors
  readonly burnRatePerDay?: Quantity;
}

/** One member of a spend dimension. Colour is assigned by INDEX from the
 *  categorical ramp; the host never names a colour. */
export interface BreakdownSeries {
  readonly key: string;
  readonly label: string;
  readonly amount: Quantity;
}

/** A ledger entry. `delta` is SIGNED — consumption negative, grants positive — which
 *  is what lets a Phase-2 `subscription_grant`/`refund` land in the same column with
 *  no schema-shaped change (see pages/credits.md). `attributes` is opaque: the
 *  package passes it to registered columns and never reads a key. */
export interface LedgerRow {
  readonly id: string;
  readonly operationType: string;   // opaque string, never an enum in this package
  readonly delta: Quantity;
  readonly at: string;              // ISO
  readonly actor?: { readonly label: string; readonly kind?: string };
  readonly idempotent?: boolean;    // renders the `deduped` tag (FR-019)
  readonly attributes: Readonly<Record<string, unknown>>;
}

export interface LedgerColumn {
  readonly key: string;
  readonly header: string;
  readonly align?: "start" | "end";
  readonly accepts: (row: LedgerRow) => boolean;
  readonly render: (row: LedgerRow) => unknown;   // host's framework node
}

/** Where the commercial relationship lives. Absent = this scope IS the billing
 *  entity, and the panel shows plan controls inline instead of a pointer. */
export interface BillingAnchor {
  readonly label: string;           // "Acme Corp"
  readonly kind: string;            // "organization"
  readonly href: string;
  readonly facts: ReadonlyArray<{ readonly label: string; readonly value: string }>;
  readonly rule?: string;           // the allocation rule, stated
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
```

### Plan catalogue & checkout

```typescript
/** One purchasable thing. `kind` is the ONLY branch the package makes on an offer,
 *  because the two kinds answer different user questions and must not be presented
 *  as interchangeable rows in one list. */
export interface PlanOffer {
  readonly code: string;                       // stable slug; never a provider price id
  readonly name: string;
  readonly kind: "subscription" | "one_time";
  readonly tagline?: string;
  /** null = not self-serve (invoiced). Renders a `contact` action, NOT a disabled
   *  buy button — a greyed-out purchase control implies checkout that doesn't exist. */
  readonly price: { readonly minor: number; readonly currency: string } | null;
  readonly interval?: "month" | "year";
  readonly creditAllotment: number | null;     // null = custom/negotiated
  readonly features?: ReadonlyArray<{ readonly label: string; readonly value: string }>;
  readonly current?: boolean;                  // renders inert, never a no-op button
  /** Set when this offer cannot be taken from the current position. The package
   *  renders `reason` and `remedy` and refuses to expose a checkout action —
   *  there is no "proceed anyway" path. */
  readonly blocked?: {
    readonly reason: string;
    readonly remedy?: { readonly label: string; readonly href: string };
  };
}

export interface PlanCatalog {
  readonly offers: readonly PlanOffer[];
  /** false → the viewer may browse but not buy (server would 403). The package
   *  renders the stated reason + `requestPath`, never a control that will fail. */
  readonly canPurchase: boolean;
  readonly cannotPurchaseReason?: string;
  readonly requestPath?: { readonly label: string; readonly href: string };
}

/** Fulfilment is webhook-driven, so `granted` is a state the SERVER reports —
 *  never one the client infers from having returned from checkout. */
export type FulfilmentState =
  | { readonly kind: "none" }
  | { readonly kind: "processing"; readonly since: string }
  | { readonly kind: "granted"; readonly amount: Quantity; readonly receiptHref?: string }
  | { readonly kind: "failed"; readonly reason: string; readonly retryHref?: string };

export interface CheckoutSource {
  catalog(scope: Scope): Promise<PlanCatalog>;
  /** Returns a URL to hand off to. It MUST NOT resolve to a granted state — the
   *  caller navigates, and fulfilment arrives later via `fulfilment()`. */
  begin(scope: Scope, planCode: string): Promise<{ readonly redirectUrl: string }>;
  fulfilment(scope: Scope): Promise<FulfilmentState>;
  portalUrl?(scope: Scope): Promise<string>;
}
```

---

## Invariants every implementation MUST uphold

1. **The meter fill encodes consumption, never remainder.** A hero reading "12,480 left" beside a bar filled to 82% is only coherent under one reading, and it must be the same reading on every screen. The component derives fill from `used/cap` and refuses to accept a pre-computed percentage.
2. **`warnAtPct` comes from the limit, never from a constant.** The threshold is admin-configurable (FR-017); a hardcoded 80 in the view silently overrides an operator's setting.
3. **Never a silent failure state.** A limit at or past `cap` renders a blocking notice carrying `denyCode` and a remedy affordance (FR-018, SC-010). "Renders nothing" is not a permitted state for an exhausted limit.
4. **Money is integer, end to end.** No float reaches a balance, a delta or a total. Display rounding never changes a rendered figure's value.
5. **Totals shown together must reconcile.** If the panel shows both a total and its parts, the parts sum to the total, or the panel renders an explicit residual row. This is the view-layer half of SC-006 — a screen selling exact reconciliation must not itself present arithmetic that does not close.
6. **The package never interprets `unit`, `operationType`, or an `attributes` key.** Any behavior conditioned on the string `"credits"`, `"query"`, or `"model"` inside `credits-ui/**` is a defect.
7. **Colour never carries identity alone.** Every series renders its label and value as text; the categorical ramp is decoration on top of an already-readable row.
8. **Status hues and categorical hues are disjoint sets** (MASTER.md). A limit's tone comes from its threshold state; a series' colour comes from its index. Neither borrows from the other.
9. **Every meter is a `role="progressbar"`** with `aria-valuenow` and an `aria-valuetext` that states the ratio in the host's units.
10. **`granted` is never inferred from a checkout return.** The package renders `processing` on return and only shows `granted` when `CheckoutSource.fulfilment()` reports it. Any client-side optimism here produces the single worst failure this surface can have — a user who believes a payment failed and pays twice.
11. **A blocked offer exposes no checkout action.** `PlanOffer.blocked` renders reason + remedy; there is no override affordance, because the block exists to prevent a change that would silently shrink someone else's budget.
12. **A non-purchasing viewer is told, not tested.** When `canPurchase` is false the package states the reason, offers the request path, and renders every purchase control **inert** — never live. The distinction from invariant 11 matters: a `blocked` offer exposes *no* action because the purchase itself is invalid, whereas here the purchase is valid and the *viewer* lacks authority, so the control is shown disabled to convey exactly that. Either way the 403 is a server guarantee, never a UI surprise.
13. **Provider identifiers and card data never enter the package.** `PlanOffer.code` is a host slug; a `provider_price_id` reaching this layer is a defect.
14. **Role checks live in one place.** A host surface that links to the catalogue must not re-implement the purchase-permission test — it navigates, and the catalogue degrades. Two copies of an authorization rule is one copy too many.

---

## Reference wiring: AISAT credits as ONE implementation

```typescript
// frontend/src/features/credits/  — the APP side. All product specifics live here.
const labels: UnitLabels = { unit: "credits", one: "credit", abbr: "cr" };

const columns: LedgerColumn[] = [
  { key: "model",  header: "Model",  accepts: r => "model" in r.attributes,
    render: r => <Mono>{r.attributes.model as string}</Mono> },
  { key: "tokens", header: "Tokens", align: "end",
    accepts: r => "tokens" in r.attributes,
    render: r => <Mono>{fmt(r.attributes.tokens as number)}</Mono> },
];

<CreditsPanel
  source={new BffCreditsSource("/credits")}
  scope={{ kind: "workspace", id: workspaceId }}
  labels={labels}
  columns={columns}
  billing={{ planCard: <OrgPlanPointer/> }}   // BillingSlots — optional
/>
```

Swapping this for a storage product is: a different `UnitLabels`, an `EgressColumn` instead of `ModelColumn`/`TokensColumn`, no `BillingAnchor`, and a different `CreditsSource`. **No component edit.** That is the same swap `metering-ports.md` promises on the server (`Pricer` + `Scope`), and the two halves now match.

---

## Wire gap this exposes (must be closed for the screen to be buildable)

`GET /credits` currently returns `{ balance, warning_threshold_pct, near_limit }` ([bff-rest.md](./bff-rest.md)). The designed screen needs materially more, and the gap is real rather than cosmetic — today there is **no endpoint** that returns the per-user daily allowance, the per-call cap, the time series, the breakdown, or the ledger page:

| Snapshot field | Backing source | Status |
|---|---|---|
| `balance.remaining` / `granted` | `workspace_credits` | present |
| `limits[]` (pool, daily, per-call) | kernel `[]Limit` | **missing from the wire** — the kernel has it; the BFF does not expose it |
| `balance.burnRatePerDay` | `token_usage_daily` | **missing** |
| `series[]` (14-day) | `token_usage_daily` | **missing** |
| `breakdown[]` (by feature) | `llm_cost_daily` | admin-only today (`/admin/usage`); needs a member-scoped, self-only view |
| `ledger[]` + `idempotent` | `credit_ledger` | **missing** — FR-019's "charged at most once" is currently unobservable to the member it protects |
| `anchor` (Phase 2) | `organization_credits` | Phase 2 |

Extending `GET /credits` to return `CreditsSnapshot` keeps one round trip and one shape. The `limits[]` array is the load-bearing part: it is what makes the ceiling panel generic instead of three hardcoded cards, and the kernel already has the data in exactly that shape.

---

## Extraction-ready code organization

```text
frontend/src/credits-ui/          # the package — zero app imports
  ports.ts                        # the types above
  model.ts                        # pure snapshot → view derivation
  CreditsPanel.tsx                # composition root
  components/{BalanceHero,LimitMeter,SpendChart,Breakdown,LedgerTable}.tsx
  theme.css                       # reads --su-* ONLY
frontend/src/features/credits/    # the app — every product specific
  BffCreditsSource.ts  labels.ts  columns.tsx  OrgPlanPointer.tsx
```

Enforced by the same ESLint `no-restricted-paths` rule that guards `stream-ui/` (coupling #3 there), plus the static scan below. The litmus: **`git mv frontend/src/credits-ui/ ../some-other-app/src/ && npm run build` compiles with zero edits.**

---

## Generalization checklist (before reusing this in another product)

- [ ] **Unit labelled** — `UnitLabels` supplied; no occurrence of the string `credit` inside `credits-ui/**`.
- [ ] **Limits sourced from the kernel** — `limits[]` comes from `[]Limit`, including `warnAtPct` and `denyCode`; the view hardcodes no threshold and no ceiling count.
- [ ] **Ledger columns registered** — product columns via `LedgerColumnRegistry`; an unknown key falls back to the generic column rather than erroring.
- [ ] **Signed deltas** — one signed column, not debit/credit pairs, so grants and refunds need no new column.
- [ ] **Billing anchor described or omitted** — no assumption that an org exists above the scope.
- [ ] **Catalogue supplied or omitted** — a product with no self-serve purchase passes no `CheckoutSource`; the panel renders without a single plan affordance and without error.
- [ ] **Fulfilment is server-reported** — `granted` comes from `fulfilment()`, never from a redirect.
- [ ] **Purchase permission is data** — `canPurchase` + reason supplied by the host; the package embeds no role model.
- [ ] **Blocked states wired** — every `denyCode` maps to a visible, actionable notice.
- [ ] **Theme via `--su-*` only** — no hex, no Tailwind palette utility, no literal `rounded-*` under `credits-ui/**`.
- [ ] **Reconciliation holds** — parts sum to totals, or a residual is shown.

---

## Non-goals (stays in the host, by design)

- **Pricing and rate cards** — a `Pricer` concern; this package renders what was charged, never what things cost.
- **Checkout, payment methods, receipts** — the payments boundary (`kernel/billing`, Phase 2). The panel links to them via `BillingAnchor`/`BillingSlots` and renders no card data and no provider IDs, ever ([pages/credits.md](../../../design-system/aisat-intel/pages/credits.md) Don'ts).
- **What produces spend** — call sites build the events; this package never instruments anything.
- **Tenancy semantics** — what a `Scope` *means* is the host's; the package treats it as an opaque identity.
