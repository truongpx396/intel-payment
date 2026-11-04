package entitlements_test

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/entitlements"
	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// R105 (found by running the process test on the finished branch, not by reading): paymentd's stop is bounded. PAYMENT_SHUTDOWN_TIMEOUT
// is how long it waits for the calls in flight, and past it the process stops anyway. The entitlement cache's subscription is closed
// after that, and closing a Redis Streams subscription waits for its loop, which can be inside a Redis call that a stalled Redis does
// not answer. A stack that waited for it held the whole process until Redis did: 4.5 s past a 1 s bound in the process test, long
// enough for a record that had been cut to be executed after all. The subscription is a freshness hint (a change it misses reaches
// the cache at its TTL), so it must not hold a stop.

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// stuckBus hands out subscriptions whose Close does not return until released: a loop inside a Redis call that does not come back.
type stuckBus struct{ release <-chan struct{} }

func (b stuckBus) Publish(context.Context, string, []byte) error { return nil }
func (b stuckBus) Subscribe(context.Context, string, string, mports.Handler) (mports.Subscription, error) {
	return stuckSub(b), nil
}
func (stuckBus) Pending(context.Context, string, string) (int64, time.Duration, error) {
	return 0, 0, nil
}
func (stuckBus) Trim(context.Context, string, mports.RetentionPolicy) error { return nil }

type stuckSub struct{ release <-chan struct{} }

func (s stuckSub) Close() error { <-s.release; return nil }

// slowBus hands out subscriptions that take a moment to stop and say when they have: a healthy loop finishing the handler in flight.
type slowBus struct {
	stopped *atomic.Int32
	after   time.Duration
}

func (b slowBus) Publish(context.Context, string, []byte) error { return nil }
func (b slowBus) Subscribe(context.Context, string, string, mports.Handler) (mports.Subscription, error) {
	return slowSub(b), nil
}
func (slowBus) Pending(context.Context, string, string) (int64, time.Duration, error) {
	return 0, 0, nil
}
func (slowBus) Trim(context.Context, string, mports.RetentionPolicy) error { return nil }

type slowSub slowBus

func (s slowSub) Close() error { time.Sleep(s.after); s.stopped.Add(1); return nil }

func open(t *testing.T, bus mports.Bus, log *sink) *entitlements.Stack {
	t.Helper()
	s, err := entitlements.Open(t.Context(), entitlements.Common{
		Clock: wallClock{}, Bus: bus, Log: slog.New(slog.NewTextHandler(log, nil)),
		Settings:      envcfg.Entitlements{CacheTTL: 30 * time.Second, InstanceID: "replica-1"},
		SubjectPrefix: "billing",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAStackWhoseSubscriptionIsStuckBehindARedisThatDoesNotAnswerDoesNotHoldTheProcess(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // frees the goroutine the stack abandons, so the leak check stays honest
	log := &sink{}
	s := open(t, stuckBus{release}, log)

	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close held the process for 3 s because a subscription would not stop: a stop is bounded, and a freshness hint does not get to extend it")
	}
	if out := log.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "did not stop") {
		t.Errorf("giving up on a subscription must be said, as a warning that names what it costs:\n%s", out)
	}
}

// The other half: the bound is not "do not wait". A subscription that is stopping is waited for, so a handler in flight finishes.
func TestAStackWaitsForASubscriptionThatIsStoppingAndSaysNothing(t *testing.T) {
	t.Parallel()
	var stopped atomic.Int32
	log := &sink{}
	s := open(t, slowBus{stopped: &stopped, after: 100 * time.Millisecond}, log)

	s.Close()
	if got := stopped.Load(); got != 2 {
		t.Errorf("%d of the 2 subscriptions had stopped when Close returned: a stack that does not wait cuts a handler off mid-eviction", got)
	}
	if out := log.String(); strings.Contains(out, "did not stop") {
		t.Errorf("a subscription that stopped in time was reported as stuck:\n%s", out)
	}
}
