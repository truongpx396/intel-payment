package contracts

import (
	"context"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// MemDefects are the deliberate faults the suites are shown to catch: each switches on one way a store could be wrong. They
// exist so that "the suite has teeth" is a test result.
type MemDefects struct {
	IgnoreRealm       bool // reads ignore the realm: one product's plans, defaults and overrides reach another's scopes
	IgnoreOverrides   bool // Overrides returns nothing
	DropGrace         bool // Subscription forgets the grace window
	ClampExpiry       bool // Overrides forgets when each expires
	SortEnumOrder     bool // Keys returns each enum's order alphabetically instead of as declared
	ListRetiredPlans  bool // Plans lists plans that are off sale
	AppendOverrides   bool // PutOverride adds a second row for a scope and key instead of replacing
	ForgetRetired     bool // PlanEntitlements answers nothing for a plan that is off sale
	LiveCanceled      bool // Subscription reports a canceled subscription as live
	PromoteToActive   bool // Subscription reports every live subscription as active, whatever its state
	MatchByID         bool // Overrides match a scope by its id alone, not its kind
	AcceptUnexplained bool // PutOverride stores an override with no reason, no actor, or no standing as an override
}

type memSub struct {
	scope md.Scope
	sub   domain.Subscription
}

type memPlan struct {
	code    string
	order   int
	ents    []domain.Entitlement
	retired bool
}

// MemStore is a ports.EntitlementStore, and the Seeder for it, in memory: the reference the suites are first run against.
type MemStore struct {
	mu        sync.Mutex
	keys      map[md.Realm][]domain.KeyDecl
	plans     map[md.Realm][]*memPlan
	defaults  map[md.Realm][]domain.Entitlement
	subs      map[string]memSub
	overrides map[string][]domain.Grant
	// Defects switches on the faults above. The zero value is an honest store.
	Defects MemDefects
}

// NewMemStore is an empty, honest store.
func NewMemStore() *MemStore {
	return &MemStore{
		keys: map[md.Realm][]domain.KeyDecl{}, plans: map[md.Realm][]*memPlan{}, defaults: map[md.Realm][]domain.Entitlement{},
		subs: map[string]memSub{}, overrides: map[string][]domain.Grant{},
	}
}

var (
	_ ports.EntitlementStore = (*MemStore)(nil)
	_ Seeder                 = (*MemStore)(nil)
)

// realms is the realms a read covers: its own, or all of them under the IgnoreRealm defect.
func (m *MemStore) realms(r md.Realm) []md.Realm {
	if !m.Defects.IgnoreRealm {
		return []md.Realm{r.Or()}
	}
	var all []md.Realm
	for rr := range m.keys {
		all = append(all, rr)
	}
	for rr := range m.plans {
		all = append(all, rr)
	}
	for rr := range m.defaults {
		all = append(all, rr)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// DeclareKey implements Seeder.
func (m *MemStore) DeclareKey(t testing.TB, realm md.Realm, d domain.KeyDecl) {
	t.Helper()
	if err := d.Validate(); err != nil {
		t.Fatalf("DeclareKey(%s): %v", d.Key, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := realm.Or()
	decls := slices.DeleteFunc(slices.Clone(m.keys[r]), func(x domain.KeyDecl) bool { return x.Key == d.Key })
	d.EnumOrder = slices.Clone(d.EnumOrder)
	m.keys[r] = append(decls, d)
}

// PutPlan implements Seeder.
func (m *MemStore) PutPlan(t testing.TB, realm md.Realm, code string, order int, ents ...domain.Entitlement) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	r := realm.Or()
	m.plans[r] = slices.DeleteFunc(slices.Clone(m.plans[r]), func(p *memPlan) bool { return p.code == code })
	m.plans[r] = append(m.plans[r], &memPlan{code: code, order: order, ents: slices.Clone(ents)})
}

// RetirePlan implements Seeder.
func (m *MemStore) RetirePlan(t testing.TB, realm md.Realm, code string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plans[realm.Or()] {
		if p.code == code {
			p.retired = true
			return
		}
	}
	t.Fatalf("RetirePlan: no plan %q in realm %q", code, realm)
}

// PutDefaults implements Seeder.
func (m *MemStore) PutDefaults(t testing.TB, realm md.Realm, ents ...domain.Entitlement) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaults[realm.Or()] = slices.Clone(ents)
}

// Subscribe implements Seeder.
func (m *MemStore) Subscribe(t testing.TB, s md.Scope, plan string, status domain.SubStatus, graceUntil *time.Time) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[s.Tag()] = memSub{scope: s, sub: domain.Subscription{PlanCode: plan, Status: status, GraceUntil: graceUntil}}
}

// Unsubscribe implements Seeder.
func (m *MemStore) Unsubscribe(t testing.TB, s md.Scope) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	held, ok := m.subs[s.Tag()]
	if !ok {
		return
	}
	held.sub.Status = domain.Canceled
	m.subs[s.Tag()] = held
}

// Keys implements ports.EntitlementStore.
func (m *MemStore) Keys(_ context.Context, realm md.Realm) ([]domain.KeyDecl, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.KeyDecl
	for _, r := range m.realms(realm) {
		for _, d := range m.keys[r] {
			d.EnumOrder = slices.Clone(d.EnumOrder)
			if m.Defects.SortEnumOrder {
				slices.Sort(d.EnumOrder)
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Defaults implements ports.EntitlementStore.
func (m *MemStore) Defaults(_ context.Context, realm md.Realm) ([]domain.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Entitlement
	for _, r := range m.realms(realm) {
		out = append(out, m.defaults[r]...)
	}
	return out, nil
}

// PlanEntitlements implements ports.EntitlementStore.
func (m *MemStore) PlanEntitlements(_ context.Context, realm md.Realm, planCode string) ([]domain.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.Entitlement
	for _, r := range m.realms(realm) {
		for _, p := range m.plans[r] {
			if p.code == planCode && (!p.retired || !m.Defects.ForgetRetired) {
				out = append(out, p.ents...)
			}
		}
	}
	return out, nil
}

// Overrides implements ports.EntitlementStore.
func (m *MemStore) Overrides(_ context.Context, s md.Scope) ([]domain.Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Defects.IgnoreOverrides {
		return nil, nil
	}
	var out []domain.Grant
	for _, gs := range m.overrides {
		for _, g := range gs {
			mine := g.Scope.Tag() == s.Tag() ||
				(m.Defects.IgnoreRealm && g.Scope.Kind == s.Kind && g.Scope.ID == s.ID) ||
				(m.Defects.MatchByID && g.Scope.Realm.Or() == s.Realm.Or() && g.Scope.ID == s.ID)
			if !mine {
				continue
			}
			if m.Defects.ClampExpiry {
				g.ExpiresAt = nil
			}
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Subscription implements ports.EntitlementStore.
func (m *MemStore) Subscription(_ context.Context, s md.Scope) (domain.Subscription, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, ok := m.subs[s.Tag()]
	if !ok && m.Defects.IgnoreRealm { // a store that forgot the realm finds the same kind and id wherever it is
		for _, h := range m.subs {
			if h.scope.Kind == s.Kind && h.scope.ID == s.ID {
				held, ok = h, true
				break
			}
		}
	}
	if !ok {
		return domain.Subscription{}, false, nil
	}
	sub := held.sub
	live := sub.Status == domain.Trialing || sub.Status == domain.Active || sub.Status == domain.PastDue || sub.Status == domain.Paused
	if !live && (!m.Defects.LiveCanceled || sub.Status != domain.Canceled) {
		return domain.Subscription{}, false, nil
	}
	if m.Defects.DropGrace {
		sub.GraceUntil = nil
	}
	if m.Defects.PromoteToActive {
		sub.Status = domain.Active
	}
	return sub, true, nil
}

// Plans implements ports.EntitlementStore.
func (m *MemStore) Plans(_ context.Context, realm md.Realm) ([]domain.Offer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ps []*memPlan
	for _, r := range m.realms(realm) {
		ps = append(ps, m.plans[r]...)
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].order != ps[j].order {
			return ps[i].order < ps[j].order
		}
		return ps[i].code < ps[j].code
	})
	var out []domain.Offer
	for _, p := range ps {
		if p.retired && !m.Defects.ListRetiredPlans {
			continue
		}
		out = append(out, domain.Offer{PlanCode: p.code, Entitlements: slices.Clone(p.ents)})
	}
	return out, nil
}

// PutOverride implements ports.EntitlementStore. An override is one row per scope and key: writing it again replaces it.
func (m *MemStore) PutOverride(_ context.Context, g domain.Grant, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.Defects.AcceptUnexplained {
		if err := g.ValidateOverride(actor); err != nil {
			return err
		}
	}
	tag := g.Scope.Tag()
	if !m.Defects.AppendOverrides {
		m.overrides[tag] = slices.DeleteFunc(slices.Clone(m.overrides[tag]), func(x domain.Grant) bool { return x.Key == g.Key })
	}
	m.overrides[tag] = append(m.overrides[tag], g)
	return nil
}
