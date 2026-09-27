# Contracts

The normative surface of `intel-payment`. Read them in this order.

| Contract | What it owns |
|---|---|
| [metering-ports.md](./metering-ports.md) | **The engine.** `Realm`/`Scope`/`Pricer`/`Ledger`/`LedgerWriter`/`Meter`, 14 invariants, the settlement-durability and money policies, the hexagonal layout, the machine-enforced boundary, the observability contract |
| [payment-provider-ports.md](./payment-provider-ports.md) | **The fiat boundary.** `PaymentProvider`/`Catalog`/`TaxStrategy`, the webhook flow, 14 invariants, Stripe/Polar/PayPal notes |
| [entitlement-ports.md](./entitlement-ports.md) | **What a plan unlocks besides credits.** `Entitler`, precedence, 10 invariants |
| [credits-ui-ports.md](./credits-ui-ports.md) | **The browser half.** `CreditsSource`/`LimitView`/`LedgerColumn`/`PlanCatalog`/`CheckoutSource`, 14 invariants |
| [grpc-surface.md](./grpc-surface.md) | The service-mode wire contract for the hot path |
| [rest-api.md](./rest-api.md) | The HTTP surface: host API, webhook ingress, operations, error codes |
| [bus-subjects.md](./bus-subjects.md) | The async seam, the scheduled ticks, and the two `Bus` adapters (Redis Streams by default, NATS JetStream optional) |

Post-paid settlement — rating, tiered schedules, commitments, immutable invoices, credit notes,
discounts, trials — is [feature 002](../../002-postpaid-invoicing/), additive to everything here.

## How to read an invariant

Every invariant is a property an implementation MUST uphold, paired with the failure it
prevents. They are not style rules: each one exists because its absence produces a specific
money bug — a double charge, a silent under-bill, a forgotten refund, a capability someone
did not buy. Where an invariant is testable, the contract's conformance suite tests it, and
CI runs that suite against every implementation rather than trusting a reading.

## Diagrams

[../diagrams/credit-metering-swimlane.excalidraw](../diagrams/credit-metering-swimlane.excalidraw) — the consumption hot path, outbox drain and reconcile.
[../diagrams/billing-payment-flow.excalidraw](../diagrams/billing-payment-flow.excalidraw) — checkout, webhook verification and the grant path.
