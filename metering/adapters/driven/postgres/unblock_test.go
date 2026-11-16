//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

const unblockShards = 256

func booksWorld(books *pgadapter.Books) contracts.BooksUnblockWorld {
	ctx := context.Background()
	return contracts.BooksUnblockWorld{
		Books: books,
		Put: func(tb testing.TB, s domain.Scope, w ports.Watermark) {
			tb.Helper()
			if err := books.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
				return tx.SetWatermark(ctx, w, s.Normalized().Shard(unblockShards))
			}); err != nil {
				tb.Fatalf("write the watermark of %s: %v", s.Tag(), err)
			}
		},
		Get: func(tb testing.TB, s domain.Scope) ports.Watermark {
			tb.Helper()
			var w ports.Watermark
			if err := books.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) (err error) { w, err = tx.Watermark(ctx); return }); err != nil {
				tb.Fatalf("read the watermark of %s: %v", s.Tag(), err)
			}
			return w
		},
	}
}

// R112a against PostgreSQL 16: the same contract the in-memory clearer honours.
func TestPostgresBooksUnblockHonoursTheContract(t *testing.T) {
	t.Parallel()
	world := booksWorld(pgadapter.NewBooks(db(t)))
	contracts.BooksUnblockContract(t, func(testing.TB) contracts.BooksUnblockWorld { return world })
}

// The writer books under the scope's transaction lock, and an unblock must wait for it: a booking that read the block, and writes it back,
// would otherwise undo a clear that landed in the middle. Here a booking holds the lock, reads the watermark, is asked to be cleared, and
// writes its own watermark; the clear is applied after it, and the scope ends clear.
func TestAnUnblockWaitsForABookingInProgressAndClearsWhatItWrote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	books := pgadapter.NewBooks(db(t))
	world := booksWorld(books)
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w-lock"}
	world.Put(t, s, ports.Watermark{Exists: true, Gen: 1, AppliedSeq: 10, Blocked: true})

	clearing := make(chan struct{})
	type outcome struct {
		got ports.BookUnblock
		err error
	}
	done := make(chan outcome, 1)
	err := books.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		w, err := tx.Watermark(ctx)
		if err != nil || !w.Blocked {
			t.Fatalf("the booking read %+v, %v: it should see the block", w, err)
		}
		go func() {
			close(clearing)
			got, err := books.ClearBlocked(ctx, s)
			done <- outcome{got, err}
		}()
		<-clearing
		select {
		case o := <-done:
			t.Errorf("the unblock finished (%+v) while a booking held the scope's lock", o)
		case <-time.After(300 * time.Millisecond): // long enough for an unblock that does not wait to have finished
		}
		w.AppliedSeq, w.Blocked = 11, true // the booking books one more intent, and still holds the block
		return tx.SetWatermark(ctx, w, s.Shard(unblockShards))
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		// It reports the sequence the booking left, not the one it started from: the hot account is cleared at the sequence the books are at.
		if o.err != nil || !o.got.Cleared || o.got.AppliedSeq != 11 {
			t.Fatalf("the unblock after the booking: %+v, want it to have cleared the block the booking left, at sequence 11", o)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the unblock never finished after the booking committed")
	}
	if got := world.Get(t, s); got.Blocked || got.AppliedSeq != 11 {
		t.Fatalf("the watermark is %+v: want the booking's sequence (11) and the block cleared", got)
	}
}
