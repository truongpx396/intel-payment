# Reference Screen: Credits & Budgets

> The reference *binding* of the [`credits-ui`](../../specs/001-metering-billing-core/contracts/credits-ui-ports.md)
> package — one product's vocabulary over generic components. A host copies this page,
> swaps the unit and the columns, and keeps the behaviour. The **Don'ts** at the bottom are
> not stylistic: each one is a way a money screen misleads the person reading it, and they
> are normative for any binding.

## Purpose
Show the scope's spendable balance in real time, warn near limits, and explain **each
independent ceiling** — the reference binding has three (scope balance, per-user daily,
per-call cap), but the count is the product's, not the component's.

## Layout
- App shell; active nav = **Credits**.
- Top: **balance hero** — large monospace remaining-balance figure + a usage meter
  (healthy → warn → blocked). The warn threshold comes from `LimitView.warnAtPct`
  (operator-configurable, default 80%), never from a constant in the view.
  Beside it, **"runs out in ~N days"** from `burnRatePerDay` — the figure people act on;
  a raw balance tells nobody whether to top up.
- Grid of metric cards + a usage-over-time chart + a recent-activity ledger table.

## Key components
- **Credit meter**: horizontal bar, segment color by state. A **near-limit warning banner** with a top-up CTA when `warnAtPct` is crossed; a **blocking banner** carrying the `denyCode` and a remedy when the limit is reached. "Renders nothing" is not a permitted state for an exhausted limit. **The fill encodes the amount *consumed*, never the amount remaining** — the hero pairs it with a "12,480 left" headline, and the two only read coherently under one convention. Rendered as a real `role="progressbar"`; the painted width and the ARIA value both derive from one `--meter-pct`.
- **Ceiling panel**: one gauge per `LimitView` — in this binding: scope balance, per-user daily, per-call output cap — each with current/limit in monospace, and each stating **its own window** (billing period / midnight in the limit's time zone / none), because the ceilings are independent and any one of them alone can block an operation. The per-call cap is a ceiling rather than a balance, so its bar is labelled as the last call against the cap instead of implying a draining budget. **The count is data.** The panel renders `LimitView[]`, mapped one-to-one from the kernel's `[]Limit`, so one ceiling or five renders correctly with no component edit ([credits-ui-ports.md](../../specs/001-metering-billing-core/contracts/credits-ui-ports.md)).
- **Usage chart**: streaming area / line chart of credits consumed over time (per the chart guidance: Streaming Area or Line). Toggle by feature (the host's dimension members). Single series, so no legend — and drawn in the **categorical** ramp, not the status-healthy hue: green means "healthy balance" in the meters above, and a green line climbing toward a ceiling would contradict the colour language on its own screen.
- **Cost-by-feature breakdown**: list with per-feature credit totals, coloured from the categorical ramp slots in fixed slot order. **Not** from the status palette: amber cannot mean "near limit" in the meter and a series label in the legend below it. Colour follows the feature, never its rank, so switching scope repaints nothing.
- **Ledger table**: durable record of balance changes (operation, actor, registered product columns, delta, timestamp, idempotency status). Show a `deduped` tag on a replayed op — this is what makes "charged at most once" **observable to the person it protects** instead of a claim in a spec. Deltas render **signed** (`−42` consumption, `+50,000` grant), so `purchase` / `subscription_grant` / `refund` / `chargeback` / `writeoff` rows all land in the same column with no schema-shaped change. The domain columns (`model`, `tokens` here) come from the registry, not the table.
- **Trial / new-account notice**: stricter-limits indicator, stating which ceiling differs and when it relaxes.

## Plan & billing pointer
Sits **below the ledger**. Which of the two shapes renders is decided by `BillingAnchor`,
not by the page:

**`anchor` absent** — this scope *is* the billing entity. The panel renders the
`PlanCatalog` inline: offers, the current plan rendered inert (never a no-op button), and a
portal link. A `blocked` offer shows its reason and remedy and exposes **no** checkout
action; a viewer without purchase authority sees the reason and every control **inert**.

**`anchor` present** — something above this scope buys, and this scope receives an
allocation. The panel then renders a *pointer*, never a second catalogue, because
duplicating plan controls across two screens gives two places to change one thing.
- **Pointer card**: states that this scope holds no plan of its own, that the anchor buys
  once and allocates, and links to it.
- **Four read-only figures**: anchor plan, this scope's allocation, anchor pool remaining,
  renewal date — enough to understand the budget without leaving.
- **The allocation rule, stated**: exhausting this allocation pauses work *in this scope
  only* and never silently draws down another scope's budget.
- The **balance hero** reads as an *allocation*, not a purchase.

## States to show
- Healthy balance.
- Near-limit (amber banner visible, meter amber).
- Exhausted (red blocking banner, metered actions disabled elsewhere).

## Reusability
This screen is built from the **`credits-ui` package**, not as a bespoke page:
[credits-ui-ports.md](../../specs/001-metering-billing-core/contracts/credits-ui-ports.md).
The layout, copy and vocabulary here are *one binding* — the unit ("credits"), the feature
names, the domain ledger columns (`model`, `tokens`) and the billing anchor are all
injected by the host. This mirrors what
[metering-ports.md](../../specs/001-metering-billing-core/contracts/metering-ports.md)
does on the server, where a different product swaps one `Pricer` and one `Scope`. Without
the UI half, that generality would be discarded at the last step.

## Don'ts
- Don't fail silently — always show warning/block messaging with an upgrade path.
- Don't display balance figures in the body font; use a monospace face for all numerics, so digits align down a column and a changed figure does not reflow its neighbours.
- Don't hardcode the warn threshold in the view — it is operator-configurable, and a constant in the component silently overrides an operator's setting. Read it from the limit.
- Don't let a meter's fill mean "remaining" anywhere. One convention, every screen.
- Don't use a status colour as a chart series, or a series colour as a state.
- Don't show a total beside its parts unless they reconcile — a screen whose whole claim is exact accounting cannot present arithmetic that doesn't close. Show a residual row instead.
- Don't put plan controls or receipts on this screen. One billing entity, one place to manage it.
- Don't merge the receipts table into the credit ledger. Money and credits are different units with different lifecycles (a refund reverses fiat *and* appends a negative ledger row — two records, one reconciliation key).
- Don't imply credits are granted at checkout return. Fulfilment happens on the verified provider webhook, so the UI's honest state after redirect is "payment processing", not "credits added".
- Don't render provider price/customer IDs or any card data in the UI.
- Don't show another member's spend in a member-scoped breakdown. A credits screen is not an admin report, and usage is personal data.
- Don't let a zero-cost operation render as "free" without saying why. A cached or refused op that spent nothing should say *cached* or *refused*, not show a blank.

---

## Affordances and their backing contracts

| Affordance | Backing contract |
|---|---|
| Ceiling panel, hero, chart, breakdown, ledger | [credits-ui-ports.md](../../specs/001-metering-billing-core/contracts/credits-ui-ports.md) |
| Billing pointer + allocation figures | `BillingAnchor` — described by the host, never assumed |
| Plan catalogue, checkout, portal, receipts | `PlanCatalog` / `CheckoutSource` → [payment-provider-ports.md](../../specs/001-metering-billing-core/contracts/payment-provider-ports.md) |
| `subscription_grant` / `refund` / `writeoff` ledger rows | `credit_ledger.operation_type` — one signed column, no new columns |
| Feature gates shown as plan benefits | [entitlement-ports.md](../../specs/001-metering-billing-core/contracts/entitlement-ports.md) |

The ledger uses the **signed-delta** convention — resolved, not open; see
[design-decisions.md D1](../../specs/001-metering-billing-core/design-decisions.md).
