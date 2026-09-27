# Contract: Entitlements (what a plan unlocks besides credits)

**Spec**: [../spec.md](../spec.md) | **Plan**: [../plan.md](../plan.md) | **Status**: Normative — **new in `intel-payment`**, no counterpart in the originating host.

[metering-ports.md](./metering-ports.md) answers *how much has been consumed*. [payment-provider-ports.md](./payment-provider-ports.md) answers *what was bought*. Neither answers the question a host asks on nearly every request:

> **Is this scope allowed to do this at all?**

A credit balance cannot answer it. "Pro includes SSO", "Team allows 25 members", "Free cannot export" are not quantities drawn down per call — they are **capabilities attached to a plan**, and they are the reason a plan is worth more than a credit pack.

> **Why this contract exists.** Without it, every host re-derives entitlements from plan codes at call sites: `if plan == "pro" || plan == "team"`. That pattern fails the same way three times — the checks scatter across the codebase, launching a plan means editing code in every one of them, and the UI's idea of what a plan includes drifts from the server's. The fix is to make entitlements **data that travels with the plan**, checked through one port. Nothing here is novel; it is simply the piece a credit engine is missing when it tries to be a billing system.

---

## Ports at a glance

```text
PLAN (data)                               HOST
  entitlements: [                            │  Entitler.Allowed(scope, "sso")        → bool
    {sso,        bool,  true},                │  Entitler.Quota(scope, "seats")        → 25, ok
    {seats,      int,   25},                  │  Entitler.Consume(scope, "seats", 1)   → ok | over
    {export,     bool,  false},               ▼
    {region,     enum,  "eu"},        ┌── Entitler (app) ──────────────────────────────────┐
  ]                                   │  resolve the scope's ACTIVE grants (sub + overrides)│
                                      │  merge by precedence · evaluate · cache with a TTL  │
GRANT SOURCES                         └──────────────────┬─────────────────────────────────┘
  subscription (from a paid plan)                        │
  override    (sales deal, trial, comp)   ← audited      ▼
  default     (the free tier)              EntitlementStore · Clock · Metrics
```

One seam a host swaps: the **`EntitlementStore`**. Everything else is evaluation, and evaluation is deliberately dull — a capability check that is clever is a capability check nobody can predict.

---

## Domain types

```go
package entitlement // entitlement/domain

import "github.com/truongpx396/intel-payment/metering/domain"

// Key is a host-defined capability name. The engine ships NONE as special — it has no
// opinion about what "sso" means, only about how a value attached to it is resolved.
type Key string

type Kind string

const (
	Flag  Kind = "flag"  // on/off:        "sso", "audit_export", "custom_domain"
	Quota Kind = "quota" // a ceiling:     "seats", "projects", "webhooks"
	Enum  Kind = "enum"  // one of a set:  "support_tier" = bronze|gold, "region" = eu|us
)

// Value is a closed union. Deliberately not `any`: an untyped value means every call site
// asserts a type, and the one that asserts wrong fails at runtime in a billing check.
type Value struct {
	Kind Kind
	Bool bool
	Int  int64  // -1 means UNLIMITED. Chosen over a nil pointer so "unlimited" cannot be
	           // confused with "unset" — the two must behave differently and often do not.
	Text string
}

type Entitlement struct {
	Key   Key
	Value Value
}

// Source records WHERE a grant came from, because the answer to "why can this customer
// use SSO?" is an operational question asked under time pressure.
type Source string

const (
	FromDefault      Source = "default"      // the realm's free tier
	FromSubscription Source = "subscription" // the active paid plan
	FromOverride     Source = "override"     // a deliberate exception — always audited
)

// Grant is one resolved entitlement with its provenance and lifetime.
type Grant struct {
	Scope     domain.Scope
	Key       Key
	Value     Value
	Source    Source
	ExpiresAt *time.Time // nil = as long as its source lives
	Reason    string     // required for FromOverride: who approved it and why
}
```

---

## Port: `Entitler`

```go
package ports // entitlement/ports

type Entitler interface {
	// Allowed answers a flag check. It returns FALSE for an unknown key — fail closed.
	// A capability the host never declared must not be silently granted because a typo
	// made it unrecognizable.
	Allowed(ctx context.Context, s domain.Scope, k domain.Key) (bool, error)

	// Quota returns the ceiling for a quota key. `ok` is false when no grant applies.
	// A -1 limit means unlimited; callers MUST handle it explicitly rather than comparing
	// against it numerically.
	Quota(ctx context.Context, s domain.Scope, k domain.Key) (limit int64, ok bool, err error)

	// Consume checks a quota against CURRENT usage the host supplies, and is the method
	// a host actually calls before creating the 26th seat.
	//
	// It does NOT store usage counts. Seat and project counts live in the host's own tables,
	// and duplicating them here would create a second source of truth that drifts — the
	// exact failure this engine refuses to accept for balances.
	Consume(ctx context.Context, s domain.Scope, k domain.Key, current, delta int64) (Decision, error)

	// Choice returns an enum entitlement.
	Choice(ctx context.Context, s domain.Scope, k domain.Key) (string, bool, error)

	// Resolve returns EVERY effective grant with provenance. This backs the plan card in the
	// UI and the "why is this allowed?" support answer, and it is one call so the UI cannot
	// assemble a different picture than the server enforces.
	Resolve(ctx context.Context, s domain.Scope) ([]domain.Grant, error)
}

type Decision struct {
	Allowed   bool
	Limit     int64  // -1 = unlimited
	Current   int64
	Remaining int64  // -1 when unlimited
	// Reason is set when refused, and is written for a HUMAN: "Team plan includes 25 seats;
	// you have 25." An entitlement refusal the user cannot act on is an outage to them.
	Reason string
	// UpgradePath is the plan code that would permit it, when one exists. This is what makes
	// a refusal a sales moment rather than a dead end.
	UpgradePath string
}
```

## Port: `EntitlementStore`

```go
type EntitlementStore interface {
	// PlanEntitlements are the benefits attached to a plan — data, versioned with the plan.
	PlanEntitlements(ctx context.Context, realm domain.Realm, planCode string) ([]domain.Entitlement, error)
	// Overrides are per-scope exceptions (a sales deal, a trial extension, a comp account).
	Overrides(ctx context.Context, s domain.Scope) ([]domain.Grant, error)
	// Defaults are the realm's free tier: what a scope with no subscription gets.
	Defaults(ctx context.Context, realm domain.Realm) ([]domain.Entitlement, error)
	// PutOverride writes an exception. It REQUIRES a reason and an actor, and writes an audit
	// row in the same transaction — an unexplained override is indistinguishable from a bug,
	// and it is usually discovered during a revenue investigation.
	PutOverride(ctx context.Context, g domain.Grant, actor string) error
}
```

---

## Precedence (exactly one order, stated once)

```text
override  >  subscription  >  default
```

1. **Override wins**, always. It exists to say "yes despite the plan" or "no despite the plan", and a precedence that lets a plan change silently erase a negotiated deal is a broken precedence.
2. **Subscription** next. `past_due` within `GraceUntil` still counts (payments invariant 13); past the grace window it does not.
3. **Default** last — the free tier.

Within one level, for the same key: a **flag** ORs (any grant enabling it wins), a **quota** takes the **maximum**, and an **enum** takes the highest-ranked member of the host-declared order. Stated explicitly because "two grants for one key" is not hypothetical — it is what a migration between plans looks like for as long as it runs.

---

## Invariants

1. **Unknown key ⇒ denied.** A key the realm never declared is `false` / `!ok`, never a permissive default. Typos must fail closed, and they will happen.
2. **Fail closed on an error.** If the store is unreachable, `Allowed` returns an error and the host denies. There is no `FailOpen` knob here, unlike `Admit`: unmetered *usage* is a bounded, recoverable cost, while an un-gated *capability* can expose another tenant's data. The two failures are not comparable, so they do not share a policy.
3. **Entitlements are data, never plan-code branches.** No `if plan == "pro"` anywhere — in this repo or in a host. A new plan is rows, not a release.
4. **`Consume` never stores usage.** The host owns its own counts. A second count is a second truth.
5. **Overrides are audited.** Every `PutOverride` writes who, what, why and when, in the same transaction as the override itself.
6. **`-1` means unlimited, and is handled explicitly.** Never compared numerically, never coerced to a large number, never confused with "unset".
7. **A refusal is actionable.** Every denied `Decision` carries a human-readable `Reason` and, when one exists, an `UpgradePath`.
8. **The UI reads the same resolution the server enforces.** `Resolve` is the single source for both. A plan card assembled from a separate source will eventually promise something the server refuses.
9. **Cache is bounded and invalidated on change.** Entitlements are read on nearly every request, so they are cached with a short TTL — and a subscription change, an override write or a grace expiry invalidates the scope's entry immediately. A customer who just upgraded must not wait out a TTL to use what they paid for.
10. **Realm isolation.** Plans, defaults and overrides never cross a realm (metering invariant 11).

---

## Contract-test skeleton

```go
func EntitlerContract(t *testing.T, e ports.Entitler, fx Fixtures) {
	ctx := context.Background()

	t.Run("unknown key is denied", func(t *testing.T) {
		ok, err := e.Allowed(ctx, fx.Scope, "no_such_capability")
		mustNoErr(t, err)
		if ok { t.Fatal("an undeclared key must fail closed") }
	})
	t.Run("store outage denies, never permits", func(t *testing.T) {
		fx.Store.BreakWith(errors.New("connection refused"))
		if _, err := e.Allowed(ctx, fx.Scope, "sso"); err == nil {
			t.Fatal("must surface the error so the host denies (invariant 2)")
		}
	})
	t.Run("override beats subscription", func(t *testing.T) {
		fx.GivenPlan("free", flag("sso", false))
		fx.GivenOverride("sso", true, "2026-Q3 enterprise pilot, approved by sales")
		ok, _ := e.Allowed(ctx, fx.Scope, "sso")
		if !ok { t.Fatal("a negotiated override must survive the plan") }
	})
	t.Run("past_due inside grace keeps entitlements", func(t *testing.T) {
		fx.GivenSubscription("pro", domain.PastDue, graceUntil(fx.Clock.Now().Add(72*time.Hour)))
		ok, _ := e.Allowed(ctx, fx.Scope, "sso")
		if !ok { t.Fatal("a transient decline must not revoke inside the grace window") }
	})
	t.Run("past grace falls back to the default tier", func(t *testing.T) {
		fx.GivenSubscription("pro", domain.PastDue, graceUntil(fx.Clock.Now().Add(-1*time.Hour)))
		ok, _ := e.Allowed(ctx, fx.Scope, "sso")
		if ok { t.Fatal("entitlements must lapse once the grace window closes") }
	})
	t.Run("quota refusal is actionable", func(t *testing.T) {
		fx.GivenPlan("team", quota("seats", 25))
		d, err := e.Consume(ctx, fx.Scope, "seats", 25, 1)
		mustNoErr(t, err)
		if d.Allowed { t.Fatal("26th seat on a 25-seat plan must be refused") }
		if d.Reason == "" { t.Fatal("a refusal with no reason is an unexplained failure (invariant 7)") }
		if d.UpgradePath == "" { t.Fatal("a refusal with an available upgrade must name it") }
	})
	t.Run("unlimited is -1 and is not a number", func(t *testing.T) {
		fx.GivenPlan("enterprise", quota("seats", -1))
		d, _ := e.Consume(ctx, fx.Scope, "seats", 1_000_000, 1)
		if !d.Allowed || d.Remaining != -1 { t.Fatalf("unlimited mishandled: %+v", d) }
	})
	t.Run("upgrade is visible immediately, not after a TTL", func(t *testing.T) {
		fx.GivenPlan("free", flag("sso", false))
		_, _ = e.Allowed(ctx, fx.Scope, "sso") // warm the cache
		fx.UpgradeTo("pro")                    // must invalidate
		ok, _ := e.Allowed(ctx, fx.Scope, "sso")
		if !ok { t.Fatal("a customer who just paid must not wait out a cache TTL (invariant 9)") }
	})
	t.Run("Resolve reports provenance for every grant", func(t *testing.T) {
		for _, g := range mustResolve(t, e, fx.Scope) {
			if g.Source == "" { t.Fatalf("grant %q has no source — unanswerable in support", g.Key) }
			if g.Source == domain.FromOverride && g.Reason == "" { t.Fatal("an override must say why") }
		}
	})
	t.Run("realms do not leak", func(t *testing.T) {
		fx.GivenPlanInRealm("other", "pro", flag("sso", true))
		ok, _ := e.Allowed(ctx, fx.Scope, "sso") // fx.Scope is in realm "t1"
		if ok { t.Fatal("another realm's plan granted a capability here") }
	})
}
```

---

## Adoption checklist (per host product)

- [ ] **Keys declared** — every capability the host gates, with its `Kind`, registered for the realm. Undeclared keys deny.
- [ ] **Plan entitlements populated** — rows in `plan_entitlements` per plan, including the free tier's `Defaults`.
- [ ] **Enum orders declared** — for every `Enum` key, the host's ranking, so "highest wins" is defined.
- [ ] **Call sites converted** — no `if plan == "…"` remains in the host; every gate goes through `Entitler`.
- [ ] **Quota counts sourced from the host** — `Consume` is called with the host's real current count, not a cached one.
- [ ] **Refusal UX built** — `Reason` and `UpgradePath` rendered wherever a gate can refuse.
- [ ] **Cache invalidation wired** — subscription change, override write and grace expiry all evict the scope.
- [ ] **Override audit reviewed** — someone can answer "why does this customer have this?" from the audit rows alone.

---

## Non-goals

- **End-user authorization** (roles, permissions, row-level access) — the host's own concern. This port answers what the *plan* permits, not what the *person* may do. Conflating them puts a customer's org chart inside a billing service.
- **Usage counting** — the host counts its own seats and projects; `Consume` evaluates against a number it is handed.
- **Feature flags for rollout** — a release-engineering tool. Entitlements are commercial and stable; rollout flags are temporary and operational, and mixing them means deleting a stale flag can revoke something a customer bought.
