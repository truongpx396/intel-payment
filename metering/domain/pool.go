package domain

import (
	"fmt"
	"regexp"
	"sort"
)

// Pool names a credit pool within a scope. GeneralPool always exists, applies to every resource
// and is drawn last.
type Pool string

// GeneralPool is implicit in every realm.
const GeneralPool Pool = "general"

var poolPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Or returns p, or GeneralPool when p is empty.
func (p Pool) Or() Pool {
	if p == "" {
		return GeneralPool
	}
	return p
}

// Validate reports whether p is a usable pool name. The empty pool means GeneralPool.
func (p Pool) Validate() error {
	if p == "" {
		return nil
	}
	if !poolPattern.MatchString(string(p)) {
		return fmt.Errorf("%w: pool %q must match %s", ErrInvalid, string(p), poolPattern)
	}
	return nil
}

// PoolDef is one realm-configured pool (credit_pools). Lower Priority is drawn first.
type PoolDef struct {
	Name      Pool
	Priority  int
	AppliesTo []string // resources this pool may pay for; empty = every resource
}

// Covers reports whether the pool may pay for resource. GeneralPool covers everything.
func (d PoolDef) Covers(resource string) bool {
	if d.Name == GeneralPool || len(d.AppliesTo) == 0 {
		return true
	}
	for _, r := range d.AppliesTo {
		if r == resource {
			return true
		}
	}
	return false
}

// EligiblePools returns the pools that may pay for resource, in draw order: configured pools by
// ascending priority (ties broken by name, so the order is total and deterministic), then
// GeneralPool, which is always last and absorbs any overshoot. A realm that defines no pools has
// exactly [general].
func EligiblePools(defs []PoolDef, resource string) []Pool {
	eligible := make([]PoolDef, 0, len(defs))
	for _, d := range defs {
		if d.Name != GeneralPool && d.Covers(resource) {
			eligible = append(eligible, d)
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Priority != eligible[j].Priority {
			return eligible[i].Priority < eligible[j].Priority
		}
		return eligible[i].Name < eligible[j].Name
	})
	out := make([]Pool, 0, len(eligible)+1)
	for _, d := range eligible {
		out = append(out, d.Name)
	}
	return append(out, GeneralPool)
}
