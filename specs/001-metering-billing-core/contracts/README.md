# Contracts

The normative surface of `intel-payment`. Read them in this order.

| Contract | What it owns |
|---|---|
| [metering-ports.md](./metering-ports.md) | **The engine.** `Realm`/`Scope`/`Subjects`/`Pool`, the `Pricer`/`Ledger`/`Meter`/`LedgerWriter` ports, 18 invariants, the settlement and money policies, the hexagonal layout, the machine-enforced boundary, the observability contract |
| [hot-path-consistency.md](./hot-path-consistency.md) | **The mechanism.** How the Redis hot tier and the Postgres books stay consistent: the cluster-safe key layout, the intent, the per-scope sequence watermark, suspense, two-phase transfers, recovery, idempotency lifetimes, reconcile |
| [reference/hot_path.lua](./reference/hot_path.lua) | The reference Redis Functions — executable, verified on Redis in cluster mode by [`scripts/verify-hot-path.sh`](../../../scripts/verify-hot-path.sh) |
| [payment-provider-ports.md](./payment-provider-ports.md) | **The fiat boundary.** `PaymentProvider`, provider accounts, the webhook inbox, refunds, disputes, auto top-up, 19 invariants |
| [entitlement-ports.md](./entitlement-ports.md) | **What a plan unlocks besides credits** — and the `QuotaSource` that sizes plan-dependent ceilings. 10 invariants |
| [outbound-events.md](./outbound-events.md) | **How hosts hear what happened.** Signed webhooks and a cursor feed, never the internal bus |
| [credits-ui-ports.md](./credits-ui-ports.md) | **The browser half.** `CreditsSource`/`LimitView`/`LedgerColumn`/`PlanCatalog`/`CheckoutSource`, 14 invariants |
| [grpc-surface.md](./grpc-surface.md) | The service-mode wire contract and its status mapping |
| [rest-api.md](./rest-api.md) | The HTTP surface: host API, admin API, webhook ingress, errors, versioning and deprecation |
| [bus-subjects.md](./bus-subjects.md) | The internal async seam: intents, events, scheduled ticks, and the two `Bus` adapters |

Post-paid settlement — rating, tiered schedules, commitments, immutable invoices, credit notes,
discounts, trials — is [feature 002](../../002-postpaid-invoicing/), additive to everything here.

## How to read an invariant

Every invariant is a property an implementation MUST uphold, paired with the failure it
prevents. They are not style rules: each one exists because its absence produces a specific
money bug — a double charge, a silent under-bill, a lost grant, a capability someone did not buy.
Where an invariant is testable, the contract's conformance suite tests it, and CI runs that suite
against every implementation rather than trusting a reading. Where the contract is executable
already — the schema, the hot-path functions — CI runs *it*.

## Diagrams

[../diagrams/credit-metering-swimlane.excalidraw](../diagrams/credit-metering-swimlane.excalidraw) — the consumption hot path, outbox drain and reconcile.
[../diagrams/billing-payment-flow.excalidraw](../diagrams/billing-payment-flow.excalidraw) — checkout, webhook verification and the grant path.

The diagrams are **illustrative and predate the second design review** ([D29–D46](../design-decisions.md#decisions-from-the-second-design-review)):
they show the shape of the flow, not the shard-tagged keys, the sequence watermark or the webhook
inbox. Where a diagram and a contract disagree, the contract is right.
