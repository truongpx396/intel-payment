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
// thing left to do, which asking again does. A write-off the hot tier has issued and the writer has not yet booked would set the block in the
// books AFTER the operator cleared it, so the hot account is cleared only at the sequence the books were at: a drain can book one batch and
// leave more behind, and a write-off can arrive after any drain, so "the writer is caught up" is checked where it can be, at the clear.

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
	clearIt func(domain.Scope) (ports.BookUnblock, error)
}

func (b *unblockBooks) EnsureShard(_ context.Context, sh domain.Shard) (ports.ShardRecord, error) {
	b.mu.Lock()
	b.shards = append(b.shards, sh)
	b.mu.Unlock()
	return ports.ShardRecord{Shard: sh, Gen: 1}, nil
}

func (b *unblockBooks) ClearBlocked(_ context.Context, s domain.Scope) (ports.BookUnblock, error) {
	b.log.add("books")
	b.mu.Lock()
	b.scopes = append(b.scopes, s)
	b.mu.Unlock()
	return b.clearIt(s)
}

type unblockHot struct {
	ports.BalanceStore
	log      *unblockLog
	mu       sync.Mutex
	scopes   []domain.Scope
	seqs     []int64 // the sequence each clear was asked to be made at
	clearIt  func(domain.Scope, int64) (ports.HotUnblockOutcome, error)
	snapshot func() (ports.HotAccount, error)
}

func (h *unblockHot) ShardState(context.Context, domain.Shard) (ports.ShardState, error) {
	return ports.ShardState{Open: true, Gen: 1}, nil
}

func (h *unblockHot) ClearBlocked(_ context.Context, s domain.Scope, appliedSeq int64) (ports.HotUnblockOutcome, error) {
	h.log.add("hot")
	h.mu.Lock()
	h.scopes = append(h.scopes, s)
	h.seqs = append(h.seqs, appliedSeq)
	h.mu.Unlock()
	return h.clearIt(s, appliedSeq)
}

func (h *unblockHot) Snapshot(context.Context, domain.Scope) (ports.HotAccount, error) {
	h.log.add("snapshot")
	return h.snapshot()
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
	r.books = &unblockBooks{log: log, clearIt: func(domain.Scope) (ports.BookUnblock, error) {
		return ports.BookUnblock{Cleared: true, AppliedSeq: 41}, nil
	}}
	r.hot = &unblockHot{
		log:      log,
		clearIt:  func(domain.Scope, int64) (ports.HotUnblockOutcome, error) { return ports.HotUnblockCleared, nil },
		snapshot: func() (ports.HotAccount, error) { return ports.HotAccount{Cold: true}, nil }, // nothing installed it again
	}
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
	if want := []string{"drain", "books", "hot", "snapshot"}; !slices.Equal(r.log.got(), want) {
		t.Fatalf("the steps were %v, want %v: the writer is given the chance to catch up, the durable block goes before the one the gate reads, and the account is looked at after", r.log.got(), want)
	}
	if want := (UnblockResult{Books: true, Hot: true}); got != want {
		t.Fatalf("the result is %+v, want %+v", got, want)
	}
	if !slices.Equal(r.hot.seqs, []int64{41}) {
		t.Fatalf("the hot account was cleared at sequence %v, want the one the books were at, 41", r.hot.seqs)
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
			r.books.clearIt = func(domain.Scope) (ports.BookUnblock, error) {
				return ports.BookUnblock{Cleared: c.books, AppliedSeq: 41}, nil
			}
			r.hot.clearIt = func(domain.Scope, int64) (ports.HotUnblockOutcome, error) {
				if c.hot {
					return ports.HotUnblockCleared, nil
				}
				return ports.HotUnblockNothing, nil
			}
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
	r.books.clearIt = func(domain.Scope) (ports.BookUnblock, error) {
		held := booksBlocked
		booksBlocked = false
		return ports.BookUnblock{Cleared: held, AppliedSeq: 41}, nil
	}
	hotDown := true
	r.hot.clearIt = func(domain.Scope, int64) (ports.HotUnblockOutcome, error) {
		if hotDown {
			return ports.HotUnblockNothing, domain.ErrHotStoreUnavailable
		}
		if hotBlocked {
			hotBlocked = false
			return ports.HotUnblockCleared, nil
		}
		return ports.HotUnblockNothing, nil
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
	r.books.clearIt = func(domain.Scope) (ports.BookUnblock, error) { return ports.BookUnblock{}, boom }
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

// A drain books one batch and returns; more intents than a batch leave some behind, and the one that set the block may be among them. The books
// then say they are at a sequence the hot account is past, the clear is refused as a race, and the unblock drains again and looks again — and
// the hot account is cleared when the books and it are at the same sequence, and not before.
func TestAnUnblockThatRacedDrainsAgainAndClearsWhenTheBooksHaveCaughtUp(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	call := 0
	r.books.clearIt = func(domain.Scope) (ports.BookUnblock, error) {
		call++
		if call == 1 {
			return ports.BookUnblock{Cleared: true, AppliedSeq: 40}, nil // one intent behind: the write-off the hot tier issued last
		}
		return ports.BookUnblock{Cleared: false, AppliedSeq: 41}, nil // the writer has booked it, and the books say what it carried
	}
	r.hot.clearIt = func(_ domain.Scope, at int64) (ports.HotUnblockOutcome, error) {
		if at != 41 {
			return ports.HotUnblockRaced, nil
		}
		return ports.HotUnblockCleared, nil
	}
	got, err := r.w.Unblock(context.Background(), unblockScope)
	if err != nil {
		t.Fatal(err)
	}
	if want := (UnblockResult{Books: true, Hot: true}); got != want {
		t.Fatalf("the result is %+v, want %+v: the books were cleared on the first look, the hot account on the second", got, want)
	}
	if !slices.Equal(r.hot.seqs, []int64{40, 41}) {
		t.Fatalf("the hot account was asked at sequences %v, want 40 and then 41", r.hot.seqs)
	}
	if want := []string{"drain", "books", "hot", "drain", "books", "hot", "snapshot"}; !slices.Equal(r.log.got(), want) {
		t.Fatalf("the steps were %v, want %v: a race drains and looks again", r.log.got(), want)
	}
}

// A shard that never settles — a gap that is not filling, a hot tier written to faster than it is booked — does not loop for ever and does
// not clear a block it cannot be sure of: it fails, says the books are clear, and says it may be asked again.
func TestAnUnblockThatKeepsRacingGivesUpLeavingTheHotBlockAndSaysSo(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	r.hot.clearIt = func(domain.Scope, int64) (ports.HotUnblockOutcome, error) { return ports.HotUnblockRaced, nil }
	got, err := r.w.Unblock(context.Background(), unblockScope)
	if !errors.Is(err, ErrUnblockRaced) {
		t.Fatalf("came to %v, want %v", err, ErrUnblockRaced)
	}
	if !got.Books || got.Hot {
		t.Fatalf("the result is %+v: the books were cleared and the hot account was not", got)
	}
	if n := len(r.hot.seqs); n != unblockAttempts {
		t.Fatalf("the hot account was asked %d times, want %d: bounded", n, unblockAttempts)
	}
}

// A rehydrate that read the books before they were cleared can install the block after the hot account was looked at and found cold. The
// account is read once more after the clear, and a block that is back is cleared again.
func TestABlockInstalledAfterTheClearIsClearedAgain(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	looks := 0
	r.hot.snapshot = func() (ports.HotAccount, error) {
		looks++
		return ports.HotAccount{Seq: 41, Blocked: looks == 1}, nil // blocked the first time we look, clear the second
	}
	got, err := r.w.Unblock(context.Background(), unblockScope)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Hot || len(r.hot.seqs) != 2 {
		t.Fatalf("%+v after %d hot clears, want the block cleared the second time", got, len(r.hot.seqs))
	}
}

func TestAnUnblockWhoseAccountCannotBeReadAfterwardsSaysSoAndKeepsWhatItDid(t *testing.T) {
	t.Parallel()
	r := newUnblockRig(t)
	boom := errors.New("the hot tier went away")
	r.hot.snapshot = func() (ports.HotAccount, error) { return ports.HotAccount{}, boom }
	got, err := r.w.Unblock(context.Background(), unblockScope)
	if !errors.Is(err, boom) || !got.Books || !got.Hot {
		t.Fatalf("%+v, %v: want what was done kept, and the error reported", got, err)
	}
}
