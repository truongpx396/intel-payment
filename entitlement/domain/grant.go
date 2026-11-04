package domain

import (
	"fmt"
	"strings"
	"time"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Entitlement is a key and its value: what a plan attaches, before anyone holds it.
type Entitlement struct {
	Key   Key
	Value Value
}

// Source records WHERE a grant came from, because the answer to "why can this customer use SSO?" is an operational question
// asked under time pressure.
type Source string

// The three levels, and the strings Resolve reports.
const (
	FromDefault      Source = "default"      // the realm's free tier
	FromSubscription Source = "subscription" // the active paid plan
	FromOverride     Source = "override"     // a deliberate exception — always audited
)

// Valid reports whether s is one of the three.
func (s Source) Valid() bool { return s.precedence() > 0 }

func (s Source) precedence() int {
	switch s {
	case FromOverride:
		return 3
	case FromSubscription:
		return 2
	case FromDefault:
		return 1
	}
	return 0
}

// Beats reports whether s outranks o. The precedence is exactly one order, stated once: override > subscription > default.
// A source does not beat itself — two grants at one level are merged, not ranked — and an unknown source takes no part.
func (s Source) Beats(o Source) bool {
	return s.Valid() && o.Valid() && s.precedence() > o.precedence()
}

// Grant is one resolved entitlement with its provenance and lifetime.
type Grant struct {
	Scope     md.Scope
	Key       Key
	Value     Value
	Source    Source
	ExpiresAt *time.Time // nil = as long as its source lives
	Reason    string     // required for FromOverride: who approved it and why
}

// Validate reports whether g can be held: a good scope, key, value and source, and for an override a reason that says
// something (invariant 5).
func (g Grant) Validate() error {
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if err := g.Key.Validate(); err != nil {
		return err
	}
	if err := g.Value.Validate(); err != nil {
		return fmt.Errorf("grant %q: %w", string(g.Key), err)
	}
	if !g.Source.Valid() {
		return fmt.Errorf("%w: grant %q has no valid source (%q)", ErrInvalid, string(g.Key), string(g.Source))
	}
	if g.Source == FromOverride && strings.TrimSpace(g.Reason) == "" {
		return ErrReasonRequired
	}
	return nil
}

// ActiveAt reports whether g holds at now. The instant a grant expires it is already over: a comp that "ends at noon" ends at
// noon.
func (g Grant) ActiveAt(now time.Time) bool {
	return g.ExpiresAt == nil || now.Before(*g.ExpiresAt)
}

// ValidateOverride reports whether g may be written to the overrides table by actor (invariant 5): a valid grant that is an
// override, with a reason that says something, written by a named actor. An unexplained override is indistinguishable from a bug,
// and is usually discovered during a revenue investigation.
func (g Grant) ValidateOverride(actor string) error {
	if err := g.Validate(); err != nil {
		return err
	}
	if g.Source != FromOverride {
		return fmt.Errorf("%w: only an override is written here, this grant is from %q", ErrInvalid, string(g.Source))
	}
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: an override says who wrote it", ErrInvalid)
	}
	return nil
}
