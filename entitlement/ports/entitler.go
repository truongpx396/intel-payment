package ports

import (
	"context"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Entitler answers "is this scope allowed to do this at all?" — what a plan permits besides credits.
//
// Every method fails closed. A key the realm never declared is false / !ok with no error (invariant 1); a store that cannot
// answer is an error wrapping domain.ErrUnavailable, never a permit (invariant 2). There is no FailOpen knob: unmetered
// usage is a bounded, recoverable cost, while an un-gated capability can expose another tenant's data.
type Entitler interface {
	// Allowed answers a flag check. It returns FALSE for an unknown key.
	Allowed(ctx context.Context, s md.Scope, k domain.Key) (bool, error)

	// Quota returns the ceiling for a quota key. ok is false when no grant applies. A -1 limit means unlimited; callers MUST
	// handle it explicitly rather than comparing against it numerically.
	Quota(ctx context.Context, s md.Scope, k domain.Key) (limit int64, ok bool, err error)

	// Consume checks a quota against CURRENT usage the host supplies, and is the method a host actually calls before creating
	// the 26th seat. It does NOT store usage counts (invariant 4): a second count is a second truth.
	Consume(ctx context.Context, s md.Scope, k domain.Key, current, delta int64) (domain.Decision, error)

	// Choice returns an enum entitlement.
	Choice(ctx context.Context, s md.Scope, k domain.Key) (string, bool, error)

	// Resolve returns EVERY effective grant with provenance, ordered by key. It backs the plan card in the UI and the "why is
	// this allowed?" support answer, and it is one call so the UI cannot assemble a different picture than the server enforces.
	Resolve(ctx context.Context, s md.Scope) ([]domain.Grant, error)
}

// EntitlementStore is the one seam a host swaps. Reads are by realm or by scope and never cross a realm (invariant 10).
type EntitlementStore interface {
	// Keys are the realm's declared capabilities, with each enum's ranking exactly as declared (lowest to highest).
	Keys(ctx context.Context, realm md.Realm) ([]domain.KeyDecl, error)
	// Defaults are the realm's free tier: what a scope with no subscription gets.
	Defaults(ctx context.Context, realm md.Realm) ([]domain.Entitlement, error)
	// PlanEntitlements are the benefits attached to a plan — data, versioned with the plan. A plan the realm does not have,
	// or has taken off sale, still answers for its subscribers: taking a plan off sale is not revoking it. An unknown code has
	// none, and is not an error.
	PlanEntitlements(ctx context.Context, realm md.Realm, planCode string) ([]domain.Entitlement, error)
	// Overrides are per-scope exceptions (a sales deal, a trial extension, a comp account), expired ones included: whether
	// an override still applies is decided at the instant of the question, from the clock the Entitler was given.
	Overrides(ctx context.Context, s md.Scope) ([]domain.Grant, error)
	// Subscription is the scope's live subscription (trialing, active, past_due or paused), if it has one. A canceled or
	// incomplete one is not live. Whether a live subscription entitles anything is Subscription.Holds, decided at the instant
	// of the question.
	Subscription(ctx context.Context, s md.Scope) (domain.Subscription, bool, error)
	// Plans are the subscription plans the realm sells, in catalogue order, with what each unlocks: where a refusal looks for
	// the plan that would permit what was asked.
	Plans(ctx context.Context, realm md.Realm) ([]domain.Offer, error)
	// PutOverride writes an exception. The database's audit trigger records who, what, why and when in the same transaction
	// (invariant 5), so an unexplained override is as unwritable as it is unexplainable.
	PutOverride(ctx context.Context, g domain.Grant, actor string) error
}
