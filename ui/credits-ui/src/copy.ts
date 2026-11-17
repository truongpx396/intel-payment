// Every word the package says itself. A host replaces any of them (`copy` on the panel); none names a unit, because the unit is the
// host's (UnitLabels). Functions take already-formatted text, so a translation reorders words without touching a number.
import type { LimitView } from "./ports.js";

export interface Copy {
  readonly balance: string;
  readonly left: string;
  readonly grantedOf: (granted: string) => string;
  readonly daysLeft: (days: number) => string;
  readonly nearLimit: (label: string, pct: number) => string;
  readonly poolsHeading: string;
  readonly residualLabel: string;
  readonly otherLabel: string;
  readonly totalLabel: string;

  readonly limitsHeading: string;
  readonly noLimits: string;
  readonly meterText: (kind: "window" | "job" | "call" | "balance", used: string, cap: string, pct: number) => string;
  readonly balanceMeterLabel: string;
  readonly windowText: Readonly<Record<LimitView["window"], string>>;
  readonly resets: (at: string) => string;
  readonly uncapped: string;
  readonly noOverdraft: string;
  readonly overdraft: string;
  readonly lastCall: string;
  readonly blockedTitle: (label: string) => string;
  readonly blockedBody: string;
  readonly denyCode: string;
  readonly noRemedy: string;

  readonly spendHeading: string;
  readonly spendSummary: (total: string, peak: string, days: number) => string;
  readonly noSpend: string;
  readonly breakdownHeading: string;
  readonly noBreakdown: string;

  readonly ledgerHeading: string;
  readonly noLedger: string;
  readonly colOperation: string;
  readonly colActor: string;
  readonly colChange: string;
  readonly colWhen: string;
  readonly colDetails: string;
  readonly deduped: string;

  readonly plansHeading: string;
  readonly subscriptions: string;
  readonly oneTime: string;
  readonly noPlans: string;
  readonly currentPlan: string;
  readonly buy: (name: string) => string;
  readonly notSelfServe: string;
  readonly cannotPurchaseDefault: string;
  readonly redirecting: string;
  readonly interval: Readonly<Record<"month" | "year", string>>;
  readonly allotment: (amount: string) => string;
  readonly allotmentCustom: string;

  readonly loading: string;
  readonly loadFailed: string;
  readonly cannotShow: string;
  readonly retry: string;
  readonly processing: string;
  readonly granted: (amount: string) => string;
  readonly viewReceipt: string;
  readonly failed: (reason: string) => string;
  readonly tryAgain: string;
  readonly checkoutFailed: string;
  readonly plansCannotShow: string;
  readonly billingHeading: string;
  readonly manageBilling: string;
  readonly anchorHolds: (label: string) => string;
  readonly anchorOpen: (label: string) => string;
}

export const defaultCopy: Copy = {
  balance: "Balance",
  left: "left",
  grantedOf: (granted) => `of ${granted} granted`,
  daysLeft: (days) => (days === 1 ? "Runs out in about 1 day" : `Runs out in about ${days} days`),
  nearLimit: (label, pct) => `${label}: ${pct}% used — you are close to the limit`,
  poolsHeading: "Balance by pool",
  residualLabel: "Unallocated",
  otherLabel: "Other",
  totalLabel: "Total",

  limitsHeading: "Limits",
  noLimits: "No limits apply.",
  meterText: (kind, used, cap, pct) => (kind === "call" ? `${used} of a ${cap} per-call cap (${pct}%)` : `${used} of ${cap} used (${pct}%)`),
  balanceMeterLabel: "Balance used",
  windowText: { period: "Per billing period", day: "Daily limit", hour: "Hourly limit", job: "For this job", call: "Per call", none: "Balance" },
  resets: (at) => `Resets ${at}`,
  uncapped: "No cap",
  noOverdraft: "No overdraft allowed",
  overdraft: "Overdraft",
  lastCall: "Last call against the cap",
  blockedTitle: (label) => `${label} reached`,
  blockedBody: "This limit has been reached, so work that needs it is refused.",
  denyCode: "Code",
  noRemedy: "Contact your administrator to continue.",

  spendHeading: "Spend over time",
  spendSummary: (total, peak, days) => `${total} over ${days} ${days === 1 ? "day" : "days"}, at most ${peak} in one day`,
  noSpend: "No spend in this window yet.",
  breakdownHeading: "Spend by member",
  noBreakdown: "No spend to break down yet.",

  ledgerHeading: "Activity",
  noLedger: "No activity yet.",
  colOperation: "Operation",
  colActor: "By",
  colChange: "Change",
  colWhen: "When",
  colDetails: "Details",
  deduped: "deduped",

  plansHeading: "Plans",
  subscriptions: "Subscriptions",
  oneTime: "One-time purchases",
  noPlans: "No plans are available.",
  currentPlan: "Current plan",
  buy: (name) => `Choose ${name}`,
  notSelfServe: "Not available to buy online.",
  cannotPurchaseDefault: "You can browse plans, but you do not have permission to buy.",
  redirecting: "Taking you to checkout…",
  interval: { month: "month", year: "year" },
  allotment: (amount) => `${amount} included`,
  allotmentCustom: "Custom allotment",

  loading: "Loading…",
  loadFailed: "Could not load this balance.",
  cannotShow: "This balance cannot be shown.",
  retry: "Try again",
  processing: "Payment processing — your balance updates once the provider confirms it. Please do not pay again.",
  granted: (amount) => `Payment confirmed: ${amount} added.`,
  viewReceipt: "View receipt",
  failed: (reason) => `Payment failed: ${reason}`,
  tryAgain: "Try again",
  checkoutFailed: "We could not start checkout. Please try again.",
  plansCannotShow: "Plans cannot be shown.",
  billingHeading: "Billing",
  manageBilling: "Manage billing",
  anchorHolds: (label) => `${label} manages billing for this scope.`,
  anchorOpen: (label) => `Open ${label}`,
};

export function resolveCopy(partial?: Partial<Copy>): Copy {
  return partial === undefined ? defaultCopy : { ...defaultCopy, ...partial };
}
