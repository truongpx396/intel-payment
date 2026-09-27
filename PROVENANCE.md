# Provenance

`intel-payment` was extracted from
[**truongpx396/aisat-intel**](https://github.com/truongpx396/aisat-intel), where this engine was
designed as that product's credit-metering backbone and deliberately factored for reuse. This file
records exactly what moved, what was generalized, and what was fixed — so any claim here can be
checked against the upstream repository.

Extracted at upstream `main` = `5e8b963632e8710fc430cefdfdb6c6b7e59a2b9f` on 2026-09-27.

---

## Files carried across **with their git history**

`git filter-repo` preserved the real commit history for the five artifacts that were already
dedicated to metering and billing. `git log --follow <path>` in this repository shows their
evolution in the originating project.

| This repository | Upstream path | Commits carried |
|---|---|---|
| `specs/001-metering-billing-core/contracts/metering-ports.md` | `specs/001-contextengine-mvp/contracts/metering-ports.md` | 5 |
| `specs/001-metering-billing-core/contracts/credits-ui-ports.md` | `specs/001-contextengine-mvp/contracts/credits-ui-ports.md` | 1 |
| `specs/001-metering-billing-core/diagrams/billing-payment-flow.excalidraw` | `specs/001-contextengine-mvp/diagrams/addition/billing-payment-flow.excalidraw` | 3 |
| `specs/001-metering-billing-core/diagrams/credit-metering-swimlane.excalidraw` | `specs/001-contextengine-mvp/diagrams/credit-metering-swimlane.excalidraw` | — |
| `design-system/pages/credits.md` | `design-system/aisat-intel/pages/credits.md` | 2 |

15 commits reach this repository's root, spanning the design's full evolution.

### Upstream commits, oldest first

| Upstream SHA | Subject |
|---|---|
| `e6b689d` | add payment billing design |
| `9c3b099` | update billing payment design, add notes for scalability, add readme |
| `829fd19` | re-organize future phases |
| `870914a` | docs(spec): add Phase 3 trust & knowledge-health notes; rename to AISAT-INTEL |
| `33142c5` | docs(spec): factor credit metering into reusable Meter/Pricer/Ledger ports |
| `6585966` | docs(spec): add reference LLMTokenPricer, contract tests, service topology |
| `3ec4ca1` | docs(spec): add MeteringService gRPC surface + extraction-ready code layout |
| `a1dabca` | docs(spec): machine-enforce the metering boundary (lint + config) |
| `efe6876` | docs(specs): add audit-ports contract, close hash-chain + coverage gaps |
| `1aad41f` | docs(specs): reconcile credit/billing UI, generate shared chrome, add credits-ui + plan-catalogue ports |

SHAs are rewritten by the filter; `git log` here shows the new ones with the same messages.

---

## Content derived from shared upstream files (history **not** carried)

Three upstream files contain the billing design interleaved with a great deal of unrelated
material. Carrying their history would have dragged the whole originating roadmap into this
repository's history, which works against the point of the extraction — so the billing content was
lifted and the provenance recorded here instead.

| This repository | Upstream source | What was taken |
|---|---|---|
| `specs/001-metering-billing-core/contracts/payment-provider-ports.md` | `specs/draft-plan.md` § Phase 2 Billing and Payments (~195 lines) | The `PaymentProvider` port, design principles, entity definitions, the webhook flow, per-provider notes, the security checklist |
| `specs/001-metering-billing-core/contracts/rest-api.md` | `specs/001-contextengine-mvp/contracts/bff-rest.md` § Billing & payments | The endpoint set and error semantics |
| `specs/001-metering-billing-core/contracts/bus-subjects.md` | `specs/001-contextengine-mvp/contracts/nats-subjects.md` | `billing.deduct`, `billing.grant`, `billing.reconcile.tick` and their rules |
| `README.md` (architecture prose) | upstream `README.md` § Credit Metering & Billing | The Redis-outbox explanation and the reconciliation rationale |

Relevant upstream commits for the derived material: `e6b689d`, `9c3b099`, `829fd19`,
`cc02bc4` (*stage Phase 2 surfaces in mockups; resolve all open design decisions*),
`2c63287` (README billing section).

---

## Generalization applied to the carried files

The carried contracts were written as *a seam inside one product*. They now describe *the product*,
so the framing changed while the substance did not:

- **Host-specific vocabulary removed** — `workspace_credits` became `account_credits`; `workspace_id` became the opaque `(realm, scope_kind, scope_id)` triple; "ContextEngine" became "one implementation"; LLM-token metering was demoted from the subject to the reference example, alongside seat and storage pricers.
- **Cross-references repointed** — links into the originating spec (its research sections, its `FR-0NN`/`SC-0NN` ids, sibling contracts that did not come across) were replaced with this repository's own ids, or with the reasoning they stood in for. No dangling reference remains; CI checks this.
- **Import paths rewritten** — the upstream kernel path became `github.com/truongpx396/intel-payment/metering`, and the in-repo tree became this repository's root layout.
- **"Extraction-ready" became "extracted"** — the *"move it and it still compiles"* litmus test now runs in the other direction, as a CI gate asserting the engine stands alone.
- **Phase language removed** — "Phase 1 ships metering, Phase 2 adds payments" was the originating product's roadmap. Here both are in scope.

## Design work added during the lift

21 decisions are recorded in
[design-decisions.md](specs/001-metering-billing-core/design-decisions.md): three questions the
originating design left open and resolved here, and eighteen refinements. The ones that change
behaviour rather than presentation:

| # | Change | Why it mattered |
|---|---|---|
| **D4** | `Realm` as the outermost isolation axis | Without it, two products sharing a deployment share an **idempotency key space**, so one's `invoice-1` is silently swallowed as a replay of the other's |
| **D5** | `AdmitRequest.MaxCost` + `Admission.Headroom` | The original gate could only ask "already at a ceiling?", so a scope with 1 credit admitted an arbitrarily expensive call — while the design asserted an overshoot bound it never received the input for |
| **D6** | `rate_card_version` on every ledger row | Pricing was required to be replayable, but nothing recorded *which* card priced a row, so a replay would use today's rates and disagree with the charge it was verifying |
| **D7** | `NegativeBalancePolicy` + a mandatory compensating row | A refund past consumed credits has no arithmetic that avoids going negative, and the original had no answer. Flooring silently also breaks the invariant that the balance equals the ledger sum |
| **D8** | The `Entitler` port | A plan's value is rarely only credits. With no port, every host re-derives capabilities from plan codes at call sites |
| **D16** | Subscription drift sweep | Webhooks are the source of truth and webhooks get missed; a dropped cancellation event otherwise leaves a cancelled customer entitled forever |
| **D21** | Idempotency guard moved to its own non-partitioned table | **The inherited schema was not constructible.** See below |

### D21 — a constraint that could not exist

The inherited design specified `credit_ledger` as both **partitioned by `created_at`** and carrying
a global **unique index on `idem_key`**. PostgreSQL forbids that combination, and that one index is
the backstop for every exactly-once guarantee in the system:

```
ERROR:  unique constraint on partitioned table must include all partitioning columns
DETAIL:  UNIQUE constraint on table "credit_ledger" lacks column "created_at"
         which is part of the partition key.
```

Found by applying the migrations rather than by reading them. Resolved with a dedicated
non-partitioned `credit_idem (realm, idem_key)` guard written in the same transaction as the ledger
row — which keeps both properties and is better than either compromise, because the guard is narrow
and hot, and it **outlives ledger partitions**, so dropping an aged partition cannot resurrect the
ability to double-charge an old key.

---

## What this repository does **not** take

Deliberately left in the originating project, because they are that product's concerns:

- LLM gateway, retrieval, ingestion, agent runtime, sandboxing.
- Authorization, audit, notification and approval ports — sibling reuse seams for other subsystems.
- The design system beyond the credits screen.
- The originating spec's own requirements, success criteria and roadmap.

## Relationship going forward

The two repositories are independent. `aisat-intel` now points here as the canonical home for the
payment/billing design and keeps a short binding note describing how it adopts these ports; this
repository does not depend on it.

Where the two disagree, **this repository is authoritative** for the metering, payments,
entitlement and credits-UI contracts.
