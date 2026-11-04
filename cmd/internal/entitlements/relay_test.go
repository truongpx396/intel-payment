package entitlements_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/entitlements"
)

// R105 (found by running the whole integration suite): the config relay tells every replica's cache that configuration changed. It
// is a freshness hint — a change it cannot announce reaches caches at their TTL — and the process that runs it is the sole durable
// writer, which books money. So a relay that cannot start must be LOUD and RETRIED, and must never stop the writer: a worker
// that crash-looped over a hint would delay booking, and one that failed on the relay would hide the failure that matters (the
// low-balance subscription it cannot join) behind one that does not.

type sink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *sink) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.buf.Write(p) }
func (s *sink) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.buf.String() }

func TestARelayThatCannotStartIsLoggedAndRetriedAndNeverStopsTheCaller(t *testing.T) {
	t.Parallel()
	log := &sink{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// No pool: the relay cannot start, and never will.
		entitlements.RunRelay(ctx, nil, nil, "billing", slog.New(slog.NewTextHandler(log, nil)), entitlements.RelayRetry{Initial: 5 * time.Millisecond, Max: 20 * time.Millisecond})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(log.String(), "config relay cannot start") < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the relay was not retried (it is logged once, or not at all):\n%s", log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out := log.String(); !strings.Contains(out, "TTL") || !strings.Contains(out, "level=ERROR") {
		t.Errorf("a relay that cannot start must say, as an error, what that costs (caches wait for their TTL):\n%s", out)
	}
	select {
	case <-done:
		t.Fatal("RunRelay returned while its context was live: a relay that gives up is a relay nobody notices is gone")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRelay did not return when its context was cancelled")
	}
}

// A relay told to stop before it started does not start.
func TestARelayWhoseContextIsAlreadyDoneReturnsAtOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		entitlements.RunRelay(ctx, nil, nil, "billing", slog.New(slog.NewTextHandler(&sink{}, nil)), entitlements.RelayRetry{})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRelay ignored an already-cancelled context")
	}
}
