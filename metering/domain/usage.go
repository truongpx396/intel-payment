package domain

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// The usage_daily rollup (FR-039) is derived data: a day's spend, per scope, per dimension (the resource that was metered) and per
// member, summed from booked ledger rows. It backs a burn rate, a spend series and a breakdown, and it is never a billing input — the
// ledger is the record, and the rollup is recomputed from it rather than added to.

// LedgerUse is the part of a booked usage row the rollup reads.
type LedgerUse struct {
	Scope Scope
	// At is when the spend happened (the event's own time, else the instant it was booked). It decides the day.
	At time.Time
	// Dimension is what was metered: the ledger row's resource. Empty when the row names none.
	Dimension string
	// Actor is the member whose spend it was. Empty when unattributed.
	Actor string
	// Credits is the spend, positive: what the row consumed.
	Credits    Credits
	CostMicros int64
	Events     int64
}

// UsageKey is one row of the rollup.
type UsageKey struct {
	Scope     Scope
	Day       string // the calendar date in the rollup's zone, YYYY-MM-DD
	Dimension string
	Actor     string
}

// UsageTotals are a day's sums.
type UsageTotals struct {
	Credits    Credits
	CostMicros int64
	Events     int64
}

// ErrUsageOverflow is a sum no int64 can hold. Arithmetic that could wrap is refused, never wrapped: a rollup that silently wrapped would
// show a customer a burn rate that is not theirs.
var ErrUsageOverflow = errors.New("usage rollup: a sum overflows int64")

// DayOf is the calendar date of an instant in loc. It is the date on a wall clock there, which is what a person means by "that day":
// a zone that skipped a date (Pacific/Apia on 30 December 2011) has no such day, and a day of 23 or 25 hours is still one day. Building
// days by adding 24 hours to a midnight is how an event near such a boundary is lost or counted twice, so no day is ever computed that way.
func DayOf(at time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return at.In(loc).Format(time.DateOnly)
}

func add(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrUsageOverflow
	}
	return a + b, nil
}

// Rollup sums uses into days. It is a pure function of the set of uses: the order they come in does not matter, and the rollup of two
// sets added together is the rollup of the two sets joined — which is what lets the store recompute a window of days from the ledger and
// replace them, instead of adding to what is already there.
func Rollup(uses []LedgerUse, loc *time.Location) (map[UsageKey]UsageTotals, error) {
	out := map[UsageKey]UsageTotals{}
	for _, u := range uses {
		k := UsageKey{Scope: u.Scope.Normalized(), Day: DayOf(u.At, loc), Dimension: u.Dimension, Actor: u.Actor}
		t := out[k]
		var err error
		c, err := add(int64(t.Credits), int64(u.Credits))
		if err != nil {
			return nil, fmt.Errorf("%w (credits of %s on %s)", err, k.Scope.Tag(), k.Day)
		}
		cost, err := add(t.CostMicros, u.CostMicros)
		if err != nil {
			return nil, fmt.Errorf("%w (cost of %s on %s)", err, k.Scope.Tag(), k.Day)
		}
		ev, err := add(t.Events, u.Events)
		if err != nil {
			return nil, fmt.Errorf("%w (events of %s on %s)", err, k.Scope.Tag(), k.Day)
		}
		out[k] = UsageTotals{Credits: Credits(c), CostMicros: cost, Events: ev}
	}
	return out, nil
}

// Totals is the sum of a rollup: what the days add up to.
func Totals(r map[UsageKey]UsageTotals) (UsageTotals, error) {
	var t UsageTotals
	for _, v := range r {
		c, err := add(int64(t.Credits), int64(v.Credits))
		if err != nil {
			return UsageTotals{}, err
		}
		cost, err := add(t.CostMicros, v.CostMicros)
		if err != nil {
			return UsageTotals{}, err
		}
		ev, err := add(t.Events, v.Events)
		if err != nil {
			return UsageTotals{}, err
		}
		t = UsageTotals{Credits: Credits(c), CostMicros: cost, Events: ev}
	}
	return t, nil
}

// BreakdownQuery asks for a scope's spend by dimension over a range of days. A breakdown is one member's own, or — only when the caller
// says so by name — everyone's: the zero query is neither, and is refused, so no caller reaches the aggregate by leaving a field out. A
// member must not learn colleagues' usage from a credits screen (D20).
type BreakdownQuery struct {
	Scope Scope
	// From and To are dates (YYYY-MM-DD), inclusive.
	From, To string
	// Member is the actor whose rows are returned, and only theirs.
	Member string
	// AllMembers returns every member's rows, summed. It is for the caller that holds the administrative entitlement.
	AllMembers bool
}

// Validate refuses a query that is not exactly one of a member's or everyone's.
func (q BreakdownQuery) Validate() error {
	if err := q.Scope.Validate(); err != nil {
		return err
	}
	for _, d := range []string{q.From, q.To} {
		if _, err := time.Parse(time.DateOnly, d); err != nil {
			return fmt.Errorf("%w: %q is not a date (YYYY-MM-DD)", ErrInvalid, d)
		}
	}
	if q.To < q.From {
		return fmt.Errorf("%w: the range ends (%s) before it starts (%s)", ErrInvalid, q.To, q.From)
	}
	switch {
	case q.AllMembers && q.Member != "":
		return fmt.Errorf("%w: a breakdown is one member's or everyone's, not both", ErrInvalid)
	case !q.AllMembers && q.Member == "":
		return fmt.Errorf("%w: a breakdown names its member, or says it is for everyone", ErrInvalid)
	}
	return nil
}

// BreakdownRow is one member of a spend dimension over a range.
type BreakdownRow struct {
	Dimension  string
	Credits    Credits
	CostMicros int64
	Events     int64
}

// Breakdown reads a rollup the way the store reads usage_daily: the rows of one scope, between two dates, of one member or of everyone,
// grouped by dimension, the largest spend first (ties by name, so the order is stable). It is the reference the store is held to.
func Breakdown(rollup map[UsageKey]UsageTotals, q BreakdownQuery) ([]BreakdownRow, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	scope := q.Scope.Normalized()
	by := map[string]*BreakdownRow{}
	for k, v := range rollup {
		if k.Scope != scope || k.Day < q.From || k.Day > q.To || (!q.AllMembers && k.Actor != q.Member) {
			continue
		}
		r := by[k.Dimension]
		if r == nil {
			r = &BreakdownRow{Dimension: k.Dimension}
			by[k.Dimension] = r
		}
		c, err := add(int64(r.Credits), int64(v.Credits))
		if err != nil {
			return nil, err
		}
		cost, err := add(r.CostMicros, v.CostMicros)
		if err != nil {
			return nil, err
		}
		ev, err := add(r.Events, v.Events)
		if err != nil {
			return nil, err
		}
		r.Credits, r.CostMicros, r.Events = Credits(c), cost, ev
	}
	out := make([]BreakdownRow, 0, len(by))
	for _, r := range by {
		out = append(out, *r)
	}
	sortBreakdown(out)
	return out, nil
}

func sortBreakdown(rows []BreakdownRow) {
	slices.SortFunc(rows, func(a, b BreakdownRow) int {
		if c := cmp.Compare(b.Credits, a.Credits); c != 0 {
			return c // the largest spend first
		}
		return strings.Compare(a.Dimension, b.Dimension) // ties by name, so the order is stable
	})
}
