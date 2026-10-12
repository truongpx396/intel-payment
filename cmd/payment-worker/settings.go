package main

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/metering"
)

// settings is everything the worker is told, parsed once at start from PAYMENT_* variables.
type settings struct {
	Engine metering.Config // defaults applied

	DrainInterval         time.Duration // how long a shard with nothing to book waits before it looks again
	OwnershipRetry        time.Duration // how often unowned shards are tried, and held ones checked
	TransferRedrive       time.Duration // a transfer in transit longer than this is re-driven
	JournalReplayInterval time.Duration // how often journalled usage is replayed
	ShutdownTimeout       time.Duration // how long a stop waits for the batch in hand
	OpsAddr               string        // /healthz and /metrics
	LogLevel              slog.Level
}

// parseSettings reads and checks the whole environment. Every problem is reported at once, each naming the
// variable to fix, and none echoes a credential.
func parseSettings(getenv func(string) string) (settings, error) {
	r := envcfg.NewReader(getenv)
	cfg := r.Engine()
	bus := r.Bus()
	if bus.Kind == envcfg.BusJetStream {
		r.Fail("PAYMENT_BUS", "nats_jetstream is not read by payment-worker yet: it drains the Redis outbox, so use redis_streams")
	}

	s := settings{
		DrainInterval: 250 * time.Millisecond, OwnershipRetry: 2 * time.Second, TransferRedrive: 30 * time.Second,
		JournalReplayInterval: 5 * time.Second, ShutdownTimeout: 30 * time.Second,
	}
	r.PositiveDuration("PAYMENT_DRAIN_INTERVAL", &s.DrainInterval, "a worker that never waits spins on an idle shard")
	r.PositiveDuration("PAYMENT_OWNERSHIP_RETRY", &s.OwnershipRetry, "a worker that never waits hammers the database for shards it cannot have")
	r.PositiveDuration("PAYMENT_TRANSFER_REDRIVE", &s.TransferRedrive, "a transfer in transit would be re-driven the instant it started")
	r.PositiveDuration("PAYMENT_JOURNAL_REPLAY_INTERVAL", &s.JournalReplayInterval, "journalled usage would be replayed in a tight loop")
	r.PositiveDuration("PAYMENT_SHUTDOWN_TIMEOUT", &s.ShutdownTimeout, "a stop that waits for nothing abandons the batch in hand")
	s.OpsAddr = r.StrOr("PAYMENT_WORKER_ADDR", ":9091")
	if !strings.HasPrefix(s.OpsAddr, "unix:") {
		if _, _, err := net.SplitHostPort(s.OpsAddr); err != nil {
			r.Fail("PAYMENT_WORKER_ADDR", fmt.Sprintf("must be host:port or unix:/path, got %q", s.OpsAddr))
		}
	}
	s.LogLevel = r.LogLevel()

	if err := r.Err(); err != nil {
		return settings{}, err
	}
	s.Engine = cfg.WithDefaults()
	return s, nil
}
