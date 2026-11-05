package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var epoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// metrics records counts and gauges by name and labels.
type metrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64
	obs    map[string]int
}

func newMetrics() *metrics {
	return &metrics{counts: map[string]int64{}, gauges: map[string]int64{}, obs: map[string]int{}}
}

func key(name string, l ports.Labels) string {
	return fmt.Sprintf("%s{%s%s%s%s}", name, l["type"], l["outcome"], l["realm"], l["endpoint"])
}

func (m *metrics) Count(name string, n int64, l ports.Labels) {
	m.mu.Lock()
	m.counts[key(name, l)] += n
	m.mu.Unlock()
}
func (m *metrics) Gauge(name string, v int64, l ports.Labels) {
	m.mu.Lock()
	m.gauges[key(name, l)] = v
	m.mu.Unlock()
}
func (m *metrics) Observe(name string, _ float64, l ports.Labels) {
	m.mu.Lock()
	m.obs[key(name, l)]++
	m.mu.Unlock()
}
func (m *metrics) count(name string, l ports.Labels) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[key(name, l)]
}

// request is what the sender was asked to send.
type request struct {
	URL    string
	Header map[string]string
	Body   []byte
	At     time.Time
}

// sender answers every request with the status (or error) a test chose, and remembers each.
type sender struct {
	mu     sync.Mutex
	clock  *contracts.Clock
	status func(url string) (int, error)
	reqs   []request
}

func (s *sender) Send(_ context.Context, url string, header map[string]string, body []byte) (int, error) {
	s.mu.Lock()
	h := map[string]string{}
	for k, v := range header {
		h[k] = v
	}
	s.reqs = append(s.reqs, request{url, h, append([]byte(nil), body...), s.clock.Now()})
	f := s.status
	s.mu.Unlock()
	if f == nil {
		return 200, nil
	}
	return f(url)
}

func (s *sender) sent() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]request(nil), s.reqs...)
}

// secrets resolves env:NAME from a map.
type secrets map[string]string

func (s secrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, fmt.Errorf("no such secret %s", ref)
	}
	return []byte(v), nil
}

// store counts the writes a tick makes.
type store struct {
	*contracts.MemStore
	mu     sync.Mutex
	writes map[string]int
}

func newStore(clock *contracts.Clock) *store {
	return &store{MemStore: contracts.NewMemStore(clock), writes: map[string]int{}}
}

func (s *store) note(what string) {
	s.mu.Lock()
	s.writes[what]++
	s.mu.Unlock()
}

func (s *store) Delivered(ctx context.Context, id string, status int, at time.Time) error {
	s.note("delivered")
	return s.MemStore.Delivered(ctx, id, status, at)
}

func (s *store) Failed(ctx context.Context, id string, f events.Failure) error {
	s.note("failed")
	return s.MemStore.Failed(ctx, id, f)
}

func (s *store) wrote(what string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[what]
}

var errBus = errors.New("bus down")

type bus struct {
	ports.Bus
	mu     sync.Mutex
	joined []string
	failOn string
}

func (b *bus) Subscribe(_ context.Context, subject, group string, _ ports.Handler) (ports.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failOn != "" && subject == b.failOn {
		return nil, errBus
	}
	b.joined = append(b.joined, subject+" as "+group)
	return closer{}, nil
}

type closer struct{}

func (closer) Close() error { return nil }
