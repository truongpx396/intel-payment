package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a: an operator's unblock clears block_and_flag where it is held — in the books (durable, and what a rehydrate reads), then in the hot
// account (what the admission gate reads) — in that order, so a crash between the two leaves the books clear and the hot account the only
// thing left to do, which asking again does. Before either it drains the shard: a write-off the hot tier has issued and the writer has not
// yet booked would otherwise set the block in the books AFTER the operator cleared it, leaving the two disagreeing about it.

type unblockLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *unblockLog) add(s string) {
	l.mu.Lock()
	l.steps = append(l.steps, s)
	l.mu.Unlock()
}

func (l *unblockLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

type unblockBooks struct {
	ports.BookStore
	log     *unblockLog
	mu      sync.Mutex
	scopes  []domain.Scope
	shards  []domain.Shard
	clearIt func(domain.Scope) (bool, error)
}

func (b *unblockBooks) EnsureShard(_ context.Context, sh domain.Shard) (ports.ShardRecord, error) {
	b.mu.Lock()
	b.shards = append(b.shards, sh)
	b.mu.Unlock()
	return ports.ShardRecord{Shard: sh, Gen: 1}, nil
}

func (b *unblockBooks) ClearBlocked(_ context.Context, s domain.Scope) (bool, error) {
	b.log.add("books")
	b.mu.Lock()
	b.scopes = append(b.scopes, s)
	b.mu.Unlock()
	return b.clearIt(s)
}

type unblockHot struct {
	ports.BalanceStore
	log     *unblockLog
	mu      sync.Mutex
	scopes  []domain.Scope
	clearIt func(domain.Scope) (bool, error)
}

func (h *unblockHot) ShardState(context.Context, domain.Shard) (ports.ShardState, error) {
	return ports.ShardState{Open: true, Gen: 1}, nil
}

func (h *unblockHot) ClearBlocked(_ context.Context, s domain.Scope) (bool, error) {
	h.log.add("hot")
	h.mu.Lock()
	h.scopes = append(h.scopes, s)
	h.mu.Unlock()
	return h.clearIt(s)
}

type unblockStream struct {
	ports.IntentStream
	log     *unblockLog
	readErr error
}

func (s *unblockStream) Read(context.Context, domain.Shard, int, time.Duration) ([]ports.Delivery, error) {
	s.log.add("drain")
	return nil, s.readErr
}
func (*unblockStream) Ack(context.Context, domain.Shard, ...string) error { return nil }
func (*unblockStream) Stats(context.Context, domain.Shard) (ports.StreamStats, error) {
	return ports.StreamStats{}, nil
}

type unblockRig struct {
	w      *Writer
	log    *unblockLog
	books  *unblockBooks
	hot    *unblockHot
	stream *unblockStream
	shards int
}

func newUnblockRig(t *testing.T) *unblockRig {
	t.Helper()
	log := &unblockLog{}
	r := &unblockRig{log: log}
	r.books = &unblockBooks{log: log, clearIt: func(domain.Scope) (bool, error) { return true, nil }}
	r.hot = &unblockHot{log: log, clearIt: func(domain.Scope) (bool, error) { return true, nil }}
	r.stream = &unblockStream{log: log}
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}
	r.shards = cfg.WithDefaults().Shards
	w, err := NewWriter(cfg, Deps{Balance: r.hot, Books: r.books, Bus: struct{ ports.Bus }{}, Stream: r.stream, Clock: fixedClock(time.Unix(1_700_000_000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	r.w = w
	return r
}

var unblockScope = domain.Scope{Realm: "acme", Kind: "workspace", ID: "w-1"}

func TestAnUnblockDrainsTheShardThenClearsTheBooksThenTheHotAccount(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	got, err := r.w.Unblock(context.Background(), unblockScope)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"drain", "books", "hot"}; !slices.Equal(r.log.got(), want) {
		t.Fatalf("the steps were %v, want %v: the writer is caught up first, the durable block goes before the one the gate reads", r.log.got(), want)
	}
	if want := (UnblockResult{Books: true, Hot: true}); got != want {
		t.Fatalf("the result is %+v, want %+v", got, want)
	}
}

func TestWhatEachTierHeldIsReportedApartFromTheOther(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		books, hot bool
	}{
		{"both held it", true, true},
		{"only the books held it (the hot account was rebuilt, or never had it)", true, false},
		{"only the hot account held it (a crash fell between the tiers, and this is asking again)", false, true},
		{"neither held it", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newUnblockRig(t)
			r.books.clearIt = func(domain.Scope) (bool, error) { return c.books, nil }
			r.hot.clearIt = func(domain.Scope) (bool, error) { return c.hot, nil }
			got, err := r.w.Unblock(context.Background(), unblockScope)
			if err != nil || got != (UnblockResult{Books: c.books, Hot: c.hot}) {
				t.Fatalf("%+v, %v, want books %v, hot %v", got, err, c.books, c.hot)
			}
		})
	}
}

// The books are cleared first, so a crash — or a hot tier that cannot be reached — between the two leaves the books clear and the hot
// account holding the block, and the only thing left to do is the hot account.
func TestAnUnblockThatFellBetweenTheTiersIsRepairedByAskingAgain(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	booksBlocked, hotBlocked := true, true
	r.books.clearIt = func(domain.Scope) (bool, error) { held := booksBlocked; booksBlocked = false; return held, nil }
	hotDown := true
	r.hot.clearIt = func(domain.Scope) (bool, error) {
		if hotDown {
			return false, domain.ErrHotStoreUnavailable
		}
		held := hotBlocked
		hotBlocked = false
		return held, nil
	}

	if _, err := r.w.Unblock(context.Background(), unblockScope); !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("an unblock whose hot tier could not be reached came to %v, want it reported", err)
	}
	if booksBlocked || !hotBlocked {
		t.Fatalf("after the failed call the books hold the block: %v, the hot account: %v; want the books clear and the hot account still holding it", booksBlocked, hotBlocked)
	}

	hotDown = false
	got, err := r.w.Unblock(context.Background(), unblockScope)
	if err != nil || got != (UnblockResult{Books: false, Hot: true}) {
		t.Fatalf("asking again: %+v, %v, want only the hot account cleared", got, err)
	}
	if hotBlocked {
		t.Fatal("the hot account still holds the block after asking again")
	}
}

func TestAnUnblockThatCannotClearTheBooksNeverTouchesTheHotAccount(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	boom := errors.New("the books are down")
	r.books.clearIt = func(domain.Scope) (bool, error) { return false, boom }
	if _, err := r.w.Unblock(context.Background(), unblockScope); !errors.Is(err, boom) {
		t.Fatalf("came to %v, want the books' error", err)
	}
	if slices.Contains(r.log.got(), "hot") {
		t.Fatalf("the hot account was cleared (%v) while the books still hold the block: a rehydrate would put it back", r.log.got())
	}
}

func TestAnUnblockThatCannotDrainTheShardClearsNothing(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	boom := errors.New("the stream is down")
	r.stream.readErr = boom
	if _, err := r.w.Unblock(context.Background(), unblockScope); !errors.Is(err, boom) {
		t.Fatalf("came to %v, want the stream's error", err)
	}
	if steps := r.log.got(); slices.Contains(steps, "books") || slices.Contains(steps, "hot") {
		t.Fatalf("steps %v: a block was cleared with intents the writer had not yet booked, which may set it again", steps)
	}
}

func TestAnUnblockOfAScopeThatIsNotOneChangesNothing(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]domain.Scope{"empty": {}, "no id": {Realm: "acme", Kind: "workspace"}, "no kind": {Realm: "acme", ID: "w"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newUnblockRig(t)
			if _, err := r.w.Unblock(context.Background(), s); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("came to %v, want %v", err, domain.ErrInvalid)
			}
			if steps := r.log.got(); len(steps) != 0 {
				t.Fatalf("steps %v were taken for a scope that is not one", steps)
			}
		})
	}
}

// The default realm is spelled two ways; the shard it hashes into, and the key both tiers look it up by, must not depend on which.
func TestAnUnblockActsOnTheNormalisedScopeAndTheShardItLivesIn(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	s := domain.Scope{Kind: "workspace", ID: "w-9"} // the realm left to its default
	if _, err := r.w.Unblock(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	want := s.Normalized()
	if !reflect.DeepEqual(r.books.scopes, []domain.Scope{want}) || !reflect.DeepEqual(r.hot.scopes, []domain.Scope{want}) {
		t.Fatalf("the tiers were asked about %v and %v, want %v", r.books.scopes, r.hot.scopes, want)
	}
	if len(r.books.shards) == 0 || r.books.shards[0] != want.Shard(r.shards) {
		t.Fatalf("the shard drained was %v, want %d: the one the scope lives in", r.books.shards, want.Shard(r.shards))
	}
}
