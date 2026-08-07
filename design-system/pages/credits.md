# Page Override: Credits & Budgets

> Overrides `../MASTER.md` for the Credits screen. Covers User Story 4 (FR-016…FR-020).

## Purpose
Show the shared workspace credit balance in real time, warn near limits, and explain
the three independent ceilings (workspace balance, per-user daily, per-call cap).

## Layout
- App shell; active nav = **Credits**.
- Top: **balance hero** — large Fira Code remaining-credit figure + a usage meter
  (green → amber at ≥80% → red when exhausted). Admin-configurable warning threshold (default 80%).
- Grid of metric cards + a usage-over-time chart + a recent-activity ledger table.

## Key components
- **Credit meter**: horizontal bar, segment color by state. Amber **near-limit warning banner** with an **Upgrade** CTA when the threshold is crossed (FR-017). Red blocking banner when exhausted (FR-018). **The fill encodes credits *consumed*, never credits remaining** — the hero pairs it with a "12,480 left" headline, and the two only read coherently under one convention. Rendered as a real `role="progressbar"`; the painted width and the ARIA value both derive from one `--meter-pct`.
- **Three-ceiling panel**: three small gauges — Workspace balance, Per-user daily, Per-call output cap — each with current/limit in Fira Code, and each stating **its own window** (billing period / midnight UTC / none), because the three are independent and any one of them alone can block an operation. The per-call cap is a ceiling rather than a balance, so its bar is labelled as the last call against the cap instead of implying a draining budget. *Three is this product's count, not the component's* — the panel renders the kernel's `[]Limit`, so a deployment with one ceiling or five renders correctly ([credits-ui-ports.md](../../../specs/001-contextengine-mvp/contracts/credits-ui-ports.md)).
- **Usage chart**: streaming area / line chart of credits consumed over time (per the chart guidance: Streaming Area or Line). Toggle by feature (ingest / caption / query / rerank). Single series, so no legend — and drawn in the **categorical** ramp, not run-green: green means "healthy balance" in the meters above, and a green line climbing toward a ceiling would contradict the colour language on its own screen.
- **Cost-by-feature breakdown**: list with per-feature credit totals, coloured from `--color-chart-1…4` in fixed slot order. **Not** from the status palette: amber cannot mean "near limit" in the meter and "Captioning" in the legend below it. Colour follows the feature, never its rank, so switching scope repaints nothing.
- **Ledger table**: durable record of credit changes (operation, feature, model, tokens, credits, timestamp, idempotency status). Reinforces "charged at most once" (FR-019) — show a `deduped` tag on retried ops. Credit values render as **signed deltas** (`−42` consumption in red, `+50,000` grant in green); Phase 2 adds `purchase` / `subscription_grant` / `refund` / `chargeback` rows in the same column with no schema-shaped change to the table.
- **New/trial account notice**: stricter limits indicator (FR-020).

## Plan & billing pointer *(Phase 2)*
Sits **below the ledger**, above the exhausted-state preview. **Billing anchors to the
organization, not the workspace** (see [organization.md](./organization.md)), so this
screen deliberately does *not* carry a plan catalogue, a subscription card, or receipts —
duplicating them across two screens would give two places to change one thing.
- **Pointer card**: states that the workspace holds no plan of its own, that the
  organization buys once and allocates, and links to **Manage in organization**.
- **Four read-only figures**: org plan, this workspace's allocation, org pool remaining,
  renewal date — enough for a workspace admin to understand their budget without leaving.
- **The allocation rule, stated**: exhausting this allocation pauses AI operations *in this
  workspace only* and never silently draws down another workspace's budget. An org admin
  can raise the allocation while the pool has credits.
- The **balance hero** reads as an *allocation from the organization*, not a purchase.

## States to show
- Healthy balance.
- Near-limit (amber banner visible, meter amber).
- Exhausted (red blocking banner, AI actions disabled elsewhere).

## Reusability
This screen is built from the **`credits-ui` package**, not as a bespoke page: see
[credits-ui-ports.md](../../../specs/001-contextengine-mvp/contracts/credits-ui-ports.md).
The layout, copy and vocabulary here are *one binding* of it — the unit ("credits"), the
feature names, the LLM ledger columns (`model`, `tokens`) and the org billing anchor are
all injected by `frontend/src/features/credits/`. This mirrors what
[metering-ports.md](../../../specs/001-contextengine-mvp/contracts/metering-ports.md)
already does on the server, where a different product swaps one `Pricer` and one `Scope`
binding; without the UI half, that generality would be discarded at the last step.

## Don'ts
- Don't fail silently — always show warning/block messaging with an upgrade path.
- Don't display credit figures in the body font; use Fira Code for all numerics.
- Don't hardcode the 80% threshold in the view — it is admin-configurable, and a constant in the component silently overrides an operator's setting. Read it from the limit.
- Don't let a meter's fill mean "remaining" anywhere. One convention, every screen.
- Don't use a status colour as a chart series, or a series colour as a state.
- Don't show a total beside its parts unless they reconcile — a screen whose whole claim is exact accounting cannot present arithmetic that doesn't close. Show a residual row instead.
- Don't put plan controls or receipts on this screen. One billing entity, one place to manage it.
- Don't merge the receipts table into the credit ledger. Money and credits are different units with different lifecycles (a refund reverses fiat *and* appends a negative ledger row — two records, one reconciliation key).
- Don't imply credits are granted at checkout return. Fulfilment happens on the verified provider webhook, so the UI's honest state after redirect is "payment processing", not "credits added".
- Don't render provider price/customer IDs or any card data in the UI.

---

## Phase 2+ affordances on this screen

| Affordance | Phase | Backing contract |
|---|---|---|
| Org billing pointer + allocation figures | 2 | `organization_credits`; `workspace_credits` as allocation |
| Plan catalog, subscription, portal, receipts | 2 | Moved to [organization.md](./organization.md) — billing anchors to `organization_id` |
| `subscription_grant` / `refund` ledger rows | 2 | `credit_ledger.operation_type` extension |

The mocked ledger uses the **signed-delta** convention (draft-plan open decision #1) — the Phase 1 mockup already rendered `+50,000` alongside `−42`, so signed is the convention the design has effectively assumed all along. Worth confirming as the implementation decision rather than adopting separate debit/credit columns. See [specs/draft-plan.md](../../../specs/draft-plan.md).
