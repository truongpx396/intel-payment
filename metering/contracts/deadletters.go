package contracts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// DeadLetterFactory builds a fresh, empty store that reads time from the clock it is given.
type DeadLetterFactory func(tb testing.TB, clock *FakeClock) ports.DeadLetterStore

// DeadLetterCase is one named obligation of the parking lot.
type DeadLetterCase struct {
	Name string
	Run  func(tb testing.TB, f DeadLetterFactory)
}

// The names of the cases, so a test that plants a defect can say which case must catch it.
const (
	CaseDLParkedWhole          = "a letter is parked whole: every byte, its attempts, its error and when"
	CaseDLParkedOnce           = "the same letter parked twice is one letter, as first written"
	CaseDLParkedOnceTogether   = "the same letter parked from sixteen places at once is one letter and one parking"
	CaseDLIdentity             = "a letter is its subject and its source id together"
	CaseDLUnknown              = "a letter that does not exist is not found by any operation"
	CaseDLClaimOnce            = "a replay claims a letter once; a second is refused while the first is live"
	CaseDLClaimTogether        = "sixteen replays at once: exactly one holds the claim"
	CaseDLLapsedClaim          = "a claim whose holder was killed lapses, and the next replay completes the work"
	CaseDLReleasedClaim        = "a claim given back after a failed publish can be taken at once"
	CaseDLReplayedIsKept       = "a replayed letter is kept, is never claimed again, and cannot be un-replayed"
	CaseDLOpenCounts           = "open counts the unreplayed per realm, and a realm with none open reads zero"
	CaseDLNothingIsEverDeleted = "nothing is ever deleted: every letter ever parked is still read back whole"
)

// DeadLetterCases are the obligations of a DeadLetterStore (FR-043, bus-subjects.md § dead letters).
func DeadLetterCases() []DeadLetterCase {
	return []DeadLetterCase{
		{CaseDLParkedWhole, dlParkedWhole},
		{CaseDLParkedOnce, dlParkedOnce},
		{CaseDLParkedOnceTogether, dlParkedOnceTogether},
		{CaseDLIdentity, dlIdentity},
		{CaseDLUnknown, dlUnknown},
		{CaseDLClaimOnce, dlClaimOnce},
		{CaseDLClaimTogether, dlClaimTogether},
		{CaseDLLapsedClaim, dlLapsedClaim},
		{CaseDLReleasedClaim, dlReleasedClaim},
		{CaseDLReplayedIsKept, dlReplayedIsKept},
		{CaseDLOpenCounts, dlOpenCounts},
		{CaseDLNothingIsEverDeleted, dlNothingIsEverDeleted},
	}
}

// DeadLetterContract runs every case, each against a store of its own, in parallel.
func DeadLetterContract(t *testing.T, f DeadLetterFactory) {
	t.Helper()
	for _, c := range DeadLetterCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

var dlEpoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

const dlUnknownID = "00000000-0000-4000-8000-0000000000ff"

func dlWorld(tb testing.TB, f DeadLetterFactory) (ports.DeadLetterStore, *FakeClock) {
	tb.Helper()
	clock := NewFakeClock(dlEpoch)
	return f(tb, clock), clock
}

func dlLetter(realm, subject, source string) ports.DeadLetter {
	return ports.DeadLetter{Realm: realm, Subject: subject, SourceID: source, Payload: []byte(`{"realm":"` + realm + `"}`), Attempts: 5, LastError: "boom"}
}

func dlPark(tb testing.TB, s ports.DeadLetterStore, l ports.DeadLetter) ports.DeadLetter {
	tb.Helper()
	stored, created, err := s.Park(context.Background(), l)
	if err != nil || !created || stored.ID == "" {
		tb.Fatalf("parking %s/%s: id %q, created %v, err %v", l.Subject, l.SourceID, stored.ID, created, err)
	}
	return stored
}

func dlGet(tb testing.TB, s ports.DeadLetterStore, id string) ports.DeadLetter {
	tb.Helper()
	l, err := s.Get(context.Background(), id)
	if err != nil {
		tb.Fatalf("reading dead letter %s back: %v", id, err)
	}
	return l
}

func dlOpen(tb testing.TB, s ports.DeadLetterStore) map[string]int64 {
	tb.Helper()
	open, err := s.Open(context.Background())
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	return open
}

func dlParkedWhole(tb testing.TB, f DeadLetterFactory) {
	s, clock := dlWorld(tb, f)
	all := make([]byte, 0, 258)
	for i := range 256 {
		all = append(all, byte(i))
	}
	all = append(all, 0, 0) // trailing zero bytes are payload too
	in := ports.DeadLetter{Realm: "acme", Subject: "billing.payment.acme%2Fworkspace:w1", SourceID: "1700000000000-0", Payload: all, Attempts: 5, LastError: "book: deadlock detected"}
	stored := dlPark(tb, s, in)
	for name, l := range map[string]ports.DeadLetter{"as returned by Park": stored, "as read back": dlGet(tb, s, stored.ID)} {
		if l.Realm != in.Realm || l.Subject != in.Subject || l.SourceID != in.SourceID || l.Attempts != in.Attempts || l.LastError != in.LastError {
			tb.Fatalf("%s: %+v", name, l)
		}
		if !bytes.Equal(l.Payload, all) {
			tb.Fatalf("%s: the payload was altered: %d bytes in, %d out", name, len(all), len(l.Payload))
		}
		if !l.ParkedAt.Equal(clock.Now()) || l.ReplayedAt != nil {
			tb.Fatalf("%s: parked at %v (now %v), replayed %v", name, l.ParkedAt, clock.Now(), l.ReplayedAt)
		}
	}
	empty := dlPark(tb, s, ports.DeadLetter{Realm: "acme", Subject: "billing.x", SourceID: "e", Payload: nil})
	if got := dlGet(tb, s, empty.ID); len(got.Payload) != 0 {
		tb.Fatalf("an empty payload came back as %d bytes", len(got.Payload))
	}
}

func dlParkedOnce(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	first := dlPark(tb, s, dlLetter("acme", "billing.payment.t1", "7-0"))
	again := dlLetter("acme", "billing.payment.t1", "7-0")
	again.Payload, again.Attempts, again.LastError = []byte("something else"), 9, "another failure"
	stored, created, err := s.Park(context.Background(), again)
	if err != nil {
		tb.Fatalf("parking the same letter again: %v", err)
	}
	if created || stored.ID != first.ID {
		tb.Fatalf("a letter the bus delivered twice was parked twice: created %v, id %s then %s", created, first.ID, stored.ID)
	}
	if !bytes.Equal(stored.Payload, first.Payload) || stored.Attempts != 5 || stored.LastError != "boom" {
		tb.Fatalf("the second parking rewrote the first: %+v", stored)
	}
	if got := dlGet(tb, s, first.ID); !bytes.Equal(got.Payload, first.Payload) || got.Attempts != 5 || got.LastError != "boom" {
		tb.Fatalf("the second parking rewrote what is stored: %+v", got)
	}
	if n := dlOpen(tb, s)["acme"]; n != 1 {
		tb.Fatalf("%d letters are open, want 1", n)
	}
}

func dlParkedOnceTogether(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		ids     = map[string]bool{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, c, err := s.Park(context.Background(), dlLetter("acme", "billing.tick", "99-0"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				tb.Errorf("Park: %v", err)
				return
			}
			if c {
				created++
			}
			ids[l.ID] = true
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		tb.Fatalf("%d concurrent parkings of one message: %d reported creating it, %d letters exist", n, created, len(ids))
	}
	if open := dlOpen(tb, s)["acme"]; open != 1 {
		tb.Fatalf("%d letters are open, want 1", open)
	}
}

func dlIdentity(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	a := dlPark(tb, s, dlLetter("acme", "billing.payment.a", "5-0"))
	b := dlPark(tb, s, dlLetter("acme", "billing.payment.b", "5-0")) // another subject, the same id
	c := dlPark(tb, s, dlLetter("acme", "billing.payment.a", "6-0")) // the same subject, another id
	if a.ID == b.ID || a.ID == c.ID || b.ID == c.ID {
		tb.Fatalf("distinct letters share an id: %s %s %s", a.ID, b.ID, c.ID)
	}
	if n := dlOpen(tb, s)["acme"]; n != 3 {
		tb.Fatalf("%d letters are open, want 3", n)
	}
}

func dlUnknown(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	ctx := context.Background()
	if _, err := s.Get(ctx, dlUnknownID); !errors.Is(err, domain.ErrNotFound) {
		tb.Fatalf("Get of a letter that does not exist: %v", err)
	}
	if _, claimed, err := s.ClaimReplay(ctx, dlUnknownID, time.Minute); !errors.Is(err, domain.ErrNotFound) || claimed {
		tb.Fatalf("ClaimReplay of a letter that does not exist: claimed %v, %v", claimed, err)
	}
	if err := s.FinishReplay(ctx, dlUnknownID); !errors.Is(err, domain.ErrNotFound) {
		tb.Fatalf("FinishReplay of a letter that does not exist: %v", err)
	}
	if err := s.ReleaseReplay(ctx, dlUnknownID); !errors.Is(err, domain.ErrNotFound) {
		tb.Fatalf("ReleaseReplay of a letter that does not exist: %v", err)
	}
}

func dlClaimOnce(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	ctx := context.Background()
	l := dlPark(tb, s, dlLetter("acme", "billing.payment.t1", "1-0"))
	got, claimed, err := s.ClaimReplay(ctx, l.ID, time.Minute)
	if err != nil || !claimed || got.ID != l.ID || !bytes.Equal(got.Payload, l.Payload) || got.Subject != l.Subject {
		tb.Fatalf("the first claim: claimed %v, %+v, %v", claimed, got, err)
	}
	held, claimed, err := s.ClaimReplay(ctx, l.ID, time.Minute)
	if err != nil || claimed || held.ReplayedAt != nil {
		tb.Fatalf("a second claim while the first is live: claimed %v, replayed %v, %v", claimed, held.ReplayedAt, err)
	}
	if got := dlGet(tb, s, l.ID); got.ReplayedAt != nil {
		tb.Fatalf("a claim alone marked the letter replayed: nothing has been published yet")
	}
}

func dlClaimTogether(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	l := dlPark(tb, s, dlLetter("acme", "billing.payment.t1", "1-0"))
	const n = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.ClaimReplay(context.Background(), l.ID, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				tb.Errorf("ClaimReplay: %v", err)
			}
			if ok {
				claimed++
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		tb.Fatalf("%d of %d concurrent replays hold the claim, want exactly 1", claimed, n)
	}
}

func dlLapsedClaim(tb testing.TB, f DeadLetterFactory) {
	s, clock := dlWorld(tb, f)
	ctx := context.Background()
	l := dlPark(tb, s, dlLetter("acme", "billing.payment.t1", "1-0"))
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Minute); err != nil || !ok {
		tb.Fatalf("claim: %v %v", ok, err)
	}
	clock.Advance(59 * time.Second)
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Minute); err != nil || ok {
		tb.Fatalf("a claim inside its lease was taken over: %v %v", ok, err)
	}
	clock.Advance(2 * time.Second) // the holder was killed: 61 s have passed
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Minute); err != nil || !ok {
		tb.Fatalf("a claim past its lease could not be taken over, so a replay killed midway is never completed: %v %v", ok, err)
	}
	if _, ok, _ := s.ClaimReplay(ctx, l.ID, time.Minute); ok {
		tb.Fatalf("the takeover did not renew the lease")
	}
}

func dlReleasedClaim(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	ctx := context.Background()
	l := dlPark(tb, s, dlLetter("acme", "billing.payment.t1", "1-0"))
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Hour); err != nil || !ok {
		tb.Fatalf("claim: %v %v", ok, err)
	}
	if err := s.ReleaseReplay(ctx, l.ID); err != nil {
		tb.Fatalf("release: %v", err)
	}
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Hour); err != nil || !ok {
		tb.Fatalf("a released claim still held for its hour, so a failed publish cannot be retried: %v %v", ok, err)
	}
	if got := dlGet(tb, s, l.ID); got.ReplayedAt != nil {
		tb.Fatalf("releasing a claim marked the letter replayed")
	}
}

func dlReplayedIsKept(tb testing.TB, f DeadLetterFactory) {
	s, clock := dlWorld(tb, f)
	ctx := context.Background()
	l := dlPark(tb, s, dlLetter("acme", "billing.payment.t1", "1-0"))
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Minute); err != nil || !ok {
		tb.Fatalf("claim: %v %v", ok, err)
	}
	clock.Advance(time.Second)
	replayedAt := clock.Now()
	if err := s.FinishReplay(ctx, l.ID); err != nil {
		tb.Fatalf("FinishReplay: %v", err)
	}
	got := dlGet(tb, s, l.ID)
	if got.ReplayedAt == nil || !got.ReplayedAt.Equal(replayedAt) || !bytes.Equal(got.Payload, l.Payload) || got.Subject != l.Subject {
		tb.Fatalf("a replayed letter is kept whole and says when: %+v", got)
	}
	clock.Advance(time.Hour)
	if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Minute); err != nil || ok {
		tb.Fatalf("a replayed letter was claimed again: %v %v", ok, err)
	}
	clock.Advance(time.Hour)
	if err := s.FinishReplay(ctx, l.ID); err != nil {
		tb.Fatalf("repeating FinishReplay: %v", err)
	}
	if err := s.ReleaseReplay(ctx, l.ID); err != nil {
		tb.Fatalf("ReleaseReplay after a finished replay: %v", err)
	}
	if got := dlGet(tb, s, l.ID); got.ReplayedAt == nil || !got.ReplayedAt.Equal(replayedAt) {
		tb.Fatalf("a repeated finish or a late release moved when the letter was replayed: %v", got.ReplayedAt)
	}
	// The bus delivering the same envelope again must not bring the letter back.
	again, created, err := s.Park(ctx, dlLetter("acme", "billing.payment.t1", "1-0"))
	if err != nil || created || again.ID != l.ID || again.ReplayedAt == nil {
		tb.Fatalf("parking a replayed letter's envelope again: created %v, replayed %v, %v", created, again.ReplayedAt, err)
	}
}

func dlOpenCounts(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	ctx := context.Background()
	var acme []ports.DeadLetter
	for i := range 3 {
		acme = append(acme, dlPark(tb, s, dlLetter("acme", "billing.payment.t1", fmt.Sprintf("a%d", i))))
	}
	dlPark(tb, s, dlLetter("beta", "billing.payment.t2", "b0"))
	dlPark(tb, s, dlLetter("beta", "billing.payment.t2", "b1"))
	only := dlPark(tb, s, dlLetter("gamma", "billing.tick", "g0"))
	for _, l := range []ports.DeadLetter{acme[0], only} {
		if _, ok, err := s.ClaimReplay(ctx, l.ID, time.Minute); err != nil || !ok {
			tb.Fatalf("claim: %v %v", ok, err)
		}
		if err := s.FinishReplay(ctx, l.ID); err != nil {
			tb.Fatalf("finish: %v", err)
		}
	}
	open := dlOpen(tb, s)
	if open["acme"] != 2 || open["beta"] != 2 {
		tb.Fatalf("open letters per realm: %v, want acme 2 and beta 2", open)
	}
	if n, present := open["gamma"]; !present || n != 0 {
		tb.Fatalf("a realm whose letters were all replayed must read zero, not vanish (a gauge made from it would stay at its last value): %v", open)
	}
}

func dlNothingIsEverDeleted(tb testing.TB, f DeadLetterFactory) {
	s, _ := dlWorld(tb, f)
	ctx := context.Background()
	want := map[string][]byte{}
	var ids []string
	for i := range 8 {
		l := dlLetter("acme", "billing.payment.t1", fmt.Sprintf("n%d", i))
		l.Payload = fmt.Appendf(nil, "payload-%d", i)
		stored := dlPark(tb, s, l)
		want[stored.ID] = l.Payload
		ids = append(ids, stored.ID)
	}
	for i, id := range ids {
		switch i % 3 {
		case 0: // replayed
			if _, ok, err := s.ClaimReplay(ctx, id, time.Minute); err != nil || !ok {
				tb.Fatalf("claim: %v %v", ok, err)
			}
			if err := s.FinishReplay(ctx, id); err != nil {
				tb.Fatalf("finish: %v", err)
			}
		case 1: // claimed and released
			if _, _, err := s.ClaimReplay(ctx, id, time.Minute); err != nil {
				tb.Fatalf("claim: %v", err)
			}
			if err := s.ReleaseReplay(ctx, id); err != nil {
				tb.Fatalf("release: %v", err)
			}
		}
	}
	for id, payload := range want {
		l, err := s.Get(ctx, id)
		if err != nil || !bytes.Equal(l.Payload, payload) {
			tb.Fatalf("letter %s: %v, payload %q, want %q", id, err, l.Payload, payload)
		}
	}
}
