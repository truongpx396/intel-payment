package envcfg

import (
	"fmt"
	"regexp"
	"time"
)

// Entitlements is what the entitlement layer is told. Everything else about it — keys, plans, free tier, overrides — is data
// behind the admin API, not configuration (constitution VIII).
type Entitlements struct {
	// CacheTTL bounds how long a resolution is kept. It is what a lost announcement costs, so it is bounded: past ten minutes a
	// customer who just paid could wait that long behind a bus message that never arrived. Zero turns the cache off.
	CacheTTL time.Duration
	// InstanceID names this replica's subscription to the announcements that evict its cache. Empty means the hostname.
	InstanceID string
}

const maxEntitlementCacheTTL = 10 * time.Minute

// An instance id becomes part of a bus consumer-group name, so it is a short token and not anything a subject or a stream key
// reads specially.
var instancePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Entitlements reads the entitlement layer's settings. An unusable value is reported, never replaced by a default nobody chose.
func (r *Reader) Entitlements() Entitlements {
	e := Entitlements{CacheTTL: 30 * time.Second}
	before := len(r.errs)
	r.Duration("PAYMENT_ENTITLEMENT_CACHE_TTL", &e.CacheTTL)
	if len(r.errs) == before && e.CacheTTL > maxEntitlementCacheTTL {
		r.Fail("PAYMENT_ENTITLEMENT_CACHE_TTL", fmt.Sprintf("must be at most %s: it is how long a lost announcement is believed, and a customer who just paid would wait that long", maxEntitlementCacheTTL))
	}
	if v := r.Str("PAYMENT_INSTANCE_ID"); v != "" {
		if !instancePattern.MatchString(v) {
			r.Fail("PAYMENT_INSTANCE_ID", fmt.Sprintf("must be 1-63 letters, digits, '.', '_' or '-', starting with a letter or digit, got %q", v))
		} else {
			e.InstanceID = v
		}
	}
	return e
}
