package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a: an executor that did not come to an outcome — and whose operation is safe to do again, because its effect is keyed by the operation —
// says so with app.ErrRetryOperation. The worker leaves the operation as it would one whose worker died, to be claimed again when the lease
// lapses, and fails it only when it has been tried app.MaxAdminAttempts times. It does not record a failure that a person must interpret.

func TestAnOperationThatIsSafeToDoAgainIsLeftForTheNextClaimAndNotRecordedFailed(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminReconcile, `{"shard":1}`, "k-1"))
	attempt := 0
	ex := map[ports.AdminOperationKind]app.AdminExecutor{ports.AdminReconcile: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
		attempt++
		if attempt == 1 {
			return nil, fmt.Errorf("%w: the hot tier did not confirm", app.ErrRetryOperation)
		}
		return json.RawMessage(`{"ok":true}`), nil
	}}
	w := r.worker(t, ex)

	if did, err := w.RunOne(t.Context()); !did || err != nil {
		t.Fatalf("RunOne = %v, %v", did, err)
	}
	got, err := r.store.Get(t.Context(), "default", op.ID)
	if err != nil || got.Status != ports.AdminRunning || got.FinishedAt != nil {
		t.Fatalf("after the first run the operation is %+v, %v: want it still running, with no outcome", got, err)
	}
	if did, _ := w.RunOne(t.Context()); did {
		t.Fatal("the operation was claimed again before its lease lapsed")
	}

	r.clock.Advance(2 * time.Minute) // past the worker's one-minute lease
	if did, err := w.RunOne(t.Context()); !did || err != nil {
		t.Fatalf("RunOne after the lease lapsed = %v, %v", did, err)
	}
	got, _ = r.store.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminSucceeded || got.Attempts != 2 || attempt != 2 {
		t.Fatalf("the operation is %+v after %d runs, want succeeded on the second attempt", got, attempt)
	}
	if n := r.metrics.ops(ports.AdminReconcile, "retry"); n != 1 {
		t.Fatalf("%d retries were counted, want 1: a request left for another claim is not a finished one, and is not silent", n)
	}
	if n := r.metrics.ops(ports.AdminReconcile, "failed"); n != 0 {
		t.Fatalf("%d failures were counted for an operation that came to succeed", n)
	}
}

func TestAnOperationThatIsAlwaysLeftForTheNextClaimIsFailedAtTheCapWithItsOwnText(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminReconcile, `{"shard":1}`, "k-1"))
	runs := 0
	ex := map[ports.AdminOperationKind]app.AdminExecutor{ports.AdminReconcile: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
		runs++
		return nil, fmt.Errorf("%w: it may have been applied: look for idem_key k", app.ErrRetryOperation)
	}}
	w := r.worker(t, ex)
	for range app.MaxAdminAttempts + 3 {
		_, _ = w.RunOne(t.Context())
		r.clock.Advance(2 * time.Minute)
	}
	got, _ := r.store.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminFailed || got.Attempts != app.MaxAdminAttempts || runs != app.MaxAdminAttempts {
		t.Fatalf("the operation is %+v after %d runs, want failed at attempt %d and never claimed again", got, runs, app.MaxAdminAttempts)
	}
	if !strings.Contains(got.Error, "it may have been applied: look for idem_key k") {
		t.Fatalf("the failure says %q: it must carry the executor's own text, which says what to look for", got.Error)
	}
	if n := r.metrics.ops(ports.AdminReconcile, "retry"); n != app.MaxAdminAttempts-1 {
		t.Fatalf("%d retries were counted, want %d", n, app.MaxAdminAttempts-1)
	}
	if n := r.metrics.ops(ports.AdminReconcile, "failed"); n != 1 {
		t.Fatalf("%d failures were counted, want 1", n)
	}
}

// Only what the executor marked retryable is left; an ordinary failure is recorded and final, as it always was.
func TestAnOrdinaryFailureIsStillRecordedAndFinal(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminReconcile, `{"shard":1}`, "k-1"))
	ex := map[ports.AdminOperationKind]app.AdminExecutor{ports.AdminReconcile: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
		return nil, errors.New("shard 9 does not exist")
	}}
	w := r.worker(t, ex)
	_, _ = w.RunOne(t.Context())
	got, _ := r.store.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminFailed || got.Attempts != 1 || got.Error != "shard 9 does not exist" {
		t.Fatalf("the operation is %+v", got)
	}
}
