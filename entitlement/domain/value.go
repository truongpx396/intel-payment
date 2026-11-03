package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is what a capability's value is.
type Kind string

// The three kinds. The strings are the schema's CHECK constraints and the wire's.
const (
	Flag  Kind = "flag"  // on/off:        "sso", "audit_export", "custom_domain"
	Quota Kind = "quota" // a ceiling:     "seats", "projects", "webhooks"
	Enum  Kind = "enum"  // one of a set:  "support_tier" = bronze|gold, "region" = eu|us
)

// Valid reports whether k is one of the three.
func (k Kind) Valid() bool { return k == Flag || k == Quota || k == Enum }

// Unlimited is the quota that has no ceiling. It is -1 rather than a nil pointer so that "unlimited" cannot be confused with
// "unset" — the two behave differently and often look alike. It is never compared as a number: it is the LARGEST quota, and
// the one comparison that gets it wrong is the obvious one.
const Unlimited int64 = -1

// Value is a closed union. Deliberately not `any`: an untyped value means every call site asserts a type, and the one that
// asserts wrong fails at runtime in a billing check. Only the field of its Kind may be set; the zero Value is UNSET.
type Value struct {
	Kind Kind
	Bool bool
	Int  int64 // -1 means UNLIMITED
	Text string
}

// FlagValue is a flag.
func FlagValue(on bool) Value { return Value{Kind: Flag, Bool: on} }

// QuotaValue is a ceiling of n. -1 is unlimited; anything below it is not a quota and fails Validate.
func QuotaValue(n int64) Value { return Value{Kind: Quota, Int: n} }

// UnlimitedQuota is the quota with no ceiling.
func UnlimitedQuota() Value { return QuotaValue(Unlimited) }

// EnumValue is one member of an enum.
func EnumValue(member string) Value { return Value{Kind: Enum, Text: member} }

// IsSet reports whether v holds anything. A ceiling of zero is set; it is a ceiling.
func (v Value) IsSet() bool { return v.Kind != "" }

// IsUnlimited reports whether v is the quota with no ceiling. Only a quota can be.
func (v Value) IsUnlimited() bool { return v.Kind == Quota && v.Int == Unlimited }

// Validate reports whether v is a legal grant value: a known kind, with only that kind's field set, and a quota of at least
// -1. The unset value is not a grant.
func (v Value) Validate() error {
	switch v.Kind {
	case Flag:
		if v.Int != 0 || v.Text != "" {
			return fmt.Errorf("%w: a flag carries only a boolean", ErrInvalid)
		}
	case Quota:
		if v.Bool || v.Text != "" {
			return fmt.Errorf("%w: a quota carries only a number", ErrInvalid)
		}
		if v.Int < Unlimited {
			return fmt.Errorf("%w: a quota is at least -1 (unlimited), got %d", ErrInvalid, v.Int)
		}
	case Enum:
		if v.Bool || v.Int != 0 {
			return fmt.Errorf("%w: an enum carries only a member", ErrInvalid)
		}
		if strings.TrimSpace(v.Text) == "" {
			return fmt.Errorf("%w: an enum value names a member", ErrInvalid)
		}
	case "":
		return fmt.Errorf("%w: a grant has a value: this one is unset", ErrInvalid)
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, string(v.Kind))
	}
	return nil
}

// String is what a person reads. Unlimited reads as "unlimited", never as a number.
func (v Value) String() string {
	switch v.Kind {
	case Flag:
		return strconv.FormatBool(v.Bool)
	case Quota:
		if v.IsUnlimited() {
			return "unlimited"
		}
		return strconv.FormatInt(v.Int, 10)
	case Enum:
		return v.Text
	case "":
		return "unset"
	}
	return "invalid"
}

// Decision is what a quota check answers. Reason and UpgradePath are the caller's to fill: the value knows the arithmetic,
// not the plans.
type Decision struct {
	Allowed bool
	Limit   int64 // -1 = unlimited
	Current int64
	// Remaining is the room left once the requested delta is taken; a refusal leaves the room it found, never below zero.
	// -1 when unlimited.
	Remaining int64
	// Reason is set when refused, and is written for a HUMAN: "Team plan includes 25 seats; you have 25." An entitlement
	// refusal the user cannot act on is an outage to them.
	Reason string
	// UpgradePath is the plan code that would permit it, when one exists. It is what makes a refusal a sales moment rather
	// than a dead end.
	UpgradePath string
}

// Admits measures `current + delta` against a quota the host supplies the count for (invariant 4: the host owns its counts).
// Unlimited admits anything, whatever the counts. Otherwise the sum must fit WITHOUT wrapping: it is decided as
// `delta <= limit - current`, which cannot overflow, so a sum no int64 can hold is refused rather than wrapped. A negative
// count, or a value that is not a quota, is the caller's mistake.
func (v Value) Admits(current, delta int64) (Decision, error) {
	if v.Kind != Quota {
		return Decision{}, fmt.Errorf("%w: only a quota is measured against a count, this is %q", ErrInvalid, string(v.Kind))
	}
	if err := v.Validate(); err != nil {
		return Decision{}, err
	}
	if current < 0 || delta < 0 {
		return Decision{}, fmt.Errorf("%w: counts are not negative (current %d, delta %d)", ErrInvalid, current, delta)
	}
	if v.IsUnlimited() {
		return Decision{Allowed: true, Limit: Unlimited, Current: current, Remaining: Unlimited}, nil
	}
	d := Decision{Limit: v.Int, Current: current}
	if current > v.Int { // already over: the plan shrank under a count
		return d, nil
	}
	room := v.Int - current // both are >= 0 and current <= limit: no overflow
	if delta <= room {
		d.Allowed, d.Remaining = true, room-delta
		return d, nil
	}
	d.Remaining = room
	return d, nil
}
