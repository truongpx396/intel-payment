package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// fakeBus delivers each publication, synchronously, to every subscription whose pattern matches (NATS tokens: `*` is one token,
// `>` is the rest), so a test can say "the announcement arrived" and look at the cache the next line. Every subscription is its
// own group here: a cache replica needs to see every message, not share them with the others.
type fakeBus struct {
	mu        sync.Mutex
	subs      []*fakeSub
	published []published
	failSub   int // the Nth Subscribe fails (1-based); 0 never
	nsub      int
}

type published struct {
	Subject string
	Payload []byte
}

type fakeSub struct {
	bus     *fakeBus
	pattern string
	group   string
	h       mports.Handler
	closed  bool
}

func (s *fakeSub) Close() error {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.closed = true
	return nil
}

func (b *fakeBus) Publish(ctx context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	b.published = append(b.published, published{subject, append([]byte(nil), payload...)})
	var targets []*fakeSub
	for _, s := range b.subs {
		if !s.closed && matches(s.pattern, subject) {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()
	for _, s := range targets {
		if err := s.h(ctx, mports.Message{Subject: subject, Payload: payload}); err != nil {
			return err
		}
	}
	return nil
}

// Inject hands a message to every live subscription's handler whatever its pattern: a bus that delivers more than was asked
// for, which a handler must not take for an invalidation it was not subscribed to.
func (b *fakeBus) Inject(ctx context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	var targets []*fakeSub
	for _, s := range b.subs {
		if !s.closed {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()
	var errs []error
	for _, s := range targets {
		errs = append(errs, s.h(ctx, mports.Message{Subject: subject, Payload: payload}))
	}
	return errors.Join(errs...)
}

func (b *fakeBus) Subscribe(_ context.Context, subject, group string, h mports.Handler) (mports.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nsub++
	if b.failSub != 0 && b.nsub == b.failSub {
		return nil, errors.New("bus: cannot subscribe")
	}
	s := &fakeSub{bus: b, pattern: subject, group: group, h: h}
	b.subs = append(b.subs, s)
	return s, nil
}

func (b *fakeBus) Pending(context.Context, string, string) (int64, time.Duration, error) {
	return 0, 0, nil
}
func (b *fakeBus) Trim(context.Context, string, mports.RetentionPolicy) error { return nil }

func (b *fakeBus) live() []*fakeSub {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*fakeSub
	for _, s := range b.subs {
		if !s.closed {
			out = append(out, s)
		}
	}
	return out
}

func (b *fakeBus) messages() []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]published(nil), b.published...)
}

func matches(pattern, subject string) bool {
	p, s := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, tok := range p {
		switch {
		case tok == ">":
			return len(s) > i
		case i >= len(s):
			return false
		case tok != "*" && tok != s[i]:
			return false
		}
	}
	return len(p) == len(s)
}

// recorder counts what the Entitler reports.
type recorder struct {
	mu     sync.Mutex
	counts map[string]int64
}

func newRecorder() *recorder { return &recorder{counts: map[string]int64{}} }

func (r *recorder) Count(name string, n int64, l mports.Labels) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := name
	for _, k := range []string{"realm", "outcome", "result", "source"} {
		if v, ok := l[k]; ok {
			key += "|" + k + "=" + v
		}
	}
	r.counts[key] += n
}
func (r *recorder) Gauge(string, int64, mports.Labels)     {}
func (r *recorder) Observe(string, float64, mports.Labels) {}

func (r *recorder) get(key string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[key]
}
