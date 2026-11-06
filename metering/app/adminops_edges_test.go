package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The edges of R112's use case: the lengths a record may have exactly, the characters a key may not, and what the worker does when its
// store or its context lets it down. Each of these was found by planting a defect where it would hurt and seeing whether anything noticed.

func TestTheLongestPersonReasonAndKeyAreAcceptedAndOneByteMoreIsNot(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	q := req(ports.AdminRecover, `{"shard":1}`, strings.Repeat("k", app.MaxAdminKey))
	q.Actor, q.Reason = strings.Repeat("a", app.MaxAdminActor), strings.Repeat("r", app.MaxAdminReason)
	op := r.submit(t, q)
	if len(op.Actor) != app.MaxAdminActor || len(op.Reason) != app.MaxAdminReason || len(op.Key) != app.MaxAdminKey {
		t.Fatalf("the longest permitted were kept as %d, %d and %d bytes", len(op.Actor), len(op.Reason), len(op.Key))
	}
}

func TestAKeyMayHoldSpacesButNoControlCharacter(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	r.submit(t, req(ports.AdminRecover, `{"shard":1}`, "ticket 4412 retry"))
	for _, key := range []string{"k\n1", "k\x007", "k\t1", "\x1bk", "k\u0085x"} {
		if _, _, err := r.ops.Submit(t.Context(), req(ports.AdminRecover, `{"shard":1}`, key)); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("key %q: %v, want %v: a key reaches logs and audit rows", key, err, domain.ErrInvalid)
		}
	}
}

func TestWhoCarriedARequestIsKeptWithIt(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminReconcile, `{"shard":1}`, "k-1"))
	if op.Via != "ops-console" {
		t.Fatalf("via = %q: the credential that carried the request is part of its record", op.Via)
	}
}

func TestADeadLettersIdIsAUUIDOfAVersionAndVariantThereAre(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	for i, id := range []string{"5b0e6f2a-1c3d-1e5f-8a9b-0c1d2e3f4a5b", "5b0e6f2a-1c3d-4e5f-9a9b-0c1d2e3f4a5b", "5b0e6f2a-1c3d-7e5f-aa9b-0c1d2e3f4a5b", "5b0e6f2a-1c3d-8e5f-ba9b-0c1d2e3f4a5b"} {
		r.submit(t, req(ports.AdminReplay, fmt.Sprintf(`{"id":%q}`, id), fmt.Sprintf("ok-%d", i)))
	}
	for i, id := range []string{
		"5b0e6f2a-1c3d-0e5f-8a9b-0c1d2e3f4a5b", "5b0e6f2a-1c3d-9e5f-8a9b-0c1d2e3f4a5b", "5b0e6f2a-1c3d-4e5f-7a9b-0c1d2e3f4a5b",
		"5b0e6f2a-1c3d-4e5f-ca9b-0c1d2e3f4a5b", "5b0e6f2a1c3d4e5f8a9b0c1d2e3f4a5b", "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5", "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5bb",
		"g00e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b", " 5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b", "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b\n",
	} {
		if _, _, err := r.ops.Submit(t.Context(), req(ports.AdminReplay, fmt.Sprintf(`{"id":%q}`, id), fmt.Sprintf("bad-%d", i))); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("id %q: %v, want %v", id, err, domain.ErrInvalid)
		}
	}
}

// ctxStrict is a store that refuses what a real database refuses: a call made with a context that is already over.
type ctxStrict struct {
	*contracts.MemAdminOps
	leases   []time.Duration
	claimErr error
	finish   func(context.Context, string, int) (bool, error)
	gaugeErr error
	finished atomic.Int64
}

func (c *ctxStrict) Claim(ctx context.Context, lease time.Duration) (ports.AdminOperation, bool, error) {
	c.leases = append(c.leases, lease)
	if c.claimErr != nil {
		return ports.AdminOperation{}, false, c.claimErr
	}
	return c.MemAdminOps.Claim(ctx, lease)
}

func (c *ctxStrict) Finish(ctx context.Context, id string, attempt int, st ports.AdminOperationStatus, res json.RawMessage, text string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if c.finish != nil {
		return c.finish(ctx, id, attempt)
	}
	c.finished.Add(1)
	return c.MemAdminOps.Finish(ctx, id, attempt, st, res, text)
}

func (c *ctxStrict) Backlog(ctx context.Context) (ports.AdminBacklog, error) {
	if c.gaugeErr != nil {
		return ports.AdminBacklog{}, c.gaugeErr
	}
	return c.MemAdminOps.Backlog(ctx)
}

type strictRig struct {
	*adminRig
	st *ctxStrict
}

func newStrictRig(t *testing.T) *strictRig {
	t.Helper()
	r := newAdminRig(t)
	return &strictRig{adminRig: r, st: &ctxStrict{MemAdminOps: r.store}}
}

func (s *strictRig) worker(t *testing.T, ex map[ports.AdminOperationKind]app.AdminExecutor, lease time.Duration) *app.AdminWorker {
	t.Helper()
	w, err := app.NewAdminWorker(app.AdminWorkerDeps{Store: s.st, Executors: ex, Metrics: s.metrics, Lease: lease, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func okExecutor(res string) map[ports.AdminOperationKind]app.AdminExecutor {
	return map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
			if res == "" {
				return nil, nil
			}
			return json.RawMessage(res), nil
		},
	}
}

// Work that finished as the worker was told to stop is recorded, and a real database would refuse to record it on a context that is over.
func TestAnOutcomeIsRecordedOnAFreshContextWhenTheWorkersIsOver(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	op := s.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := s.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
			cancel()
			return json.RawMessage(`{"done":true}`), nil
		},
	}, time.Minute)
	if did, err := w.RunOne(ctx); !did || err != nil {
		t.Fatalf("RunOne = %v, %v", did, err)
	}
	if got, _ := s.ops.Get(t.Context(), "default", op.ID); got.Status != ports.AdminSucceeded {
		t.Fatalf("recorded as %+v", got)
	}
}

func TestAnInterruptedWorkerSaysWhyItStoppedAndRecordsNothing(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	s.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	ctx, cancel := context.WithCancel(t.Context())
	w := s.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(c context.Context, _ ports.AdminOperation) (json.RawMessage, error) {
			cancel()
			return nil, c.Err()
		},
	}, time.Minute)
	did, err := w.RunOne(ctx)
	if !did || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOne = %v, %v, want it to have claimed the request and to say it was cancelled", did, err)
	}
	if s.st.finished.Load() != 0 || s.metrics.ops(ports.AdminRecover, "failed") != 0 || s.metrics.ops(ports.AdminRecover, "succeeded") != 0 {
		t.Fatal("an interrupted operation was recorded or counted as if it had an outcome")
	}
}

func TestAnExecutorThatSaysNothingSucceedsWithAnEmptyObjectAndAFailureHasNoResult(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	a := s.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-a"))
	if _, err := s.worker(t, okExecutor(""), time.Minute).RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ops.Get(t.Context(), "default", a.ID); string(got.Result) != `{}` || got.Status != ports.AdminSucceeded {
		t.Fatalf("a success that said nothing was recorded as %+v: the record always holds JSON", got)
	}
	b := s.submit(t, req(ports.AdminRecover, `{"shard":2}`, "k-b"))
	failing := map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
			return json.RawMessage(`{"half":"done"}`), errors.New("it stopped halfway")
		},
	}
	if _, err := s.worker(t, failing, time.Minute).RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ops.Get(t.Context(), "default", b.ID); got.Status != ports.AdminFailed || len(got.Result) != 0 {
		t.Fatalf("a failure was recorded as %+v: what a failed operation half-did is not a result", got)
	}
}

func TestAFailureTextIsCutBetweenCharactersAndAShortOneIsKeptWhole(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	long := s.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-long"))
	exact := s.submit(t, req(ports.AdminRecover, `{"shard":2}`, "k-exact"))
	texts := map[string]string{long.ID: strings.Repeat("é", app.MaxAdminError), exact.ID: strings.Repeat("e", app.MaxAdminError)} // 2-byte characters: the limit falls inside one
	w := s.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(_ context.Context, o ports.AdminOperation) (json.RawMessage, error) {
			return nil, errors.New(texts[o.ID])
		},
	}, time.Minute)
	for range 2 {
		if _, err := w.RunOne(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.ops.Get(t.Context(), "default", long.ID)
	if !utf8.ValidString(got.Error) || len(got.Error) > app.MaxAdminError || len(got.Error) < app.MaxAdminError-1 || !strings.HasPrefix(texts[long.ID], got.Error) {
		t.Fatalf("a long text was stored as %d bytes, valid %v", len(got.Error), utf8.ValidString(got.Error))
	}
	if got, _ := s.ops.Get(t.Context(), "default", exact.ID); got.Error != texts[exact.ID] {
		t.Fatalf("a text exactly at the limit was cut to %d bytes", len(got.Error))
	}
}

func TestTheWorkersOwnLeaseIsTheDefaultWhenNoneIsGivenAndOtherwiseTheOneGiven(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	if _, err := s.worker(t, okExecutor(""), 0).RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.worker(t, okExecutor(""), 7*time.Minute).RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(s.st.leases) != 2 || s.st.leases[0] != app.DefaultAdminLease || s.st.leases[1] != 7*time.Minute {
		t.Fatalf("claims were made for %v, want the default (%v) and then the one given", s.st.leases, app.DefaultAdminLease)
	}
}

func TestAStoreThatCannotBeReadOrWrittenIsTheWorkersErrorAndNotAnOperationsOutcome(t *testing.T) {
	t.Parallel()
	boom := errors.New("the database is down")
	s := newStrictRig(t)
	s.st.claimErr = boom
	if did, err := s.worker(t, okExecutor(""), time.Minute).RunOne(t.Context()); did || !errors.Is(err, boom) {
		t.Fatalf("a claim that failed: %v, %v", did, err)
	}
	s.st.claimErr = nil
	op := s.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	s.st.finish = func(context.Context, string, int) (bool, error) { return false, boom }
	if did, err := s.worker(t, okExecutor(""), time.Minute).RunOne(t.Context()); !did || !errors.Is(err, boom) {
		t.Fatalf("an outcome that could not be written: %v, %v, want the work reported as claimed and the error returned", did, err)
	}
	if s.metrics.ops(ports.AdminRecover, "succeeded") != 0 {
		t.Fatal("an outcome that was not recorded was counted")
	}
	s.st.finish = nil
	s.clock.Advance(time.Hour)
	if _, err := s.worker(t, okExecutor(""), time.Minute).RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ops.Get(t.Context(), "default", op.ID); got.Status != ports.AdminSucceeded || got.Attempts != 2 {
		t.Fatalf("after the database came back: %+v, want the second attempt to have recorded it", got)
	}
}

func TestAnOutcomeAnotherWorkerOwnsNowIsNeitherAnErrorNorCounted(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	s.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	s.st.finish = func(context.Context, string, int) (bool, error) { return false, nil }
	if did, err := s.worker(t, okExecutor(""), time.Minute).RunOne(t.Context()); !did || err != nil {
		t.Fatalf("RunOne = %v, %v", did, err)
	}
	if s.metrics.ops(ports.AdminRecover, "succeeded") != 0 {
		t.Fatal("an outcome this worker did not record was counted as its own")
	}
}

func TestTheBacklogCannotBeExportedWhenTheStoreCannotBeRead(t *testing.T) {
	t.Parallel()
	s := newStrictRig(t)
	boom := errors.New("the database is down")
	s.st.gaugeErr = boom
	if err := s.worker(t, nil, 0).ExportGauges(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v: a gauge that is silently stale is worse than none", err, boom)
	}
	if _, ok := s.metrics.gauge(ports.MetricAdminWaiting); ok {
		t.Fatal("a gauge was set from a read that failed")
	}
}

func TestARequestForAnOperationReadsBackExactlyWhatItIsAndNothingElseIsFound(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminReconcile, `{"shard":1}`, "k-1"))
	got, err := r.ops.Get(t.Context(), "default", op.ID)
	if err != nil || got.ID != op.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := r.ops.Get(t.Context(), "elsewhere", op.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another realm's read: %v, want %v", err, domain.ErrNotFound)
	}
}
