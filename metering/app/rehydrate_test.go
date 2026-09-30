package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Fifty requests hit one cold scope at once: they must share ONE read of the books, and every one of
// them must go on to be served.
func TestConcurrentColdRequestsShareOneRehydrate(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.books.gate = make(chan struct{})
	var cold atomic.Bool
	cold.Store(true)
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		if cold.Load() {
			return domain.Receipt{}, domain.ErrColdScope
		}
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Seq: 1}, nil
	}
	r.bal.rehyd = func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
		cold.Store(false)
		return ports.RehydrateInstalled, nil
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := event("k")
			e.IdemKey = "k" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			_, err := r.m.Record(context.Background(), e)
			errs <- err
		}(i)
	}
	time.Sleep(100 * time.Millisecond) // let them all pile up on the gate
	close(r.books.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.books.booked > 5 {
		t.Fatalf("50 concurrent cold requests read the books %d times: they must share a read", r.books.booked)
	}
}

func TestRehydrateReReadsWhenTheShardGenerationMovesUnderIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	stale := 0
	r.bal.rehyd = func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
		stale++
		if stale <= 2 {
			r.books.mu.Lock()
			r.books.gen++ // a recovery bumped the shard while we were reading
			r.books.mu.Unlock()
			return ports.RehydrateStaleGen, nil
		}
		return ports.RehydrateInstalled, nil
	}
	calls := 0
	r.bal.debit = func(domain.Charge) (domain.Receipt, error) {
		calls++
		if calls == 1 {
			return domain.Receipt{}, domain.ErrColdScope
		}
		return domain.Receipt{Applied: true}, nil
	}
	if _, err := r.m.Record(context.Background(), event("k")); err != nil {
		t.Fatal(err)
	}
	if len(r.bal.rehydOn) != 3 || r.bal.rehydOn[2].ShardGen <= r.bal.rehydOn[0].ShardGen {
		t.Fatalf("each attempt must read the CURRENT generation: %+v", r.bal.rehydOn)
	}
}

func TestRehydrateGivesUpWhenTheGenerationNeverSettles(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.rehyd = func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
		return ports.RehydrateStaleGen, nil
	}
	r.bal.debit = func(domain.Charge) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrColdScope }
	rc, err := r.m.Record(context.Background(), event("k"))
	if err != nil || !rc.Deferred {
		t.Fatalf("a shard that will not settle is an outage: the usage is journaled, not lost: %+v %v", rc, err)
	}
}

func TestRehydrateSurfacesAHotRefusal(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.bal.grant = func(domain.Grant) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrColdScope }
	r.bal.rehyd = func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
		return 0, errors.New("redis exploded")
	}
	if _, err := r.m.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 1, IdemKey: "k", Reason: "x"}); err == nil {
		t.Fatal("a rehydrate failure is the caller's error")
	}
}
