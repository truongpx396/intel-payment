import { CreditsPanel, type Scope } from "@intel-payment/credits-ui";
import "@intel-payment/credits-ui/theme.css";
import { useMemo } from "react";
import "./tokens.css";
import { modelColumn, tokensColumn } from "./columns.js";
import { llmLabels } from "./labels.js";
import { OrgPlanPointer, type OrganisationPlan } from "./OrgPlanPointer.js";
import { RestCheckoutSource, RestCreditsSource } from "./RestCreditsSource.js";

// A breakdown row of this product's spend is a feature (the engine's dimension is the resource that was metered): the package's own title is
// neutral, and this is the product saying what its rows are. Module-level, so the panel is handed the same object on every render.
const copy = { breakdownHeading: "Spend by feature" } as const;

export interface LlmCreditsScreenProps {
  readonly baseUrl: string;
  readonly scope: Scope;
  readonly currency: string;
  readonly fetch?: typeof fetch;
  readonly returnedFromCheckout?: boolean;
  readonly topUpHref?: string;
  /** Set when an organisation above this workspace holds the plan: the screen then points at it instead of selling one. */
  readonly organisation?: OrganisationPlan;
  readonly canPurchase?: boolean;
  readonly navigate?: (url: string) => void;
  /** A member's view of this scope: the member's own ledger and no spend section (the server cannot tell a member's days from a colleague's). */
  readonly member?: { readonly kind: string; readonly id: string };
  /** The length of the spend series, in days; the server's default when unset. */
  readonly days?: number;
  /** How many ledger rows a page holds, on the snapshot and on every older page; the server's default when unset. */
  readonly pageSize?: number;
}

const here = (query: string) => `${window.location.origin}${window.location.pathname}${query}`;

/** The LLM-credits product, assembled: its unit, its columns, its names, its remedy, its way to the server. No component is edited. */
export function LlmCreditsScreen({ baseUrl, scope, currency, fetch: f, returnedFromCheckout, topUpHref = "/billing", organisation, canPurchase = true, navigate, member, days, pageSize }: LlmCreditsScreenProps) {
  const memberKind = member?.kind;
  const memberId = member?.id;
  const source = useMemo(
    () => new RestCreditsSource({ baseUrl, ...(f === undefined ? {} : { fetch: f }), ...(memberKind === undefined || memberId === undefined ? {} : { member: { kind: memberKind, id: memberId } }), ...(days === undefined ? {} : { days }), ...(pageSize === undefined ? {} : { pageSize }) }),
    [baseUrl, f, memberKind, memberId, days, pageSize],
  );
  const checkout = useMemo(
    () => (organisation === undefined ? new RestCheckoutSource({ baseUrl, ...(f === undefined ? {} : { fetch: f }), currency, successUrl: here("?checkout=done"), cancelUrl: here(""), returnUrl: here(""), canPurchase, ...(canPurchase ? {} : { cannotPurchaseReason: "Only an owner can change the plan.", requestPath: { label: "Ask an owner", href: "/members" } }) }) : undefined),
    [baseUrl, f, currency, organisation, canPurchase],
  );
  return (
    <CreditsPanel
      source={source}
      scope={scope}
      labels={llmLabels}
      copy={copy}
      columns={[modelColumn, tokensColumn]}
      checkout={checkout}
      billing={organisation === undefined ? undefined : { planCard: <OrgPlanPointer {...organisation} /> }}
      returnedFromCheckout={returnedFromCheckout}
      navigate={navigate}
      // a spent balance has one way out; a daily cap resets by itself, so the panel says to wait rather than selling anything
      remedyFor={(limit) => (limit.denyCode === "payment_required" ? { label: "Top up", href: topUpHref } : undefined)}
    />
  );
}
