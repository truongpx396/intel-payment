package domain_test

import (
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// FR-039, the part a credits screen reads: a spend series (the chart), and a burn rate (the figure behind "runs out in about N days"), both
// derived from the usage_daily rollup and neither ever a billing input. Three promises, each held to an independent reference:
//
//   - the series reconciles with the breakdown: the days add up to the dimensions, for the same query, or the screen would show a total
//     beside parts that do not close;
//   - a member's series holds their days and no one else's, and the aggregate is never reached by omission (D20), as for a breakdown;
//   - the burn rate never understates the spend: it is the smallest whole figure that, held for the days it was measured over, covers it —
//     and a sum no int64 can hold is refused, never wrapped, because a wrapped burn rate is a runway that is not the customer's.

var spendLo, spendHi = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)

// referenceDays is the series by the plainest means: a loop over the uses, the day taken from the instant, no rollup involved.
func referenceDays(uses []domain.LedgerUse, q domain.BreakdownQuery) (days []string, credits map[string]int64) {
	credits = map[string]int64{}
	want := q.Scope.Normalized()
	for _, u := range uses {
		d := domain.DayOf(u.At, time.UTC)
		if u.Scope.Normalized() != want || d < q.From || d > q.To || (!q.AllMembers && u.Actor != q.Member) {
			continue
		}
		if _, seen := credits[d]; !seen {
			days = append(days, d)
		}
		credits[d] += int64(u.Credits)
	}
	slices.Sort(days)
	return days, credits
}

func TestTheDailySeriesIsTheDaysOfTheSpendAndReconcilesWithTheBreakdown(t *testing.T) {
	t.Parallel()
	uses := randomUses(rand.New(rand.NewPCG(11, 11)), 800, spendLo, spendHi)
	model, err := domain.Rollup(uses, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []domain.BreakdownQuery{
		{Scope: acme, From: "2026-11-01", To: "2026-11-14", AllMembers: true},
		{Scope: acme, From: "2026-11-04", To: "2026-11-09", AllMembers: true},
		{Scope: acme, From: "2026-11-01", To: "2026-11-14", Member: "u1"},
		{Scope: acme, From: "2026-11-01", To: "2026-11-14", Member: "u2"},
		{Scope: acme, From: "2026-11-01", To: "2026-11-14", Member: "nobody"},
		{Scope: domain.Scope{Realm: "beta", Kind: "user", ID: "x"}, From: "2026-11-01", To: "2026-11-14", AllMembers: true},
		{Scope: acme, From: "2026-12-01", To: "2026-12-05", AllMembers: true}, // a window nothing happened in
	} {
		got, err := domain.DailySpend(model, q)
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		wantDays, wantCredits := referenceDays(uses, q)
		if len(got) != len(wantDays) {
			t.Fatalf("%+v: %d days, the loop over the events finds %d", q, len(got), len(wantDays))
		}
		var total int64
		for i, r := range got {
			if r.Day != wantDays[i] || int64(r.Credits) != wantCredits[r.Day] {
				t.Errorf("%+v: day %d is %+v, the loop over the events says %s: %d", q, i, r, wantDays[i], wantCredits[wantDays[i]])
			}
			total += int64(r.Credits)
		}
		// the same query, read by dimension: the parts must close on the days
		parts, err := domain.Breakdown(model, q)
		if err != nil {
			t.Fatal(err)
		}
		var byDimension int64
		for _, p := range parts {
			byDimension += int64(p.Credits)
		}
		if total != byDimension {
			t.Errorf("%+v: the days add up to %d and the dimensions to %d: a screen showing both would not close", q, total, byDimension)
		}
	}
}

func TestADailySeriesIsOneMembersOrEveryonesAndNeverByOmission(t *testing.T) {
	t.Parallel()
	model, _ := domain.Rollup(randomUses(rand.New(rand.NewPCG(2, 3)), 50, spendLo, spendHi), time.UTC)
	for name, q := range map[string]domain.BreakdownQuery{
		"neither a member nor everyone": {Scope: acme, From: "2026-11-01", To: "2026-11-05"},
		"both":                          {Scope: acme, From: "2026-11-01", To: "2026-11-05", Member: "u1", AllMembers: true},
		"a range that ends first":       {Scope: acme, From: "2026-11-05", To: "2026-11-01", AllMembers: true},
	} {
		if _, err := domain.DailySpend(model, q); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAWindowIsOnePointPerDayAndAnAbsentDayIsZeroNotMissing(t *testing.T) {
	t.Parallel()
	got, err := domain.FillDays([]domain.DailyRow{{Day: "2026-11-02", Credits: 100}, {Day: "2026-11-05", Credits: 7}}, "2026-11-01", "2026-11-05")
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.DailyRow{{Day: "2026-11-01"}, {Day: "2026-11-02", Credits: 100}, {Day: "2026-11-03"}, {Day: "2026-11-04"}, {Day: "2026-11-05", Credits: 7}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	// the longest series the endpoint will serve is still a window: 366 days, a leap year's worth
	if year, err := domain.FillDays(nil, "2028-01-01", "2028-12-31"); err != nil || len(year) != domain.MaxSeriesDays {
		t.Fatalf("a window of %d days was %d points, %v", domain.MaxSeriesDays, len(year), err)
	}

	// one day is a window; a window across a month end and a leap day counts the days the calendar has
	one, _ := domain.FillDays(nil, "2026-11-01", "2026-11-01")
	if len(one) != 1 {
		t.Fatalf("a one-day window has %d points", len(one))
	}
	span, _ := domain.FillDays(nil, "2028-02-27", "2028-03-02")
	if len(span) != 5 || span[2].Day != "2028-02-29" {
		t.Fatalf("27 Feb to 2 Mar of a leap year is 5 days with the 29th among them: %+v", span)
	}
}

func TestAnInconsistentSeriesIsRefusedNotQuietlyTrimmed(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		rows     []domain.DailyRow
		from, to string
	}{
		"a day before the window":             {[]domain.DailyRow{{Day: "2026-10-31", Credits: 5}}, "2026-11-01", "2026-11-03"},
		"a day after the window":              {[]domain.DailyRow{{Day: "2026-11-04", Credits: 5}}, "2026-11-01", "2026-11-03"},
		"the same day twice":                  {[]domain.DailyRow{{Day: "2026-11-02", Credits: 5}, {Day: "2026-11-02", Credits: 6}}, "2026-11-01", "2026-11-03"},
		"a window that ends first":            {nil, "2026-11-03", "2026-11-01"},
		"a day that is not a date":            {[]domain.DailyRow{{Day: "Tuesday", Credits: 5}}, "2026-11-01", "2026-11-03"},
		"a window that is not dates":          {nil, "yesterday", "2026-11-03"},
		"a day that exists in no month":       {[]domain.DailyRow{{Day: "2026-02-30", Credits: 5}}, "2026-02-01", "2026-03-05"},
		"a window longer than the series cap": {nil, "2026-01-01", "2027-01-02"}, // 367 days
	} {
		// dropping a row would make the points add up to less than the rollup holds
		if _, err := domain.FillDays(c.rows, c.from, c.to); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func series(spend ...int64) []domain.DailyRow {
	out := make([]domain.DailyRow, len(spend))
	for i, c := range spend {
		out[i] = domain.DailyRow{Day: spendLo.AddDate(0, 0, i).Format(time.DateOnly), Credits: domain.Credits(c)}
	}
	return out
}

func TestTheBurnRateIsTheAverageFromTheFirstDayOfSpendRoundedUp(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		series []domain.DailyRow
		want   int64
	}{
		"no spend at all is no burn":                             {series(0, 0, 0, 0, 0, 0, 0), 0},
		"an empty series is no burn":                             {nil, 0},
		"spend only today is today's spend, not a 14th of it":    {series(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 500), 500},
		"a new scope with two days of history":                   {series(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 300, 100), 200},
		"spend long ago, nothing since, is thinned by the quiet": {series(100, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 8}, // ceil(100/14)
		"a whole figure is not rounded":                          {series(1400, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 100},
		"a fraction is rounded up, never down":                   {series(1401, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), 101},
		"two bursts count the days between and after them":       {series(0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 20), 3}, // 30 over 12 days
		"one credit over a long window is one, not zero":         {series(0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1), 1},
		"a single day": {series(7), 7},
		"a correction that outweighs the spend is no burn, never a negative one": {series(10, -30), 0},
	} {
		got, err := domain.BurnRate(c.series)
		if err != nil || int64(got) != c.want {
			t.Errorf("%s: got %d (%v), want %d", name, got, err, c.want)
		}
	}
}

// bigBurn is the burn rate as exact arithmetic: ceil(total / days), the days counted from the first day with spend.
func bigBurn(spend []int64) *big.Int {
	first := -1
	total := new(big.Int)
	for i, c := range spend {
		if c > 0 && first < 0 {
			first = i
		}
		total.Add(total, big.NewInt(c))
	}
	if first < 0 {
		return new(big.Int)
	}
	days := big.NewInt(int64(len(spend) - first))
	q, m := new(big.Int).QuoRem(total, days, new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func TestTheBurnRateIsTheSmallestWholeFigureThatNeverUnderstatesTheSpend(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(5, 8))
	for range 2000 {
		n := 1 + r.IntN(60)
		spend := make([]int64, n)
		for i := range spend {
			if r.IntN(3) > 0 {
				spend[i] = r.Int64N(1_000_000)
			}
		}
		got, err := domain.BurnRate(series(spend...))
		if err != nil {
			t.Fatal(err)
		}
		if want := bigBurn(spend); big.NewInt(int64(got)).Cmp(want) != 0 {
			t.Fatalf("%v: burn %d, exact arithmetic says %s", spend, got, want)
		}
	}
}

func FuzzBurnRateAgainstBigArithmetic(f *testing.F) {
	for _, s := range [][3]uint64{{math.MaxInt64, 1, 0}, {math.MaxInt64, 366, 0}, {1, 366, 0}, {0, 14, 0}, {366, 366, 0}, {367, 366, 0}, {math.MaxInt64, 14, 13}, {10, 14, 3}} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, total, days, lead uint64) {
		d := 1 + int(days%400)
		skip := int(lead % uint64(d))
		spend := make([]int64, d)
		spend[skip] = int64(total & math.MaxInt64) // the whole spend on one day: every figure is representable, so only the division is under test
		got, err := domain.BurnRate(series(spend...))
		if err != nil {
			t.Fatalf("%v", err)
		}
		if want := bigBurn(spend); big.NewInt(int64(got)).Cmp(want) != 0 {
			t.Fatalf("total %d over %d days (from day %d): burn %d, exact arithmetic says %s", spend[skip], d, skip, got, want)
		}
	})
}

func TestASeriesWhoseTotalNoInt64CanHoldIsRefusedNotWrapped(t *testing.T) {
	t.Parallel()
	if _, err := domain.BurnRate(series(math.MaxInt64, 1)); !errors.Is(err, domain.ErrUsageOverflow) {
		t.Fatalf("a total of 2^63 was %v", err)
	}
	if got, err := domain.BurnRate(series(math.MaxInt64)); err != nil || int64(got) != math.MaxInt64 {
		t.Fatalf("a total of exactly 2^63-1 over one day was %d, %v", got, err)
	}
}

// Each dimension's day is within int64 (the rollup refuses otherwise), but two dimensions on one day can add up to more: the day's figure is
// the sum a chart would draw, so it is refused rather than wrapped into a smaller, wrong one.
func TestADaysSumAcrossDimensionsNoInt64CanHoldIsRefusedNotWrapped(t *testing.T) {
	t.Parallel()
	key := func(dim string) domain.UsageKey {
		return domain.UsageKey{Scope: acme, Day: "2026-11-03", Dimension: dim}
	}
	q := domain.BreakdownQuery{Scope: acme, From: "2026-11-01", To: "2026-11-05", AllMembers: true}

	over := map[domain.UsageKey]domain.UsageTotals{key("gpt"): {Credits: math.MaxInt64}, key("embed"): {Credits: 1}}
	if _, err := domain.DailySpend(over, q); !errors.Is(err, domain.ErrUsageOverflow) {
		t.Fatalf("a day of 2^63 credits across two dimensions was %v", err)
	}
	fits := map[domain.UsageKey]domain.UsageTotals{key("gpt"): {Credits: math.MaxInt64 - 5}, key("embed"): {Credits: 5}}
	got, err := domain.DailySpend(fits, q)
	if err != nil || len(got) != 1 || got[0].Credits != math.MaxInt64 {
		t.Fatalf("a day of exactly 2^63-1 credits was %+v, %v", got, err)
	}
}
