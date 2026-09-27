# Feature Specification: Post-paid Usage Invoicing

**Feature Branch**: `002-postpaid-invoicing` | **Status**: Designed, not started. **Depends on 001.**
**Phase**: 2 — see [../../ROADMAP.md](../../ROADMAP.md)

## Why this exists

[Feature 001](../001-metering-billing-core/spec.md) settles **prepaid**: credits are bought or
granted up front, spend draws them down, and an exhausted balance refuses work. That is the right
model for self-serve products and the wrong one for most B2B contracts, where the customer expects:

> *"Meter what we use. Invoice us at month end. We pay on net-30 terms."*

No amount of credit-engine work produces that, because post-paid inverts two assumptions 001 is
built on:

| 001 assumes | Post-paid requires |
|---|---|
| A balance must be positive before work runs | Work runs on **credit terms**; a spend *cap* replaces a balance |
| Each event is priced independently and purely | Price depends on **period-to-date volume** — tiered, graduated and volume pricing cannot be computed per event |
| Settlement is instant (debit at `Record`) | Settlement is at **period close**, into a document a human reads and disputes |

The third is the one that matters commercially: an invoice is a legal artifact, not a balance
readout. It must be immutable once finalized, correctable only by credit note, traceable line by
line to the usage that produced it, and reproducible months later.

**Nothing in 001 changes.** The ledger, the idempotency guard, the rate cards, the admission gate
and the payment adapters are reused as-is. Post-paid adds a *settlement model* over the same
recorded events, selected per scope.

## The pricing problem, stated precisely

001's `Pricer` is **pure over one event** — deliberately, because that is what makes reconciliation
and dispute replay possible. But real post-paid pricing is not a function of one event:

```
First 1,000,000 requests   free
Next  9,000,000 requests   $0.40 / 1k
Thereafter                 $0.25 / 1k
Minimum commitment         $2,000 / month
```

Pricing request number 4,312,905 requires knowing it is the 4,312,905th — which a pure per-event
function cannot know without reading state, and a `Pricer` that reads state is no longer replayable.

The resolution is a **second, distinct port**: `Rater` runs **once at period close**, over the
period's complete, immutable event set, and produces invoice lines. It is still pure — pure over
*the set* rather than over one event — so it stays replayable, which is what makes a disputed
invoice re-derivable. `Pricer` keeps its job (per-event credits, for enforcement and the ledger);
`Rater` does aggregate money.

Both can coexist for one scope: `hybrid` settlement meters against credits for real-time
enforcement **and** rates the period for an overage invoice.

## User Scenarios & Testing

### User Story 1 — A customer is invoiced for what they used (P1)

A scope on post-paid terms uses the product through a billing period. At period close the system
rates the period, produces a finalized invoice with lines traceable to usage, and hands it to a
provider for collection.

**Independent test**: drive usage across a period spanning two rate-card versions, close the
period, and confirm the invoice lines sum to the subtotal, that each line names the rate-card
version that produced it, and that re-rating the closed period yields a byte-identical result.

**Acceptance**
1. Given metered usage in an open period, when the period closes, one invoice is produced with lines whose amounts sum exactly to the subtotal — or an explicit residual line is present.
2. Given a closed period, when rating is re-run, the result is identical. Rating is replayable from the immutable ledger plus the recorded rate-card versions.
3. Given a finalized invoice, when any correction is needed, a **credit note** is issued. The invoice itself is never mutated.
4. Given a period containing a rate-card change, when rated, each event is priced under the card in force **when it occurred**, not the card current at close.
5. Given a usage event that arrives after its period closed, when ingested, the configured `LateEventPolicy` applies and the closed invoice is never silently altered.
6. Given a zero-usage period, when closed, the configured zero-amount policy decides whether an invoice is produced at all.

### User Story 2 — Tiered and committed pricing is honoured (P1)

**Acceptance**
1. Given a graduated tier schedule, when a period is rated, each tier is charged at its own rate for the volume falling within it.
2. Given a volume (retroactive) schedule, when a period is rated, the **entire** volume is charged at the rate its final total reaches.
3. Given a package schedule, when a period is rated, usage is charged in whole packages, rounded up.
4. Given a minimum commitment, when rated usage falls below it, the invoice shows the true usage lines **and** a separate shortfall line reaching the minimum — never a silently inflated usage figure.
5. Given a prepaid commitment being drawn down, when rated, the drawdown is shown as a credit line against the usage, and the remaining commitment is reported.

### User Story 3 — Work is capped without a balance (P1)

**Acceptance**
1. Given a post-paid scope with a spend cap, when period-to-date charges reach it, admission is refused with a code distinguishing "over your credit limit" from "out of credits".
2. Given a post-paid scope, when a balance would go negative, this is **normal** and not a policy violation — `NegativeBalancePolicy` does not apply to post-paid scopes.
3. Given an approaching cap, when the warn fraction is crossed, a warning is emitted with the projected period total.

### User Story 4 — Discounts and trials apply predictably (P2)

**Acceptance**
1. Given multiple applicable discounts, when an invoice is rated, they apply in one documented, deterministic order, and the invoice shows each as its own line.
2. Given a percentage discount and a fixed discount, when both apply, the result does not depend on evaluation order being guessed — the order is specified and tested.
3. Given a trial, when it is active, usage is metered and rated at zero, and the invoice shows what it *would* have cost.
4. Given an expired coupon or one past its redemption limit, when rating runs, it does not apply and the reason is recorded.

### User Story 5 — Finance can reconcile (P2)

**Acceptance**
1. Given any invoice line, when audited, it resolves to the ledger rows that produced it.
2. Given an invoice, when exported, it carries the period, the rate-card versions, the discounts applied and the collection status.
3. Given a disputed line, when re-rated from the ledger, the original amount is reproduced or the discrepancy is explicit.

### Edge cases

- **A rate card changes mid-period** — events price under the card in force when they occurred. The invoice names every version it used.
- **An event arrives after close** — `LateEventPolicy`: `next_period` (default; it lands in the open period with its original `occurred_at` preserved for audit), `reopen` (only while the invoice is still `draft`), or `reject`.
- **Period close runs twice** — idempotent under a per-(scope, period) claim. Two schedulers produce one invoice.
- **Close fails midway** — the period stays `closing` and is resumable. A partially-written invoice is never `finalized`.
- **The customer never pays** — collection is a provider concern; the invoice moves to `uncollectible` by policy, and the entitlement consequence is the host's decision, not this system's.
- **Currency has no price** — rating fails closed for that scope rather than converting at a rate nobody set.
- **A refund of an invoiced period** — a credit note, never an edit, and never a negative credit grant (that is the prepaid mechanism).
- **A scope switches prepaid → post-paid mid-period** — the switch takes effect at the next period boundary; the current period settles under the model it opened with.

## Requirements

### Functional — Settlement model
- **FR-101**: The system MUST support a per-scope settlement model of `prepaid_credits`, `postpaid_invoice`, or `hybrid`, and MUST apply the model that was in force when a period opened.
- **FR-102**: For post-paid scopes, admission MUST gate on a **spend cap** over period-to-date charges rather than on a positive balance, with a deny code distinguishable from balance exhaustion.
- **FR-103**: For post-paid scopes, a negative balance MUST be a normal state and MUST NOT trigger `NegativeBalancePolicy`.
- **FR-104**: The system MUST record every metered event identically regardless of settlement model, so the model can change without altering the audit trail.

### Functional — Periods and rating
- **FR-105**: The system MUST maintain billing periods per scope with an explicit lifecycle (`open` → `closing` → `closed` → `invoiced`), and MUST make close idempotent under a per-(scope, period) claim.
- **FR-106**: The system MUST rate a closed period from its **immutable** ledger events, pricing each under the rate card in force when it occurred.
- **FR-107**: Rating MUST be replayable: re-rating a closed period MUST produce an identical result.
- **FR-108**: The system MUST support graduated, volume, package and flat schedules, and MUST state which it applied on every line.
- **FR-109**: The system MUST support minimum commitments, showing true usage and any shortfall as **separate** lines.
- **FR-110**: The system MUST support prepaid commitments drawn down by usage, reporting the remaining balance.
- **FR-111**: The system MUST apply a configured `LateEventPolicy` to events arriving after their period closed, and MUST NOT silently alter a finalized invoice.

### Functional — Invoices
- **FR-112**: The system MUST produce invoices whose lines sum exactly to the subtotal, or MUST render an explicit residual line. No float may touch any amount.
- **FR-113**: A finalized invoice MUST be immutable. Corrections MUST be credit notes referencing it.
- **FR-114**: Every invoice line MUST be traceable to the ledger rows that produced it.
- **FR-115**: The system MUST record the rate-card versions and discounts used, so an invoice is reproducible without external state.
- **FR-116**: The system MUST apply a configured policy to zero-amount invoices (`skip` or `issue`).
- **FR-117**: The system MUST hand a finalized invoice to a `Collector` for payment, and MUST treat collection status as provider-reported, never inferred.
- **FR-118**: The system MUST NOT compute tax; it MUST carry provider-reported or hook-supplied tax as its own line.

### Functional — Discounts, trials, quantities
- **FR-119**: The system MUST apply discounts in one documented, deterministic order, rendering each as its own line, and MUST record any discount that did not apply together with the reason.
- **FR-120**: The system MUST support percentage and fixed-amount discounts, scoped to an invoice, a product, or a line, with validity windows and redemption limits.
- **FR-121**: The system MUST support trials during which usage is metered and rated at zero, with the would-be cost shown.
- **FR-122**: The system MUST support quantity-bearing subscriptions and MUST prorate a mid-period quantity change under the configured `PlanChangePolicy`.

### Key entities
- **BillingPeriod** — a scope's open or closed window of accrual.
- **RateSchedule** — how a metered dimension converts to money over a period: flat, graduated, volume or package.
- **Commitment** — a minimum spend, or a prepaid amount drawn down by usage.
- **Invoice / InvoiceLine** — the immutable document and its traceable components.
- **CreditNote** — the only sanctioned correction to a finalized invoice.
- **Discount** — a percentage or fixed reduction with a scope, a window and limits.

## Success Criteria

- **SC-101**: Re-rating any closed period reproduces its invoice exactly, including across rate-card changes.
- **SC-102**: Invoice lines sum to the subtotal in 100% of invoices, or a residual line is present. No invoice ever presents arithmetic that does not close.
- **SC-103**: Every line resolves to its ledger rows, and a disputed amount is re-derivable without external state.
- **SC-104**: A finalized invoice is never mutated; every correction is a credit note.
- **SC-105**: Running period close twice, or from two schedulers, produces exactly one invoice.
- **SC-106**: Graduated, volume, package and commitment schedules each produce the amount a hand calculation produces, on a fixture suite derived from published vendor pricing pages.
- **SC-107**: A post-paid scope is refused at its spend cap with a code distinguishable from balance exhaustion.
- **SC-108**: Late events never alter a finalized invoice.

## Out of scope (and where it goes)

- **Tax computation** — provider or external hook, as in 001 (FR-118).
- **Invoice PDF rendering and delivery** — the provider's hosted invoice, or the host's own templates. This system produces the data.
- **Revenue recognition, deferred revenue, double-entry GL export** — [Phase 3](../../ROADMAP.md). An invoice is not a journal entry, and pretending otherwise is how a billing system acquires an accounting system it cannot support.
- **Collections, dunning workflows, credit scoring** — the provider's tooling plus the host's policy.
- **Multi-currency FX** — prices are set per currency; no rate is applied here.
- **Purchase orders, quotes, contract lifecycle** — CPQ is a different product.
