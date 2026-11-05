package events

import "time"

// Window is how long a delivery is retried before it is marked dead (outbound-events.md).
const Window = 72 * time.Hour

// retries are the waits after the first, second, third and fourth failure; every later one is the last.
var retries = [...]time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

// Delay is the wait after the nth failed attempt (n counts from 1): 1 m, 5 m, 30 m, 2 h, then every 6 h.
func Delay(failed int) time.Duration {
	if failed < 1 {
		failed = 1
	}
	return retries[min(failed, len(retries))-1]
}

// Jitter spreads d by up to 20% either way, so a thousand endpoints that failed together do not come back together. u is a uniform
// number in [0,1); the schedule stays testable by choosing it.
func Jitter(d time.Duration, u float64) time.Duration {
	if u < 0 {
		u = 0
	}
	if u >= 1 {
		u = 0.999999
	}
	return d + time.Duration((u*0.4-0.2)*float64(d))
}

// Verdict is what a failed attempt turns into.
type Verdict struct {
	Next time.Time
	Dead bool
}

// AfterFailure decides what follows the nth failed attempt of a delivery created at created: the next try, or — when that try would
// fall after the 72-hour window — the end of the retries. A dead delivery is counted, paged, and can be re-driven from the admin API;
// the feed always has the event.
func AfterFailure(created, now time.Time, failed int, u float64) Verdict {
	next := now.Add(Jitter(Delay(failed), u))
	if next.After(created.Add(Window)) {
		return Verdict{Dead: true}
	}
	return Verdict{Next: next}
}
