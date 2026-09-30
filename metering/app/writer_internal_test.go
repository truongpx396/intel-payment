package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// metricRecorder keeps the last gauge and the running total of every counter it is handed.
type metricRecorder struct {
	mu     sync.Mutex
	gauges map[string]int64
	counts map[string]int64
}

func (g *metricRecorder) Observe(string, float64, ports.Labels) {}

func (g *metricRecorder) Gauge(name string, v int64, _ ports.Labels) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gauges == nil {
		g.gauges = map[string]int64{}
	}
	g.gauges[name] = v
}

func (g *metricRecorder) Count(name string, n int64, _ ports.Labels) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.counts == nil {
		g.counts = map[string]int64{}
	}
	g.counts[name] += n
}

type statsStream struct {
	ports.IntentStream
	stats ports.StreamStats
}

func (s statsStream) Stats(context.Context, domain.Shard) (ports.StreamStats, error) {
	return s.stats, nil
}

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

func writerOver(t *testing.T, stream ports.IntentStream, met ports.Metrics) *Writer {
	t.Helper()
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}
	w, err := NewWriter(cfg, Deps{
		Balance: struct{ ports.BalanceStore }{}, Books: struct{ ports.BookStore }{}, Bus: struct{ ports.Bus }{},
		Stream: stream, Clock: fixedClock(time.Unix(1_700_000_000, 0)), Metrics: met,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// A writer handed a Metrics recorder must report into it — the outbox depth and age are what the
// pages are built on (invariant 15) — and a writer handed none must still run.
func TestTheWriterReportsTheOutboxToTheMetricsItWasGiven(t *testing.T) {
	t.Parallel()
	rec := &metricRecorder{}
	stream := statsStream{stats: ports.StreamStats{Pending: 3, Undelivered: 4, OldestPendingAge: 9 * time.Second, OldestUndelivered: 12 * time.Second}}
	writerOver(t, stream, rec).observe(context.Background(), 2)
	if got := rec.gauges[ports.MetricOutboxDepth]; got != 7 {
		t.Fatalf("outbox depth is pending + undelivered = 7, got %d (the recorder was not used)", got)
	}
	if got := rec.gauges[ports.MetricOutboxAge]; got != 12 {
		t.Fatalf("outbox age is the older of the two, 12s, got %d", got)
	}
	writerOver(t, stream, nil).observe(context.Background(), 2) // no recorder: a no-op, never a nil dereference
}
