// What a workspace under an organisation shows instead of a plan catalogue: the organisation buys once and allocates, so this scope holds
// no plan of its own and the commercial relationship lives one step up. The host's own card, drawn as the panel's billing section.
export interface OrganisationPlan {
  readonly name: string;
  readonly href: string;
  readonly plan: string;
  readonly allocation: string;
  readonly renews: string;
}

export function OrgPlanPointer({ name, href, plan, allocation, renews }: OrganisationPlan) {
  return (
    <div data-example="org-plan">
      <p>
        This workspace has no plan of its own. <strong>{name}</strong> buys once and allocates.
      </p>
      <dl>
        <dt>Plan</dt> <dd>{plan}</dd>
        <dt>This workspace's allocation</dt> <dd>{allocation}</dd>
        <dt>Renews</dt> <dd>{renews}</dd>
      </dl>
      <p>Running out here pauses work in this workspace only; it never draws on another workspace's budget.</p>
      <a href={href}>Open {name}</a>
    </div>
  );
}
