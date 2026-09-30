package domain_test

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Fuzz targets state a PROPERTY and let the fuzzer hunt for the input that breaks it. Their seed
// corpus runs as an ordinary test on every `go test`; `make fuzz` explores beyond the seeds. A
// failing input is committed under testdata/fuzz/<Target>/ and from then on is a regression test.
//
// These cover the code where "it works on the examples I thought of" is not good enough: the
// canonical encodings that decide whether a retry is a replay or a conflict, the arithmetic that
// must never wrap, and the time logic that decides when a limit's counter disappears.

// Two scopes share a Tag iff they are the same scope. A collision is two hosts on one balance.
func FuzzScopeTagIsUnambiguous(f *testing.F) {
	f.Add("t1", "org", "o1", "t1", "org", "o1")
	f.Add("t1", "org", "a:b", "t1", "org", "a:b")
	f.Add("t1", "org", "a/b:c", "t1", "org", "a/b:c")
	f.Add("", "org", "x", "default", "org", "x") // an empty realm IS the default realm
	f.Add("a", "b", "c", "a-b", "c", "c")
	f.Add("a", "b_c", "d", "a", "b", "c_d")
	f.Add("t1", "org", "o.1 >*", "t1", "org", "o.1 >*")
	f.Fuzz(func(t *testing.T, r1, k1, i1, r2, k2, i2 string) {
		a := domain.Scope{Realm: domain.Realm(r1), Kind: k1, ID: i1}
		b := domain.Scope{Realm: domain.Realm(r2), Kind: k2, ID: i2}
		if a.Validate() != nil || b.Validate() != nil {
			t.Skip("only valid scopes can reach a key")
		}
		same := a.Normalized() == b.Normalized()
		if (a.Tag() == b.Tag()) != same {
			t.Fatalf("tag collision or split: %+v and %+v (same=%v) gave %q and %q", a, b, same, a.Tag(), b.Tag())
		}
		if (a.SubjectToken() == b.SubjectToken()) != same {
			t.Fatalf("subject token collision or split: %+v and %+v gave %q and %q", a, b, a.SubjectToken(), b.SubjectToken())
		}
		if strings.ContainsAny(a.SubjectToken(), ". *>\t\r\n") {
			t.Fatalf("a subject token must not contain a separator, wildcard or space: %q", a.SubjectToken())
		}
	})
}

func FuzzShardIsStableAndInRange(f *testing.F) {
	f.Add("t1", "org", "o1", 1)
	f.Add("t1", "org", "o1", 256)
	f.Add("prod-a", "org", "1", 16384)
	f.Fuzz(func(t *testing.T, realm, kind, id string, n int) {
		if n < 1 || n > 1<<20 {
			t.Skip()
		}
		s := domain.Scope{Realm: domain.Realm(realm), Kind: kind, ID: id}
		got := s.Shard(n)
		if got < 0 || int(got) >= n {
			t.Fatalf("shard %d is outside [0,%d)", got, n)
		}
		if s.Shard(n) != got || s.Normalized().Shard(n) != got {
			t.Fatal("placement must be a pure function of the tag")
		}
	})
}

// The fingerprint separates what identifies a request: the same three strings split differently
// across fields must never encode alike, or one reused key would replay silently as another request.
func FuzzChargeFingerprintSeparatesFields(f *testing.F) {
	f.Add("ab", "c", "k", "a", "bc", "k")
	f.Add("a", "", "bc", "", "a", "bc")
	f.Add("1:a", "b", "c", "1", "a:b", "c")
	f.Add("x", "y", "z", "x", "y", "z")
	f.Fuzz(func(t *testing.T, reason1, resource1, rate1, reason2, resource2, rate2 string) {
		a, b := charge(), charge()
		a.Reason, a.Resource, a.RateKey = reason1, resource1, rate1
		b.Reason, b.Resource, b.RateKey = reason2, resource2, rate2
		same := reason1 == reason2 && resource1 == resource2 && rate1 == rate2
		if (a.Fingerprint() == b.Fingerprint()) != same {
			t.Fatalf("(%q,%q,%q) vs (%q,%q,%q): same=%v but fingerprints say %v",
				reason1, resource1, rate1, reason2, resource2, rate2, same, a.Fingerprint() == b.Fingerprint())
		}
	})
}

// A retry that lists the same quantities in another order is the same request. That includes an
// event that names one unit twice: the pricer sums them, so their order carries no meaning either.
func FuzzChargeFingerprintIgnoresQuantityOrder(f *testing.F) {
	f.Add("in", int64(10), "out", int64(5), "in", int64(7), uint8(0))
	f.Add("in", int64(10), "in", int64(7), "out", int64(1), uint8(1)) // the same unit twice, swapped
	f.Add("u", int64(1), "u", int64(2), "u", int64(3), uint8(3))
	f.Add("a", int64(0), "b", int64(0), "c", int64(0), uint8(5))
	f.Fuzz(func(t *testing.T, u1 string, a1 int64, u2 string, a2 int64, u3 string, a3 int64, pick uint8) {
		qs := []domain.Quantity{{Unit: domain.Unit(u1), Amount: a1}, {Unit: domain.Unit(u2), Amount: a2}, {Unit: domain.Unit(u3), Amount: a3}}
		perms := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
		p := perms[int(pick)%len(perms)]
		shuffled := []domain.Quantity{qs[p[0]], qs[p[1]], qs[p[2]]}

		a, b := charge(), charge()
		a.Quantities, b.Quantities = qs, shuffled
		if a.Fingerprint() != b.Fingerprint() {
			t.Fatalf("the same quantities in another order changed the fingerprint: %+v vs %+v", qs, shuffled)
		}
	})
}

func FuzzParseWindowInvertsString(f *testing.F) {
	for _, s := range []string{"balance", "daily", "hourly", "rolling", "job", "", "weekly", "Daily", "window(0)", "daily "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		w, err := domain.ParseWindow(s)
		if err != nil {
			return
		}
		if w.String() != s {
			t.Fatalf("ParseWindow(%q) = %v, which prints as %q", s, w, w.String())
		}
	})
}

// Credits and Money never wrap: the sum is exact or the operation is refused. The reference is
// math/big, which cannot overflow.
func FuzzCreditsAddIsExactOrRefused(f *testing.F) {
	for _, p := range [][2]int64{
		{0, 0}, {1, 2}, {-1, -2}, {math.MaxInt64, 1}, {math.MaxInt64 - 1, 1}, {math.MinInt64, -1},
		{math.MinInt64 + 1, -1}, {math.MaxInt64, math.MinInt64}, {math.MaxInt64, 0}, {math.MinInt64, 0}, {1 << 62, 1 << 62},
	} {
		f.Add(p[0], p[1])
	}
	f.Fuzz(func(t *testing.T, a, b int64) {
		want := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
		got, err := domain.Credits(a).Add(domain.Credits(b))
		if want.IsInt64() {
			if err != nil || int64(got) != want.Int64() {
				t.Fatalf("%d + %d = %d, %v; want %s", a, b, got, err, want)
			}
		} else if err == nil {
			t.Fatalf("%d + %d wrapped to %d; the true sum is %s", a, b, got, want)
		}

		m, merr := domain.Money{MinorUnits: a, Currency: "USD"}.Add(domain.Money{MinorUnits: b, Currency: "USD"})
		if want.IsInt64() {
			if merr != nil || m.MinorUnits != want.Int64() {
				t.Fatalf("money %d + %d = %+v, %v; want %s", a, b, m, merr, want)
			}
		} else if merr == nil {
			t.Fatalf("money %d + %d wrapped to %d; the true sum is %s", a, b, m.MinorUnits, want)
		}
	})
}

// fuzzZones include the awkward ones: a 30-minute DST step, a 45-minute offset, a zone that skipped
// a calendar day, one whose clocks change at midnight, and the two that do not change at all.
var fuzzZones = []string{
	"UTC", "America/New_York", "Europe/London", "Australia/Lord_Howe", "Asia/Kolkata", "Asia/Kathmandu",
	"Pacific/Apia", "America/Sao_Paulo", "America/Havana", "Asia/Ho_Chi_Minh", "Pacific/Chatham", "America/St_Johns",
}

// A counter must outlive the window it counts: at the instant the key expires, the key a settlement
// would use has already moved on. If not, a daily ceiling resets in the middle of the day.
func FuzzWindowCounterOutlivesItsWindow(f *testing.F) {
	ny := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT, the first of two 01:30s
	f.Add(uint8(1), false, ny.Unix(), int32(0))
	f.Add(uint8(1), true, ny.Unix(), int32(0))
	f.Add(uint8(1), true, ny.Add(time.Hour).Unix(), int32(0)) // the second 01:30
	f.Add(uint8(1), false, time.Date(2026, 3, 8, 6, 59, 59, 0, time.UTC).Unix(), int32(999_999_999))
	f.Add(uint8(3), true, time.Date(2026, 4, 5, 15, 29, 59, 0, time.UTC).Unix(), int32(0)) // Lord Howe, 30-minute step
	f.Add(uint8(6), false, time.Date(2011, 12, 29, 12, 0, 0, 0, time.UTC).Unix(), int32(0))
	f.Add(uint8(0), false, int64(0), int32(0))
	f.Add(uint8(9), true, time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC).Unix(), int32(0))
	f.Fuzz(func(t *testing.T, zone uint8, hourly bool, sec int64, nsec int32) {
		const span = int64(130 * 365 * 24 * 3600) // 1970 .. 2100
		sec = sec % span
		if sec < 0 {
			sec = -sec
		}
		nsec = nsec % 1_000_000_000
		if nsec < 0 {
			nsec = -nsec
		}
		now := time.Unix(sec, int64(nsec)).UTC()
		l := domain.Limit{Name: "l", Window: domain.Daily, TZ: fuzzZones[int(zone)%len(fuzzZones)]}
		if hourly {
			l.Window = domain.Hourly
		}
		b, err := l.WriteBucket(now)
		if err != nil {
			t.Fatalf("%s at %s: %v", l.TZ, now, err)
		}
		const margin = time.Hour
		if b.TTL <= margin {
			t.Fatalf("%s %s at %s: ttl %v leaves no time before the window ends", l.TZ, l.Window, now, b.TTL)
		}
		after, err := l.WriteBucket(now.Add(b.TTL))
		if err != nil {
			t.Fatal(err)
		}
		if after.Key == b.Key {
			t.Fatalf("%s %s at %s: the counter %q expires after %v, yet that is still the current bucket — the limit would reset early",
				l.TZ, l.Window, now, b.Key, b.TTL)
		}
		limit := 26 * time.Hour
		if hourly {
			limit = 2*time.Hour + margin
		}
		if b.TTL-margin > limit {
			t.Fatalf("%s %s at %s: ttl %v is longer than any window of this kind can be", l.TZ, l.Window, now, b.TTL)
		}
	})
}

func FuzzRollingWindowIsWellFormed(f *testing.F) {
	f.Add(int64(3600), int64(1_790_000_000), int32(0))
	f.Add(int64(6), int64(1_790_000_000), int32(999_999_999))
	f.Add(int64(1), int64(0), int32(0))
	f.Fuzz(func(t *testing.T, durSeconds, sec int64, nsec int32) {
		if durSeconds < 1 || durSeconds > 90*24*3600 || sec < 0 || sec > 1<<35 || nsec < 0 || nsec >= 1_000_000_000 {
			t.Skip()
		}
		l := domain.Limit{Name: "r", Window: domain.Rolling, Dur: time.Duration(durSeconds) * time.Second}
		now := time.Unix(sec, int64(nsec))
		read, err := l.ReadBuckets(now)
		if err != nil || len(read) != domain.RollingBuckets {
			t.Fatalf("read %v, %v", read, err)
		}
		w, err := l.WriteBucket(now)
		if err != nil {
			t.Fatal(err)
		}
		if read[0] != w.Key {
			t.Fatalf("what a settlement writes must be what the next admit reads first: %q vs %q", w.Key, read[0])
		}
		seen := map[string]bool{}
		for _, k := range read {
			if seen[k] {
				t.Fatalf("the same bucket read twice would double count: %v", read)
			}
			seen[k] = true
		}
		if w.TTL < l.Dur {
			t.Fatalf("a bucket must outlive the window that reads it: ttl %v < %v", w.TTL, l.Dur)
		}
	})
}
