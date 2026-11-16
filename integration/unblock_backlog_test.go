//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a: a drain books one batch and returns. When more intents are queued than a batch holds — and the write-off that set the block is not in
// the first — an unblock that cleared the hot account after one drain would leave it open while the books, still to be told of the write-off,
// became blocked: the two disagreeing about whether the account may spend, with nothing in reconcile to say so, until the next rehydrate put the
// block back. The hot account is cleared only at the sequence the books are at, so the unblock keeps draining until they agree, and the block is
// cleared in both tiers and stays cleared when the rest has been booked.
func TestAnUnblockWithMoreIntentsQueuedThanOneDrainBooksEndsClearInBothTiersAndStaysClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newWriterStack(t, wopts{LedgerOptions: contracts.LedgerOptions{Policy: domain.BlockAndFlag}, cfg: metering.Config{DrainBatch: 1}})
	l := ledger{s}
	sc := s.workspace("ub")
	s.track(sc)

	// Three intents, none booked: a purchase, a chargeback larger than the balance (a write-off, which under block_and_flag blocks the hot
	// account on the spot), and a top-up after it. The writer has not run.
	for _, g := range []domain.Grant{
		{Scope: sc, Amount: 10, IdemKey: "g1", Reason: "purchase"},
		{Scope: sc, Amount: -100, IdemKey: "cb1", Reason: "chargeback"},
		{Scope: sc, Amount: 50, IdemKey: "g2", Reason: "purchase"},
	} {
		if _, err := l.Grant(ctx, g); err != nil {
			t.Fatalf("grant %s: %v", g.IdemKey, err)
		}
	}
	if acct, err := s.store.Snapshot(ctx, sc); err != nil || !acct.Blocked || acct.Seq != 3 {
		t.Fatalf("the hot account is %+v, %v: want it blocked, at sequence 3, before the writer has booked anything", acct, err)
	}
	if w := booksWatermark(t, s, sc); w.Exists {
		t.Fatalf("the books already know of the scope: %+v", w)
	}

	res, err := s.writer.Unblock(ctx, sc)
	if err != nil {
		t.Fatalf("the unblock: %v", err)
	}
	if !res.Books || !res.Hot {
		t.Fatalf("the unblock says %+v: the write-off was booked on its way, so the books held the block, and so did the hot account", res)
	}

	check := func(when string) {
		t.Helper()
		if w := booksWatermark(t, s, sc); w.Blocked || w.AppliedSeq != 3 {
			t.Fatalf("%s: the books are %+v, want clear and at sequence 3", when, w)
		}
		if acct, err := s.store.Snapshot(ctx, sc); err != nil || acct.Blocked || acct.Seq != 3 {
			t.Fatalf("%s: the hot account is %+v, %v, want clear and at sequence 3", when, acct, err)
		}
	}
	check("right after the unblock")
	s.drain() // whatever was left to book is booked now
	check("after the writer has booked everything")
	if got, want := s.booked(sc, domain.GeneralPool), domain.Credits(50); got != want {
		t.Fatalf("the booked balance is %d, want %d: the unblock books nothing of its own", got, want)
	}
}

func booksWatermark(t *testing.T, s *wstack, sc domain.Scope) ports.Watermark {
	t.Helper()
	var w ports.Watermark
	must(t, s.books.InTx(context.Background(), sc, func(ctx context.Context, tx ports.BookTx) (err error) { w, err = tx.Watermark(ctx); return }))
	return w
}
