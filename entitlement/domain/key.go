package domain

import (
	"fmt"
	"regexp"
	"strings"
)

// Key is a host-defined capability name. The engine ships NONE as special — it has no opinion about what "sso" means, only
// about how a value attached to it is resolved.
type Key string

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Validate reports whether k is a usable key: a short lower-case word. A key sits inside cache entries, URLs and log lines.
func (k Key) Validate() error {
	if !keyPattern.MatchString(string(k)) {
		return fmt.Errorf("%w: key %q must match %s", ErrInvalid, string(k), keyPattern)
	}
	return nil
}

// KeyDecl declares a capability to a realm: its kind and, for an enum, its ranking from lowest to highest. A key a realm
// never declared is DENIED (invariant 1), so a typo fails closed.
type KeyDecl struct {
	Key       Key
	Kind      Kind
	EnumOrder []string // lowest to highest; enums only
}

// Validate reports whether d is a coherent declaration: a good key, a known kind, and an order exactly when it is an enum
// (the schema's CHECK says the same), with no duplicate or blank member.
func (d KeyDecl) Validate() error {
	if err := d.Key.Validate(); err != nil {
		return err
	}
	if !d.Kind.Valid() {
		return fmt.Errorf("%w: key %q has unknown kind %q", ErrInvalid, string(d.Key), string(d.Kind))
	}
	if d.Kind != Enum {
		if len(d.EnumOrder) != 0 {
			return fmt.Errorf("%w: key %q is a %s and has no enum order", ErrInvalid, string(d.Key), string(d.Kind))
		}
		return nil
	}
	if len(d.EnumOrder) == 0 {
		return fmt.Errorf("%w: enum key %q needs its members, lowest to highest", ErrInvalid, string(d.Key))
	}
	seen := make(map[string]bool, len(d.EnumOrder))
	for _, m := range d.EnumOrder {
		if strings.TrimSpace(m) == "" || seen[m] {
			return fmt.Errorf("%w: enum key %q has a blank or repeated member %q", ErrInvalid, string(d.Key), m)
		}
		seen[m] = true
	}
	return nil
}

// Rank is a member's position in the declared order, lowest first. A member the key never declared has none: ok is false,
// and the absence is not a position.
func (d KeyDecl) Rank(member string) (rank int, ok bool) {
	for i, m := range d.EnumOrder {
		if m == member {
			return i, true
		}
	}
	return 0, false
}

// Check reports whether v is a value this declaration can hold: set, valid, of its kind, and for an enum a ranked member.
func (d KeyDecl) Check(v Value) error {
	if err := v.Validate(); err != nil {
		return fmt.Errorf("key %q: %w", string(d.Key), err)
	}
	if v.Kind != d.Kind {
		return fmt.Errorf("%w: key %q is a %s, not a %s", ErrInvalid, string(d.Key), string(d.Kind), string(v.Kind))
	}
	if d.Kind == Enum {
		if _, ok := d.Rank(v.Text); !ok {
			return fmt.Errorf("%w: %q is not a declared member of key %q", ErrInvalid, v.Text, string(d.Key))
		}
	}
	return nil
}

// Merge joins two values for one key at ONE level (override, subscription or default). "Two grants for one key" is not
// hypothetical — it is what a migration between plans looks like for as long as it runs. A flag ORs (any grant enabling it
// wins); a quota takes the maximum, and unlimited is above every number; an enum takes the highest-ranked member of the
// DECLARED order, not the alphabet's. An unset side contributes nothing: it is not zero and it is not unlimited. The join is
// commutative, associative and idempotent, so the order two grants arrive in decides nothing.
func (d KeyDecl) Merge(a, b Value) (Value, error) {
	switch {
	case !a.IsSet() && !b.IsSet():
		return Value{}, nil
	case !a.IsSet():
		return b, d.Check(b)
	case !b.IsSet():
		return a, d.Check(a)
	}
	if err := d.Check(a); err != nil {
		return Value{}, err
	}
	if err := d.Check(b); err != nil {
		return Value{}, err
	}
	switch d.Kind {
	case Flag:
		return FlagValue(a.Bool || b.Bool), nil
	case Quota:
		if a.IsUnlimited() || b.IsUnlimited() {
			return UnlimitedQuota(), nil
		}
		return QuotaValue(max(a.Int, b.Int)), nil
	default: // Enum: Check proved both are ranked
		ra, _ := d.Rank(a.Text)
		rb, _ := d.Rank(b.Text)
		if rb > ra {
			return b, nil
		}
		return a, nil
	}
}
