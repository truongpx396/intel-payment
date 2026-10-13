package domain_test

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// When does a ceiling start over? A refusal the caller can act on says so ("409 limit_reached … resets_at"),
// and the credits panel draws it. The answer must be the instant the counter itself starts anew — not a
// calendar approximation — so it is the end of the limit's current window in the limit's own zone.

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestResetsAtIsTheEndOfTheCurrentWindow(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		l    domain.Limit
		now  string
		want string
	}{
		{"daily in UTC", domain.Limit{Window: domain.Daily}, "2026-09-29T12:00:00Z", "2026-09-30T00:00:00Z"},
		{"daily in a zone ahead of UTC: 01:00 local on the 30th", domain.Limit{Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh"}, "2026-09-29T18:00:00Z", "2026-09-30T17:00:00Z"},
		{"daily, exactly on the boundary, starts the next window", domain.Limit{Window: domain.Daily}, "2026-09-30T00:00:00Z", "2026-10-01T00:00:00Z"},
		{"daily on a 25-hour day (clocks fall back in New York)", domain.Limit{Window: domain.Daily, TZ: "America/New_York"}, "2026-11-01T04:30:00Z", "2026-11-02T05:00:00Z"},
		{"daily on a 23-hour day (clocks spring forward)", domain.Limit{Window: domain.Daily, TZ: "America/New_York"}, "2026-03-08T05:30:00Z", "2026-03-09T04:00:00Z"},
		{"daily when the zone skips the calendar date (Samoa, 30 Dec 2011)", domain.Limit{Window: domain.Daily, TZ: "Pacific/Apia"}, "2011-12-30T09:30:00Z", "2011-12-30T10:00:00Z"},
		{"hourly in UTC", domain.Limit{Window: domain.Hourly}, "2026-09-29T12:34:56Z", "2026-09-29T13:00:00Z"},
		{"hourly: a fraction of a second does not move the boundary", domain.Limit{Window: domain.Hourly}, "2026-09-29T12:34:56.789123456Z", "2026-09-29T13:00:00Z"},
		{"hourly: a fraction of a second in a half-hour zone", domain.Limit{Window: domain.Hourly, TZ: "Asia/Kolkata"}, "2026-09-29T12:20:00.5Z", "2026-09-29T12:30:00Z"},
		{"daily: a fraction of a second does not move the boundary", domain.Limit{Window: domain.Daily}, "2026-09-29T23:59:59.999999999Z", "2026-09-30T00:00:00Z"},
		{"hourly in a half-hour zone: the local hour ends at :30", domain.Limit{Window: domain.Hourly, TZ: "Asia/Kolkata"}, "2026-09-29T12:20:00Z", "2026-09-29T12:30:00Z"},
		{"hourly in a 45-minute zone: 18:05 local ends at 19:00", domain.Limit{Window: domain.Hourly, TZ: "Asia/Kathmandu"}, "2026-09-29T12:20:00Z", "2026-09-29T13:15:00Z"},
		{"hourly in the first 01:30 of a fall-back night", domain.Limit{Window: domain.Hourly, TZ: "America/New_York"}, "2026-11-01T05:30:00Z", "2026-11-01T06:00:00Z"},
		{"hourly in the second 01:30: a different window from the first", domain.Limit{Window: domain.Hourly, TZ: "America/New_York"}, "2026-11-01T06:30:00Z", "2026-11-01T07:00:00Z"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, ok := row.l.ResetsAt(at(t, row.now))
			if !ok || !got.Equal(at(t, row.want)) {
				t.Fatalf("ResetsAt(%s) = %v, %v; want %s", row.now, got.UTC(), ok, row.want)
			}
		})
	}
}

// A balance has no reset, a rolling window slides and a job budget ends with the job: none of them has one
// instant to name, and saying so is the answer — a made-up one would be a promise to a caller.
func TestOnlyCalendarWindowsHaveASingleResetInstant(t *testing.T) {
	t.Parallel()
	now := at(t, "2026-09-29T12:00:00Z")
	for name, l := range map[string]domain.Limit{
		"balance":        {Window: domain.Balance},
		"rolling":        {Window: domain.Rolling, Dur: time.Hour},
		"job":            {Window: domain.Job, Dur: time.Hour},
		"a bad zone":     {Window: domain.Daily, TZ: "Mars/Olympus_Mons"},
		"an unknown one": {Window: domain.Window(9)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := l.ResetsAt(now); ok {
				t.Fatalf("%s reports a reset at %v", name, got)
			}
		})
	}
}

// The reset is the instant the counter ends: what a caller is told and what the books do are one fact.
func TestResetsAtIsWhenTheCounterEnds(t *testing.T) {
	t.Parallel()
	now := at(t, "2026-09-29T18:00:00Z")
	for _, l := range []domain.Limit{
		{Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh"},
		{Window: domain.Hourly, TZ: "Asia/Kolkata"},
	} {
		b, err := l.WriteBucket(now)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := l.ResetsAt(now)
		// The counter outlives its window by a margin, so a bucket never expires early.
		if !ok || b.TTL-got.Sub(now) != time.Hour {
			t.Fatalf("%v: reset in %v, but the counter lives %v (margin should be exactly 1h)", l.Window, got.Sub(now), b.TTL)
		}
	}
}

// Held to an independent reference: walk forward a minute at a time until the local date (or the local
// hour, offset included) changes. Every zone here has whole-minute offsets and transitions, so the first
// minute whose key differs is the boundary; the function must name exactly that instant.
func FuzzResetsAtIsTheNextLocalBoundary(f *testing.F) {
	zones := []string{"UTC", "Asia/Ho_Chi_Minh", "Asia/Kolkata", "Asia/Kathmandu", "America/New_York", "America/St_Johns",
		"Europe/London", "Australia/Lord_Howe", "Pacific/Chatham", "Pacific/Apia", "Pacific/Kiritimati"}
	seeds := []struct {
		sec  int64
		ns   int64
		zone uint8
		hour bool
	}{
		{1790000000, 0, 1, false}, {1790000000, 0, 2, true}, {1793500000, 0, 4, false}, {1793500000, 0, 4, true}, {1793500000, 0, 5, true},
		{1325237400, 0, 9, false}, {1325237400, 0, 9, true}, {1790000000, 0, 3, true}, {1790000000, 0, 7, false}, {1790000000, 0, 10, false},
		{1790000000, 789123456, 0, true}, {1790000000, 999999999, 2, true}, {1790000000, 500000000, 1, false},
	}
	for _, s := range seeds {
		f.Add(s.sec, s.ns, s.zone, s.hour)
	}
	f.Fuzz(func(t *testing.T, sec, ns int64, zone uint8, hourly bool) {
		const lo, hi = 946684800, 4102444800 // 2000 … 2100
		if sec < lo || sec >= hi {
			t.Skip()
		}
		name := zones[int(zone)%len(zones)]
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skip()
		}
		l := domain.Limit{Window: domain.Daily, TZ: name}
		key := func(t time.Time) string { return t.In(loc).Format("2006-01-02") }
		if hourly {
			l.Window = domain.Hourly
			key = func(t time.Time) string { return t.In(loc).Format("2006-01-02T15 -0700") }
		}
		if ns < 0 || ns >= int64(time.Second) {
			t.Skip()
		}
		now := time.Unix(sec, ns).UTC()

		got, ok := l.ResetsAt(now)
		if !ok {
			t.Fatalf("%s %v: no reset", name, l.Window)
		}
		if !got.After(now) {
			t.Fatalf("%s %v at %v: the reset %v is not ahead", name, l.Window, now, got.UTC())
		}
		want := now.Truncate(time.Minute)
		start := key(now)
		for i := 0; i < 27*60 && key(want) == start; i++ {
			want = want.Add(time.Minute)
		}
		if key(want) == start {
			t.Fatalf("%s %v at %v: the reference found no boundary in 27 hours", name, l.Window, now)
		}
		if !got.Equal(want) {
			t.Fatalf("%s %v at %v: reset %v, the next local boundary is %v", name, l.Window, now, got.UTC(), want.UTC())
		}
	})
}
