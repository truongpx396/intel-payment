# @intel-payment/credits-ui

Balances, ceilings, spend, a ledger and a plan catalogue, drawn over an **opaque unit** and an **opaque scope**. Only the labels, the registered
ledger columns, the breakdown members and the billing anchor are the host's; the package never names a unit.

- `ports.ts` and `model.ts` are **framework-free** (no React, no fetch, no store, no clock): `derivePanel(snapshot)` is a pure function, and a
  Vue or Svelte port reuses it unchanged. Every integer-meant field is a branded `Int`; `quantity()` refuses a float rather than rounding it.
- The **components** are React 18 or 19 (`CreditsPanel`, `BalanceHero`, `LimitMeters`, `SpendChart`, `Breakdown`, `LedgerTable`, `PlanCatalogView`).
- `theme.css` **reads design tokens and defines none**: the host supplies `--cu-surface`, `--cu-status-ok` … `--cu-series-1` … (the full list is in the file).

```tsx
import "@intel-payment/credits-ui/theme.css";
import { CreditsPanel } from "@intel-payment/credits-ui";

<CreditsPanel
  source={source}                      // load(scope) → CreditsSnapshot, and optionally subscribe()
  scope={{ kind: "team", id: "t1" }}
  labels={{ unit: "seats", one: "seat" }}
  checkout={checkout}                  // optional: omit it and the panel sells nothing, with no error
  returnedFromCheckout={returned}      // the panel then says "processing" until the server reports otherwise
/>
```

What it will not do, on purpose: show a payment as granted because the browser came back from checkout; draw a provider's price id or card data; treat a
blocked offer as buyable; draw a fraction as money; or follow a `javascript:` URL. The contract and the reasons are in
[`credits-ui-ports.md`](../../specs/001-metering-billing-core/contracts/credits-ui-ports.md); a complete binding is in
[`../examples/llm-credits`](../examples/llm-credits/src/RestCreditsSource.ts).

**Status.** Built and tested in jsdom, shown to pack, install into an unrelated app and build, and looked at once in Chromium by hand. No automated browser test
yet; not published to a registry; not yet used against a live server.
