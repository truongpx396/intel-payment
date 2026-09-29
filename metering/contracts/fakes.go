package contracts

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Static, in-memory stores of configuration data, and a controllable clock. They are what an
// adapter's tests hand to the code under test, so a suite never needs Postgres just to say which
// pools or limits exist.

// StaticPools serves a fixed set of pools to every realm.
type StaticPools []domain.PoolDef

func (p StaticPools) Pools(context.Context, domain.Realm) ([]domain.PoolDef, error) { return p, nil }

// StaticLimits serves a fixed set of limits to every realm.
type StaticLimits []domain.Limit

func (l StaticLimits) Limits(context.Context, domain.Realm) ([]domain.Limit, error) { return l, nil }

// StaticCards serves rate cards by realm. The first card of a realm is its active one.
type StaticCards map[domain.Realm][]domain.RateCard

func (c StaticCards) Active(_ context.Context, r domain.Realm) (domain.RateCard, error) {
	if cards := c[r.Or()]; len(cards) > 0 {
		return cards[0], nil
	}
	return domain.RateCard{}, fmt.Errorf("%w: realm %q has no active rate card", domain.ErrUnpriceable, r)
}

func (c StaticCards) Get(_ context.Context, r domain.Realm, version string) (domain.RateCard, error) {
	for _, card := range c[r.Or()] {
		if card.Version == version {
			return card, nil
		}
	}
	return domain.RateCard{}, fmt.Errorf("%w: no card %q in realm %q", domain.ErrUnpriceable, version, r)
}

// FakeClock is a Clock that moves only when told to. Window counters, staleness checks and TTL
// arithmetic all take their "now" from it, so a test can cross midnight without waiting for it.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

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

var (
	_ ports.PoolStore     = StaticPools(nil)
	_ ports.LimitStore    = StaticLimits(nil)
	_ ports.RateCardStore = StaticCards(nil)
	_ ports.Clock         = (*FakeClock)(nil)
)
