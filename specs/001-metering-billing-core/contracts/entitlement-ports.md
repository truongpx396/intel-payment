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

**Layout.** `entitlement/domain` (the vocabulary and the arithmetic) → `entitlement/ports` → `entitlement/app` (resolution, the `Entitler`) → `entitlement/adapters/driven/postgres`, one direction, enforced by `go-arch-lint` and `depguard` as the engine's and the payment layer's are. `entitlement/contracts` holds the conformance suites and the in-memory store they first run on. The layer reaches metering through its *ports* and the payment layer **not at all**: the two share a database schema, never a package, so the payment layer's rule for which subscriptions entitle a scope (`Sub.HoldsEntitlements`) is restated in `entitlement/domain` (`Subscription.Holds`) and a parity test holds the two together.

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

// KeyDecl declares a capability to a realm: its kind and, for an enum, its ranking from lowest to highest.
type KeyDecl struct {
	Key       Key
	Kind      Kind
	EnumOrder []string // lowest to highest; enums only
}

// Subscription is the part of a scope's paid plan that decides what it is entitled to.
type Subscription struct {
	PlanCode   string
	Status     SubStatus // trialing | active | past_due | paused | canceled | incomplete
	GraceUntil *time.Time
}

// Offer is a plan a realm sells, with what it unlocks: where a refusal looks for the plan that would permit what was asked.
type Offer struct {
	PlanCode     string
	Entitlements []Entitlement
}

type Decision struct {
	Allowed   bool
	Limit     int64  // -1 = unlimited
	Current   int64
	// Remaining is the room left once the requested delta is taken; a refusal leaves the room it found, never below zero.
	// -1 when unlimited.
	Remaining int64
	// Reason is set when refused, and is written for a HUMAN: "Your "team" plan includes 25 seats; you have 25 and asked for 1
	// more." An entitlement refusal the user cannot act on is an outage to them.
	Reason string
	// UpgradePath is the plan code that would permit it, when one exists. This is what makes a refusal a sales moment rather
	// than a dead end.
	UpgradePath string
}
```

The pure rules live in the domain, where they can be held to properties (`entitlement/domain`, fuzzed against `math/big`):

- `Value.Admits(current, delta)` decides `current + delta <= limit` as `delta <= limit - current`, so a sum no `int64` can hold is **refused, never wrapped**. Unlimited admits any count. A count already over the limit (the plan shrank under it) is refused even for a delta of zero. A negative count, or a value that is not a quota, is `ErrInvalid`.
- `KeyDecl.Merge(a, b)` joins two values for one key at one level: a flag ORs, a quota takes the maximum with **unlimited above every number**, an enum takes the highest-ranked member of the declared order. An unset side contributes nothing — it is not zero and it is not unlimited. The join is commutative, associative and idempotent.
- `KeyDecl.Check(v)` — a value is only meaningful against its declaration: the kind must match and an enum member must be ranked.
- `Subscription.Holds(now)` — `active` and `trialing` hold; `past_due` holds until the instant `GraceUntil` and not after; everything else holds nothing.

Errors: `ErrInvalid` is the engine's (a malformed key, value or grant; asking a key the wrong kind of question; an override with no reason — `ErrReasonRequired` — are all the caller's mistake, 400). `ErrUnavailable` is an entitlement that could not be resolved (the store did not answer): never "not entitled" and never "entitled", 503, with the cause wrapped for the operator.

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

// Decision is entitlement/domain.Decision, above.
```

**Failing closed, on every method.** A key the realm never declared is `false` / `!ok` with **no error** (invariant 1); a store that cannot answer is an error wrapping `ErrUnavailable` and never a permit (invariant 2); asking a key the wrong kind of question — `Allowed` of a quota, `Quota` of a flag — is `ErrInvalid`, never a guess. `Consume` of a key nothing grants is a *refusal* that says why and, when a plan would grant it, names that plan.

**The `UpgradePath`** is the first plan in the catalogue (`Plans`, in catalogue order) other than the one the scope holds whose quota for the key would permit the request — unlimited, or large enough for `current + delta`. A refusal whose limit comes from an **override** names none: an override outranks every plan, so upgrading would not lift it.

## Port: `EntitlementStore`

```go
type EntitlementStore interface {
	// Keys are the realm's declared capabilities, with each enum's ranking exactly as declared (lowest to highest).
	Keys(ctx context.Context, realm domain.Realm) ([]domain.KeyDecl, error)
	// Defaults are the realm's free tier: what a scope with no subscription gets.
	Defaults(ctx context.Context, realm domain.Realm) ([]domain.Entitlement, error)
	// PlanEntitlements are the benefits attached to a plan — data, versioned with the plan. A plan taken off sale still
	// answers for its subscribers; an unknown code has none, and is not an error.
	PlanEntitlements(ctx context.Context, realm domain.Realm, planCode string) ([]domain.Entitlement, error)
	// Overrides are per-scope exceptions (a sales deal, a trial extension, a comp account), expired ones included: whether an
	// override still applies is decided at the instant of the question, from the clock the Entitler was given.
	Overrides(ctx context.Context, s domain.Scope) ([]domain.Grant, error)
	// Subscription is the scope's LIVE subscription (trialing, active, past_due or paused), if it has one; a canceled or
	// incomplete one is not live. Whether a live one entitles anything is Subscription.Holds, decided at the instant of the
	// question.
	Subscription(ctx context.Context, s domain.Scope) (domain.Subscription, bool, error)
	// Plans are the subscription plans the realm sells, in catalogue order (sort_order, then code), with what each unlocks.
	Plans(ctx context.Context, realm domain.Realm) ([]domain.Offer, error)
	// PutOverride writes an exception. The database's audit trigger records who, what, why and when in the same transaction
	// (metering invariant 18) — an unexplained override is indistinguishable from a bug, and is usually discovered during a
	// revenue investigation.
	PutOverride(ctx context.Context, g domain.Grant, actor string) error
}
```

`Subscription` and `Plans` were missing from the first draft of this port: without them an `Entitler` could not know a scope's plan, whether a grace window had closed, or which plan a refusal should point at. Every read is by realm or by scope and never crosses a realm (invariant 10). The PostgreSQL store reads the payment layer's `subscriptions` and `plans` tables and imports none of its code.

## Adapter: metering's `QuotaSource` — plan-sized ceilings

A metering ceiling is usually a *plan* decision: Free members get 10 000 000 credits a day, Pro
members 50 000 000, and one enterprise customer negotiated more. That is exactly what entitlements
already model — values per plan, a free-tier default, audited per-scope overrides, a precedence
order, provenance, and cache invalidation on change. So metering does not grow a parallel system:
a `limits` row may name a `max_entitlement` quota key, and the entitlement module provides
metering's `QuotaSource` port, wired in `cmd/`:

```go
// metering/ports — implemented by entitlement, never imported by metering's core.
type QuotaSource interface {
	// Quota returns the scope's effective quota for a key: a size, or -1 for unlimited (the ceiling is dropped).
	// domain.ErrQuotaNotGranted means nothing grants it, and the limit row's own max is used. Any other error means the quota
	// could not be resolved.
	Quota(ctx context.Context, s domain.Scope, key string) (int64, error)
}
```

(The first draft of this port returned `(value, ok, err)`. The shipped port folds `ok=false` into `ErrQuotaNotGranted`, so that "nothing grants it" and "could not be resolved" cannot be confused at the call site — the one confusion that would turn an outage into an uncapped scope.)

`app.NewQuotaSource(entitler)` is the adapter, wired in `cmd/`. It answers from the **same resolution the `Entitler` enforces** — precedence, grace, expiry, the cache and its announcements — so an upgrade raises the ceiling on the next `Admit`, not after a TTL:

| The entitler says | The adapter returns | `Admit` does |
|---|---|---|
| a size `n` | `n` | enforces `n` |
| unlimited | `-1` | drops the ceiling (an unlimited plan is a grant, not an absence) |
| nothing grants the key | `ErrQuotaNotGranted` | falls back to the limit row's own `max` |
| an error (the store is down) | the error, untouched | applies `AdmitFailPolicy` — **never** "unlimited" (metering invariant 17) |
| the key is a flag or an enum | the error (`ErrInvalid`), untouched | applies `AdmitFailPolicy` |

**A non-numeric quota key is refused when it is configured, not when it is spent.** A limit that names a flag or an enum as its size would otherwise be discovered by the first customer to hit `Admit`, as a `503 quota_unavailable` — or, under `fail_open`, as an uncapped scope. So the database refuses it, which holds for the admin API and for a write that bypasses it: a `limits` row whose `max_entitlement` names a key the realm *declares* as anything but a quota is `check_violation` (`limits_max_entitlement_is_quota`, `400` at the API), and a quota key that a limit names cannot be re-declared as a flag or an enum (`entitlement_keys_quota_in_use`). A key not declared yet is accepted — declare keys in any order; until then nothing grants it and the limit's own `max` applies.

---

## Precedence (exactly one order, stated once)

```text
override  >  subscription  >  default
```

1. **Override wins**, always. It exists to say "yes despite the plan" or "no despite the plan", and a precedence that lets a plan change silently erase a negotiated deal is a broken precedence.
2. **Subscription** next. `past_due` within `GraceUntil` still counts (payments invariant 13); past the grace window it does not.
3. **Default** last — the free tier.

Within one level, for the same key: a **flag** ORs (any grant enabling it wins), a **quota** takes the **maximum**, and an **enum** takes the highest-ranked member of the host-declared order. Stated explicitly because "two grants for one key" is not hypothetical — it is what a migration between plans looks like for as long as it runs. (The shipped schema allows one row per key per level, so a store that honours it never produces two; the rule is what makes any other store deterministic.)

Three more rules, found by writing the suite:

4. **A value its key cannot hold grants nothing.** A stored entitlement for a key the realm never declared, of the wrong kind for its key, or naming an enum member the key never ranked, is skipped — and counted on `entitlement_invalid_total` — so the level below answers. It is never read as permission, and never an error that takes the whole scope down.
5. **An override ends at its expiry instant**, and nothing announces it: no tick expires an override. An implementation that caches must bound an entry by the earliest expiry it holds.
6. **A grant held by grace says so.** While a `past_due` subscription holds, its grants report `ExpiresAt = GraceUntil`, so a plan card can say when the plan lapses.

---

## The cache

One entry per scope holds the whole resolution (the declared keys, the free tier, the plan's entitlements and the overrides as they were read at one instant), so every method — `Allowed`, `Quota`, `Choice`, `Consume`, `Resolve` and the metering `QuotaSource` — answers from the same picture. `Deps.CacheTTL` of zero means no cache.

**An entry is valid until the earliest of three things:** its TTL; the end of the grace window it was resolved under (the subscription's grants say so in `ExpiresAt`); and the expiry of any override it applied. The last two matter because **nothing announces them** — no tick expires an override, and a lost `grace_expired` message must not extend a plan the customer stopped paying for. An entry never outlives a boundary it knew about, at the nanosecond.

**It is evicted at once by exactly two announcements, and by nothing else:**

| Subject | Evicts | Published by |
|---|---|---|
| `<prefix>.entitlement.<token>` — `{realm, scope:{kind,id}, reason}`, `reason` = `subscription` · `grace_expired` · `override` | the one scope the token names | the webhook processor (a subscription change; the sweep that ends a grace window); `Overrides.Put` (an override write) |
| `<prefix>.config.<realm>` — `{table, realm}` | every scope of the realm; `(global)` (a table that belongs to no realm) evicts everything | the **config relay**: the database's audit trigger `NOTIFY`s on every configuration write, and `ConfigRelay` forwards it, so a change made by SQL reaches every process as surely as one made through the API |

A payment, a low balance, a warning, another deployment's prefix, a subject with too many tokens or a token that is not a scope's — none evicts, and none is an error (a message the handler does not own is acknowledged, never redelivered for ever). The subject is the address: a body that cannot be read on the right subject still evicts, because the safe direction is fresh.

**What a lost announcement costs is the TTL, and no more.** The bus is at-least-once and a replica can be down while one is published; the TTL is the bound that makes that a bounded staleness and not a permanent one. An entry that raced an announcement — a load that began before a change and finished after its announcement — is **not kept**: each eviction advances an epoch, and a load that spans one is returned to its caller (the request raced the change) and never stored. Concurrent misses for one scope are one read, not one each. Errors are never cached.

**A warm entry is served through a store outage until its TTL.** That is what bounded staleness means, and the alternative — every request failing the moment the store blinks — is the outage invariant 2 exists to avoid being worse than. Past the TTL the outage is an error, never a permit.

**Every replica must see every announcement**, so each runs its `Watch` under a group of its own (`entitlement-cache-<instance>`), not one shared group — a shared group would hand each message to one replica. The name must be **stable across restarts**: a bus stream is only trimmed up to what every group has acknowledged, so a group that is abandoned (a replica retired for good) holds the stream open until it is deleted. Retiring a replica includes retiring its group ([docs/operations.md](../../../docs/operations.md)).

---

## Invariants

1. **Unknown key ⇒ denied.** A key the realm never declared is `false` / `!ok`, never a permissive default. Typos must fail closed, and they will happen.
2. **Fail closed on an error.** If the store is unreachable, `Allowed` returns an error and the host denies. There is no `FailOpen` knob here, unlike `Admit`: unmetered *usage* is a bounded, recoverable cost, while an un-gated *capability* can expose another tenant's data. The two failures are not comparable, so they do not share a policy.
3. **Entitlements are data, never plan-code branches.** No `if plan == "pro"` anywhere — in this repo or in a host. A new plan is rows, not a release. `scripts/check-no-plan-branches.sh` fails the build on a comparison of a plan's identity with a string literal, a `switch` on one, or a string-matching helper given one, in production Go; it is shown to reject planted violations by `scripts/verify-boundary.sh`, and a host should run it over its own tree.
4. **`Consume` never stores usage.** The host owns its own counts. A second count is a second truth.
5. **Overrides are audited.** Every override write records who, what, why and when, in the same transaction as the override itself — written by the database's audit trigger, so it holds even for a write that bypasses the API.
6. **`-1` means unlimited, and is handled explicitly.** Never compared numerically, never coerced to a large number, never confused with "unset".
7. **A refusal is actionable.** Every denied `Decision` carries a human-readable `Reason` and, when one exists, an `UpgradePath`.
8. **The UI reads the same resolution the server enforces.** `Resolve` is the single source for both. A plan card assembled from a separate source will eventually promise something the server refuses.
9. **Cache is bounded and invalidated on change.** Entitlements are read on nearly every request, so they are cached with a short TTL — and a subscription change, an override write or a grace expiry invalidates the scope's entry immediately. A customer who just upgraded must not wait out a TTL to use what they paid for. See [The cache](#the-cache) for what that costs and what it guarantees.
10. **Realm isolation.** Plans, defaults and overrides never cross a realm (metering invariant 11).

---

## Conformance suites

The skeleton that stood here is now `entitlement/contracts`: two suites that run unchanged against every implementation, each given a constructor and importing no adapter. Each case builds a **fresh world**, so the cases run in parallel and none depends on another's side effects. Each suite is shown to reject deliberately broken implementations **by name** (`Probe`), so "the suite has teeth" is a test result.

**`EntitlerContract(t, mk WorldFactory)`** — the behaviour a host depends on. A `World` holds the `Entitler`, the store beneath it (and a `Breakable` wrapper to take it down), a `Seeder`, a fake clock, a scope in realm `t1`, and `Announce`, which does what the payment layer does after a change — publishes `billing.entitlement.<tag>`.

| Case | Pins |
|---|---|
| an undeclared key is denied on every method, and denying it is not an error | invariant 1 |
| a store outage is an error on every method and never a permit | invariant 2 |
| an override beats the subscription, whichever way it points | precedence |
| `past_due` inside the grace window keeps what the plan unlocks | payments invariant 13 |
| when the grace window closes the scope falls back to the free tier, at that instant | the boundary, to the nanosecond |
| a canceled, incomplete or paused subscription holds nothing | |
| a scope with no subscription gets the free tier and nothing else | |
| a refused quota names why in words a person can act on, and the plan that would permit it | invariant 7 |
| unlimited is `-1` and is not a number | invariant 6 |
| an upgrade, an override and a lapse are visible at once, not after a TTL | invariant 9 |
| `Resolve` reports where every grant came from, and is what the checks enforce | invariants 5, 8 |
| realms do not leak | invariant 10 |
| an override ends at its expiry instant, with nobody announcing it | |
| asking a key the wrong kind of question is an error, never a guess | |
| a stored value its key cannot hold grants nothing and the lower level answers | |

**`StoreContract(t, mk StoreFactory)`** — what an `EntitlementStore` keeps true whatever it is built on: a realm's keys come back as declared, each enum's order as it was declared and not the alphabet's; keys, defaults, plans and overrides never cross a realm; an override is read back whole and writing it again replaces it (one per scope and key); an override belongs to its scope alone; a scope's subscription is its live one with its grace window, and a canceled one is not live; the plans on sale are listed in catalogue order with what each unlocks and a plan off sale is not listed; a plan taken off sale still entitles its subscribers.

Both run over the in-memory store (`entitlement/contracts`) and over PostgreSQL 16 (`entitlement/adapters/driven/postgres`, integration).

---

## Observability

Part of the contract: a host writes alerts against these names before it reads the code. `ports.MetricCatalog` is held equal to this table by a test, and both processes declare every metric from the first scrape.

| Metric | Kind | Meaning |
|---|---|---|
| `entitlement_decisions_total` | counter (`realm`, `outcome`) | `Allowed` and `Consume` by outcome: `allowed`, `denied`, or `error` — the store could not answer and the host denied. A rising `error` rate is an outage of the gate |
| `entitlement_invalid_total` | counter (`realm`, `source`) | a stored grant skipped because its key cannot hold it (undeclared key, wrong kind, unranked enum member). Never granted; it is a data bug and worth a look |
| `entitlement_cache_total` | counter (`result`) | resolutions served from the cache (`hit`) or read from the store (`miss`). A hit rate that falls with no deploy is a cache being evicted faster than it fills |
| `entitlement_cache_invalidations_total` | counter (`source`) | announcements applied: one `scope`, or a whole `realm`. A rate of zero over a day with subscription changes means the replica's `Watch` is not receiving |

---

## Adoption checklist (per host product)

- [ ] **Keys declared** — every capability the host gates, with its `Kind`, as `entitlement_keys` rows for the realm. Undeclared keys deny.
- [ ] **Plan-sized ceilings linked** — each metering limit whose size depends on the plan names its quota key in `max_entitlement`.
- [ ] **Plan entitlements populated** — rows in `plan_entitlements` per plan, including the free tier's `Defaults`.
- [ ] **Enum orders declared** — `entitlement_keys.enum_order` for every `Enum` key, so "highest wins" is defined.
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
