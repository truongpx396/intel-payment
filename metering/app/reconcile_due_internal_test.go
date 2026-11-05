package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// A reconcile that runs always records its run, so the tick asks first whether anything is due. This is the question, tested on its own:
// the end-to-end test only shows it from the outside.

type reconcileBooks struct {
	ports.BookStore
	last       time.Time
	lastErr    error
	scopes     []ports.ScopeState
	scopesErr  error
	askedSince []time.Time
	askedLimit []int
	lastCalls  int
}

func (b *reconcileBooks) LastReconcile(context.Context, domain.Shard, domain.ReconcileKind) (time.Time, error) {
	b.lastCalls++
	return b.last, b.lastErr
}

func (b *reconcileBooks) ScopesSince(_ context.Context, _ domain.Shard, since time.Time, _ *domain.Scope, limit int) ([]ports.ScopeState, error) {
	b.askedSince = append(b.askedSince, since)
	b.askedLimit = append(b.askedLimit, limit)
	return b.scopes, b.scopesErr
}

func TestAnIncrementalReconcileIsDueOnlyWhenAScopeWasBookedSinceTheLastOne(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	b := &reconcileBooks{last: last}
	w := &Writer{d: Deps{Books: b}}
	ctx := context.Background()

	due, err := w.ReconcileDue(ctx, 3, domain.ReconcileIncremental)
	if err != nil || due {
		t.Fatalf("nothing booked since the last reconcile: due %v, %v", due, err)
	}
	if len(b.askedSince) != 1 || !b.askedSince[0].Equal(last) || b.askedLimit[0] != 1 {
		t.Fatalf("it must ask for the scopes since the last reconcile, and for one: since %v, limit %v", b.askedSince, b.askedLimit)
	}
	b.scopes = []ports.ScopeState{{Scope: domain.Scope{Realm: "r", Kind: "k", ID: "i"}}}
	if due, err := w.ReconcileDue(ctx, 3, domain.ReconcileIncremental); err != nil || !due {
		t.Fatalf("a scope was booked since: due %v, %v", due, err)
	}
}

func TestAnIncrementalReconcileThatNeverRanIsDueForEverythingOnce(t *testing.T) {
	t.Parallel()
	b := &reconcileBooks{scopes: []ports.ScopeState{{}}} // never reconciled: the zero time, i.e. every scope
	w := &Writer{d: Deps{Books: b}}
	if due, err := w.ReconcileDue(context.Background(), 0, domain.ReconcileIncremental); err != nil || !due {
		t.Fatalf("%v, %v", due, err)
	}
	if !b.askedSince[0].IsZero() {
		t.Fatalf("a shard never reconciled is asked from the zero time: %v", b.askedSince[0])
	}
}

func TestAFullSweepIsDueWheneverTheShardHoldsAScopeWhateverTheLastRunWas(t *testing.T) {
	t.Parallel()
	b := &reconcileBooks{last: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC), scopes: []ports.ScopeState{{}}}
	w := &Writer{d: Deps{Books: b}}
	if due, err := w.ReconcileDue(context.Background(), 1, domain.ReconcileFull); err != nil || !due {
		t.Fatalf("%v, %v", due, err)
	}
	if !b.askedSince[0].IsZero() || b.lastCalls != 0 {
		t.Fatalf("a full sweep looks at every scope and does not consult the last run: since %v, %d lookups", b.askedSince[0], b.lastCalls)
	}
	b.scopes = nil
	if due, err := w.ReconcileDue(context.Background(), 1, domain.ReconcileFull); err != nil || due {
		t.Fatalf("a shard with no scope has nothing to sweep: %v, %v", due, err)
	}
}

func TestReconcileDueRefusesAKindItDoesNotKnowAndReportsWhatTheBooksSay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := &reconcileBooks{}
	w := &Writer{d: Deps{Books: b}}
	if _, err := w.ReconcileDue(ctx, 0, "weekly"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("an unknown kind: %v", err)
	}
	if len(b.askedSince) != 0 {
		t.Fatal("the books were asked about a kind that does not exist")
	}
	boom := errors.New("connection reset")
	if _, err := (&Writer{d: Deps{Books: &reconcileBooks{lastErr: boom}}}).ReconcileDue(ctx, 0, domain.ReconcileIncremental); !errors.Is(err, boom) {
		t.Fatalf("a failing last-run lookup must not read as 'nothing due': %v", err)
	}
	if due, err := (&Writer{d: Deps{Books: &reconcileBooks{scopesErr: boom}}}).ReconcileDue(ctx, 0, domain.ReconcileFull); !errors.Is(err, boom) || due {
		t.Fatalf("a failing scope lookup must not read as 'nothing due': %v, %v", due, err)
	}
}
