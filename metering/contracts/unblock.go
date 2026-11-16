package contracts

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// What an operator's unblock reaches (R112a): block_and_flag is held in two places — the books' watermark (durable, and what a rehydrate
// reads) and the hot account (what the admission gate reads) — and each has a clearer of its own. The two suites below are the
// specification of those clearers. The order they are called in, and what happens when a crash falls between them, is the use case's
// (metering/app, Writer.Unblock), not theirs.

var unblockScopeSeq atomic.Int64

func ubScope(realm domain.Realm) domain.Scope {
	return domain.Scope{Realm: realm, Kind: "workspace", ID: fmt.Sprintf("ub-%d-%d", time.Now().UnixNano(), unblockScopeSeq.Add(1))}
}

// ------------------------------------------------------------------ the books --

// BooksUnblockWorld is the durable side of an unblock under test: the clearer, and what a case needs to set up and to read the watermark
// behind it. Put writes a watermark as the writer would have; Get reads it back.
type BooksUnblockWorld struct {
	Books ports.BookUnblocker
	Put   func(tb testing.TB, s domain.Scope, w ports.Watermark)
	Get   func(tb testing.TB, s domain.Scope) ports.Watermark
}

// BooksUnblockFactory builds a world. Cases take scopes no other case uses, so a factory may hand every case the same database.
type BooksUnblockFactory func(tb testing.TB) BooksUnblockWorld

// BooksUnblockCase is one named obligation of ports.BookUnblocker.
type BooksUnblockCase struct {
	Name string
	Run  func(tb testing.TB, f BooksUnblockFactory)
}

// The names of the cases, so a test that plants a defect can say which case must catch it.
const (
	CaseBUClears       = "a held block is cleared and said to be, and nothing else about the watermark moves"
	CaseBUNotBlocked   = "a scope that is not blocked is left exactly as it is, and is not said to have been cleared"
	CaseBUUnknown      = "a scope the books have never heard of is not invented"
	CaseBUTwice        = "books: asking again is asking again, and the second call finds nothing to clear"
	CaseBUOnlyThat     = "only the scope named is cleared: not its neighbours, not the same name in another realm"
	CaseBUTogether     = "books: sixteen callers at once clear the block once, and exactly one of them is told so"
	CaseBUInvalidScope = "a scope that is not a scope is refused and clears nothing"
	CaseBUCancelled    = "a call that is cancelled clears nothing"
)

// BooksUnblockCases are the obligations of the books' side of an unblock.
func BooksUnblockCases() []BooksUnblockCase {
	return []BooksUnblockCase{
		{CaseBUClears, buClears},
		{CaseBUNotBlocked, buNotBlocked},
		{CaseBUUnknown, buUnknown},
		{CaseBUTwice, buTwice},
		{CaseBUOnlyThat, buOnlyThat},
		{CaseBUTogether, buTogether},
		{CaseBUInvalidScope, buInvalidScope},
		{CaseBUCancelled, buCancelled},
	}
}

// BooksUnblockContract runs every case, in parallel, each on scopes of its own.
func BooksUnblockContract(t *testing.T, f BooksUnblockFactory) {
	t.Helper()
	for _, c := range BooksUnblockCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

func buClear(tb testing.TB, w BooksUnblockWorld, s domain.Scope) bool {
	tb.Helper()
	cleared, err := w.Books.ClearBlocked(context.Background(), s)
	if err != nil {
		tb.Fatalf("ClearBlocked(%s): %v", s.Tag(), err)
	}
	return cleared
}

func buClears(tb testing.TB, f BooksUnblockFactory) {
	w, s := f(tb), ubScope("t1")
	w.Put(tb, s, ports.Watermark{Exists: true, Gen: 3, AppliedSeq: 41, Blocked: true})
	if !buClear(tb, w, s) {
		tb.Fatal("a block was held and ClearBlocked said it cleared nothing")
	}
	got := w.Get(tb, s)
	if want := (ports.Watermark{Exists: true, Gen: 3, AppliedSeq: 41}); got != want {
		tb.Fatalf("the watermark is %+v, want %+v: clearing the block moves nothing else", got, want)
	}
}

func buNotBlocked(tb testing.TB, f BooksUnblockFactory) {
	w, s := f(tb), ubScope("t1")
	before := ports.Watermark{Exists: true, Gen: 2, AppliedSeq: 9}
	w.Put(tb, s, before)
	if buClear(tb, w, s) {
		tb.Fatal("ClearBlocked said it cleared a block the scope did not hold")
	}
	if got := w.Get(tb, s); got != before {
		tb.Fatalf("the watermark is %+v after a clear of nothing, was %+v", got, before)
	}
}

func buUnknown(tb testing.TB, f BooksUnblockFactory) {
	w, s := f(tb), ubScope("t1")
	if buClear(tb, w, s) {
		tb.Fatal("ClearBlocked said it cleared a scope that has no watermark")
	}
	if got := w.Get(tb, s); got.Exists {
		tb.Fatalf("ClearBlocked wrote a watermark for a scope the books had none for: %+v", got)
	}
}

func buTwice(tb testing.TB, f BooksUnblockFactory) {
	w, s := f(tb), ubScope("t1")
	w.Put(tb, s, ports.Watermark{Exists: true, Gen: 1, AppliedSeq: 5, Blocked: true})
	first, second := buClear(tb, w, s), buClear(tb, w, s)
	if !first || second {
		tb.Fatalf("two calls said %v then %v, want true then false", first, second)
	}
	if got := w.Get(tb, s); got.Blocked || got.AppliedSeq != 5 {
		tb.Fatalf("after two calls the watermark is %+v", got)
	}
}

func buOnlyThat(tb testing.TB, f BooksUnblockFactory) {
	w := f(tb)
	target, neighbour := ubScope("t1"), ubScope("t1")
	elsewhere := domain.Scope{Realm: "t2", Kind: target.Kind, ID: target.ID}
	held := ports.Watermark{Exists: true, Gen: 1, AppliedSeq: 7, Blocked: true}
	for _, s := range []domain.Scope{target, neighbour, elsewhere} {
		w.Put(tb, s, held)
	}
	// A realm with no such scope has nothing to clear, however many other realms have a scope of that name.
	if buClear(tb, w, domain.Scope{Realm: "t3", Kind: target.Kind, ID: target.ID}) {
		tb.Fatal("a scope of a realm that has none was said to be cleared")
	}
	for name, s := range map[string]domain.Scope{"the scope of another realm": target, "the one of a third": elsewhere} {
		if got := w.Get(tb, s); got != held {
			tb.Fatalf("clearing a scope no realm holds changed %s to %+v", name, got)
		}
	}
	if !buClear(tb, w, target) {
		tb.Fatal("the named scope was not cleared")
	}
	if w.Get(tb, target).Blocked {
		tb.Fatal("the named scope is still blocked")
	}
	for name, s := range map[string]domain.Scope{"a neighbour in the realm": neighbour, "the same name in another realm": elsewhere} {
		if got := w.Get(tb, s); got != held {
			tb.Fatalf("%s was changed to %+v: only the scope named is the operator's to clear", name, got)
		}
	}
}

func buTogether(tb testing.TB, f BooksUnblockFactory) {
	w, s := f(tb), ubScope("t1")
	w.Put(tb, s, ports.Watermark{Exists: true, Gen: 1, AppliedSeq: 12, Blocked: true})
	var wg sync.WaitGroup
	var told atomic.Int64
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cleared, err := w.Books.ClearBlocked(context.Background(), s)
			if err != nil {
				tb.Errorf("ClearBlocked: %v", err)
				return
			}
			if cleared {
				told.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := told.Load(); n != 1 {
		tb.Fatalf("%d of sixteen concurrent callers were told they cleared the block, want exactly 1", n)
	}
	if got := w.Get(tb, s); got.Blocked || got.AppliedSeq != 12 {
		tb.Fatalf("after sixteen concurrent clears the watermark is %+v", got)
	}
}

func buInvalidScope(tb testing.TB, f BooksUnblockFactory) {
	w := f(tb)
	for name, s := range map[string]domain.Scope{"empty": {}, "no id": {Realm: "t1", Kind: "workspace"}, "no kind": {Realm: "t1", ID: "x"}} {
		if cleared, err := w.Books.ClearBlocked(context.Background(), s); !errors.Is(err, domain.ErrInvalid) || cleared {
			tb.Fatalf("a scope with %s: cleared %v, err %v, want %v", name, cleared, err, domain.ErrInvalid)
		}
	}
}

func buCancelled(tb testing.TB, f BooksUnblockFactory) {
	w, s := f(tb), ubScope("t1")
	held := ports.Watermark{Exists: true, Gen: 1, AppliedSeq: 3, Blocked: true}
	w.Put(tb, s, held)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if cleared, err := w.Books.ClearBlocked(ctx, s); err == nil || cleared {
		tb.Fatalf("a cancelled call: cleared %v, err %v: it must fail, not claim a clear", cleared, err)
	}
	if got := w.Get(tb, s); got != held {
		tb.Fatalf("a cancelled call changed the watermark to %+v", got)
	}
}

// -------------------------------------------------------------------- the hot tier --

// HotUnblockWorld is the hot side of an unblock under test. NewScope returns a scope that has a shard of its own — open, at generation
// one, with no account in it — so a case that freezes the shard or moves its generation on cannot reach another case's account.
type HotUnblockWorld struct {
	Hot      ports.HotUnblocker
	NewScope func(tb testing.TB) domain.Scope
	// Install makes the scope's account live in the shard's current generation, as a rehydrate does, with or without the block.
	Install func(tb testing.TB, s domain.Scope, blocked bool)
	Account func(tb testing.TB, s domain.Scope) ports.HotAccount
	// Stale moves the shard's generation on: every account in it is now one built in an older generation.
	Stale func(tb testing.TB, s domain.Scope)
	// Freeze freezes the shard, or lifts the freeze.
	Freeze func(tb testing.TB, s domain.Scope, frozen bool)
}

// HotUnblockFactory builds a world.
type HotUnblockFactory func(tb testing.TB) HotUnblockWorld

// HotUnblockCase is one named obligation of ports.HotUnblocker.
type HotUnblockCase struct {
	Name string
	Run  func(tb testing.TB, f HotUnblockFactory)
}

// The names of the cases.
const (
	CaseHUClears     = "a live account that holds the block is cleared and said to be, and its sequence and balances do not move"
	CaseHUNotBlocked = "a live account that holds no block is left as it is, and is not said to have been cleared"
	CaseHUNoAccount  = "a scope with no account has nothing to clear, and is not made one"
	CaseHUStale      = "an account built in an older generation is not touched: it is rebuilt from the books, which are already clear"
	CaseHUFrozen     = "a frozen shard is refused as frozen and nothing is cleared"
	CaseHUTwice      = "hot: asking again is asking again, and the second call finds nothing to clear"
	CaseHUTogether   = "hot: sixteen callers at once clear the block once, and exactly one of them is told so"
)

// HotUnblockCases are the obligations of the hot tier's side of an unblock.
func HotUnblockCases() []HotUnblockCase {
	return []HotUnblockCase{
		{CaseHUClears, huClears},
		{CaseHUNotBlocked, huNotBlocked},
		{CaseHUNoAccount, huNoAccount},
		{CaseHUStale, huStale},
		{CaseHUFrozen, huFrozen},
		{CaseHUTwice, huTwice},
		{CaseHUTogether, huTogether},
	}
}

// HotUnblockContract runs every case, in parallel, each on a shard of its own.
func HotUnblockContract(t *testing.T, f HotUnblockFactory) {
	t.Helper()
	for _, c := range HotUnblockCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

func huClear(tb testing.TB, w HotUnblockWorld, s domain.Scope) bool {
	tb.Helper()
	cleared, err := w.Hot.ClearBlocked(context.Background(), s)
	if err != nil {
		tb.Fatalf("ClearBlocked(%s): %v", s.Tag(), err)
	}
	return cleared
}

func huClears(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	w.Install(tb, s, true)
	before := w.Account(tb, s)
	if !before.Blocked || before.Cold {
		tb.Fatalf("the case could not set up a live, blocked account: %+v", before)
	}
	if !huClear(tb, w, s) {
		tb.Fatal("a block was held and ClearBlocked said it cleared nothing")
	}
	after := w.Account(tb, s)
	want := before
	want.Blocked = false
	if !reflect.DeepEqual(after, want) {
		tb.Fatalf("the account is %+v, want %+v: clearing the block moves nothing else", after, want)
	}
}

func huNotBlocked(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	w.Install(tb, s, false)
	before := w.Account(tb, s)
	if huClear(tb, w, s) {
		tb.Fatal("ClearBlocked said it cleared a block the account did not hold")
	}
	if after := w.Account(tb, s); !reflect.DeepEqual(after, before) {
		tb.Fatalf("the account is %+v after a clear of nothing, was %+v", after, before)
	}
}

func huNoAccount(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	if huClear(tb, w, s) {
		tb.Fatal("ClearBlocked said it cleared an account that does not exist")
	}
	if a := w.Account(tb, s); !a.Cold {
		tb.Fatalf("ClearBlocked made an account for a scope that had none: %+v", a)
	}
}

func huStale(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	w.Install(tb, s, true)
	w.Stale(tb, s)
	if huClear(tb, w, s) {
		tb.Fatal("ClearBlocked said it cleared a block in an account that belongs to an older generation")
	}
	if a := w.Account(tb, s); !a.Cold {
		tb.Fatalf("the account is %+v: a stale account stays stale until it is rebuilt", a)
	}
}

func huFrozen(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	w.Install(tb, s, true)
	w.Freeze(tb, s, true)
	cleared, err := w.Hot.ClearBlocked(context.Background(), s)
	if !errors.Is(err, domain.ErrShardFrozen) || cleared {
		tb.Fatalf("a frozen shard: cleared %v, err %v, want %v", cleared, err, domain.ErrShardFrozen)
	}
	w.Freeze(tb, s, false)
	if a := w.Account(tb, s); !a.Blocked {
		tb.Fatalf("a refused clear changed the account: %+v", a)
	}
}

func huTwice(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	w.Install(tb, s, true)
	first, second := huClear(tb, w, s), huClear(tb, w, s)
	if !first || second {
		tb.Fatalf("two calls said %v then %v, want true then false", first, second)
	}
}

func huTogether(tb testing.TB, f HotUnblockFactory) {
	w := f(tb)
	s := w.NewScope(tb)
	w.Install(tb, s, true)
	var wg sync.WaitGroup
	var told atomic.Int64
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cleared, err := w.Hot.ClearBlocked(context.Background(), s)
			if err != nil {
				tb.Errorf("ClearBlocked: %v", err)
				return
			}
			if cleared {
				told.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := told.Load(); n != 1 {
		tb.Fatalf("%d of sixteen concurrent callers were told they cleared the block, want exactly 1", n)
	}
	if a := w.Account(tb, s); a.Blocked {
		tb.Fatalf("the account is still blocked after sixteen clears: %+v", a)
	}
}
