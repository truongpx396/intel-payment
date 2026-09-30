//go:build integration

package natsjetstream_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/natsjetstream"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/natsjetstream/natstest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var (
	server *natstest.Server
	rds    *redistest.Redis // the outbox the relay reads
	seq    atomic.Int64
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if server, err = natstest.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if rds, err = redistest.Run(ctx, redistest.Standalone); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = server.Terminate(ctx)
		os.Exit(1)
	}
	code := m.Run()
	_ = rds.Terminate(ctx)
	_ = server.Terminate(ctx)
	if code == 0 {
		if err := goleak.Find(
			goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func connect(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := server.Connect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return nc
}

func newBus(t *testing.T, o contracts.BusOptions) *natsjetstream.Bus {
	t.Helper()
	b, err := natsjetstream.NewBus(context.Background(), connect(t), natsjetstream.BusOptions{
		Clock: system.Clock{}, Prefix: o.Prefix, AckWait: o.AckWait, MaxAttempts: o.MaxAttempts, BackoffBase: o.Backoff, Batch: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The very suite the Redis Streams bus runs: that is what makes the two swappable (SC-014).
func TestBusContract(t *testing.T) {
	t.Parallel()
	contracts.BusContract(t, func(t *testing.T, o contracts.BusOptions) contracts.BusHarness {
		return contracts.BusHarness{Bus: newBus(t, o), Peer: func(t *testing.T) ports.Bus { return newBus(t, o) }}
	})
}

func TestNewBusRefusesWhatItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	if _, err := natsjetstream.NewBus(context.Background(), nil, natsjetstream.BusOptions{Clock: system.Clock{}}); err == nil {
		t.Fatal("no connection")
	}
	if _, err := natsjetstream.NewBus(context.Background(), connect(t), natsjetstream.BusOptions{}); err == nil {
		t.Fatal("no clock")
	}
}

func TestPublishingOutsideTheStreamIsRefused(t *testing.T) {
	t.Parallel()
	o := contracts.BusOptions{Prefix: fmt.Sprintf("out%d", seq.Add(1))}
	b := newBus(t, o)
	if err := b.Publish(context.Background(), "elsewhere.warn.a", []byte("x")); err == nil {
		t.Fatal("a subject outside <prefix>.> would be silently dropped by JetStream; it must be an error")
	}
}

// A message whose consumers keep dying on it is never silently retried for ever: once it has been
// delivered more often than MaxAttempts it goes to the DLQ without another attempt.
func TestAMessageDeliveredTooOftenIsDeadLetteredWithoutAnotherAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := contracts.BusOptions{Prefix: fmt.Sprintf("dead%d", seq.Add(1)), AckWait: 100 * time.Millisecond, MaxAttempts: 3, Backoff: 10 * time.Millisecond}
	bus := newBus(t, o)
	subj := o.Prefix + ".payment.a"
	if err := bus.Publish(ctx, subj, []byte("crashy")); err != nil {
		t.Fatal(err)
	}

	// Consumers that took it and died: five deliveries, none acknowledged.
	js, err := jetstream.New(connect(t))
	if err != nil {
		t.Fatal(err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, strings.ToUpper(o.Prefix), jetstream.ConsumerConfig{
		Durable: natsjetstream.ConsumerName("g", subj), FilterSubject: subj, AckPolicy: jetstream.AckExplicitPolicy,
		AckWait: 100 * time.Millisecond, MaxDeliver: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		got := 0
		deadline := time.Now().Add(5 * time.Second)
		for got == 0 && time.Now().Before(deadline) {
			b, err := cons.Fetch(1, jetstream.FetchMaxWait(300*time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			for range b.Messages() {
				got++ // taken, never acknowledged
			}
		}
		if got == 0 {
			t.Fatalf("delivery %d never came", i+1)
		}
	}

	var calls atomic.Int64
	var mu sync.Mutex
	var dead [][]byte
	s1, err := bus.Subscribe(ctx, subj, "g", func(context.Context, ports.Message) error { calls.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s1.Close() })
	s2, err := bus.Subscribe(ctx, o.Prefix+".dlq.>", "dlq", func(_ context.Context, m ports.Message) error {
		mu.Lock()
		dead = append(dead, m.Payload)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n := len(dead)
		mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no dead letter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() != 0 {
		t.Fatalf("the handler was called %d times for a message past its attempts", calls.Load())
	}
}
