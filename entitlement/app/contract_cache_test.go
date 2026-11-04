package app_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R103: the Entitler contract is run, unchanged, over an Entitler that CACHES — with a TTL far longer than any case, so that
// nothing but the announcement (or a boundary the entry knows about) can make a change visible. If the contract passed an
// Entitler whose cache was never invalidated, "an upgrade is visible at once" would be a test of nothing.

// cachedWorld is the in-memory store beneath the real Entitler with a 24-hour cache, a bus, and a Watch; Announce does what the
// payment layer does after a change, which is to publish on <prefix>.entitlement.<tag>. With announce false the announcements
// are lost.
func cachedWorld(announce bool) contracts.WorldFactory {
	return func(t testing.TB) *contracts.World {
		store := contracts.NewMemStore()
		faults := contracts.NewBreakable(store)
		clock := contracts.NewFakeClock(contracts.T0)
		e, err := app.New(app.Deps{Store: faults, Clock: clock, CacheTTL: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		bus := &fakeBus{}
		w, err := e.Watch(context.Background(), bus, prefix, "entitlement-cache-contract")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		world := &contracts.World{Entitler: e, Store: store, Faults: faults, Seed: store, Clock: clock, Scope: scope}
		if announce {
			world.Announce = func(reason string) {
				body, _ := json.Marshal(map[string]any{"realm": string(scope.Realm), "scope": map[string]string{"kind": scope.Kind, "id": scope.ID}, "reason": reason})
				if err := bus.Publish(context.Background(), prefix+".entitlement."+scope.SubjectToken(), body); err != nil {
					t.Fatal(err)
				}
			}
		}
		return world
	}
}

func TestEntitlerHonoursTheEntitlerContractWithACacheAndItsAnnouncements(t *testing.T) {
	t.Parallel()
	contracts.EntitlerContract(t, cachedWorld(true))
}

// blind caches the answer it first gave a key until it is told something changed, and knows no clock: an entry that does not
// know a grant or an override ends.
type blind struct {
	ports.Entitler
	mu   sync.Mutex
	seen map[domain.Key]bool
}

func (b *blind) Allowed(ctx context.Context, s md.Scope, k domain.Key) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.seen[k]; ok {
		return v, nil
	}
	v, err := b.Entitler.Allowed(ctx, s, k)
	if err == nil {
		b.seen[k] = v
	}
	return v, err
}

func (b *blind) clear() { b.mu.Lock(); b.seen = map[domain.Key]bool{}; b.mu.Unlock() }

func TestEntitlerContractRejectsABrokenCacheByName(t *testing.T) {
	t.Parallel()
	blindWorld := func(t testing.TB) *contracts.World {
		w := cachedWorld(true)(t)
		b := &blind{Entitler: w.Entitler, seen: map[domain.Key]bool{}}
		announce := w.Announce
		w.Entitler, w.Announce = b, func(reason string) { b.clear(); announce(reason) }
		return w
	}
	broken := []struct {
		name     string
		world    contracts.WorldFactory
		caughtBy []string
	}{
		{"a cache whose announcements are lost", cachedWorld(false), []string{contracts.CaseUpgradeAtOnce, contracts.CaseOverrideBeats}},
		{"a cache that does not know when an override ends", blindWorld, []string{contracts.CaseExpiry}},
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
				found := false
				for _, r := range rejected {
					found = found || r == want
				}
				if !found {
					t.Errorf("the contract PASSED %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}
