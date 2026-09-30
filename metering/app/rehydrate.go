package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// rehydrator rebuilds a stale or brand-new hot account from the books: booked balances plus open
// suspense, at the watermark's applied_seq (hot-path-consistency.md §5).
//
// It is safe to run at any time an account is COLD, because a cold account cannot have taken a
// mutation since it went cold — every function refuses it — so there is no intent in flight that
// the books have not seen. Recovery drains a shard BEFORE bumping its generation for exactly that
// reason. The hot function installs only while the account is still stale, so any number of
// callers, in this process or another, converge on one rebuild.
type rehydrator struct {
	cfg  metering.Config
	d    Deps
	met  ports.Metrics
	mu   sync.Mutex
	call map[string]*flight
}

type flight struct {
	done chan struct{}
	err  error
}

func newRehydrator(cfg metering.Config, d Deps, met ports.Metrics) *rehydrator {
	return &rehydrator{cfg: cfg, d: d, met: met, call: map[string]*flight{}}
}

// maxRehydrateAttempts bounds re-reads when the shard's generation moves under us.
const maxRehydrateAttempts = 3

// Rehydrate rebuilds s's hot account. Concurrent calls for one scope in this process share one read
// of the books.
func (r *rehydrator) Rehydrate(ctx context.Context, s domain.Scope) error {
	s = s.Normalized()
	tag := s.Tag()
	r.mu.Lock()
	if f, ok := r.call[tag]; ok {
		r.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	r.call[tag] = f
	r.mu.Unlock()

	f.err = r.rehydrate(ctx, s)
	r.mu.Lock()
	delete(r.call, tag)
	r.mu.Unlock()
	close(f.done)
	return f.err
}

func (r *rehydrator) rehydrate(ctx context.Context, s domain.Scope) error {
	sh := s.Shard(r.cfg.Shards)
	for attempt := 0; attempt < maxRehydrateAttempts; attempt++ {
		rec, err := r.d.Books.EnsureShard(ctx, sh)
		if err != nil {
			return fmt.Errorf("rehydrate %s: %w", s.Tag(), err)
		}
		st, err := r.d.Books.Booked(ctx, s)
		if err != nil {
			return fmt.Errorf("rehydrate %s: %w", s.Tag(), err)
		}
		pools := make(map[domain.Pool]domain.Credits, len(st.Booked)+len(st.Suspense))
		for p, v := range st.Booked {
			pools[p] += v
		}
		for p, v := range st.Suspense {
			pools[p] += v // hot == booked + open suspense at every sequence number
		}
		out, err := r.d.Balance.Rehydrate(ctx, s, ports.RehydrateState{
			ShardGen: rec.Gen, AppliedSeq: st.Watermark.AppliedSeq, Blocked: st.Watermark.Blocked, Pools: pools,
		})
		if err != nil {
			return err
		}
		switch out {
		case ports.RehydrateInstalled:
			cause := "stale"
			if !st.Watermark.Exists {
				cause = "new_scope"
			}
			r.met.Count(ports.MetricRehydrate, 1, ports.Labels{"realm": string(s.Realm), "cause": cause})
			return nil
		case ports.RehydrateAlready:
			return nil
		case ports.RehydrateStaleGen:
			continue // the shard was bumped between reading Postgres and installing: read again
		}
	}
	return fmt.Errorf("%w: rehydrate %s: the shard generation kept moving", domain.ErrHotStoreUnavailable, s.Tag())
}

// heal runs op and, if the account was cold, rehydrates it and runs op once more. A second cold
// answer means another bump raced the rebuild: that is the hot tier being unavailable, not a defect.
func (r *rehydrator) heal(ctx context.Context, s domain.Scope, op func() error) error {
	err := op()
	if !errors.Is(err, domain.ErrColdScope) {
		return err
	}
	if rerr := r.Rehydrate(ctx, s); rerr != nil {
		return rerr
	}
	if err = op(); errors.Is(err, domain.ErrColdScope) {
		return fmt.Errorf("%w: account %s is still cold after a rehydrate", domain.ErrHotStoreUnavailable, s.Tag())
	}
	return err
}
