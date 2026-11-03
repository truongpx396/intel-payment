package app

import (
	"sync"
	"time"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// cache holds one resolution per scope. It is bounded in number, each entry is bounded in time, and every eviction advances an
// epoch so that a load that began before it cannot store what it read.
type cache struct {
	mu      sync.Mutex
	max     int
	entries map[string]cacheEntry
	realms  map[md.Realm]map[string]struct{}
	epoch   uint64
}

type cacheEntry struct {
	res   *resolution
	until time.Time
	realm md.Realm
}

func newCache(capacity int) *cache {
	return &cache{max: capacity, entries: map[string]cacheEntry{}, realms: map[md.Realm]map[string]struct{}{}}
}

// get is the entry for a scope's tag if it is still valid at now: the instant it expires it is already gone.
func (c *cache) get(tag string, now time.Time) *resolution {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[tag]
	if !ok {
		return nil
	}
	if !now.Before(e.until) {
		c.deleteLocked(tag)
		return nil
	}
	return e.res
}

// generation is the epoch a load records before it reads the store.
func (c *cache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// put keeps res until the given instant, unless an eviction has happened since the load began (gen): what such a load read may
// be what the eviction was announced to remove. It reports whether res was kept.
func (c *cache) put(tag string, realm md.Realm, res *resolution, until time.Time, gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.epoch {
		return false
	}
	if _, present := c.entries[tag]; !present && len(c.entries) >= c.max {
		for victim := range c.entries { // any one: map order is random, and an eviction costs one read, never a wrong answer
			c.deleteLocked(victim)
			break
		}
	}
	c.entries[tag] = cacheEntry{res: res, until: until, realm: realm}
	if c.realms[realm] == nil {
		c.realms[realm] = map[string]struct{}{}
	}
	c.realms[realm][tag] = struct{}{}
	return true
}

// evict removes one scope.
func (c *cache) evict(tag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.deleteLocked(tag)
}

// evictRealm removes every scope of a realm.
func (c *cache) evictRealm(realm md.Realm) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	for tag := range c.realms[realm] {
		delete(c.entries, tag)
	}
	delete(c.realms, realm)
}

// flush removes everything.
func (c *cache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	c.entries = map[string]cacheEntry{}
	c.realms = map[md.Realm]map[string]struct{}{}
}

func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *cache) deleteLocked(tag string) {
	e, ok := c.entries[tag]
	if !ok {
		return
	}
	delete(c.entries, tag)
	if set := c.realms[e.realm]; set != nil {
		delete(set, tag)
		if len(set) == 0 {
			delete(c.realms, e.realm)
		}
	}
}
