package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// A spend series and a burn rate (FR-039): what a credits screen's chart and its "runs out in about N days" read from the usage_daily
// rollup. Both are derived data and never a billing input; they are as stale as the rollup (a few minutes), and say nothing the ledger
// does not.

// MaxSeriesDays is the longest spend series: the ceiling of the credits endpoint's ?days=.
const MaxSeriesDays = 366

// DailyRow is one day of a scope's spend.
type DailyRow struct {
	Day     string // YYYY-MM-DD: the rollup's own day
	Credits Credits
}

// DailySpend reads a rollup the way the store reads usage_daily for a series: the days of one scope, between two dates, of one member or of
// everyone, oldest first, one row for each day that has a rollup row. Like a breakdown it is one member's or — only when the query says so by
// name — everyone's (D20), and for any query the days add up to the breakdown's dimensions, because both read the same rows. It is the
// reference the store is held to.
func DailySpend(rollup map[UsageKey]UsageTotals, q BreakdownQuery) ([]DailyRow, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	scope := q.Scope.Normalized()
	by := map[string]int64{}
	for k, v := range rollup {
		if k.Scope != scope || k.Day < q.From || k.Day > q.To || (!q.AllMembers && k.Actor != q.Member) {
			continue
		}
		// a day is the sum of its dimensions: each is within int64, their sum may not be, and a wrapped day is a chart that lies
		c, err := add(by[k.Day], int64(v.Credits))
		if err != nil {
			return nil, fmt.Errorf("%w (credits of %s on %s)", err, scope.Tag(), k.Day)
		}
		by[k.Day] = c
	}
	out := make([]DailyRow, 0, len(by))
	for d, c := range by {
		out = append(out, DailyRow{Day: d, Credits: Credits(c)})
	}
	slices.SortFunc(out, func(a, b DailyRow) int { return strings.Compare(a.Day, b.Day) })
	return out, nil
}

// FillDays makes a series one point per day from `from` through `to`, both inclusive: a day with no spend is a zero, not a gap, so a chart
// draws the quiet days as quiet. Every row must lie inside the window and no day may repeat; a row that does not fit is refused rather than
// dropped, because a series that quietly adds up to less than the rollup holds is a screen whose figures do not close.
func FillDays(rows []DailyRow, from, to string) ([]DailyRow, error) {
	f, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a date (YYYY-MM-DD)", ErrInvalid, from)
	}
	e, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not a date (YYYY-MM-DD)", ErrInvalid, to)
	}
	if e.Before(f) {
		return nil, fmt.Errorf("%w: the window ends (%s) before it starts (%s)", ErrInvalid, to, from)
	}
	// both are midnights in UTC, so the difference is whole days: there is no zone here for a day to be 23 or 25 hours long in
	if int(e.Sub(f).Hours()/24)+1 > MaxSeriesDays {
		return nil, fmt.Errorf("%w: a series is at most %d days", ErrInvalid, MaxSeriesDays)
	}
	by := make(map[string]Credits, len(rows))
	for _, r := range rows {
		d, err := time.Parse(time.DateOnly, r.Day)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a date (YYYY-MM-DD)", ErrInvalid, r.Day)
		}
		if d.Before(f) || d.After(e) {
			return nil, fmt.Errorf("%w: %s is outside the window %s to %s", ErrInvalid, r.Day, from, to)
		}
		if _, dup := by[r.Day]; dup {
			return nil, fmt.Errorf("%w: %s appears twice", ErrInvalid, r.Day)
		}
		by[r.Day] = r.Credits
	}
	out := make([]DailyRow, 0, int(e.Sub(f).Hours()/24)+1)
	for d := f; !d.After(e); d = d.AddDate(0, 0, 1) {
		day := d.Format(time.DateOnly)
		out = append(out, DailyRow{Day: day, Credits: by[day]})
	}
	return out, nil
}

// BurnRate is how much a scope spends in a day, as the figure "runs out in about N days" divides the balance by: the smallest whole number
// that, held for every day from the first day with spend through the last day of the series, covers what was spent. series is one point per
// day, as FillDays makes it.
//
// Two choices decide what it says. The days are counted from the first day with spend, not from the start of the window, so an account that
// began spending yesterday is not averaged over a fortnight it did not exist in — that would say a balance lasts fourteen times longer than it
// will. And the quotient is rounded up, so the runway is never overstated: a person told "about 6 days" has at least 6.
//
// No spend is no burn (0), and so is a series whose spend is outweighed by corrections; a burn rate is never negative. A total no int64 can
// hold is refused (ErrUsageOverflow), never wrapped.
func BurnRate(series []DailyRow) (Credits, error) {
	first := -1
	var total int64
	for i, r := range series {
		if r.Credits > 0 && first < 0 {
			first = i
		}
		var err error
		if total, err = add(total, int64(r.Credits)); err != nil {
			return 0, err
		}
	}
	if first < 0 || total <= 0 {
		return 0, nil
	}
	days := int64(len(series) - first)
	q := total / days
	if total%days != 0 {
		q++ // days >= 2 here, so q <= MaxInt64/2: it cannot wrap
	}
	return Credits(q), nil
}
