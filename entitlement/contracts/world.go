package contracts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Ctx is the context a suite runs under.
func Ctx() context.Context { return context.Background() }

// T0 is the instant every world's clock starts at.
var T0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// FakeClock is a Clock that moves only when told to. A grace window, an override's expiry and a cache entry's lifetime all take
// their "now" from it, so a case can cross any of them without waiting.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

// Now is the clock's instant.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Seeder puts a store into the state a case needs. It is the part of a store a SUITE writes and a host's admin API owns, so it
// is not part of the port: each implementation gives its own (the in-memory store, and SQL rows for PostgreSQL).
type Seeder interface {
	// DeclareKey declares a capability to a realm, replacing an earlier declaration of the same key.
	DeclareKey(t testing.TB, realm md.Realm, d domain.KeyDecl)
	// PutPlan puts a subscription plan on sale at catalogue position order, replacing the plan of that code. Its
	// entitlements may name keys the realm never declared: that is what a stale row looks like, and it must grant nothing.
	PutPlan(t testing.TB, realm md.Realm, code string, order int, ents ...domain.Entitlement)
	// RetirePlan takes a plan off sale. Its subscribers keep what it unlocks.
	RetirePlan(t testing.TB, realm md.Realm, code string)
	// PutDefaults sets the realm's free tier, replacing the earlier one.
	PutDefaults(t testing.TB, realm md.Realm, ents ...domain.Entitlement)
	// Subscribe gives the scope a subscription to a plan of its realm, replacing its earlier one.
	Subscribe(t testing.TB, s md.Scope, plan string, status domain.SubStatus, graceUntil *time.Time)
	// Unsubscribe ends the scope's subscription: it is canceled, and no longer live.
	Unsubscribe(t testing.TB, s md.Scope)
}

// World is the Entitler under test and the means to arrange and disturb what it reads.
type World struct {
	Entitler ports.Entitler
	// Store is the store beneath the Entitler, unbroken: where a case writes an override and reads one back.
	Store ports.EntitlementStore
	// Faults breaks and heals the store the Entitler reads through.
	Faults *Breakable
	Seed   Seeder
	Clock  *FakeClock
	// Scope is the scope the case acts for, in realm "t1".
	Scope md.Scope
	// Announce delivers what the payment layer does after a change to the scope's entitlements — a subscription change, an
	// override write, a grace expiry — which is to publish billing.entitlement.<tag>. Nil when the implementation under test
	// holds nothing to evict.
	Announce func(reason string)
}

// WorldFactory builds a fresh world for one case: an empty store, a clock at T0, and an Entitler over them.
type WorldFactory func(t testing.TB) *World

// Case is one subtest of a suite.
type Case[F any] struct {
	Name string
	Run  func(t testing.TB, mk F)
}

func runCases[F any](t *testing.T, cases []Case[F], mk F) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}

// Breakable wraps a store and makes its calls fail on demand: the store outage invariant 2 is about. It breaks every call, or
// only the named methods — a half-broken store (the overrides table is down and the rest answers) is the outage that tempts an
// implementation to answer from what it did read.
type Breakable struct {
	inner   ports.EntitlementStore
	mu      sync.Mutex
	err     error
	methods map[string]bool // nil: every method
}

// NewBreakable wraps inner, healthy.
func NewBreakable(inner ports.EntitlementStore) *Breakable { return &Breakable{inner: inner} }

// BreakWith makes every call fail with err until Heal.
func (b *Breakable) BreakWith(err error) { b.mu.Lock(); b.err, b.methods = err, nil; b.mu.Unlock() }

// BreakOnly makes the named methods (Keys, Defaults, PlanEntitlements, Overrides, Subscription, Plans) fail with err until Heal;
// the others answer.
func (b *Breakable) BreakOnly(err error, methods ...string) {
	b.mu.Lock()
	b.err, b.methods = err, map[string]bool{}
	for _, m := range methods {
		b.methods[m] = true
	}
	b.mu.Unlock()
}

// Heal ends the outage.
func (b *Breakable) Heal() { b.mu.Lock(); b.err, b.methods = nil, nil; b.mu.Unlock() }

func (b *Breakable) broken(method string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil && (b.methods == nil || b.methods[method]) {
		return b.err
	}
	return nil
}

// Keys implements ports.EntitlementStore.
func (b *Breakable) Keys(ctx context.Context, r md.Realm) ([]domain.KeyDecl, error) {
	if err := b.broken("Keys"); err != nil {
		return nil, err
	}
	return b.inner.Keys(ctx, r)
}

// Defaults implements ports.EntitlementStore.
func (b *Breakable) Defaults(ctx context.Context, r md.Realm) ([]domain.Entitlement, error) {
	if err := b.broken("Defaults"); err != nil {
		return nil, err
	}
	return b.inner.Defaults(ctx, r)
}

// PlanEntitlements implements ports.EntitlementStore.
func (b *Breakable) PlanEntitlements(ctx context.Context, r md.Realm, code string) ([]domain.Entitlement, error) {
	if err := b.broken("PlanEntitlements"); err != nil {
		return nil, err
	}
	return b.inner.PlanEntitlements(ctx, r, code)
}

// Overrides implements ports.EntitlementStore.
func (b *Breakable) Overrides(ctx context.Context, s md.Scope) ([]domain.Grant, error) {
	if err := b.broken("Overrides"); err != nil {
		return nil, err
	}
	return b.inner.Overrides(ctx, s)
}

// Subscription implements ports.EntitlementStore.
func (b *Breakable) Subscription(ctx context.Context, s md.Scope) (domain.Subscription, bool, error) {
	if err := b.broken("Subscription"); err != nil {
		return domain.Subscription{}, false, err
	}
	return b.inner.Subscription(ctx, s)
}

// Plans implements ports.EntitlementStore.
func (b *Breakable) Plans(ctx context.Context, r md.Realm) ([]domain.Offer, error) {
	if err := b.broken("Plans"); err != nil {
		return nil, err
	}
	return b.inner.Plans(ctx, r)
}

// PutOverride implements ports.EntitlementStore.
func (b *Breakable) PutOverride(ctx context.Context, g domain.Grant, actor string) error {
	if err := b.broken("PutOverride"); err != nil {
		return err
	}
	return b.inner.PutOverride(ctx, g, actor)
}

var _ ports.EntitlementStore = (*Breakable)(nil)
