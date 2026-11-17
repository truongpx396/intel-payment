// The product's vocabulary. The server labels a pool and a limit with its own key ("promo", "org_daily"); what a person calls them is the
// host's to say, and this is where it is said.
import type { UnitLabels } from "@intel-payment/credits-ui";

export const llmLabels: UnitLabels = { unit: "credits", one: "credit", abbr: "cr" };

export const poolLabels: Readonly<Record<string, string>> = { general: "General", promo: "Promotional" };

export const limitLabels: Readonly<Record<string, string>> = {
  scope_balance: "Workspace balance",
  user_daily: "Per-user daily",
  org_daily: "Organisation daily",
  run_cap: "Per-call output cap",
};

/** A key this product has no name for is still shown, as words: "burst_per_minute" is "Burst per minute", never a blank or a raw slug. */
export function humanize(key: string): string {
  const words = key.replace(/[_-]+/g, " ").trim();
  return words === "" ? key : words.charAt(0).toUpperCase() + words.slice(1);
}
