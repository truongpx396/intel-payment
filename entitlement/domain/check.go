package domain

import "fmt"

// MaxChecks bounds a batch: a page render asks about a dozen capabilities, not a thousand, and an unbounded batch is an unbounded
// amount of work for one request.
const MaxChecks = 100

const maxKeyLen = 128

// CheckRequest is one question of a batch: may this scope have this capability — and, for a quota, may it have one more (or Delta
// more) given that it has Current now. Current is the host's own count (invariant 4: the host owns its counts); with none, a quota
// is asked only whether it is granted, and how large.
type CheckRequest struct {
	Key     Key
	Current *int64
	Delta   *int64 // meaningful only with Current; one more when absent
}

// Validate reports whether the question is well formed. A key the realm never declared is NOT malformed — it is denied, because a
// typo must fail closed and not fail the page.
func (r CheckRequest) Validate() error {
	switch {
	case r.Key == "":
		return fmt.Errorf("%w: a check names a key", ErrInvalid)
	case len(r.Key) > maxKeyLen:
		return fmt.Errorf("%w: a key is at most %d bytes", ErrInvalid, maxKeyLen)
	case r.Current != nil && *r.Current < 0:
		return fmt.Errorf("%w: counts are not negative (current %d)", ErrInvalid, *r.Current)
	case r.Delta != nil && r.Current == nil:
		return fmt.Errorf("%w: a delta is measured against a current count, and %q gave none", ErrInvalid, string(r.Key))
	case r.Delta != nil && *r.Delta < 0:
		return fmt.Errorf("%w: counts are not negative (delta %d)", ErrInvalid, *r.Delta)
	}
	return nil
}

// CheckResult is the answer to one question: what the scope may do, what it holds, and — when it may not — why, in words a person
// can act on, and the plan that would permit it.
type CheckResult struct {
	Key Key
	// Kind is the key's kind; empty for a key the realm never declared.
	Kind    Kind
	Allowed bool
	// Value is what the scope holds for the key, unset when nothing grants it.
	Value Value
	// Limit, Current and Remaining are the quota measured against a count; nil when it was not measured. Unlimited is -1 in each.
	Limit, Current, Remaining *int64
	Reason, UpgradePath       string
}
