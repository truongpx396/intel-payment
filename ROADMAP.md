# Roadmap

Written to be honest about what this is and is not, because a billing system that overstates its
scope gets adopted for jobs it cannot do.

## What this is

**A metering, credits and payment-collection engine**, tuned for one shape: an **internal credit
unit** with **sub-millisecond admission enforcement** on a request hot path. That is the shape of AI
products, API products, and anything where you must refuse work *before* performing it because
performing it costs real money.

## What this is not

It is not a general-purpose billing platform today. If you need **post-paid usage invoicing**
("meter us, invoice us monthly, we pay net-30"), that is Phase 2 below — designed, not built. If you
need **revenue recognition or a general ledger**, that is Phase 3 and is not designed.

**If you do not need credits-as-internal-unit plus hot-path enforcement, you probably want something
else**, and saying so is more useful than pretending otherwise:

| Instead | When |
|---|---|
| **Stripe Billing** | Subscriptions and simple metered usage, no infrastructure, no hot-path enforcement needed. The right default for most teams |
| **Lago** (open source) | Post-paid usage billing with invoices, today, with real code |
| **Orb / Metronome** | Usage billing at scale, managed |
| **OpenMeter** | Usage metering only, no billing |
| **Kill Bill** | Mature, complex subscription billing; JVM |
| **this** | You need an internal credit unit, sub-ms refuse-before-spend on every request, and you want to own the engine |

---

## Phase 1 — Metering, credits & payment collection

**Status: designed and normative. Implementation not started.** → [specs/001-metering-billing-core](specs/001-metering-billing-core/)

Prepaid credits in pools, exact rational pricing, the admission gate with subject- and plan-sized
ceilings, the single durable writer with a per-scope sequence watermark, cluster-safe hot-path
functions, reconcile and recovery, idempotency with fingerprints, payment-provider adapters behind a
webhook inbox (refunds, disputes, auto top-up), entitlements, outbound events, an admin API, and the
credits UI package.

**Executable today:** the schema (applied and behaviourally asserted on PostgreSQL 16) and the
reference hot-path functions (run on Redis 7 in cluster mode) — both in CI.

**Required infrastructure: Redis + Postgres.** The bus defaults to Redis Streams, so there is no
broker to stand up; NATS JetStream is an option for downstream replication.

## Phase 2 — Post-paid usage invoicing

**Status: designed, not started. Depends on Phase 1.** → [specs/002-postpaid-invoicing](specs/002-postpaid-invoicing/)

The settlement model most B2B contracts actually use. Adds a `Rater` (pure over the period's event
*set* rather than over one event — which is precisely why it cannot be a smarter `Pricer`),
graduated/volume/package schedules, minimum and prepaid commitments, immutable invoices with
traceable lines, credit notes, discounts with a specified application order, trials, and
quantity-bearing subscriptions.

This is the phase that turns "credit engine" into "billing system", and it is the largest gap between
what a B2B buyer expects and what Phase 1 delivers.

## Phase 3 — Finance-grade

**Status: not designed.** Listed so its absence is a decision rather than an oversight.

- **Double-entry GL export** — an invoice is not a journal entry, and a credit ledger is not a general ledger.
- **Revenue recognition / deferred revenue** — an annual subscription paid up front is recognized monthly. Any company past seed stage needs this, and it is not a small addition.
- **Multi-currency FX** — today prices are set per currency and no rate is applied. Correct, but it limits reach.
- **Tax computation** — still deliberately out of scope; the `TaxStrategy` seam names who does it.

**Recommendation: do not build Phase 3 here.** Export to an accounting system that already does it.
The temptation to grow a billing engine into an ERP is how billing engines become unmaintainable.

---

## Known gaps

Tracked openly rather than discovered later.

| Gap | Phase | Note |
|---|---|---|
| **No implementation — 0 lines of Go** | 1 | The single largest gap. `make up` cannot work until `cmd/` exists. The schema and the reference hot path *are* executable and verified; the Go Redis adapter must load `hot_path.lua` unchanged so that verification stays meaningful |
| Post-paid invoicing | 2 | Designed, not built |
| Coupons, discounts, trials, quantity subscriptions | 2 | Designed in 002; absent from 001 |
| Invoice generation | 2 | Phase 1 collects payments; it issues no invoices |
| Revenue recognition, GL export | 3 | Not designed. Export instead |
| Client libraries | 1 | **Designed, not built**: a wrapped Go client, and TypeScript and Python clients generated from `api/openapi/v1.yaml` and the protos (T063b) |
| Active-active multi-region | — | **Out of scope by decision** ([D45](specs/001-metering-billing-core/design-decisions.md)): one home region, Postgres replicated to a DR region, Redis rebuilt from the books |
| GDPR erasure story | 1 | Holds `billing_email` and `actor_id`. Ledger rows must survive an erasure request for audit, so the answer is pseudonymization rather than deletion — designed nowhere yet |
| Load tested | 1 | The [scale envelope](specs/001-metering-billing-core/plan.md#scale-envelope) states targets; none is a measurement until T130 |
| Security reviewed | 1 | Checklists exist; none has been executed |
| Runbook exercised | 1 | [docs/operations.md](docs/operations.md) is written from reasoning, not from an incident |

Migrations are **forward-only by policy**, not by omission — see [migrations/README.md](migrations/README.md).

## Sequencing advice

Build **Phase 1, phases 0–3** before writing another line of specification. The spec-to-code ratio
here is inverted, and designs elaborated this far ahead of implementation get invalidated on contact
with code. The evidence is in this repository twice over:
[D21](specs/001-metering-billing-core/design-decisions.md) — a constraint that turned out not to be
constructible in PostgreSQL — and the second design review ([D29–D46](specs/001-metering-billing-core/design-decisions.md#decisions-from-the-second-design-review)),
whose most serious finding — a hot-path script Redis Cluster refuses outright — was confirmed in
seconds by *running* it. Both were found by executing the design, not by reading it; the remaining
mechanism can only be validated the same way, in code, under load and chaos (T130–T131).
