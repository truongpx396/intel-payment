package app_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// FR-039 / FR-036: the credits snapshot carries what the usage_daily rollup says — a burn rate, a spend series and a breakdown — over the
// last Days days (14 unless asked), in UTC days as the rollup keeps them. Held here to the snapshot's own arithmetic: the rollup is a fake
// whose reads are the model's (domain.DailySpend, domain.Breakdown), so what is under test is what the snapshot does with them.
//
// What it must not do: invent (a snapshot with no rollup has no spend section, and a rollup that cannot be read fails the snapshot instead of
// saying nothing was spent), leak (a member's snapshot reads no rollup at all, because the rollup is keyed by an actor events do not carry
// and a member's own days cannot be told from a colleague's — D20), wrap (a sum past int64 is refused), or mislead (a figure whose parts do
// not close).

// fakeRollup is usage_daily as the model reads it.
type fakeRollup struct {
	mu   sync.Mutex
	rows map[domain.UsageKey]domain.UsageTotals
	err  error // fails every read
	// failBreakdown fails the breakdown read alone, so a snapshot that swallows one of its two reads cannot hide behind the other
	failBreakdown bool
	// stray is a day the store answers with that the query did not ask for: a misbehaving store, which the snapshot must not paper over
	stray []domain.DailyRow
	reads []domain.BreakdownQuery
}

func (f *fakeRollup) Rollup(context.Context, time.Time, time.Time, *time.Location) (int, error) {
	return 0, errors.New("the snapshot does not roll anything up")
}

func (f *fakeRollup) read(q domain.BreakdownQuery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, q)
	return f.err
}

func (f *fakeRollup) Breakdown(_ context.Context, q domain.BreakdownQuery) ([]domain.BreakdownRow, error) {
	if err := f.read(q); err != nil {
		return nil, err
	}
	if f.failBreakdown {
		return nil, errors.New("usage_daily: the breakdown read failed")
	}
	return domain.Breakdown(f.rows, q)
}

func (f *fakeRollup) Series(_ context.Context, q domain.BreakdownQuery) ([]domain.DailyRow, error) {
	if err := f.read(q); err != nil {
		return nil, err
	}
	rows, err := domain.DailySpend(f.rows, q)
	return append(rows, f.stray...), err
}

func (f *fakeRollup) reading() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func day(dim, actor, d string, credits int64) (domain.UsageKey, domain.UsageTotals) {
	return domain.UsageKey{Scope: ws, Day: d, Dimension: dim, Actor: actor}, domain.UsageTotals{Credits: domain.Credits(credits), Events: 1}
}

func rollupOf(entries ...func() (domain.UsageKey, domain.UsageTotals)) map[domain.UsageKey]domain.UsageTotals {
	m := map[domain.UsageKey]domain.UsageTotals{}
	for _, e := range entries {
		k, v := e()
		m[k] = v
	}
	return m
}

func spend(dim, actor, d string, credits int64) func() (domain.UsageKey, domain.UsageTotals) {
	return func() (domain.UsageKey, domain.UsageTotals) { return day(dim, actor, d, credits) }
}

// The clock reads 2026-09-29 (epoch), so a fortnight ending today is 16 September to 29 September.
func spendRig(t *testing.T, f *fakeRollup) (*rig, ports.CreditsReader) {
	t.Helper()
	return snapshotRig(t, rigOpt{rollup: f}, map[domain.Pool]domain.Credits{domain.GeneralPool: 5})
}

func TestTheSnapshotCarriesTheLastFortnightOfSpendByDayAndByDimensionAndTheyClose(t *testing.T) {
	t.Parallel()
	other := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w2"}
	f := &fakeRollup{rows: rollupOf(
		spend("gpt", "", "2026-09-20", 500), spend("embed", "", "2026-09-20", 105), spend("gpt", "u1", "2026-09-20", 0),
		spend("gpt", "", "2026-09-27", 300),
		spend("gpt", "u1", "2026-09-29", 200), // today, and a member's: the scope's view is everyone's
		spend("gpt", "", "2026-09-10", 9999),  // before the window
		func() (domain.UsageKey, domain.UsageTotals) { // another scope's
			return domain.UsageKey{Scope: other, Day: "2026-09-20", Dimension: "gpt"}, domain.UsageTotals{Credits: 77777}
		},
	)}
	_, c := spendRig(t, f)
	snap := ask(t, c, ports.SnapshotQuery{})

	if snap.Spend == nil {
		t.Fatal("a snapshot over a readable rollup has a spend section")
	}
	s := snap.Spend
	if len(s.Series) != 14 || s.Series[0].Day != "2026-09-16" || s.Series[13].Day != "2026-09-29" {
		t.Fatalf("a fortnight ending today is 14 points from 16 to 29 September: %+v", s.Series)
	}
	want := map[string]domain.Credits{"2026-09-20": 605, "2026-09-27": 300, "2026-09-29": 200}
	var total domain.Credits
	for i, p := range s.Series {
		if p.Credits != want[p.Day] {
			t.Errorf("%s: %d credits, want %d", p.Day, p.Credits, want[p.Day]) // a quiet day is a zero in the series, not a gap
		}
		if i > 0 && s.Series[i-1].Day >= p.Day {
			t.Errorf("the series is not oldest first at %s", p.Day)
		}
		total += p.Credits
	}
	if len(s.Breakdown) != 2 || s.Breakdown[0].Dimension != "gpt" || s.Breakdown[0].Credits != 1000 || s.Breakdown[1].Dimension != "embed" || s.Breakdown[1].Credits != 105 {
		t.Fatalf("by dimension, largest first, of this scope's window only: %+v", s.Breakdown)
	}
	var byDimension domain.Credits
	for _, r := range s.Breakdown {
		byDimension += r.Credits
	}
	if total != 1105 || byDimension != total {
		t.Errorf("the days add up to %d and the dimensions to %d: a panel showing both would not close", total, byDimension)
	}
	// 1,105 credits over the 10 days from the first day of spend (20 to 29 September) is 110.5 a day, and the runway must not be overstated
	if s.BurnRatePerDay != 111 {
		t.Errorf("burn rate %d, want 111 (ceil(1105/10))", s.BurnRatePerDay)
	}
	// the scope's view is everyone's, by name, over exactly the window shown
	for _, q := range f.reads {
		if !q.AllMembers || q.Member != "" || q.Scope != ws || q.From != "2026-09-16" || q.To != "2026-09-29" {
			t.Errorf("the rollup was asked %+v", q)
		}
	}
}

func TestTheSeriesIsAsLongAsAskedForAndTheBreakdownCoversTheSameDays(t *testing.T) {
	t.Parallel()
	f := &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-09-26", 4000), spend("gpt", "", "2026-09-27", 300), spend("embed", "", "2026-09-29", 200))}
	_, c := spendRig(t, f)

	three := ask(t, c, ports.SnapshotQuery{Days: 3}).Spend
	if len(three.Series) != 3 || three.Series[0].Day != "2026-09-27" {
		t.Fatalf("three days ending 29 September start on the 27th: %+v", three.Series)
	}
	var byDimension domain.Credits
	for _, r := range three.Breakdown {
		byDimension += r.Credits
	}
	if byDimension != 500 {
		t.Errorf("the breakdown covers the days the series does, not more: %d credits, want 500 (the 26th is outside)", byDimension)
	}
	if three.BurnRatePerDay != 167 { // ceil(500 / 3): the days from the 27th
		t.Errorf("burn rate %d, want 167", three.BurnRatePerDay)
	}

	if one := ask(t, c, ports.SnapshotQuery{Days: 1}).Spend; len(one.Series) != 1 || one.Series[0].Day != "2026-09-29" || one.BurnRatePerDay != 200 {
		t.Errorf("a one-day window is today: %+v", one)
	}
	if year := ask(t, c, ports.SnapshotQuery{Days: domain.MaxSeriesDays}).Spend; len(year.Series) != domain.MaxSeriesDays {
		t.Errorf("the longest window is %d points, got %d", domain.MaxSeriesDays, len(year.Series))
	}
	if def := ask(t, c, ports.SnapshotQuery{}).Spend; len(def.Series) != ports.DefaultSeriesDays || ports.DefaultSeriesDays != 14 {
		t.Errorf("no days asked for is %d points: got %d", ports.DefaultSeriesDays, len(def.Series))
	}
}

func TestADaysCountThatIsNotAWindowIsRefused(t *testing.T) {
	t.Parallel()
	_, c := spendRig(t, &fakeRollup{})
	for _, days := range []int{-1, -14, domain.MaxSeriesDays + 1, math.MaxInt32} {
		if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws, Days: days}); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("days=%d: %v", days, err)
		}
	}
}

func TestAWindowWithNoSpendIsARealZeroNotAnAbsentSection(t *testing.T) {
	t.Parallel()
	_, c := spendRig(t, &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-01-01", 5))}) // spend, long before the window
	s := ask(t, c, ports.SnapshotQuery{}).Spend
	if s == nil {
		t.Fatal("no spend in the window is a statement the rollup can make; it is not the absence of a rollup")
	}
	if len(s.Series) != 14 || len(s.Breakdown) != 0 || s.BurnRatePerDay != 0 {
		t.Errorf("a quiet fortnight is 14 zeros, no dimensions and no burn: %+v", s)
	}
	for _, p := range s.Series {
		if p.Credits != 0 {
			t.Errorf("%+v", p)
		}
	}
}

func TestAMembersSnapshotCarriesNoSpendAndTheRollupIsNeverRead(t *testing.T) {
	t.Parallel()
	f := &fakeRollup{rows: rollupOf(spend("gpt", "u1", "2026-09-29", 200), spend("gpt", "u2", "2026-09-29", 900))}
	_, c := spendRig(t, f)
	snap := ask(t, c, ports.SnapshotQuery{SubjectKind: "user", SubjectID: "u1"})
	if snap.Spend != nil {
		t.Fatalf("a member's snapshot has no spend section: %+v", snap.Spend)
	}
	if n := f.reading(); n != 0 {
		t.Errorf("the rollup was read %d times for a member: it is keyed by an actor an event does not carry, so their own days cannot be told from a colleague's", n)
	}
}

func TestWithoutARollupTheSnapshotHasNoSpendSectionAndNoError(t *testing.T) {
	t.Parallel()
	_, c := snapshotRig(t, rigOpt{}, map[domain.Pool]domain.Credits{domain.GeneralPool: 5})
	if snap := ask(t, c, ports.SnapshotQuery{}); snap.Spend != nil {
		t.Fatalf("absent, not invented: %+v", snap.Spend)
	}
}

func TestARollupThatCannotBeReadFailsTheSnapshotInsteadOfSayingNothingWasSpent(t *testing.T) {
	t.Parallel()
	down := errors.New("usage_daily: connection refused")
	_, c := spendRig(t, &fakeRollup{err: down})
	snap, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws})
	if !errors.Is(err, down) {
		t.Fatalf("an unreadable rollup was %v", err)
	}
	if snap.Spend != nil || snap.Scope != (domain.Scope{}) {
		t.Errorf("a half-built snapshot came back with the error: %+v", snap)
	}
}

func TestADayIsTheUTCDayTheClockIsIn(t *testing.T) {
	t.Parallel()
	f := &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-09-30", 40))}
	r, c := spendRig(t, f)

	r.clock.Advance(11*time.Hour + 59*time.Minute + 59*time.Second) // 23:59:59 on the 29th
	if s := ask(t, c, ports.SnapshotQuery{Days: 2}).Spend; s.Series[1].Day != "2026-09-29" || s.BurnRatePerDay != 0 {
		t.Errorf("one second before midnight is still the 29th: %+v", s)
	}
	r.clock.Advance(time.Second) // the 30th begins
	if s := ask(t, c, ports.SnapshotQuery{Days: 2}).Spend; s.Series[0].Day != "2026-09-29" || s.Series[1].Day != "2026-09-30" || s.Series[1].Credits != 40 || s.BurnRatePerDay != 40 {
		t.Errorf("at midnight the window moves a day and today's spend is in it: %+v", s)
	}
}

func TestASpendNoInt64CanHoldIsRefusedNotWrapped(t *testing.T) {
	t.Parallel()
	f := &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-09-29", math.MaxInt64), spend("embed", "", "2026-09-29", 1))}
	_, c := spendRig(t, f)
	if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); !errors.Is(err, domain.ErrUsageOverflow) {
		t.Fatalf("a day of 2^63 credits was %v", err)
	}
}

func TestABreakdownThatCannotBeReadFailsTheSnapshotToo(t *testing.T) {
	t.Parallel()
	_, c := spendRig(t, &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-09-29", 5)), failBreakdown: true})
	if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); err == nil {
		t.Fatal("a series without its breakdown is half of a section: the snapshot fails, it does not quietly drop the part it could not read")
	}
}

// Each day fits an int64 and so does each dimension; it is the burn rate's total across days that does not. The series is still drawn from
// rows the store gave, so the refusal has to come from the arithmetic that adds the days up.
func TestADaysSumNoInt64CanHoldAcrossTheWindowIsRefusedNotWrappedIntoABurnRate(t *testing.T) {
	t.Parallel()
	// two dimensions, so the breakdown (per dimension) and each day fit: only the burn rate's sum over the days does not
	f := &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-09-28", math.MaxInt64), spend("embed", "", "2026-09-29", 1))}
	_, c := spendRig(t, f)
	if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); !errors.Is(err, domain.ErrUsageOverflow) {
		t.Fatalf("2^63 credits over two days was %v", err)
	}
}

func TestAStoreThatAnswersOutsideTheWindowFailsTheSnapshotInsteadOfDroppingTheRow(t *testing.T) {
	t.Parallel()
	_, c := spendRig(t, &fakeRollup{rows: rollupOf(spend("gpt", "", "2026-09-29", 5)), stray: []domain.DailyRow{{Day: "2026-01-01", Credits: 9}}})
	if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a row from January in a fortnight ending in September was %v: dropping it would be a series that adds up to less than the store holds", err)
	}
}

func TestADaysCountThatIsNotAWindowIsRefusedWhetherOrNotThereIsARollup(t *testing.T) {
	t.Parallel()
	_, c := snapshotRig(t, rigOpt{}, map[domain.Pool]domain.Credits{domain.GeneralPool: 5}) // no rollup: nothing would use the count, and it is still a bad request
	for _, days := range []int{-1, domain.MaxSeriesDays + 1} {
		if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws, Days: days}); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("days=%d without a rollup: %v", days, err)
		}
	}
}
