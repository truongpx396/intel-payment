package domain

import (
	"fmt"
	"regexp"
)

// Realm is the top-level isolation axis: one HOST PRODUCT drawing on this deployment. Plans, rate
// cards, limits, pools, provider accounts, ledger rows and idempotency keys are all
// realm-partitioned. A single-product deployment leaves it at DefaultRealm.
type Realm string

// DefaultRealm is used when a Scope or Config leaves the realm empty.
const DefaultRealm Realm = "default"

var realmPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Validate reports whether r is a usable realm slug. The empty realm is valid: it means
// DefaultRealm. A realm is inside every key, subject and tag, so it must be unambiguous there.
func (r Realm) Validate() error {
	if r == "" {
		return nil
	}
	if !realmPattern.MatchString(string(r)) {
		return fmt.Errorf("%w: realm %q must match %s", ErrInvalid, string(r), realmPattern)
	}
	return nil
}

// Or returns r, or DefaultRealm when r is empty.
func (r Realm) Or() Realm {
	if r == "" {
		return DefaultRealm
	}
	return r
}
