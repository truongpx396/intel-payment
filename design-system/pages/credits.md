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
- **Credit meter**: horizontal bar, segment color by state. Amber **near-limit warning banner** with an **Upgrade** CTA when the threshold is crossed (FR-017). Red blocking banner when exhausted (FR-018).
- **Three-ceiling panel**: three small gauges — Workspace balance, Per-user daily, Per-call output cap — each with current/limit in Fira Code.
- **Usage chart**: streaming area / line chart of credits consumed over time (per the chart guidance: Streaming Area or Line). Toggle by feature (ingest / caption / query / rerank).
- **Cost-by-feature breakdown**: stacked bar or list with per-feature credit totals.
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

## Don'ts
- Don't fail silently — always show warning/block messaging with an upgrade path.
- Don't display credit figures in the body font; use Fira Code for all numerics.
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
