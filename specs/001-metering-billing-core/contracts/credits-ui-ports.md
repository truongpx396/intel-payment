# Contract: Credits, Limits & Billing UI (core ports)

**Spec**: [../spec.md](../spec.md) | **Plan**: [../plan.md](../plan.md) | **Status**: Normative — the browser-facing half of `intel-payment`.

The server half of this system ([metering-ports.md](./metering-ports.md)) models limits generically as `[]Limit` with `Window`/`WarnAt`/`DenyCode`, and spend generically as `Unit` + `Quantity`. **Re-hardcoding that generality away in the view layer discards it at the last step** — which is what happens by default, because credit screens get built from a mockup, and a mockup is drawn for one product.

This contract is the seam that stops it. The `credits-ui` package renders balances, limits, spend breakdowns, a ledger and a plan catalogue over an **opaque `Unit`** and an **opaque `Scope`**; only the `UnitLabels`, the registered `LedgerColumn`s, the `BreakdownSeries` and the `BillingAnchor` are product-specific.

> **Provenance.** Extracted with history from [aisat-intel](https://github.com/truongpx396/aisat-intel), where it was written as the render seam for that product's credits screen. See [PROVENANCE.md](../../../PROVENANCE.md).

> **Framework independence.** The types below are TypeScript and framework-free. `model.ts` is pure — no React, no store, no fetch. Only the components bind to a framework; the reference implementation is React, and a Vue or Svelte port reuses `ports.ts` + `model.ts` unchanged. A host that wants none of it consumes [rest-api.md](./rest-api.md) directly and renders its own.

---

## Why: the five couplings this removes

Each row is a real coupling a credit screen acquires when it is built from one product's mockup.

| # | Coupling | What it costs another product | The port that removes it |
|---|---|---|---|
| 1 | **A fixed number of ceilings**, hardcoded as N bespoke cards | A product with one ceiling renders N−1 empty cards; one with N+2 cannot render the rest | `LimitView[]` — the panel renders an **ordered list** from the kernel's `[]Limit`, whatever its length |
| 2 | **"Credits" baked into the copy** | A product metering seats, GB-months or API calls inherits a screen that lies about its own unit | `UnitLabels` — unit name, symbol, precision and pluralization injected; the package formats a `Quantity`, never a "credit" |
| 3 | **One product's spend taxonomy as a component detail** | A fixed feature list with a fixed colour ramp, in a fixed order, in two places | `BreakdownSeries[]` — the host supplies members; the ramp assigns slots by index, and overflow folds to `Other` |
| 4 | **The ledger row as a fixed column set** | Columns *are* the schema, and domain columns (`model`, `tokens`) are meaningless to a storage product | `LedgerColumn[]` + `LedgerRow` with an opaque `attributes` bag — the product registers its columns |
| 5 | **Billing anchoring assumed** | A single-tenant product has no org above the scope; a reseller has two | `BillingAnchor` — the anchor is *described* to the component, or absent |

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

The panel's billing section is exactly one of: the host's `billing.planCard` (the host's own billing UI, drawn instead of anything below, and the catalogue is not asked for); a pointer, when the snapshot has an `anchor` (and the catalogue is not asked for); the catalogue with a portal link, when a `CheckoutSource` is passed; or nothing, when it is not — no plan affordance and no error. A catalogue that cannot be drawn is an alert in its own place and does not take the balance with it. A snapshot the model refuses is refused whole: an alert with the reason and a retry, never a half-drawn screen.

Four seams a host swaps independently: the **`CreditsSource`** (its transport), the **`LedgerColumnRegistry`** (its spend vocabulary), the **`BillingSlots`** + `BillingAnchor` (its commercial model), and the **theme contract** (its design tokens). The reference implementation uses our BFF and an LLM column set, but nothing in the port signatures requires either.

---

## Domain types

```typescript
// ui/credits-ui/src/ports.ts — ZERO imports from a host: no feature dir, no app store, no fetch layer.

/** Opaque to this package. The host's billing subject: workspace, org, user, account.
 *  `realm` mirrors the server's isolation axis so a shared deployment's snapshots can
 *  never be rendered under the wrong product's labels. */
export interface Scope { readonly realm?: string; readonly kind: string; readonly id: string; }

declare const integerBrand: unique symbol;
/** An integer, and only an integer. `number` cannot say so, and a float that reaches a balance, a delta or a total is how a
 *  reconciliation discrepancy gets rounded out of sight of the person best placed to notice it. A value becomes an `Int` where it
 *  enters the package (`quantity()` in model.ts), which REFUSES anything that is not a safe integer instead of rounding it. */
export type Int = number & { readonly [integerBrand]: true };

/** Integer minor units. Never a float — the money rule from metering-ports.md holds
 *  identically in the view layer, where a rounded display is how a reconciliation
 *  discrepancy gets hidden from the person best placed to notice it. */
export type Quantity = Int;

/** What the host meters. `credits` is OUR value, not the package's vocabulary. */
export interface UnitLabels {
  readonly unit: string;            // "credits" | "seats" | "GB-months"
  readonly one: string;             // "credit"
  readonly abbr?: string;           // "cr"
  /** Internal units per displayed unit. The books keep a FINE unit (the reference is
   *  1 credit = 1 µ$ of list price); a host shows dollars with scale 1_000_000 and 2 decimals,
   *  or "credits" of $0.001 with scale 1000. Integer division plus a remainder — never a float. */
  readonly scale?: Int;
  readonly decimals?: Int;
  readonly format?: (q: Quantity) => string;  // default: grouped integer ÷ scale, `decimals` places
}

/** One ceiling. Mirrors kernel/metering `Limit` — same shape, so the wire is a
 *  pass-through and neither side owns a translation table. */
export interface LimitView {
  readonly key: string;             // stable id; also the a11y label key
  readonly label: string;
  readonly used: Quantity;
  readonly cap: Quantity | null;    // null = uncapped (render as a figure, no meter)
  /** How a PERSON should read the ceiling — a view vocabulary, deliberately not the kernel's
   *  Window enum. The server maps one to the other in exactly one place (the snapshot builder):
   *
   *    kernel Balance → "none"   (no reset: a balance, rendered with an optional overdraft)
   *    kernel Daily   → "day"    · kernel Hourly → "hour" · kernel Rolling → "period"
   *    kernel Job     → "job"    (a draining budget for one unit of work)
   *    AdmitRequest.MaxCost → "call" (a per-invocation wall; never accumulates, never a Limit row)
   *
   *  "job" and "call" read very differently — one drains, the other is a wall — so a meter must
   *  not render them the same way. */
  readonly window: "period" | "day" | "hour" | "job" | "call" | "none";
  readonly resetsAt?: string;       // ISO; omitted when window === "call" | "none"
  readonly warnAtPct: Int;          // from Limit.WarnAt, a whole percent — NOT a hardcoded 80; 0 = no warning (as in the engine)
  readonly denyCode?: string;       // maps to the host's 402/429 ([metering-ports.md](./metering-ports.md))
  readonly scopeHint?: string;      // "all members" | "you" — set for a subject limit (Limit.Subject)
}

export interface BalanceView {
  readonly remaining: Quantity;
  readonly granted: Quantity;
  readonly primaryLimitKey: string; // which LimitView the hero mirrors; "" when the realm shows no limit
  readonly burnRatePerDay?: Quantity;
  /** Per credit pool, in draw order, when the realm defines pools beyond `general` — e.g.
   *  promotional credits that are spent first, or credits usable only on some resources. */
  readonly pools?: ReadonlyArray<{ readonly key: string; readonly label: string;
                                   readonly remaining: Quantity; readonly appliesTo?: string }>;
}

/** One member of a spend dimension. Colour is assigned by INDEX from the
 *  categorical ramp; the host never names a colour. */
export interface BreakdownSeries {
  readonly key: string;
  readonly label: string;
  readonly amount: Quantity;
}

/** A ledger entry. `delta` is SIGNED — consumption negative, grants positive — which
 *  is what lets a `subscription_grant`/`refund` land in the same column with
 *  no schema-shaped change (see [design-system/pages/credits.md](../../../design-system/pages/credits.md)). `attributes` is opaque: the
 *  package passes it to registered columns and never reads a key. */
export interface LedgerRow {
  readonly id: string;
  readonly operationType: string;   // opaque string, never an enum in this package
  readonly delta: Quantity;
  readonly at: string;              // ISO
  readonly actor?: { readonly label: string; readonly kind?: string };
  readonly idempotent?: boolean;    // renders the `deduped` tag
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
  /** An opaque cursor for the next, older page of the ledger; absent at the end. The panel hands it back and reads nothing in it. */
  readonly ledgerNext?: string;
  readonly anchor?: BillingAnchor;
  readonly notices?: ReadonlyArray<{ readonly kind: string; readonly title: string; readonly body?: string }>;
}

/** One older page of the ledger, newest first, and the cursor for the page after it (absent at the end). */
export interface LedgerPage {
  readonly rows: readonly LedgerRow[];
  readonly next?: string;
}

export interface CreditsSource {
  load(scope: Scope): Promise<CreditsSnapshot>;
  /** The page of the ledger after `cursor`, a `ledgerNext` this source issued. Absent: the panel draws no paging control. */
  moreLedger?(scope: Scope, cursor: string): Promise<LedgerPage>;
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
  readonly price: { readonly minor: Int; readonly currency: string } | null;
  readonly interval?: "month" | "year";
  readonly creditAllotment: Quantity | null;   // null = custom/negotiated
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
  /** Where an offer that is not self-serve (`price: null`) sends the viewer. Absent: the offer says so in words and exposes no control. */
  readonly contact?: { readonly label: string; readonly href: string };
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

1. **The meter fill encodes consumption, never remainder.** A hero reading "12,480 left" beside a bar filled to 82% is only coherent under one reading, and it must be the same reading on every screen. The component derives fill from `used/cap` and refuses to accept a pre-computed percentage: a `LimitView` has no percentage field, and one that arrives under any name (`pct`, `percent`, `fill`, …) is a `SnapshotError`, not trusted and not quietly dropped. The fill is a whole percent, floored, and is 100 exactly when the cap is reached. **Where the hero mirrors a balance** — whose `used` is the overdraft it is into, empty while the account is healthy — the bar is the share of what was granted that is gone, `(granted − remaining) / granted`: the same reading, consumed and never remainder.
2. **`warnAtPct` comes from the limit, never from a constant.** The threshold is admin-configurable; a hardcoded 80 in the view silently overrides an operator's setting. A limit warns when `used × 100 ≥ warnAtPct × cap`, in exact integer arithmetic (a double cannot tell the two sides apart near 2^53); `warnAtPct` 0 means no warning, as in the engine (it warns only when `WarnAt > 0` and `Max > 0`).
3. **Never a silent failure state.** A limit at or past `cap` renders a blocking notice carrying `denyCode` and a remedy affordance. "Renders nothing" is not a permitted state for an exhausted limit. *A remedy affordance* is the host's: the panel asks `remedyFor(limit)` (a `denyCode` → a link) and draws what comes back; a limit with no remedy still renders the alert, the code and a line saying to ask someone — never a notice with nothing to do. A limit that is merely near its threshold raises a `status` notice, not an alert. *At or past `cap`* is `used ≥ cap` for a counter. A **balance** (`window: "none"`) is blocked when what is left plus the permitted overdraft is nothing — `remaining + cap ≤ 0`, as the engine decides it — because its own `used` and `cap` are the overdraft: a healthy balance with no overdraft has `used = cap = 0` and is not blocked.
4. **Money is integer, end to end.** No float reaches a balance, a delta or a total. Display rounding never changes a rendered figure's value. `quantity()` makes a number a `Quantity` and refuses a float, `NaN`, an infinity, or an integer a double cannot hold exactly; a total, sum or residual the panel derives that would leave the safe-integer range is refused (`out_of_range`), never wrapped. Display divides by `scale` with integer arithmetic and rounds half away from zero to `decimals` places; the value it is drawn from is untouched.
5. **Totals shown together must reconcile.** If the panel shows both a total and its parts, the parts sum to the total, or the panel renders an explicit residual row. The balance's pools are parts of `balance.remaining`; the breakdown's members are parts of the spend series' total (of their own sum when there is no series). The ramp has six slots (`SERIES_SLOTS`, tokens `--cu-series-1…6`) assigned by the member's index in the list the host supplied — not by rank or size, so a scope switch repaints nothing — members past the ramp fold into one `Other` row, and any remaining gap is one `residual` row, last. A zero residual is never emitted. This is the view-layer half of SC-002 — a screen selling exact reconciliation must not itself present arithmetic that does not close.
6. **The package never interprets `unit`, `operationType`, or an `attributes` key.** Any behavior conditioned on the string `"credits"`, `"query"`, or `"model"` inside `credits-ui/**` is a defect. The model's only reading of a ledger row is the sign of its `delta`. The check is `scripts/check-credits-ui.sh`: no token containing `credit` (any case) in `ui/credits-ui/src/**` — a string, a comment, JSX text, a CSS property or an identifier — except the identifiers this contract itself defines (`CreditsPanel`, `CreditsPanelProps`, `CreditsSource`, `CreditsSnapshot`, `creditAllotment`) and the package's own name `credits-ui`. The contract's own names make a literal "no occurrence" unsatisfiable, so they are the exemption, matched as whole tokens: a longer identifier that merely contains one is refused.
7. **Colour never carries identity alone.** Every series renders its label and value as text; the categorical ramp is decoration on top of an already-readable row.
8. **Status hues and categorical hues are disjoint sets** (see the host's design system). A limit's tone comes from its threshold state; a series' colour comes from its index. Neither borrows from the other.
9. **Every meter is a `role="progressbar"`** with `aria-valuenow` and an `aria-valuetext` that states the ratio in the host's units.
10. **`granted` is never inferred from a checkout return.** The package renders `processing` on return and only shows `granted` when `CheckoutSource.fulfilment()` reports it. Any client-side optimism here produces the single worst failure this surface can have — a user who believes a payment failed and pays twice. The decision is one pure function, `deriveFulfilment(returned, reported)`: its result is `granted` exactly when the server reported `granted`, and nothing is claimed at all when the browser did not just return. The panel (`returnedFromCheckout`) renders `processing` at once on return and keeps asking `fulfilment()` — backing off to 30 s, for as long as it is mounted — and a server that cannot be reached, or says nothing, is not a failed payment. On `granted` it shows the amount in the host's unit and the receipt, stops asking and reloads the snapshot once; on `failed` it shows the reason and a retry and stops. `begin()`'s URL is followed only if it is `https:` or a path of this site.
11. **A blocked offer exposes no checkout action.** `PlanOffer.blocked` renders reason + remedy; there is no override affordance, because the block exists to prevent a change that would silently shrink someone else's budget.
12. **A non-purchasing viewer is told, not tested.** When `canPurchase` is false the package states the reason, offers the request path, and renders every purchase control **inert** — never live. The distinction from invariant 11 matters: a `blocked` offer exposes *no* action because the purchase itself is invalid, whereas here the purchase is valid and the *viewer* lacks authority, so the control is shown disabled (`disabled` and `aria-disabled`) to convey exactly that. Either way the 403 is a server guarantee, never a UI surprise.
13. **Provider identifiers and card data never enter the package.** `PlanOffer.code` is a host slug; a `provider_price_id` reaching this layer is a defect, and `deriveCatalog` refuses one (`provider_identifier`): a field named for a provider, a price id, a customer, a payment method or card data — on the offer or inside it — or a `code` that is itself a Stripe object id, a UUID or a PayPal plan id. An ordinary slug (`pro`, `plan_enterprise`) is not mistaken for one.
14. **Role checks live in one place.** A host surface that links to the catalogue must not re-implement the purchase-permission test — it navigates, and the catalogue degrades. Two copies of an authorization rule is one copy too many.
15. **An absent figure is not a zero.** A snapshot with no `series` knows nothing about spend, and the chart and the breakdown say so ("not available here"); a snapshot whose series adds up to nothing says that nothing was spent, and draws no line along the floor of the chart. `breakdown: []` alone cannot tell the two apart, so the model does (`breakdown.known`, true when a series came with it). A member's snapshot, and a server with no rollup, send no series, and a member who read "No spend in this window yet." would believe they had spent nothing. By the same rule the ledger's empty state says nothing has been **booked**, not that nothing happened: the books trail the balance by a moment, so a balance that has moved beside an empty ledger is not a contradiction the screen may assert. The breakdown's default title is neutral ("Spend breakdown"): a row is a member of a dimension the host names, which is not necessarily a person, and the host's copy says which.
16. **The ledger pages through the cursor the server issued, and a control the panel cannot back is not drawn.** `ledgerNext` is opaque: the panel hands it to `CreditsSource.moreLedger` and reads nothing in it. The control appears only when there is a cursor *and* a source that can fetch it (a button that does nothing is worse than none); it is disabled while a page is in flight and never for good, so a failed page — which is said, and leaves what is shown as it was — is asked for again with the same button. Older rows go below the newer, a row already on the screen is not put there twice, and a page is derived and refused exactly as the first would be (a float is not shown). A fresh snapshot, or another scope, starts the list over, and a page that arrives for a list that is gone is dropped. An empty cursor is the end. When the control goes, focus moves to the ledger instead of being lost, and the rows that arrived are announced.

---

## Reference wiring: LLM credits as ONE implementation

```typescript
// host-app/src/features/credits/  — the HOST side. All product specifics live here.
const labels: UnitLabels = { unit: "credits", one: "credit", abbr: "cr" };

const columns: LedgerColumn[] = [
  { key: "model",  header: "Model",  accepts: r => "model" in r.attributes,
    render: r => <Mono>{r.attributes.model as string}</Mono> },
  { key: "tokens", header: "Tokens", align: "end",
    accepts: r => "tokens" in r.attributes,
    render: r => <Mono>{fmt(r.attributes.tokens as number)}</Mono> },
];

<CreditsPanel
  source={new RestCreditsSource({ baseUrl: "/api/payments" })}   // the host's proxy: /v1/… one to one, behind its own session
  scope={{ kind: "workspace", id: workspaceId }}
  labels={labels}
  columns={columns}
  billing={{ planCard: <OrgPlanPointer/> }}   // BillingSlots — optional
/>
```

Swapping this for a storage product is: a different `UnitLabels`, an `EgressColumn` instead of `ModelColumn`/`TokensColumn`, no `BillingAnchor`, and a different `CreditsSource`. **No component edit.** That is the same swap [metering-ports.md](./metering-ports.md) makes on the server (`Pricer` + `Scope`), and the two halves match by design:

| Server seam | UI seam | Swapped together when a product changes |
|---|---|---|
| `Pricer` | `UnitLabels` + `BreakdownSeries` | what is metered, and what it is called |
| `Scope` | `Scope` + `BillingAnchor` | who is billed, and what sits above them |
| `Unit` / `Quantity` | `Quantity` + `LedgerColumn[]` | the dimensions and how a row displays them |
| `Limit[]` | `LimitView[]` | the ceilings — **count included** |
| `PaymentProvider` | `CheckoutSource` | how money is taken, and where the user is sent |

Nothing in the middle column names a product, and nothing in the left column knows there is a UI.

---

## The wire shape (one round trip, one snapshot)

The panel needs materially more than a balance figure, and it needs it in one shape — `GET /credits` returns the whole `CreditsSnapshot`. This was the gap the original host had (a balance-and-threshold endpoint that made the designed screen unbuildable), and closing it here is why `limits[]` is the load-bearing field: it is what makes the ceiling panel generic instead of N hardcoded cards, and the server already holds the data in exactly that shape.

| Snapshot field | Backing source | Note |
|---|---|---|
| `balance.remaining` / `granted` / `pools` | the hot account, per pool | a fast copy; the ledger is the record |
| `limits[]` | kernel `[]Limit` for the realm | pass-through, including `warnAtPct` and `denyCode` — **no translation table on either side** |
| `balance.burnRatePerDay` | `usage_daily` rollup | drives "runs out in ~N days", the figure people actually act on |
| `series[]` | `usage_daily` rollup | default 14 days, `?days=` configurable |
| `breakdown[]` | `usage_daily` grouped by dimension | everyone's, in the scope-wide snapshot; **none in a member's snapshot** — a member must not learn colleagues' usage from a credits screen, and the rollup cannot yet tell a member's own days from a colleague's (it is keyed by an actor an ordinary event does not carry), so a member's view is empty rather than risk it |
| `ledger[]` + `idempotent` | `credit_ledger` | the `deduped` tag is how "charged at most once" becomes *observable to the person it protects*, rather than a claim in a spec |
| `anchor` | resolved by the host | absent when this scope *is* the billing entity |

**What is built today.** `balance.remaining` / `granted` / `pools`, `limits[]` and `ledger[]` are served by `GET /v1/credits` ([rest-api.md](./rest-api.md)); `balance.burnRatePerDay`, `series[]` and `breakdown[]` are served too, from the `usage_daily` rollup ([data-model.md](../data-model.md#usage_daily)), **by the scope-wide snapshot only**: `series[]` is one `{at, amount}` per UTC day ending today (the default 14, `?days=` 1 to 366), a quiet day a `0`; `breakdown[]` is the same days' spend by resource, largest first, and `key` is `unspecified` for spend whose row names no resource; `burnRatePerDay` is what the scope spends in a day, counted from the first day with spend in the window and **rounded up** so the runway the panel states is never overstated, and `0` when the window holds no spend (the panel then offers no estimate). A member's snapshot carries none of the three (`breakdown` is `[]`), an engine with no rollup wired omits them the same way, and a rollup that cannot be read fails the snapshot instead of saying nothing was spent. The rollup is a few minutes behind the books, and `series` and `breakdown` are two reads of the same rows, so a rewrite between them can make them differ for one response — which is what the residual row (invariant 5) is for. A limit view is the kernel's `Limit` with `window` mapped to the view's vocabulary in exactly one place (`app.Credits`); `warnAtPct` is the limit's own, as a whole percent; `resetsAt` is the instant the counter starts over, in the limit's own zone, and only a calendar window has one; and a balance's `used` is the overdraft it is into. `scopeHint` is set only in a member's view. `ledger[].idempotent` is not served: a replay writes no ledger row (it answers `applied: false`), so the snapshot has nothing to mark; the `deduped` tag renders when a host's source sets it.

See [rest-api.md](./rest-api.md) for the full endpoint contract and [entitlement-ports.md](./entitlement-ports.md) for the admin gate on `breakdown[]`.

---

## Extraction-ready code organization

```text
ui/credits-ui/                    # THE PACKAGE — publishable, zero host imports
  package.json                    #   @intel-payment/credits-ui
  src/
    ports.ts                      #   the types above — framework-free
    model.ts                      #   pure snapshot → view derivation (no React, no fetch)
    CreditsPanel.tsx              #   composition root
    components/{BalanceHero,LimitMeter,SpendChart,Breakdown,LedgerTable,PlanCatalog}.tsx
    copy.ts                       #   every word the package says itself — none names a unit; a host replaces any
    index.ts                      #   the public surface (the plan-catalogue component is exported as `PlanCatalogView`, the port type keeps `PlanCatalog`)
    theme.css                     #   reads design tokens ONLY — no hex, no palette utility

ui/examples/llm-credits/          # THE REFERENCE BINDING — every product specific
  RestCreditsSource.ts  labels.ts  columns.tsx  OrgPlanPointer.tsx
  LlmCreditsScreen.tsx  tokens.css      # the product assembled; the token values the package's stylesheet reads
```

Enforced by `import-x/no-restricted-paths` (`credits-ui/**` may not import `examples/**`, and a binding uses the package only through its entry point), by ESLint rules that keep every `.ts` file in the package free of React and of any transport or DOM global (a `.ts` file is the framework-free half; a `.tsx` file is where React binds), and by a static scan (`scripts/check-credits-ui.sh`) for a unit or a colour the package must be handed. `scripts/verify-ui-boundary.sh` plants a violation per rule and requires each to be rejected for its own reason. The litmus: **`npm pack` the package, install it in an unrelated app, `npm run build` — compiles with zero edits.**

---

## Tokens and links

`theme.css` defines no value: it reads `--cu-surface`, `--cu-text`, `--cu-muted`, `--cu-border`, `--cu-radius`, `--cu-font-mono`; the status set `--cu-status-ok`, `--cu-status-warn`, `--cu-status-blocked` (a limit's tone, from its threshold state); and the series set `--cu-series-1` … `--cu-series-6` and `--cu-series-other` (a member's colour, from its index). The two sets are disjoint (invariant 8) and a test holds the stylesheet to it.

A destination the host supplies — a remedy, a request path, a contact link, a blocked offer's remedy — is drawn as a link only if it is `http(s):`, `mailto:`, a path or a fragment. `javascript:`, `data:` and a protocol-relative `//host` are drawn as text: these strings come from the host's data, and from there from whoever can write it.

---

## Adoption checklist (per host product)

- [ ] **Unit labelled** — `UnitLabels` supplied; no occurrence of the string `credit` inside `credits-ui/**`.
- [ ] **Limits sourced from the kernel** — `limits[]` comes from `[]Limit`, including `warnAtPct` and `denyCode`; the view hardcodes no threshold and no ceiling count.
- [ ] **Ledger columns registered** — product columns via `LedgerColumnRegistry`; an unknown key falls back to the generic column rather than erroring.
- [ ] **Signed deltas** — one signed column, not debit/credit pairs, so grants and refunds need no new column.
- [ ] **Billing anchor described or omitted** — no assumption that an org exists above the scope.
- [ ] **Catalogue supplied or omitted** — a product with no self-serve purchase passes no `CheckoutSource`; the panel renders without a single plan affordance and without error.
- [ ] **Fulfilment is server-reported** — `granted` comes from `fulfilment()`, never from a redirect.
- [ ] **Purchase permission is data** — `canPurchase` + reason supplied by the host; the package embeds no role model.
- [ ] **Blocked states wired** — every `denyCode` maps to a visible, actionable notice.
- [ ] **Theme via tokens only** — no hex, no palette utility, no literal radius under `credits-ui/**`; the host supplies the token values.
- [ ] **Reconciliation holds** — parts sum to totals, or a residual is shown.

---

## Non-goals (stays in the host, by design)

- **Pricing and rate cards** — a `Pricer` concern; this package renders what was charged, never what things cost.
- **Card data, payment methods, receipts** — the provider's hosted checkout and portal own these. The panel hands off via `CheckoutSource.begin()` / `portalUrl()` and renders no card data and no provider IDs, ever ([design-system/pages/credits.md](../../../design-system/pages/credits.md) Don'ts).
- **What produces spend** — call sites build the events; this package never instruments anything.
- **Tenancy semantics** — what a `Scope` *means* is the host's; the package treats it as an opaque identity.
