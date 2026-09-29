//go:build integration

package redisstreams_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func newBus(t *testing.T, mode redistest.Mode, o contracts.BusOptions, consumer string) (*redisstreams.Bus, goredis.UniversalClient) {
	t.Helper()
	rdb := servers[mode].Client()
	t.Cleanup(func() { _ = rdb.Close() })
	b, err := redisstreams.NewBus(rdb, redisstreams.BusOptions{
		Clock: system.Clock{}, Prefix: o.Prefix, Consumer: consumer, AckWait: o.AckWait, MaxAttempts: o.MaxAttempts,
		BackoffBase: o.Backoff, Block: 100 * time.Millisecond, Batch: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, rdb
}

// The same suite runs against JetStream (natsjetstream) — that is what makes the bus swappable.
func TestBusContract(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			contracts.BusContract(t, func(t *testing.T, o contracts.BusOptions) contracts.BusHarness {
				b, _ := newBus(t, mode, o, "c1")
				return contracts.BusHarness{Bus: b, Peer: func(t *testing.T) ports.Bus {
					p, _ := newBus(t, mode, o, "c2")
					return p
				}}
			})
		})
	}
}

// blocker is a handler that acknowledges what it is given until it reaches the n-th delivery, holds
// that one until released, and records everything.
type blocker struct {
	mu      sync.Mutex
	got     []string
	n       atomic.Int64
	holdAt  int64
	release chan struct{}
}

func (b *blocker) handle(_ context.Context, m ports.Message) error {
	b.mu.Lock()
	b.got = append(b.got, string(m.Payload))
	b.mu.Unlock()
	if b.holdAt > 0 && b.n.Add(1) == b.holdAt {
		<-b.release
	}
	return nil
}

func (b *blocker) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.got) }

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Adapter-specific: the contract can only assert that nothing un-acknowledged is lost. Here the
// stream itself is read, to show the acknowledged history really goes, and only that.
func TestTrimRemovesOnlyWhatEveryGroupHasAcknowledged(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := contracts.BusOptions{Prefix: fmt.Sprintf("trim%d", seq.Add(1)), AckWait: time.Minute, MaxAttempts: 3, Backoff: time.Second}
			bus, rdb := newBus(t, mode, o, "c1")
			subj := o.Prefix + ".payment.a"
			length := func() int64 { n, _ := rdb.XLen(ctx, o.Prefix+":events").Result(); return n }

			for i := 0; i < 10; i++ {
				if err := bus.Publish(ctx, subj, []byte(fmt.Sprintf("m%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			if err := bus.Trim(ctx, subj, ports.RetentionPolicy{}); err != nil || length() != 10 {
				t.Fatalf("a stream nobody has read is not trimmed: length %d %v", length(), err)
			}

			all := &blocker{}
			slow := &blocker{holdAt: 3, release: make(chan struct{})}
			for group, h := range map[string]ports.Handler{"all": all.handle, "slow": slow.handle} {
				s, err := bus.Subscribe(ctx, subj, group, h)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
			}
			waitUntil(t, "the fast group to finish and the slow one to hold its third", func() bool { return all.count() == 10 && slow.count() >= 3 })

			if err := bus.Trim(ctx, subj, ports.RetentionPolicy{}); err != nil {
				t.Fatal(err)
			}
			if got := length(); got != 8 {
				t.Fatalf("the slow group holds its third entry un-acknowledged: entries 1–2 may go, 3–10 may not; length %d", got)
			}
			if err := bus.Trim(ctx, subj, ports.RetentionPolicy{MinAge: time.Hour}); err != nil || length() != 8 {
				t.Fatalf("history younger than MinAge is kept: %d %v", length(), err)
			}

			close(slow.release)
			waitUntil(t, "the slow group to finish", func() bool { return slow.count() == 10 })
			waitUntil(t, "everything acknowledged", func() bool {
				n, _, err := bus.Pending(ctx, subj, "slow")
				return err == nil && n == 0
			})
			// MaxLen is a length to trim TOWARD: it removes what is over it, not everything.
			if err := bus.Trim(ctx, subj, ports.RetentionPolicy{MaxLen: 4}); err != nil {
				t.Fatal(err)
			}
			if got := length(); got != 4 {
				t.Fatalf("length %d, want 4", got)
			}
			if err := bus.Trim(ctx, subj, ports.RetentionPolicy{}); err != nil || length() != 0 {
				t.Fatalf("fully acknowledged history past the floor goes: %d %v", length(), err)
			}
		})
	}
}

func TestTheOutboxIsTrimmedOnlyPastEveryGroupAndTheFloor(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rdb := servers[mode].Client()
			t.Cleanup(func() { _ = rdb.Close() })
			// A shard no other test uses, so the stream is this test's alone. The hot functions are the
			// only real writer of an outbox; here a stand-in fills it, which is all Trim needs to see.
			sh := domain.Shard(9000 + seq.Add(1))
			key := fmt.Sprintf("billing:outbox:{s%d}", sh)
			for i := 0; i < 6; i++ {
				if err := rdb.XAdd(ctx, &goredis.XAddArgs{Stream: key, Values: map[string]any{"n": i}}).Err(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := redisstreams.NewStream(rdb, redisstreams.IntentOptions{Consumer: "w1", Group: fmt.Sprintf("trim-%d", seq.Add(1)), Clock: system.Clock{}})
			if err != nil {
				t.Fatal(err)
			}
			length := func() int64 { n, _ := rdb.XLen(ctx, key).Result(); return n }

			if n, err := s.Trim(ctx, sh, ports.RetentionPolicy{}); err != nil || n != 0 {
				t.Fatalf("a stream no group has read: removed %d %v", n, err)
			}
			ds, err := s.Read(ctx, sh, 4, time.Minute) // delivers four; two remain undelivered
			if err != nil || len(ds) != 4 {
				t.Fatalf("%d %v", len(ds), err)
			}
			if n, err := s.Trim(ctx, sh, ports.RetentionPolicy{}); err != nil || n != 0 || length() != 6 {
				t.Fatalf("delivered but un-acknowledged history stays: removed %d, length %d, %v", n, length(), err)
			}
			if err := s.Ack(ctx, sh, ds[0].ID, ds[1].ID); err != nil {
				t.Fatal(err)
			}
			if n, err := s.Trim(ctx, sh, ports.RetentionPolicy{}); err != nil || n != 2 || length() != 4 {
				t.Fatalf("acknowledged entries go, ahead of the un-acknowledged: removed %d, length %d, %v", n, length(), err)
			}
			if n, _ := s.Trim(ctx, sh, ports.RetentionPolicy{MinAge: time.Hour}); n != 0 {
				t.Fatalf("the floor keeps recent history: removed %d", n)
			}
			if err := s.Ack(ctx, sh, ds[2].ID, ds[3].ID); err != nil {
				t.Fatal(err)
			}
			if n, err := s.Trim(ctx, sh, ports.RetentionPolicy{}); err != nil || n != 2 || length() != 2 {
				t.Fatalf("the two never delivered stay: removed %d, length %d, %v", n, length(), err)
			}
		})
	}
}

// A message whose consumers keep dying on it is never silently retried for ever: once it has been
// delivered more often than MaxAttempts it goes to the DLQ without another attempt.
func TestAMessageDeliveredTooOftenIsDeadLetteredWithoutAnotherAttempt(t *testing.T) {
	t.Parallel()
	for mode := range servers {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			o := contracts.BusOptions{Prefix: fmt.Sprintf("dead%d", seq.Add(1)), AckWait: 100 * time.Millisecond, MaxAttempts: 3, Backoff: 10 * time.Millisecond}
			bus, rdb := newBus(t, mode, o, "c1")
			subj := o.Prefix + ".payment.a"
			if err := bus.Publish(ctx, subj, []byte("crashy")); err != nil {
				t.Fatal(err)
			}
			// Consumers that took it and died: four deliveries, none acknowledged.
			key, group := o.Prefix+":events", "g|"+subj
			if err := rdb.XGroupCreateMkStream(ctx, key, group, "0").Err(); err != nil {
				t.Fatal(err)
			}
			if _, err := rdb.XReadGroup(ctx, &goredis.XReadGroupArgs{Group: group, Consumer: "dead-1", Streams: []string{key, ">"}, Count: 1, Block: -1}).Result(); err != nil {
				t.Fatal(err)
			}
			for i := 2; i <= 4; i++ {
				time.Sleep(20 * time.Millisecond)
				if err := rdb.Do(ctx, "XCLAIM", key, group, fmt.Sprintf("dead-%d", i), 0, mustPendingID(t, rdb, key, group)).Err(); err != nil {
					t.Fatal(err)
				}
			}

			var calls atomic.Int64
			var dead blocker
			s1, err := bus.Subscribe(ctx, subj, "g", func(context.Context, ports.Message) error { calls.Add(1); return nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s1.Close() })
			s2, err := bus.Subscribe(ctx, o.Prefix+".dlq.>", "dlq", dead.handle)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s2.Close() })
			waitUntil(t, "the dead letter", func() bool { return dead.count() == 1 })
			if calls.Load() != 0 {
				t.Fatalf("the handler was called %d times for a message past its attempts", calls.Load())
			}
		})
	}
}

func mustPendingID(t *testing.T, rdb goredis.UniversalClient, key, group string) string {
	t.Helper()
	p, err := rdb.XPendingExt(context.Background(), &goredis.XPendingExtArgs{Stream: key, Group: group, Start: "-", End: "+", Count: 1}).Result()
	if err != nil || len(p) != 1 {
		t.Fatalf("pending: %v %v", p, err)
	}
	return p[0].ID
}
