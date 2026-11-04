package app_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

var scope = md.Scope{Realm: "t1", Kind: "org", ID: "o1"}

// memWorld builds the world a case runs in: the in-memory store, optionally with a defect, beneath the real Entitler,
// optionally wrapped by a broken one.
func memWorld(defect func(*contracts.MemDefects), wrap func(ports.Entitler) ports.Entitler) contracts.WorldFactory {
	return memWorldOver(defect, nil, wrap)
}

// memWorldOver also lets a case put a broken layer between the Entitler and the store it reads.
func memWorldOver(defect func(*contracts.MemDefects), over func(ports.EntitlementStore) ports.EntitlementStore, wrap func(ports.Entitler) ports.Entitler) contracts.WorldFactory {
	return func(testing.TB) *contracts.World {
		store := contracts.NewMemStore()
		if defect != nil {
			defect(&store.Defects)
		}
		faults := contracts.NewBreakable(store)
		var read ports.EntitlementStore = faults
		if over != nil {
			read = over(faults)
		}
		clock := contracts.NewFakeClock(contracts.T0)
		e, err := app.New(app.Deps{Store: read, Clock: clock})
		if err != nil {
			panic(err)
		}
		var ent ports.Entitler = e
		if wrap != nil {
			ent = wrap(e)
		}
		return &contracts.World{Entitler: ent, Store: store, Faults: faults, Seed: store, Clock: clock, Scope: scope}
	}
}

func TestEntitlerHonoursTheEntitlerContractOverTheInMemoryStore(t *testing.T) {
	t.Parallel()
	contracts.EntitlerContract(t, memWorld(nil, nil))
}

// ---- broken entitlers: each is one way an implementation could be wrong -----------------------------------------------

// failOpen permits when it cannot tell: the failure invariant 2 forbids.
type failOpen struct{ ports.Entitler }

func (f failOpen) Allowed(ctx context.Context, s md.Scope, k domain.Key) (bool, error) {
	ok, err := f.Entitler.Allowed(ctx, s, k)
	if err != nil {
		return true, nil
	}
	return ok, nil
}

func (f failOpen) Consume(ctx context.Context, s md.Scope, k domain.Key, c, d int64) (domain.Decision, error) {
	dec, err := f.Entitler.Consume(ctx, s, k, c, d)
	if err != nil {
		return domain.Decision{Allowed: true}, nil
	}
	return dec, nil
}

// permitsUnknown treats a key nothing grants as permitted: the typo that fails open.
type permitsUnknown struct{ ports.Entitler }

func (p permitsUnknown) Allowed(ctx context.Context, s md.Scope, k domain.Key) (bool, error) {
	ok, err := p.Entitler.Allowed(ctx, s, k)
	if err != nil || ok {
		return ok, err
	}
	gs, err := p.Resolve(ctx, s)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(gs, func(g domain.Grant) bool { return g.Key == k }), nil
}

// allowedForAnyKind answers a flag question about a quota by reading it as "has some".
type allowedForAnyKind struct{ ports.Entitler }

func (a allowedForAnyKind) Allowed(ctx context.Context, s md.Scope, k domain.Key) (bool, error) {
	if ok, err := a.Entitler.Allowed(ctx, s, k); err == nil {
		return ok, nil
	}
	limit, ok, err := a.Quota(ctx, s, k)
	return err == nil && ok && limit != 0, nil
}

// refusesWithoutWords is a refusal nobody can act on.
type refusesWithoutWords struct{ ports.Entitler }

func (r refusesWithoutWords) Consume(ctx context.Context, s md.Scope, k domain.Key, c, d int64) (domain.Decision, error) {
	dec, err := r.Entitler.Consume(ctx, s, k, c, d)
	dec.Reason = ""
	return dec, err
}

// refusesWithoutAWayOut never names the plan that would permit it.
type refusesWithoutAWayOut struct{ ports.Entitler }

func (r refusesWithoutAWayOut) Consume(ctx context.Context, s md.Scope, k domain.Key, c, d int64) (domain.Decision, error) {
	dec, err := r.Entitler.Consume(ctx, s, k, c, d)
	dec.UpgradePath = ""
	return dec, err
}

// unlimitedIsMinusOne compares the ceiling as a number: -1 is below every count, so everything is over it.
type unlimitedIsMinusOne struct{ ports.Entitler }

func (u unlimitedIsMinusOne) Consume(ctx context.Context, s md.Scope, k domain.Key, c, d int64) (domain.Decision, error) {
	dec, err := u.Entitler.Consume(ctx, s, k, c, d)
	if err == nil && dec.Limit == -1 {
		dec.Allowed, dec.Remaining = c+d <= dec.Limit, dec.Limit-c-d
	}
	return dec, err
}

// stale remembers the first answer it gave a key for ever: a cache with no invalidation and no TTL.
type stale struct {
	ports.Entitler
	mu   sync.Mutex
	seen map[domain.Key]bool
}

func (s *stale) Allowed(ctx context.Context, sc md.Scope, k domain.Key) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.seen[k]; ok {
		return v, nil
	}
	v, err := s.Entitler.Allowed(ctx, sc, k)
	if err == nil {
		s.seen[k] = v
	}
	return v, err
}

// swallowsOverrides answers from what it managed to read: an overrides table that cannot be read is treated as one with nothing
// in it, so the override that says "no" is missed.
type swallowsOverrides struct{ ports.EntitlementStore }

func (s swallowsOverrides) Overrides(ctx context.Context, sc md.Scope) ([]domain.Grant, error) {
	gs, err := s.EntitlementStore.Overrides(ctx, sc)
	if err != nil {
		return nil, nil
	}
	return gs, nil
}

// alwaysSuggestsAPlan names a plan to every refusal, including ones no plan would lift.
type alwaysSuggestsAPlan struct{ ports.Entitler }

func (a alwaysSuggestsAPlan) Consume(ctx context.Context, s md.Scope, k domain.Key, c, d int64) (domain.Decision, error) {
	dec, err := a.Entitler.Consume(ctx, s, k, c, d)
	if err == nil && !dec.Allowed && dec.UpgradePath == "" {
		dec.UpgradePath = "business"
	}
	return dec, err
}

// anonymous reports grants and not where they came from.
type anonymous struct{ ports.Entitler }

func (a anonymous) Resolve(ctx context.Context, s md.Scope) ([]domain.Grant, error) {
	gs, err := a.Entitler.Resolve(ctx, s)
	for i := range gs {
		gs[i].Source = ""
	}
	return gs, err
}

// disagrees shows the plan card a picture other than the one it enforces.
type disagrees struct{ ports.Entitler }

func (d disagrees) Resolve(ctx context.Context, s md.Scope) ([]domain.Grant, error) {
	gs, err := d.Entitler.Resolve(ctx, s)
	for i := range gs {
		if gs[i].Value.Kind == domain.Flag {
			gs[i].Value.Bool = !gs[i].Value.Bool
		}
	}
	return gs, err
}

// The suite is held to what it enforces: it passes an honest Entitler, and it rejects each deliberately broken one — by name.
func TestEntitlerContractRejectsEveryBrokenEntitlerByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		world    contracts.WorldFactory
		caughtBy []string
	}{
		{"permits when the store is down", memWorld(nil, func(e ports.Entitler) ports.Entitler { return failOpen{e} }), []string{contracts.CaseOutage}},
		{"answers from what it could read when the overrides cannot be", memWorldOver(nil, func(st ports.EntitlementStore) ports.EntitlementStore { return swallowsOverrides{st} }, nil), []string{contracts.CaseOutage}},
		{"names a plan to every refusal, including ones no plan lifts", memWorld(nil, func(e ports.Entitler) ports.Entitler { return alwaysSuggestsAPlan{e} }), []string{contracts.CaseRefusalActionable}},
		{"permits a key nothing grants", memWorld(nil, func(e ports.Entitler) ports.Entitler { return permitsUnknown{e} }), []string{contracts.CaseUndeclared}},
		{"reads a quota as a flag", memWorld(nil, func(e ports.Entitler) ports.Entitler { return allowedForAnyKind{e} }), []string{contracts.CaseWrongQuestion}},
		{"refuses without saying why", memWorld(nil, func(e ports.Entitler) ports.Entitler { return refusesWithoutWords{e} }), []string{contracts.CaseRefusalActionable}},
		{"refuses without naming a way out", memWorld(nil, func(e ports.Entitler) ports.Entitler { return refusesWithoutAWayOut{e} }), []string{contracts.CaseRefusalActionable}},
		{"compares unlimited as the number -1", memWorld(nil, func(e ports.Entitler) ports.Entitler { return unlimitedIsMinusOne{e} }), []string{contracts.CaseUnlimited}},
		{"never forgets an answer", memWorld(nil, func(e ports.Entitler) ports.Entitler { return &stale{Entitler: e, seen: map[domain.Key]bool{}} }), []string{contracts.CaseUpgradeAtOnce}},
		{"forgets where a grant came from", memWorld(nil, func(e ports.Entitler) ports.Entitler { return anonymous{e} }), []string{contracts.CaseProvenance}},
		{"shows a plan card it does not enforce", memWorld(nil, func(e ports.Entitler) ports.Entitler { return disagrees{e} }), []string{contracts.CaseProvenance}},
		{"reads another realm's plans, defaults and overrides", memWorld(func(d *contracts.MemDefects) { d.IgnoreRealm = true }, nil), []string{contracts.CaseRealms}},
		{"ignores overrides", memWorld(func(d *contracts.MemDefects) { d.IgnoreOverrides = true }, nil), []string{contracts.CaseOverrideBeats}},
		{"forgets the grace window", memWorld(func(d *contracts.MemDefects) { d.DropGrace = true }, nil), []string{contracts.CaseGraceHolds}},
		{"treats every live subscription as active", memWorld(func(d *contracts.MemDefects) { d.PromoteToActive = true }, nil), []string{contracts.CaseGraceLapses, contracts.CaseNotHeld}},
		{"forgets when an override ends", memWorld(func(d *contracts.MemDefects) { d.ClampExpiry = true }, nil), []string{contracts.CaseExpiry}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.EntitlerCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, b.world) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED an entitler that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryEntitlerCaseHasAUniqueNameAndTheHonestEntitlerPassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.EntitlerCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, memWorld(nil, nil)) }); failed {
			t.Errorf("the honest entitler fails %q: %v", c.Name, said)
		}
	}
}
