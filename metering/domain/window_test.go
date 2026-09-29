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
