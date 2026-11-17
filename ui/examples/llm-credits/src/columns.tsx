// The product's ledger columns: what makes a row an LLM call. The package reads no attribute key; these are the only code that does.
import type { LedgerColumn } from "@intel-payment/credits-ui";

const grouped = new Intl.NumberFormat("en-US");

export const modelColumn: LedgerColumn = {
  key: "model",
  header: "Model",
  accepts: (r) => "model" in r.attributes,
  render: (r) => <code>{String(r.attributes.model)}</code>,
};

export const tokensColumn: LedgerColumn = {
  key: "tokens",
  header: "Tokens",
  align: "end",
  accepts: (r) => "tokens" in r.attributes,
  // the server's attribute bag holds strings; a count that is not one is shown as it came, not as NaN
  render: (r) => {
    const raw = String(r.attributes.tokens);
    const n = Number(raw);
    return <code>{Number.isSafeInteger(n) ? grouped.format(n) : raw}</code>;
  },
};
