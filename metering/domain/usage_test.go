package domain_test

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// R113 (FR-039), as a property: for any partition of a set of events into days — including a zone that skips a calendar date, the class of
// defect fuzzing found in window.go — the daily rows sum to the events; and a member-scoped breakdown never contains another member's rows.

func zone(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return loc
}

var zones = []string{"UTC", "Pacific/Apia", "Pacific/Kiritimati", "America/Los_Angeles", "Australia/Lord_Howe", "Asia/Kolkata", "Europe/London", "America/St_Johns", "Pacific/Pago_Pago"}

var acme = domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}

func use(at time.Time, dim, actor string, credits, cost, events int64) domain.LedgerUse {
	return domain.LedgerUse{Scope: acme, At: at, Dimension: dim, Actor: actor, Credits: domain.Credits(credits), CostMicros: cost, Events: events}
}

func TestADayIsTheDateOnAWallClockAndNoInstantIsLostToAZoneThatSkippedOne(t *testing.T) {
	t.Parallel()
	apia := zone(t, "Pacific/Apia")
	// Samoa moved across the date line: 30 December 2011 never happened there. The clock read 23:59:59 on the 29th and then 00:00:00 on the 31st.
	jump := time.Date(2011, 12, 30, 10, 0, 0, 0, time.UTC)
	if got := domain.DayOf(jump.Add(-time.Second), apia); got != "2011-12-29" {
		t.Fatalf("the last second before the jump is %s", got)
	}
	if got := domain.DayOf(jump, apia); got != "2011-12-31" {
		t.Fatalf("the first second after the jump is %s", got)
	}
	var uses []domain.LedgerUse
	for m := -180; m <= 180; m++ { // a spend every minute for three hours either side
		uses = append(uses, use(jump.Add(time.Duration(m)*time.Minute), "gpt", "u1", 10, 3, 1))
	}
	got, err := domain.Rollup(uses, apia)
	if err != nil {
		t.Fatal(err)
	}
	for k := range got {
		if k.Day == "2011-12-30" {
			t.Fatalf("an event was dated a day that did not exist: %+v", k)
		}
	}
	d29 := got[domain.UsageKey{Scope: acme, Day: "2011-12-29", Dimension: "gpt", Actor: "u1"}]
	d31 := got[domain.UsageKey{Scope: acme, Day: "2011-12-31", Dimension: "gpt", Actor: "u1"}]
	if d29.Events != 180 || d31.Events != 181 {
		t.Fatalf("181 minutes either side of the jump split %d / %d, want 180 before it and 181 from it", d29.Events, d31.Events)
	}
	tot, err := domain.Totals(got)
	if err != nil || tot.Credits != 3610 || tot.Events != 361 || tot.CostMicros != 1083 {
		t.Fatalf("the days do not add up to the events: %+v, %v", tot, err)
	}
}

func TestADayOf25HoursIsOneDayAndTheDayAfterIsNotShortened(t *testing.T) {
	t.Parallel()
	la := zone(t, "America/Los_Angeles")
	// 1 November 2026: the clocks went back, so the day has 25 hours.
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, la)
	var uses []domain.LedgerUse
	for h := 0; h < 25+24; h++ {
		uses = append(uses, use(start.Add(time.Duration(h)*time.Hour), "gpt", "", 1, 0, 1))
	}
	got, err := domain.Rollup(uses, la)
	if err != nil {
		t.Fatal(err)
	}
	n1 := got[domain.UsageKey{Scope: acme, Day: "2026-11-01", Dimension: "gpt"}].Events
	n2 := got[domain.UsageKey{Scope: acme, Day: "2026-11-02", Dimension: "gpt"}].Events
	if n1 != 25 || n2 != 24 {
		t.Fatalf("the 25-hour day holds %d hourly events and the next holds %d, want 25 and 24", n1, n2)
	}
}

func TestADayOf23HoursLosesNoEvent(t *testing.T) {
	t.Parallel()
	la := zone(t, "America/Los_Angeles")
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, la) // the clocks went forward: 23 hours
	var uses []domain.LedgerUse
	for h := 0; h < 23+24; h++ {
		uses = append(uses, use(start.Add(time.Duration(h)*time.Hour), "gpt", "", 1, 0, 1))
	}
	got, _ := domain.Rollup(uses, la)
	if a, b := got[domain.UsageKey{Scope: acme, Day: "2026-03-08", Dimension: "gpt"}].Events, got[domain.UsageKey{Scope: acme, Day: "2026-03-09", Dimension: "gpt"}].Events; a != 23 || b != 24 {
		t.Fatalf("%d and %d, want 23 and 24", a, b)
	}
}

func randomUses(r *rand.Rand, n int, lo, hi time.Time) []domain.LedgerUse {
	dims := []string{"gpt", "embed", "image", ""}
	actors := []string{"u1", "u2", "u3", ""}
	scopes := []domain.Scope{acme, {Realm: "acme", Kind: "workspace", ID: "w2"}, {Realm: "beta", Kind: "user", ID: "x"}}
	span := hi.Sub(lo)
	out := make([]domain.LedgerUse, n)
	for i := range out {
		out[i] = domain.LedgerUse{
			Scope: scopes[r.IntN(len(scopes))], At: lo.Add(time.Duration(r.Int64N(int64(span)))), Dimension: dims[r.IntN(len(dims))], Actor: actors[r.IntN(len(actors))],
			Credits: domain.Credits(r.Int64N(10_000)), CostMicros: r.Int64N(1_000_000), Events: 1 + r.Int64N(5),
		}
	}
	return out
}

func sumOf(uses []domain.LedgerUse) domain.UsageTotals {
	var t domain.UsageTotals
	for _, u := range uses {
		t.Credits += u.Credits
		t.CostMicros += u.CostMicros
		t.Events += u.Events
	}
	return t
}

// For any set of events, in any zone, and any split of it into two: the days of the whole are the days of the halves added, and they sum
// to the events. The reference is a loop over the events, not the rollup's own arithmetic.
func TestTheDaysOfAnyPartitionOfEventsSumToTheEventsInEveryZone(t *testing.T) {
	t.Parallel()
	lo, hi := time.Date(2011, 12, 20, 0, 0, 0, 0, time.UTC), time.Date(2012, 1, 10, 0, 0, 0, 0, time.UTC)
	for _, zn := range zones {
		loc := zone(t, zn)
		for seed := uint64(1); seed <= 40; seed++ {
			r := rand.New(rand.NewPCG(seed, 7))
			uses := randomUses(r, 1+r.IntN(200), lo, hi)
			whole, err := domain.Rollup(uses, loc)
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := domain.Totals(whole); got != sumOf(uses) {
				t.Fatalf("%s seed %d: the days sum to %+v, the events to %+v", zn, seed, got, sumOf(uses))
			}
			cut := r.IntN(len(uses) + 1)
			a, _ := domain.Rollup(uses[:cut], loc)
			b, _ := domain.Rollup(uses[cut:], loc)
			merged := map[domain.UsageKey]domain.UsageTotals{}
			for _, part := range []map[domain.UsageKey]domain.UsageTotals{a, b} {
				for k, v := range part {
					m := merged[k]
					merged[k] = domain.UsageTotals{Credits: m.Credits + v.Credits, CostMicros: m.CostMicros + v.CostMicros, Events: m.Events + v.Events}
				}
			}
			if len(merged) != len(whole) {
				t.Fatalf("%s seed %d: %d rows from the halves, %d from the whole", zn, seed, len(merged), len(whole))
			}
			for k, v := range whole {
				if merged[k] != v {
					t.Fatalf("%s seed %d: %+v is %+v from the whole and %+v from the halves", zn, seed, k, v, merged[k])
				}
			}
			r.Shuffle(len(uses), func(i, j int) { uses[i], uses[j] = uses[j], uses[i] })
			shuffled, _ := domain.Rollup(uses, loc)
			for k, v := range whole {
				if shuffled[k] != v {
					t.Fatalf("%s seed %d: the order the events arrive in changed %+v", zn, seed, k)
				}
			}
		}
	}
}

// Held to an independent computation of the day: the wall clock's year, month and day, not the rollup's own formatting.
func FuzzEveryEventLandsOnTheDateItsWallClockSaysInAnyZone(f *testing.F) {
	jump := time.Date(2011, 12, 30, 10, 0, 0, 0, time.UTC).Unix()
	for _, s := range []int64{0, jump - 1, jump, jump + 1, time.Date(2026, 11, 1, 8, 59, 59, 0, time.UTC).Unix(), time.Date(2026, 3, 8, 10, 0, 0, 0, time.UTC).Unix()} {
		for z := range zones {
			f.Add(s, uint8(z))
		}
	}
	f.Fuzz(func(t *testing.T, sec int64, z uint8) {
		if sec < -2_000_000_000 || sec > 4_000_000_000 {
			t.Skip()
		}
		loc := zone(t, zones[int(z)%len(zones)])
		at := time.Unix(sec, 0)
		y, m, d := at.In(loc).Date()
		want := fmt.Sprintf("%04d-%02d-%02d", y, int(m), d)
		if got := domain.DayOf(at, loc); got != want {
			t.Fatalf("%v in %s: %s, want %s", at.UTC(), loc, got, want)
		}
		rows, err := domain.Rollup([]domain.LedgerUse{use(at, "d", "a", 5, 2, 1)}, loc)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%v, %v", rows, err)
		}
		for k, v := range rows {
			if k.Day != want || v.Credits != 5 || v.Events != 1 || v.CostMicros != 2 {
				t.Fatalf("%+v %+v", k, v)
			}
		}
	})
}

func TestASumNoInt64CanHoldIsRefusedNotWrapped(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for name, uses := range map[string][]domain.LedgerUse{
		"credits":          {use(at, "d", "", math.MaxInt64, 0, 1), use(at, "d", "", 1, 0, 1)},
		"cost":             {use(at, "d", "", 1, math.MaxInt64, 1), use(at, "d", "", 1, 1, 1)},
		"events":           {use(at, "d", "", 1, 0, math.MaxInt64), use(at, "d", "", 1, 0, 1)},
		"negative credits": {use(at, "d", "", math.MinInt64, 0, 1), use(at, "d", "", -1, 0, 1)},
	} {
		if _, err := domain.Rollup(uses, time.UTC); !errors.Is(err, domain.ErrUsageOverflow) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The same two on different days are two rows, and fine.
	if _, err := domain.Rollup([]domain.LedgerUse{use(at, "d", "", math.MaxInt64, 0, 1), use(at.AddDate(0, 0, 1), "d", "", math.MaxInt64, 0, 1)}, time.UTC); err != nil {
		t.Errorf("two days: %v", err)
	}
	whole := map[domain.UsageKey]domain.UsageTotals{
		{Scope: acme, Day: "2026-11-05"}: {Credits: math.MaxInt64}, {Scope: acme, Day: "2026-11-06"}: {Credits: 1},
	}
	if _, err := domain.Totals(whole); !errors.Is(err, domain.ErrUsageOverflow) {
		t.Errorf("totals: %v", err)
	}
}

func TestABreakdownIsOneMembersOrEveryonesAndNeverByOmission(t *testing.T) {
	t.Parallel()
	ok := domain.BreakdownQuery{Scope: acme, From: "2026-11-01", To: "2026-11-05", Member: "u1"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]domain.BreakdownQuery{
		"neither a member nor everyone": {Scope: acme, From: "2026-11-01", To: "2026-11-05"},
		"both":                          {Scope: acme, From: "2026-11-01", To: "2026-11-05", Member: "u1", AllMembers: true},
		"no scope":                      {From: "2026-11-01", To: "2026-11-05", Member: "u1"},
		"a day that is not":             {Scope: acme, From: "yesterday", To: "2026-11-05", Member: "u1"},
		"an impossible date":            {Scope: acme, From: "2026-02-30", To: "2026-03-05", Member: "u1"},
		"a range that ends first":       {Scope: acme, From: "2026-11-05", To: "2026-11-01", Member: "u1"},
	} {
		if err := q.Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	all := domain.BreakdownQuery{Scope: acme, From: "2026-11-01", To: "2026-11-05", AllMembers: true}
	if err := all.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A member's breakdown contains their rows and no one else's; the members' breakdowns add up to everyone's; and a breakdown never reads
// another scope's days or a day outside its range.
func TestAMemberScopedBreakdownNeverContainsAnotherMembersRows(t *testing.T) {
	t.Parallel()
	lo, hi := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	for seed := uint64(1); seed <= 60; seed++ {
		r := rand.New(rand.NewPCG(seed, 11))
		uses := randomUses(r, 1+r.IntN(300), lo, hi)
		roll, err := domain.Rollup(uses, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		from, to := "2026-10-28", "2026-11-06"
		everyone, err := domain.Breakdown(roll, domain.BreakdownQuery{Scope: acme, From: from, To: to, AllMembers: true})
		if err != nil {
			t.Fatal(err)
		}
		sums := map[string]domain.BreakdownRow{}
		for _, member := range []string{"u1", "u2", "u3"} {
			rows, err := domain.Breakdown(roll, domain.BreakdownQuery{Scope: acme, From: from, To: to, Member: member})
			if err != nil {
				t.Fatal(err)
			}
			// The reference: this member's events in this scope in this range, by a loop over the events.
			want := map[string]domain.UsageTotals{}
			for _, u := range uses {
				if u.Scope == acme && u.Actor == member {
					if d := domain.DayOf(u.At, time.UTC); d >= from && d <= to {
						w := want[u.Dimension]
						want[u.Dimension] = domain.UsageTotals{Credits: w.Credits + u.Credits, CostMicros: w.CostMicros + u.CostMicros, Events: w.Events + u.Events}
					}
				}
			}
			if len(rows) != len(want) {
				t.Fatalf("seed %d member %q: %d dimensions, want %d", seed, member, len(rows), len(want))
			}
			for _, row := range rows {
				w := want[row.Dimension]
				if row.Credits != w.Credits || row.Events != w.Events || row.CostMicros != w.CostMicros {
					t.Fatalf("seed %d member %q %q: %+v, want %+v — a row of another member, scope or day leaked in", seed, member, row.Dimension, row, w)
				}
				s := sums[row.Dimension]
				sums[row.Dimension] = domain.BreakdownRow{Dimension: row.Dimension, Credits: s.Credits + row.Credits, Events: s.Events + row.Events, CostMicros: s.CostMicros + row.CostMicros}
			}
		}
		// Everyone's is the named members' plus what no member is named for, which no member's breakdown contains.
		unattributed := map[string]domain.UsageTotals{}
		for _, u := range uses {
			if u.Scope == acme && u.Actor == "" {
				if d := domain.DayOf(u.At, time.UTC); d >= from && d <= to {
					w := unattributed[u.Dimension]
					unattributed[u.Dimension] = domain.UsageTotals{Credits: w.Credits + u.Credits, CostMicros: w.CostMicros + u.CostMicros, Events: w.Events + u.Events}
				}
			}
		}
		for _, row := range everyone {
			s, w := sums[row.Dimension], unattributed[row.Dimension]
			if got := (domain.UsageTotals{Credits: s.Credits + w.Credits, CostMicros: s.CostMicros + w.CostMicros, Events: s.Events + w.Events}); got != (domain.UsageTotals{Credits: row.Credits, CostMicros: row.CostMicros, Events: row.Events}) {
				t.Fatalf("seed %d: the members' breakdowns of %q and the unattributed add up to %+v, everyone's is %+v", seed, row.Dimension, got, row)
			}
		}
		for i := 1; i < len(everyone); i++ {
			if everyone[i].Credits > everyone[i-1].Credits {
				t.Fatalf("seed %d: not largest first: %+v", seed, everyone)
			}
		}
	}
}

func TestBreakdownTiesAreOrderedByNameSoTheOrderIsStable(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	roll, _ := domain.Rollup([]domain.LedgerUse{use(at, "b", "u", 5, 0, 1), use(at, "a", "u", 5, 0, 1), use(at, "c", "u", 9, 0, 1)}, time.UTC)
	rows, err := domain.Breakdown(roll, domain.BreakdownQuery{Scope: acme, From: "2026-11-05", To: "2026-11-05", Member: "u"})
	if err != nil || len(rows) != 3 || rows[0].Dimension != "c" || rows[1].Dimension != "a" || rows[2].Dimension != "b" {
		t.Fatalf("%+v, %v", rows, err)
	}
}
