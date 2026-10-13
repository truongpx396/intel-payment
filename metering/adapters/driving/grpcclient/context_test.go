package grpcclient_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// "Deadlines are the caller's, and so is its fail policy." The client adds no clock, no retry and no
// default of its own: what the caller's context says is what the wire says.

func TestTheCallersDeadlineIsTheDeadlineOnTheWire(t *testing.T) {
	t.Parallel()
	got := make(chan time.Time, 1)
	f := &fakeServer{admit: func(ctx context.Context, _ *meteringv1.AdmitRequest) (*meteringv1.Admission, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return nil, status.Error(codes.Internal, "no deadline on the wire")
		}
		got <- d
		return &meteringv1.Admission{Allowed: true}, nil
	}}
	cl := dial(t, f)
	want := time.Now().Add(3 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	if _, err := cl.Admit(ctx, ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1}); err != nil {
		t.Fatal(err)
	}
	if d := await(t, got, "the call to reach the server"); d.After(want.Add(50*time.Millisecond)) || d.Before(want.Add(-1500*time.Millisecond)) {
		t.Fatalf("the server's deadline %v is not the caller's %v", d, want)
	}
}

func TestACallWithNoDeadlineHasNoneInventedForIt(t *testing.T) {
	t.Parallel()
	hasDeadline := make(chan bool, 1)
	f := &fakeServer{record: func(ctx context.Context, _ *meteringv1.RecordRequest) (*meteringv1.Receipt, error) {
		_, ok := ctx.Deadline()
		hasDeadline <- ok
		return &meteringv1.Receipt{Applied: true}, nil
	}}
	if _, err := dial(t, f).Record(context.Background(), event()); err != nil {
		t.Fatal(err)
	}
	if await(t, hasDeadline, "the call to reach the server") {
		t.Fatal("the client put a deadline on a call whose caller set none: the caller's fail policy is not the client's to choose")
	}
}

func TestAnExpiredDeadlineIsTheContextsErrorAndTheCallIsNotRepeated(t *testing.T) {
	t.Parallel()
	f := &fakeServer{record: func(ctx context.Context, _ *meteringv1.RecordRequest) (*meteringv1.Receipt, error) {
		<-ctx.Done() // slower than the caller will wait
		return nil, ctx.Err()
	}}
	cl := dial(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := cl.Record(ctx, event())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the caller's own deadline is context.DeadlineExceeded: %v", err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("a timed-out Record is never resent by the client: %d calls", n)
	}
}

// "A cancelled context settles nothing it did not already settle": a context that is already done sends
// nothing at all, and one cancelled mid-flight is reported as the cancellation and never resent.
func TestACancelledContextSettlesNothingMore(t *testing.T) {
	t.Parallel()
	for _, c := range allCalls {
		t.Run("already cancelled/"+c.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeServer{}
			cl := dial(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := c.do(ctx, cl, cl)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("want context.Canceled, got %v", err)
			}
			if n := f.calls.Load(); n != 0 {
				t.Fatalf("a cancelled context reached the server %d times", n)
			}
		})
	}
	t.Run("cancelled in flight", func(t *testing.T) {
		t.Parallel()
		entered, saw := make(chan struct{}, 1), make(chan error, 1)
		f := &fakeServer{grant: func(ctx context.Context, _ *meteringv1.GrantRequest) (*meteringv1.Receipt, error) {
			entered <- struct{}{}
			<-ctx.Done()
			saw <- ctx.Err()
			return nil, ctx.Err()
		}}
		cl := dial(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := cl.Grant(ctx, domain.Grant{Scope: ws, Amount: 5, IdemKey: "g", Reason: "purchase"})
			done <- err
		}()
		await(t, entered, "the call to reach the server")
		cancel()
		if err := await(t, done, "the call to return"); !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
		if err := await(t, saw, "the server to see the cancellation"); !errors.Is(err, context.Canceled) {
			t.Fatalf("the server must see the caller go: %v", err)
		}
		if n := f.calls.Load(); n != 1 {
			t.Fatalf("the client resent a cancelled Grant: %d calls", n)
		}
	})
}

// A refusal that is not transient is not retried; and neither is a transient one: the caller's fail policy
// decides, and every retry is the caller's, under the same idempotency key.
func TestTheClientNeverRetriesForTheCaller(t *testing.T) {
	t.Parallel()
	f := failing(wireStatus(codes.Unavailable, "hot_store_unavailable", "hot store unavailable", nil))
	cl := dial(t, f)
	for _, c := range allCalls {
		before := f.calls.Load()
		if err := c.do(context.Background(), cl, cl); !errors.Is(err, domain.ErrHotStoreUnavailable) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n := f.calls.Load() - before; n != 1 {
			t.Fatalf("%s was sent %d times", c.name, n)
		}
	}
}

// "A missing one is INVALID_ARGUMENT, never a generated default": the client sends what it was given,
// and an absent time is an absent timestamp, not the epoch.
func TestTheClientInventsNoKeyAndNoTime(t *testing.T) {
	t.Parallel()
	f := &fakeServer{}
	cl := dial(t, f)

	e := event()
	e.IdemKey, e.OccurredAt = "", time.Time{}
	if _, err := cl.Record(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("the Record was not sent: %d calls", f.calls.Load())
	}
	if got := f.gotRec.GetEvent(); got.GetIdemKey() != "" || got.GetOccurredAt() != nil {
		t.Fatalf("the client filled in a missing field: %+v", got)
	}

	if _, err := cl.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 5, Reason: "purchase"}); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("the Grant was not sent: %d calls", f.calls.Load())
	}
	if f.gotGrant.GetIdemKey() != "" || f.gotGrant.GetExpiresAt() != nil {
		t.Fatalf("the client filled in a missing field: %+v", f.gotGrant)
	}
}

// Credentials, when the caller wants them, ride on the connection: the client only needs to leave them be.
func TestTheClientAddsNoMetadataOfItsOwn(t *testing.T) {
	t.Parallel()
	f := &fakeServer{}
	cl := dial(t, f)
	if _, err := cl.Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1}); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("the Admit was not sent: %d calls", f.calls.Load())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.md {
		if k == "authorization" || k == "idempotency-key" || k == "x-realm" {
			t.Fatalf("the client sent %q on its own", k)
		}
	}
}
