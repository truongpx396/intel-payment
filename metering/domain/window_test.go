package domain_test

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDailyBucketFollowsTheLimitsTimeZone(t *testing.T) {
	t.Parallel()
	l := domain.Limit{Name: "d", Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh"} // UTC+7, no DST
	// 2026-09-29 18:00 UTC is 2026-09-30 01:00 in Ho Chi Minh: a different day than in UTC.
	now := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	b, err := l.WriteBucket(now)
	if err != nil || b.Key != "20260930" {
		t.Fatalf("got %+v %v; want the local day 20260930", b, err)
	}
	// 23 h until local midnight, plus the margin, so the bucket outlives its window.
	if want := 23*time.Hour + time.Hour; b.TTL != want {
		t.Fatalf("ttl %v, want %v", b.TTL, want)
	}
	utc := domain.Limit{Name: "d", Window: domain.Daily}
	if b, _ := utc.WriteBucket(now); b.Key != "20260929" {
		t.Fatalf("UTC day differs from the local one: %q", b.Key)
	}
}

func TestDailyBucketAcrossADSTChange(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	l := domain.Limit{Name: "d", Window: domain.Daily, TZ: "America/New_York"}
	// 2026-11-01 has 25 hours in New York (clocks fall back). At 00:30 local, 24.5 h remain.
	now := time.Date(2026, 11, 1, 0, 30, 0, 0, loc)
	b, err := l.WriteBucket(now)
	if err != nil {
		t.Fatal(err)
	}
	if b.Key != "20261101" || b.TTL != 24*time.Hour+30*time.Minute+time.Hour {
		t.Fatalf("a 25-hour day must be counted to its real end: %+v", b)
	}
}

func TestHourlyBucketsAreDistinctWhenClocksFallBack(t *testing.T) {
	t.Parallel()
	loc := mustLoc(t, "America/New_York")
	l := domain.Limit{Name: "h", Window: domain.Hourly, TZ: "America/New_York"}
	// 01:30 happens twice on 2026-11-01: once in EDT, once in EST.
	first := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)  // 01:30 EDT
	second := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC) // 01:30 EST
	a, _ := l.WriteBucket(first.In(loc))
	b, _ := l.WriteBucket(second.In(loc))
	if a.Key == b.Key {
		t.Fatalf("two different hours shared a counter: %q", a.Key)
	}
}

func TestRollingBucketsSlideAndCover(t *testing.T) {
	t.Parallel()
	l := domain.Limit{Name: "r", Window: domain.Rolling, Dur: time.Hour}
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	read, err := l.ReadBuckets(now)
	if err != nil || len(read) != domain.RollingBuckets {
		t.Fatalf("%v %v", read, err)
	}
	w, _ := l.WriteBucket(now)
	if read[0] != w.Key {
		t.Fatalf("the bucket being written is the first one read: %q vs %q", w.Key, read[0])
	}
	if w.TTL < time.Hour {
		t.Fatalf("a bucket must outlive the whole window, ttl %v", w.TTL)
	}
	// Five minutes later the current bucket has advanced and the old one is still read.
	later, _ := l.ReadBuckets(now.Add(5 * time.Minute))
	if later[0] == read[0] || later[1] != read[0] {
		t.Fatalf("the window must slide by one bucket: %v -> %v", read[:2], later[:2])
	}
}

func TestJobBucketIsUnbucketed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	l := domain.Limit{Name: "run", Window: domain.Job, Subject: domain.SubjectJob, Dur: time.Hour}
	r, _ := l.ReadBuckets(at)
	w, _ := l.WriteBucket(at)
	if len(r) != 1 || r[0] != "-" || w.Key != "-" || w.TTL != domain.DefaultJobTTL {
		t.Fatalf("%v %+v", r, w)
	}
	if _, err := (domain.Limit{Window: domain.Balance}).WriteBucket(at); err == nil {
		t.Fatal("a balance limit has no counter")
	}
}

// A counter must outlive its window, or a limit silently resets early; it must not outlive it by
// more than the margin, or keys pile up. Both ends are pinned with exact values.
func TestHourlyBucketLivesUntilTheHourEndsPlusTheMargin(t *testing.T) {
	t.Parallel()
	l := domain.Limit{Name: "h", Window: domain.Hourly}
	cases := []struct {
		at   time.Time
		key  string
		left time.Duration
	}{
		{time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC), "2026092912Z", 30 * time.Minute},
		{time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC), "2026092912Z", time.Hour},
		{time.Date(2026, 9, 29, 12, 59, 59, 0, time.UTC), "2026092912Z", time.Second},
		{time.Date(2026, 9, 29, 23, 15, 0, 0, time.UTC), "2026092923Z", 45 * time.Minute}, // the last hour of the day
	}
	for _, c := range cases {
		b, err := l.WriteBucket(c.at)
		if err != nil {
			t.Fatal(err)
		}
		if b.Key != c.key || b.TTL != c.left+time.Hour {
			t.Errorf("%s: got %+v, want key %q ttl %v", c.at.Format(time.RFC3339), b, c.key, c.left+time.Hour)
		}
	}
}

func TestRollingBucketLivesForTheWindowPlusOneBucket(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	hour := domain.Limit{Name: "r", Window: domain.Rolling, Dur: time.Hour} // width 5m
	if b, _ := hour.WriteBucket(now); b.TTL != time.Hour+5*time.Minute {
		t.Errorf("ttl %v, want 1h5m", b.TTL)
	}
	// A window shorter than RollingBuckets seconds still gets one-second buckets, not zero-width ones.
	short := domain.Limit{Name: "r", Window: domain.Rolling, Dur: 6 * time.Second}
	b, _ := short.WriteBucket(now)
	if b.TTL != 7*time.Second {
		t.Errorf("a 6s window has 1s buckets, so ttl is 7s: %v", b.TTL)
	}
	next, _ := short.WriteBucket(now.Add(time.Second))
	if next.Key == b.Key {
		t.Errorf("buckets advance every second: %q == %q", next.Key, b.Key)
	}
	if read, _ := short.ReadBuckets(now); len(read) != domain.RollingBuckets {
		t.Errorf("a rolling read always covers %d buckets, got %d", domain.RollingBuckets, len(read))
	}
}

func TestBucketsRefuseWhatHasNoCounter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if _, err := (domain.Limit{Name: "b", Window: domain.Balance}).ReadBuckets(now); err == nil {
		t.Error("a balance limit is read from the books, not from a counter")
	}
	if _, err := (domain.Limit{Name: "x", Window: domain.Daily, TZ: "Mars/Olympus"}).WriteBucket(now); err == nil {
		t.Error("an unknown zone must not silently count in UTC")
	}
	if _, err := (domain.Limit{Name: "x", Window: domain.Hourly, TZ: "Mars/Olympus"}).WriteBucket(now); err == nil {
		t.Error("an unknown zone must not silently count in UTC")
	}
}

// Asia/Kolkata is UTC+5:30, so its hours do not line up with UTC's. The counter must live to the end
// of the LOCAL hour, not the UTC one.
func TestHourlyBucketInAHalfHourZoneEndsOnTheLocalHour(t *testing.T) {
	t.Parallel()
	l := domain.Limit{Name: "h", Window: domain.Hourly, TZ: "Asia/Kolkata"}
	now := time.Date(2026, 9, 29, 12, 15, 0, 0, time.UTC) // 17:45 in Kolkata
	b, err := l.WriteBucket(now)
	if err != nil {
		t.Fatal(err)
	}
	if b.Key != "2026092917+0530" || b.TTL != 15*time.Minute+time.Hour {
		t.Fatalf("got %+v; want key 2026092917+0530 and 15 minutes to the local hour plus the margin", b)
	}
}

// Samoa skipped 30 December 2011 entirely. Midnight of the date that does not exist lies in the
// past, so a naive "next midnight" gave the counter a negative lifetime and Redis deleted it at once.
func TestDailyBucketSurvivesAZoneThatSkippedACalendarDate(t *testing.T) {
	t.Parallel()
	l := domain.Limit{Name: "d", Window: domain.Daily, TZ: "Pacific/Apia"}
	now := time.Date(2011, 12, 29, 12, 0, 0, 0, time.UTC) // 02:00 on the 29th, the day before the skip
	b, err := l.WriteBucket(now)
	if err != nil {
		t.Fatal(err)
	}
	if b.TTL <= time.Hour {
		t.Fatalf("a counter written on the 29th got a lifetime of %v; it would be deleted immediately", b.TTL)
	}
	// It must run to the moment local time jumps to the 31st (2011-12-30 10:00 UTC), plus the margin.
	if want := 22*time.Hour + time.Hour; b.TTL != want {
		t.Fatalf("ttl %v, want %v", b.TTL, want)
	}
}
