package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112: what an operator asks of the engine is recorded first — with the person, the reason and a key that makes a retry the same request —
// and worked afterwards by the worker that holds the sole durable writer. paymentd cannot do it itself: it has no writer. These tests hold
// the part in between: what a request must carry, how each kind's arguments are read, and what a worker does with what it claims.

type adminMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64
}

func newAdminMetrics() *adminMetrics {
	return &adminMetrics{counts: map[string]int64{}, gauges: map[string]int64{}}
}

func (m *adminMetrics) Count(name string, n int64, l ports.Labels) {
	m.mu.Lock()
	m.counts[name+"|"+l["kind"]+"|"+l["outcome"]] += n
	m.mu.Unlock()
}

func (m *adminMetrics) Gauge(name string, v int64, _ ports.Labels) {
	m.mu.Lock()
	m.gauges[name] = v
	m.mu.Unlock()
}
func (*adminMetrics) Observe(string, float64, ports.Labels) {}

func (m *adminMetrics) ops(kind ports.AdminOperationKind, outcome string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[ports.MetricAdminOperations+"|"+string(kind)+"|"+outcome]
}

func (m *adminMetrics) gauge(name string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.gauges[name]
	return v, ok
}

type adminRig struct {
	clock   *contracts.FakeClock
	store   *contracts.MemAdminOps
	metrics *adminMetrics
	ops     *app.AdminOperations
}

const rigShards = 8

func newAdminRig(t *testing.T, kinds ...app.AdminKind) *adminRig {
	t.Helper()
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	store := contracts.NewMemAdminOps(clock)
	m := newAdminMetrics()
	ops, err := app.NewAdminOperations(app.AdminOperationsDeps{Store: store, Metrics: m, Shards: rigShards, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	return &adminRig{clock: clock, store: store, metrics: m, ops: ops}
}

func req(kind ports.AdminOperationKind, params, key string) ports.AdminRequest {
	return ports.AdminRequest{Realm: "default", Kind: kind, Params: json.RawMessage(params), Actor: "alice@example.com", Reason: "INC-4412", Key: key, Via: "ops-console"}
}

func (r *adminRig) submit(t *testing.T, rq ports.AdminRequest) ports.AdminOperation {
	t.Helper()
	op, _, err := r.ops.Submit(t.Context(), rq)
	if err != nil {
		t.Fatalf("Submit(%s %s): %v", rq.Kind, rq.Params, err)
	}
	return op
}

func (r *adminRig) worker(t *testing.T, ex map[ports.AdminOperationKind]app.AdminExecutor) *app.AdminWorker {
	t.Helper()
	w, err := app.NewAdminWorker(app.AdminWorkerDeps{
		Store: r.store, Executors: ex, Metrics: r.metrics, Lease: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestARequestNeedsAPersonAReasonAKeyAndARealm(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	for name, mutate := range map[string]func(*ports.AdminRequest){
		"no actor":           func(q *ports.AdminRequest) { q.Actor = "" },
		"a blank actor":      func(q *ports.AdminRequest) { q.Actor = " \t " },
		"no reason":          func(q *ports.AdminRequest) { q.Reason = "" },
		"a blank reason":     func(q *ports.AdminRequest) { q.Reason = "  " },
		"no key":             func(q *ports.AdminRequest) { q.Key = "" },
		"a blank key":        func(q *ports.AdminRequest) { q.Key = "   " },
		"no realm":           func(q *ports.AdminRequest) { q.Realm = "" },
		"a realm that isn't": func(q *ports.AdminRequest) { q.Realm = "Not A Realm" },
		"an actor too long":  func(q *ports.AdminRequest) { q.Actor = strings.Repeat("a", app.MaxAdminActor+1) },
		"a reason too long":  func(q *ports.AdminRequest) { q.Reason = strings.Repeat("r", app.MaxAdminReason+1) },
		"a key too long":     func(q *ports.AdminRequest) { q.Key = strings.Repeat("k", app.MaxAdminKey+1) },
	} {
		q := req(ports.AdminReconcile, `{"shard":1}`, "k-1")
		mutate(&q)
		if _, created, err := r.ops.Submit(t.Context(), q); !errors.Is(err, domain.ErrInvalid) || created {
			t.Errorf("%s: created %v, err %v, want %v", name, created, err, domain.ErrInvalid)
		}
	}
	if b, _ := r.store.Backlog(t.Context()); b.Waiting != 0 {
		t.Fatalf("a request that was refused was queued anyway: %+v", b)
	}
	if _, _, err := r.ops.Submit(t.Context(), req("burn", `{}`, "k-1")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a kind nobody defined: %v", err)
	}
}

func TestEachKindReadsItsArgumentsAndRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	good := []struct {
		kind   ports.AdminOperationKind
		params string
		want   string // the canonical form it is stored in
	}{
		{ports.AdminReconcile, `{"shard":3}`, `{"shard":3,"kind":"incremental"}`},
		{ports.AdminReconcile, `{"kind":"full","shard":7}`, `{"shard":7,"kind":"full"}`},
		{ports.AdminReconcile, ` { "shard" : 0 , "kind" : "incremental" } `, `{"shard":0,"kind":"incremental"}`},
		{ports.AdminRecover, `{"shard":5}`, `{"shard":5}`},
		{ports.AdminRehydrate, `{"scope":{"id":"w1","kind":"workspace"}}`, `{"scope":{"kind":"workspace","id":"w1"}}`},
		{ports.AdminReplay, `{"id":"5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b"}`, `{"id":"5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b"}`},
	}
	for i, g := range good {
		op := r.submit(t, req(g.kind, g.params, fmt.Sprintf("good-%d", i)))
		if string(op.Params) != g.want {
			t.Errorf("%s %s was stored as %s, want %s", g.kind, g.params, op.Params, g.want)
		}
	}
	bad := []struct {
		kind   ports.AdminOperationKind
		params string
	}{
		{ports.AdminReconcile, `{}`}, {ports.AdminReconcile, `{"shard":null}`}, {ports.AdminReconcile, `{"shard":-1}`},
		{ports.AdminReconcile, fmt.Sprintf(`{"shard":%d}`, rigShards)}, {ports.AdminReconcile, `{"shard":"3"}`}, {ports.AdminReconcile, `{"shard":1.5}`},
		{ports.AdminReconcile, `{"shard":1,"kind":"weekly"}`}, {ports.AdminReconcile, `{"shard":1,"force":true}`}, {ports.AdminReconcile, `{"shrad":1}`},
		{ports.AdminReconcile, `not json`}, {ports.AdminReconcile, `[1]`}, {ports.AdminReconcile, ``}, {ports.AdminReconcile, `{"shard":1} {"shard":2}`},
		{ports.AdminRecover, `{}`}, {ports.AdminRecover, `{"shard":8}`}, {ports.AdminRecover, `{"shard":1,"kind":"full"}`},
		{ports.AdminRehydrate, `{}`}, {ports.AdminRehydrate, `{"scope":{}}`}, {ports.AdminRehydrate, `{"scope":{"kind":"workspace"}}`},
		{ports.AdminRehydrate, `{"scope":{"kind":"Work Space","id":"w1"}}`}, {ports.AdminRehydrate, `{"scope":{"kind":"workspace","id":"w1","extra":1}}`},
		{ports.AdminReplay, `{}`}, {ports.AdminReplay, `{"id":""}`}, {ports.AdminReplay, `{"id":"nope"}`},
		{ports.AdminReplay, `{"id":"5B0E6F2A-1C3D-4E5F-8A9B-0C1D2E3F4A5B"}`}, {ports.AdminReplay, `{"id":"5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b/../x"}`},
	}
	for i, b := range bad {
		if _, created, err := r.ops.Submit(t.Context(), req(b.kind, b.params, fmt.Sprintf("bad-%d", i))); !errors.Is(err, domain.ErrInvalid) || created {
			t.Errorf("%s %q: created %v, err %v, want %v", b.kind, b.params, created, err, domain.ErrInvalid)
		}
	}
	if b, _ := r.store.Backlog(t.Context()); b.Waiting != int64(len(good)) {
		t.Fatalf("%d waiting after %d good and %d refused requests", b.Waiting, len(good), len(bad))
	}
}

func TestARehydrateNamesAScopeOfTheRequestsRealm(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	q := req(ports.AdminRehydrate, `{"scope":{"kind":"workspace","id":"w1"}}`, "k-1")
	q.Realm = "tenant-b"
	op := r.submit(t, q)
	if op.Realm != "tenant-b" {
		t.Fatalf("the operation is in realm %q, want the request's", op.Realm)
	}
}

func TestAKindTheCompositionRootDefinesIsReadByItsOwnParser(t *testing.T) {
	t.Parallel()
	kind := ports.AdminOperationKind("account-sweep")
	r := newAdminRig(t, app.AdminKind{Kind: kind, Parse: func(realm string, p json.RawMessage) (json.RawMessage, error) {
		var v struct {
			Account string `json:"account"`
		}
		if err := json.Unmarshal(p, &v); err != nil || v.Account == "" {
			return nil, fmt.Errorf("%w: an account is required", domain.ErrInvalid)
		}
		return json.RawMessage(`{"account":"` + v.Account + `","realm":"` + realm + `"}`), nil
	}})
	op := r.submit(t, req(kind, `{"account":"acct"}`, "k-1"))
	if string(op.Params) != `{"account":"acct","realm":"default"}` {
		t.Fatalf("params = %s: the kind's parser did not decide them", op.Params)
	}
	if _, _, err := r.ops.Submit(t.Context(), req(kind, `{}`, "k-2")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a request its parser refuses was accepted: %v", err)
	}
}

func TestARetryIsTheSameOperationEvenWhenItsJSONIsWrittenDifferently(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	first, created, err := r.ops.Submit(t.Context(), req(ports.AdminReconcile, `{"kind":"full","shard":3}`, "k-1"))
	if err != nil || !created {
		t.Fatal(created, err)
	}
	// A client library that re-serialises its body on retry changes the order of keys and the spacing, not the request.
	again, created, err := r.ops.Submit(t.Context(), req(ports.AdminReconcile, ` {"shard": 3, "kind": "full"} `, "k-1"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("a retry made %+v (created %v, err %v) after %+v", again, created, err, first)
	}
	if _, _, err := r.ops.Submit(t.Context(), req(ports.AdminReconcile, `{"shard":4,"kind":"full"}`, "k-1")); !errors.Is(err, domain.ErrIdemConflict) {
		t.Fatalf("a key reused for another shard: %v, want %v", err, domain.ErrIdemConflict)
	}
}

func TestAPersonAndAReasonAreKeptAsGivenAndNotTrimmedIntoSomethingElse(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	q := req(ports.AdminRecover, `{"shard":2}`, "k-1")
	q.Actor, q.Reason = "  alice@example.com ", "INC-1: the failover\n  took the shard"
	op := r.submit(t, q)
	if op.Actor != "alice@example.com" || op.Reason != "INC-1: the failover\n  took the shard" {
		t.Fatalf("actor %q, reason %q: the person is trimmed of the spaces around them and the reason is theirs word for word", op.Actor, op.Reason)
	}
}

func TestAWorkerRunsWhatItClaimsAndRecordsWhatItSaid(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminReconcile, `{"shard":3,"kind":"full"}`, "k-1"))
	var saw ports.AdminOperation
	w := r.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminReconcile: func(_ context.Context, o ports.AdminOperation) (json.RawMessage, error) {
			saw = o
			return json.RawMessage(`{"checked":12}`), nil
		},
	})
	did, err := w.RunOne(t.Context())
	if err != nil || !did {
		t.Fatalf("RunOne = %v, %v", did, err)
	}
	if saw.ID != op.ID || string(saw.Params) != `{"shard":3,"kind":"full"}` || saw.Attempts != 1 || saw.Actor != "alice@example.com" {
		t.Fatalf("the executor was handed %+v", saw)
	}
	got, _ := r.ops.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminSucceeded || string(got.Result) != `{"checked":12}` || got.Error != "" || got.FinishedAt == nil {
		t.Fatalf("recorded as %+v", got)
	}
	if r.metrics.ops(ports.AdminReconcile, "succeeded") != 1 {
		t.Fatal("a success was not counted")
	}
	if did, err := w.RunOne(t.Context()); did || err != nil {
		t.Fatalf("an empty queue: %v, %v", did, err)
	}
}

func TestAnExecutorThatFailsIsRecordedAsFailedAndIsNotRunAgain(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	var runs atomic.Int64
	w := r.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
			runs.Add(1)
			return nil, errors.New("the shard is frozen by another recovery")
		},
	})
	if did, err := w.RunOne(t.Context()); !did || err != nil {
		t.Fatalf("RunOne = %v, %v: a failing operation is the operation's outcome, not the worker's", did, err)
	}
	r.clock.Advance(time.Hour)
	if did, _ := w.RunOne(t.Context()); did || runs.Load() != 1 {
		t.Fatalf("a failed operation was run %d times", runs.Load())
	}
	got, _ := r.ops.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminFailed || got.Error != "the shard is frozen by another recovery" {
		t.Fatalf("recorded as %+v", got)
	}
	if r.metrics.ops(ports.AdminRecover, "failed") != 1 || r.metrics.ops(ports.AdminRecover, "succeeded") != 0 {
		t.Fatal("a failure was not counted as one")
	}
}

func TestAFailureTextIsKeptButNotWithoutLimit(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	w := r.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
			return nil, errors.New(strings.Repeat("x", 10*app.MaxAdminError))
		},
	})
	if _, err := w.RunOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, _ := r.ops.Get(t.Context(), "default", op.ID)
	if n := len(got.Error); n == 0 || n > app.MaxAdminError {
		t.Fatalf("a failure text of %d bytes was stored (limit %d)", n, app.MaxAdminError)
	}
}

func TestAnOperationNoExecutorServesFailsAtOnceAndDoesNotWaitForever(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminRehydrate, `{"scope":{"kind":"workspace","id":"w1"}}`, "k-1"))
	w := r.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{})
	if did, err := w.RunOne(t.Context()); !did || err != nil {
		t.Fatalf("RunOne = %v, %v", did, err)
	}
	got, _ := r.ops.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminFailed || !strings.Contains(got.Error, "rehydrate") {
		t.Fatalf("recorded as %+v: a worker that cannot do a thing must say so and not leave it pending for ever", got)
	}
}

func TestAPanickingExecutorFailsTheOperationAndNotTheWorker(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	op := r.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	w := r.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminRecover: func(context.Context, ports.AdminOperation) (json.RawMessage, error) { panic("index out of range") },
	})
	if did, err := w.RunOne(t.Context()); !did || err != nil {
		t.Fatalf("RunOne = %v, %v", did, err)
	}
	got, _ := r.ops.Get(t.Context(), "default", op.ID)
	if got.Status != ports.AdminFailed || !strings.Contains(got.Error, "panic: index out of range") {
		t.Fatalf("recorded as %+v", got)
	}
}

func TestAWorkerInterruptedMidwayLeavesTheOperationForTheNextAndFinishesWhatWasDone(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	a := r.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-a"))
	b := r.submit(t, req(ports.AdminReconcile, `{"shard":2}`, "k-b"))
	ctx, cancel := context.WithCancel(t.Context())
	var interrupted atomic.Bool
	ex := map[ports.AdminOperationKind]app.AdminExecutor{
		// The worker is told to stop while it is recovering: whatever it got through, it did not finish.
		ports.AdminRecover: func(c context.Context, _ ports.AdminOperation) (json.RawMessage, error) {
			if interrupted.CompareAndSwap(false, true) {
				cancel()
				return nil, c.Err()
			}
			return json.RawMessage(`{"recovered":true}`), nil
		},
		// ...and told to stop just after it finished the work: that outcome is not lost.
		ports.AdminReconcile: func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
			cancel()
			return json.RawMessage(`{"ran":true}`), nil
		},
	}
	w := r.worker(t, ex)
	_, _ = w.RunOne(ctx)
	if got, _ := r.ops.Get(t.Context(), "default", a.ID); got.Status != ports.AdminRunning {
		t.Fatalf("an interrupted operation was recorded as %+v: it was not done, and the next worker must do it", got)
	}
	r.clock.Advance(2 * time.Minute)
	w2 := r.worker(t, ex)
	if did, err := w2.RunOne(t.Context()); !did || err != nil {
		t.Fatalf("the next worker: %v, %v", did, err)
	}
	got, _ := r.ops.Get(t.Context(), "default", a.ID)
	if got.Status != ports.AdminSucceeded || got.Attempts != 2 {
		t.Fatalf("after the next worker: %+v, want it done on the second attempt", got)
	}
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	ex[ports.AdminReconcile] = func(context.Context, ports.AdminOperation) (json.RawMessage, error) {
		cancel2()
		return json.RawMessage(`{"ran":true}`), nil
	}
	if _, err := r.worker(t, ex).RunOne(ctx2); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got, _ := r.ops.Get(t.Context(), "default", b.ID); got.Status != ports.AdminSucceeded {
		t.Fatalf("work that finished as the worker was told to stop was not recorded: %+v", got)
	}
}

// However many workers there are, and however they race, an operation is done once: this is what keeps a manual adjustment from being
// booked twice.
func TestManyWorkersRunEachOperationExactlyOnce(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	const n = 24
	for i := range n {
		r.submit(t, req(ports.AdminReconcile, fmt.Sprintf(`{"shard":%d}`, i%rigShards), fmt.Sprintf("k-%d", i)))
		r.clock.Advance(time.Millisecond)
	}
	var mu sync.Mutex
	ran := map[string]int{}
	ex := map[ports.AdminOperationKind]app.AdminExecutor{
		ports.AdminReconcile: func(_ context.Context, o ports.AdminOperation) (json.RawMessage, error) {
			mu.Lock()
			ran[o.ID]++
			mu.Unlock()
			return nil, nil
		},
	}
	var wg sync.WaitGroup
	for range 8 {
		w := r.worker(t, ex)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2 * n { // bounded: a worker that never runs dry must fail this test, not hang it
				did, err := w.RunOne(t.Context())
				if err != nil {
					t.Errorf("RunOne: %v", err)
					return
				}
				if !did {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(ran) != n {
		t.Fatalf("%d of %d operations ran", len(ran), n)
	}
	for id, c := range ran {
		if c != 1 {
			t.Fatalf("operation %s ran %d times", id, c)
		}
	}
	if r.metrics.ops(ports.AdminReconcile, "succeeded") != n {
		t.Fatalf("%d successes counted for %d operations", r.metrics.ops(ports.AdminReconcile, "succeeded"), n)
	}
}

func TestTheBacklogIsExportedAsWhatWaitsAndHowLong(t *testing.T) {
	t.Parallel()
	r := newAdminRig(t)
	w := r.worker(t, map[ports.AdminOperationKind]app.AdminExecutor{})
	if err := w.ExportGauges(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, ok := r.metrics.gauge(ports.MetricAdminWaiting); !ok || v != 0 {
		t.Fatalf("an empty queue exports %d (set %v), want a zero and not a gap: a page needs to read a number", v, ok)
	}
	r.submit(t, req(ports.AdminRecover, `{"shard":1}`, "k-1"))
	r.clock.Advance(7 * time.Minute)
	if err := w.ExportGauges(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.metrics.gauge(ports.MetricAdminWaiting); v != 1 {
		t.Fatalf("waiting = %d, want 1", v)
	}
	if v, _ := r.metrics.gauge(ports.MetricAdminOldestWait); v != 420 {
		t.Fatalf("oldest wait = %d s, want 420", v)
	}
}

func TestAdminOperationsAreBuiltOnlyWithWhatTheyNeed(t *testing.T) {
	t.Parallel()
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	store := contracts.NewMemAdminOps(clock)
	if _, err := app.NewAdminOperations(app.AdminOperationsDeps{Shards: 8}); err == nil {
		t.Error("a request side with no store was built")
	}
	if _, err := app.NewAdminOperations(app.AdminOperationsDeps{Store: store}); err == nil {
		t.Error("a request side that does not know the deployment's shards was built: it could not tell a shard from a typo")
	}
	dup := app.AdminKind{Kind: ports.AdminReconcile, Parse: func(string, json.RawMessage) (json.RawMessage, error) { return nil, nil }}
	if _, err := app.NewAdminOperations(app.AdminOperationsDeps{Store: store, Shards: 8, Kinds: []app.AdminKind{dup}}); err == nil {
		t.Error("a kind the engine already defines was defined again")
	}
	if _, err := app.NewAdminOperations(app.AdminOperationsDeps{Store: store, Shards: 8, Kinds: []app.AdminKind{{Kind: "x"}}}); err == nil {
		t.Error("a kind with no parser was accepted")
	}
	if _, err := app.NewAdminWorker(app.AdminWorkerDeps{Executors: map[ports.AdminOperationKind]app.AdminExecutor{}}); err == nil {
		t.Error("a worker with no store was built")
	}
	if _, err := app.NewAdminWorker(app.AdminWorkerDeps{Store: store, Lease: -time.Second}); err == nil {
		t.Error("a worker with a negative lease was built")
	}
	if _, err := app.NewAdminWorker(app.AdminWorkerDeps{Store: store, Executors: map[ports.AdminOperationKind]app.AdminExecutor{"x": nil}}); err == nil {
		t.Error("a worker with a nil executor was built")
	}
}
