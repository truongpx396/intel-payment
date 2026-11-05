package events_test

import (
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// R115: "A 5xx endpoint is retried on the backoff schedule then marked dead" — 1 m, 5 m, 30 m, 2 h, then every 6 h, for 72 h.

func TestTheRetryScheduleIsOneFiveThirtyTwoHoursThenEverySixHours(t *testing.T) {
	t.Parallel()
	want := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if got := events.Delay(i + 1); got != w {
			t.Errorf("after failure %d: %v, want %v", i+1, got, w)
		}
	}
	if events.Delay(0) != time.Minute || events.Delay(-3) != time.Minute || events.Delay(1<<30) != 6*time.Hour {
		t.Error("the schedule must clamp, not index out of range")
	}
}

func TestJitterSpreadsByAtMostTwentyPercentEitherWayAndNeverNegative(t *testing.T) {
	t.Parallel()
	for _, d := range events.Retries() {
		lo, hi := events.Jitter(d, 0), events.Jitter(d, 0.999999)
		if lo != d-d/5 || hi < d+d/5-d/100000 || hi > d+d/5 {
			t.Errorf("%v spreads to [%v, %v]", d, lo, hi)
		}
		if mid := events.Jitter(d, 0.5); mid != d {
			t.Errorf("%v at the middle of the range is %v", d, mid)
		}
		if events.Jitter(d, -1) != lo || events.Jitter(d, 7) < hi {
			t.Errorf("%v: u outside [0,1) must clamp", d)
		}
	}
}

// An endpoint that never answers is tried at 0, 1 m, 6 m, 36 m, 2 h 36 m, then every 6 h, and the retry that would fall past 72 h is the
// end: the delivery is dead, and the attempt count is exactly what the schedule says.
func TestAnEndpointThatNeverAnswersIsTriedOnTheScheduleThenIsDeadAt72Hours(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	at, attempts := created, []time.Duration{0}
	var dead bool
	for failed := 1; ; failed++ {
		v := events.AfterFailure(created, at, failed, 0.5) // no jitter
		if v.Dead {
			dead = true
			break
		}
		if !v.Next.After(at) {
			t.Fatalf("the next attempt %v is not after %v", v.Next, at)
		}
		at = v.Next
		attempts = append(attempts, at.Sub(created))
		if len(attempts) > 100 {
			t.Fatal("never ends")
		}
	}
	if !dead {
		t.Fatal("not dead")
	}
	want := []time.Duration{0, time.Minute, 6 * time.Minute, 36 * time.Minute, 156 * time.Minute}
	for h := 156*time.Minute + 6*time.Hour; h <= events.Window; h += 6 * time.Hour {
		want = append(want, h)
	}
	if len(attempts) != len(want) {
		t.Fatalf("%d attempts %v, want %d %v", len(attempts), attempts, len(want), want)
	}
	for i := range want {
		if attempts[i] != want[i] {
			t.Fatalf("attempt %d at %v, want %v", i, attempts[i], want[i])
		}
	}
	if last := attempts[len(attempts)-1]; last > events.Window {
		t.Fatalf("the last attempt at %v is past the %v window", last, events.Window)
	}
}

func TestTheWindowIsInclusiveAtItsEndAndExclusiveBeyond(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	// A retry that lands exactly on 72 h is still inside it; one nanosecond later is not.
	now := created.Add(events.Window - 6*time.Hour)
	if v := events.AfterFailure(created, now, 9, 0.5); v.Dead || !v.Next.Equal(created.Add(events.Window)) {
		t.Fatalf("a retry exactly at the end of the window: %+v", v)
	}
	if v := events.AfterFailure(created, now.Add(time.Nanosecond), 9, 0.5); !v.Dead {
		t.Fatalf("a retry one nanosecond past the window: %+v", v)
	}
	if v := events.AfterFailure(created, created, 1, 0.5); v.Dead {
		t.Fatal("the first failure is never the last")
	}
}
