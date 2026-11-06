package app_test

import (
	"context"
	"errors"
	"fmt"
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

// R110 (bus-subjects.md § Scheduled ticks): "every handler is an idempotent claim, so a duplicated tick has no second effect" — one table
// test over ALL of them: two concurrent runs of one tick do the work once; a run killed midway is completed by the next; a tick with
// nothing due writes nothing.

// worklist is the work behind a fake handler: a queue of items, each of which is done at most once however many runs there are, and a
// log of what was written. A run takes the items off the queue; it can be told to die after some.
type worklist struct {
	mu      sync.Mutex
	pending []int
	done    map[int]int // item → times it was done
	writes  int
	running atomic.Int32
	maxRun  atomic.Int32
	dieAt   int           // die (hang, as a killed process does) after this many items; 0 = never
	hung    chan struct{} // closed when a run has hung
	hang    chan struct{} // the hung run waits for this, which only the test's end closes
	gate    chan struct{} // when set, a run waits for it before it starts: lets many runs pile up
}

func newWorklist(n int) *worklist {
	w := &worklist{done: map[int]int{}, hung: make(chan struct{}), hang: make(chan struct{})}
	for i := range n {
		w.pending = append(w.pending, i)
	}
	return w
}

func (w *worklist) run(ctx context.Context, _ []byte) (int, error) {
	cur := w.running.Add(1)
	defer w.running.Add(-1)
	for {
		m := w.maxRun.Load()
		if cur <= m || w.maxRun.CompareAndSwap(m, cur) {
			break
		}
	}
	if w.gate != nil {
		select {
		case <-w.gate:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second): // a broken implementation fails the test; it does not hang it
			return 0, errors.New("the run was never let go")
		}
	}
	did := 0
	for {
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.mu.Unlock()
			return did, nil
		}
		item := w.pending[0]
		w.pending = w.pending[1:]
		w.done[item]++
		w.writes++
		w.mu.Unlock()
		did++
		if w.dieAt > 0 && did == w.dieAt {
			close(w.hung)
			<-w.hang // killed: it never returns, never releases
			return did, errors.New("unreachable")
		}
	}
}

func (w *worklist) timesDone() map[int]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[int]int{}
	for k, v := range w.done {
		out[k] = v
	}
	return out
}

type tickRig struct {
	ticks   *app.Ticks
	claims  *contracts.MemClaimer
	metrics *recMetrics
	work    map[string]*worklist
}

func payloadFor(name string) []byte {
	if name == "reconcile" || name == "audit" {
		return []byte(`{"shard":3}`)
	}
	return nil
}

func keyFor(name string) func([]byte) (string, error) {
	if name == "reconcile" || name == "audit" {
		return func(p []byte) (string, error) {
			sp, err := app.ParseShard(p, 8)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d|%s", *sp.Shard, sp.Kind), nil
		}
	}
	return app.NoPayload
}

func newTickRig(t *testing.T, items int, mod func(name string, w *worklist)) *tickRig {
	t.Helper()
	r := &tickRig{claims: contracts.NewMemClaimer(), metrics: newMetrics(), work: map[string]*worklist{}}
	var hs []app.TickHandler
	for _, name := range app.TickNames {
		w := newWorklist(items)
		if mod != nil {
			mod(name, w)
		}
		r.work[name] = w
		hs = append(hs, app.TickHandler{Name: name, Key: keyFor(name), Run: w.run})
	}
	tk, err := app.NewTicks(app.TicksDeps{Claims: r.claims, Metrics: r.metrics, Prefix: "billing", Handlers: hs})
	if err != nil {
		t.Fatal(err)
	}
	r.ticks = tk
	return r
}

func tickMsg(name string, payload []byte) ports.Message {
	return ports.Message{Subject: app.TickSubject("billing", name), ID: "1-0", Payload: payload, Attempts: 1}
}

func TestThereAreFourteenTicksAndEachHasItsOwnSubject(t *testing.T) {
	t.Parallel()
	want := []string{"reconcile", "audit", "transfer", "journal", "webhook", "outbound", "expiry", "dunning", "subdrift", "partitions", "idem.purge", "events.purge", "trim", "usage.rollup"}
	if fmt.Sprint(app.TickNames) != fmt.Sprint(want) {
		t.Fatalf("the ticks are %v, want the fourteen of bus-subjects.md in its order: %v", app.TickNames, want)
	}
	if got := app.TickSubject("billing", "idem.purge"); got != "billing.idem.purge.tick" {
		t.Fatalf("%q", got)
	}
}

// Two concurrent runs of one tick do the work once.
func TestEveryTickRunsTwiceAtOnceAsOne(t *testing.T) {
	t.Parallel()
	for _, name := range app.TickNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTickRig(t, 20, func(_ string, w *worklist) { w.gate = make(chan struct{}) })
			w := r.work[name]
			var wg sync.WaitGroup
			var mu sync.Mutex
			outcomes := map[app.TickOutcome]int{}
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					o, err := r.ticks.Do(context.Background(), name, payloadFor(name))
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						t.Errorf("Do: %v", err)
					}
					outcomes[o]++
				}()
			}
			// Seven of the eight are turned away at the claim while the eighth is still working.
			deadline := time.After(10 * time.Second)
			for r.metrics.count(ports.MetricTick+"{"+name+"skipped}") < 7 {
				select {
				case <-deadline:
					t.Fatalf("the other runs were not turned away: %v", r.metrics.snapshot())
				case <-time.After(5 * time.Millisecond):
				}
			}
			close(w.gate)
			wg.Wait()
			if outcomes[app.TickRan] != 1 || outcomes[app.TickSkipped] != 7 {
				t.Fatalf("outcomes %v, want one run and seven skipped", outcomes)
			}
			if w.maxRun.Load() != 1 {
				t.Fatalf("%d runs of one tick were at work at once", w.maxRun.Load())
			}
			for item, n := range w.timesDone() {
				if n != 1 {
					t.Fatalf("item %d was done %d times", item, n)
				}
			}
			if len(w.timesDone()) != 20 {
				t.Fatalf("%d of 20 items done", len(w.timesDone()))
			}
		})
	}
}

// A run killed midway is completed by the next.
func TestEveryTickKilledMidwayIsCompletedByTheNext(t *testing.T) {
	t.Parallel()
	for _, name := range app.TickNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTickRig(t, 10, func(_ string, w *worklist) { w.dieAt = 4 })
			w := r.work[name]
			go func() { _, _ = r.ticks.Do(context.Background(), name, payloadFor(name)) }() // dies after four items, holding its claim
			select {
			case <-w.hung:
			case <-time.After(10 * time.Second):
				t.Fatal("the first run never started")
			}
			// While it is alive and holds the claim, a second run does nothing.
			if o, err := r.ticks.Do(context.Background(), name, payloadFor(name)); err != nil || o != app.TickSkipped {
				t.Fatalf("a run while another holds the claim: %v, %v", o, err)
			}
			// It dies: a killed process holds nothing.
			key := name + "|" + map[bool]string{true: "3|incremental", false: ""}[name == "reconcile" || name == "audit"]
			r.claims.Kill(key)
			w.dieAt = 0
			o, err := r.ticks.Do(context.Background(), name, payloadFor(name))
			if err != nil || o != app.TickRan {
				t.Fatalf("the next run: %v, %v", o, err)
			}
			done := w.timesDone()
			if len(done) != 10 {
				t.Fatalf("%d of 10 items done after the takeover", len(done))
			}
			for item, n := range done {
				if n != 1 {
					t.Errorf("item %d was done %d times", item, n)
				}
			}
			close(w.hang)
		})
	}
}

func TestATickWhoseRunFailsHandsTheWorkToTheNextAndTheBusRedeliversIt(t *testing.T) {
	t.Parallel()
	for _, name := range app.TickNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			r := newTickRig(t, 6, nil)
			failing := app.TickHandler{Name: name, Key: keyFor(name), Run: func(ctx context.Context, p []byte) (int, error) {
				if calls.Add(1) == 1 {
					_, _ = r.work[name].run(ctx, p) // did the work... and then the database went away
					return 0, errors.New("connection reset")
				}
				return r.work[name].run(ctx, p)
			}}
			tk, err := app.NewTicks(app.TicksDeps{Claims: r.claims, Metrics: r.metrics, Prefix: "billing", Handlers: []app.TickHandler{failing}})
			if err != nil {
				t.Fatal(err)
			}
			if err := tk.Handle(context.Background(), tickMsg(name, payloadFor(name))); err == nil || !strings.Contains(err.Error(), "connection reset") {
				t.Fatalf("a failed tick must be an error, so the bus delivers it again: %v", err)
			}
			// Its claim is back, so the redelivery is not turned away.
			if err := tk.Handle(context.Background(), tickMsg(name, payloadFor(name))); err != nil {
				t.Fatal(err)
			}
			if r.metrics.count(ports.MetricTick+"{"+name+"failed}") != 1 {
				t.Fatalf("the failure is not counted: %v", r.metrics.snapshot())
			}
			if r.metrics.count(ports.MetricTick+"{"+name+"skipped}") != 0 || r.metrics.count(ports.MetricTick+"{"+name+"idle}") != 1 {
				t.Fatalf("the redelivery was turned away at a claim the failed run kept, instead of running: %v", r.metrics.snapshot())
			}
		})
	}
}

// A tick with nothing due writes nothing: it is counted idle, and holds no row anywhere.
func TestEveryTickWithNothingDueIsIdleAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, name := range app.TickNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newTickRig(t, 0, nil)
			for range 3 {
				o, err := r.ticks.Do(context.Background(), name, payloadFor(name))
				if err != nil || o != app.TickIdle {
					t.Fatalf("%v, %v", o, err)
				}
			}
			if w := r.work[name]; w.writes != 0 {
				t.Fatalf("an idle tick wrote %d times", w.writes)
			}
			if got := r.metrics.count(ports.MetricTick + "{" + name + "idle}"); got != 3 {
				t.Fatalf("%d idle ticks counted, want 3", got)
			}
			// And it left its claim: the next tick runs.
			if o, _ := r.ticks.Do(context.Background(), name, payloadFor(name)); o != app.TickIdle {
				t.Fatalf("an idle tick kept its claim: %v", o)
			}
		})
	}
}

func TestATickIsAddressedByItsSubjectAndAMessageForNoTickIsRefused(t *testing.T) {
	t.Parallel()
	r := newTickRig(t, 2, nil)
	if err := r.ticks.Handle(context.Background(), tickMsg("idem.purge", nil)); err != nil {
		t.Fatal(err)
	}
	if got := r.work["idem.purge"].timesDone(); len(got) != 2 {
		t.Fatalf("the idem.purge tick ran the wrong work: %v", got)
	}
	if len(r.work["trim"].timesDone()) != 0 || len(r.work["events.purge"].timesDone()) != 0 {
		t.Fatal("another tick's work ran")
	}
	for _, subject := range []string{"billing.mystery.tick", "billing.reconcile", "other.reconcile.tick", "billing.tick", "reconcile.tick", ""} {
		err := r.ticks.Handle(context.Background(), ports.Message{Subject: subject, Payload: payloadFor("reconcile")})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%q: %v", subject, err)
		}
	}
}

func TestAShardTickNamesItsShardAndTwoShardsAreTwoClaims(t *testing.T) {
	t.Parallel()
	r := newTickRig(t, 4, func(_ string, w *worklist) { w.gate = make(chan struct{}) })
	for _, bad := range []string{``, `{}`, `{"shard":-1}`, `{"shard":8}`, `not json`, `{"shard":"three"}`, `{"shard":null}`} {
		if err := r.ticks.Handle(context.Background(), tickMsg("reconcile", []byte(bad))); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%q: %v (a tick that means nothing must not be taken to mean everything)", bad, err)
		}
	}
	var wg sync.WaitGroup
	for _, p := range []string{`{"shard":1}`, `{"shard":2}`, `{"shard":1,"kind":"full"}`} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.ticks.Do(context.Background(), "reconcile", []byte(p)); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		}()
	}
	deadline := time.After(10 * time.Second)
	for r.work["reconcile"].running.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("shards 1 and 2, and shard 1's full sweep, were not three claims: %d at work", r.work["reconcile"].running.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(r.work["reconcile"].gate)
	wg.Wait()
}

func TestAClaimThatCannotBeAskedIsAFailureNotASkip(t *testing.T) {
	t.Parallel()
	w := newWorklist(3)
	tk, err := app.NewTicks(app.TicksDeps{Claims: brokenClaims{}, Metrics: newMetrics(), Prefix: "billing", Handlers: []app.TickHandler{{Name: "journal", Key: app.NoPayload, Run: w.run}}})
	if err != nil {
		t.Fatal(err)
	}
	if o, err := tk.Do(context.Background(), "journal", nil); err == nil || o != app.TickFailed {
		t.Fatalf("%v, %v: with the database down a tick is redelivered, not believed done", o, err)
	}
	if w.writes != 0 {
		t.Fatal("the work ran without a claim")
	}
}

type brokenClaims struct{}

func (brokenClaims) TryClaim(context.Context, string) (func(), bool, error) {
	return nil, false, errors.New("connection refused")
}

func TestATickSkippedByItsHandlerIsSkippedNotFailed(t *testing.T) {
	t.Parallel()
	m := newMetrics()
	tk, err := app.NewTicks(app.TicksDeps{Claims: contracts.NewMemClaimer(), Metrics: m, Prefix: "billing", Handlers: []app.TickHandler{{
		Name: "audit", Key: app.NoPayload, Run: func(context.Context, []byte) (int, error) {
			return 0, fmt.Errorf("shard 3 is held by another worker: %w", app.ErrTickSkipped)
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if o, err := tk.Do(context.Background(), "audit", nil); err != nil || o != app.TickSkipped {
		t.Fatalf("%v, %v", o, err)
	}
	if m.count(ports.MetricTick+"{auditskipped}") != 1 {
		t.Fatalf("%v", m.snapshot())
	}
}

func TestTheTicksAProcessServesAreJoinedOneSubjectEachUnderTheWorkerGroup(t *testing.T) {
	t.Parallel()
	r := newTickRig(t, 1, nil)
	b := &dlBus{}
	sub, err := r.ticks.Subscribe(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	if len(b.joined) != 14 || b.joined[0] != "billing.reconcile.tick as worker" || b.joined[13] != "billing.usage.rollup.tick as worker" {
		t.Fatalf("joined %d: %v", len(b.joined), b.joined)
	}
	// A process that serves only some ticks joins only those.
	partial, err := app.NewTicks(app.TicksDeps{Claims: contracts.NewMemClaimer(), Prefix: "billing", Handlers: []app.TickHandler{
		{Name: "trim", Key: app.NoPayload, Run: newWorklist(0).run}, {Name: "journal", Key: app.NoPayload, Run: newWorklist(0).run},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(partial.Served()) != "[journal trim]" {
		t.Fatalf("served %v: in table order", partial.Served())
	}
	b2 := &dlBus{}
	if _, err := partial.Subscribe(context.Background(), b2); err != nil || len(b2.joined) != 2 {
		t.Fatalf("%v, %v", b2.joined, err)
	}
}

func TestTicksWithoutWhatTheyNeedOrWithAnUnknownHandlerAreRefused(t *testing.T) {
	t.Parallel()
	ok := app.TickHandler{Name: "trim", Key: app.NoPayload, Run: newWorklist(0).run}
	for name, d := range map[string]app.TicksDeps{
		"no claimer":      {Prefix: "billing", Handlers: []app.TickHandler{ok}},
		"no prefix":       {Claims: contracts.NewMemClaimer(), Handlers: []app.TickHandler{ok}},
		"an unknown tick": {Claims: contracts.NewMemClaimer(), Prefix: "billing", Handlers: []app.TickHandler{{Name: "nightly", Key: app.NoPayload, Run: ok.Run}}},
		"no key":          {Claims: contracts.NewMemClaimer(), Prefix: "billing", Handlers: []app.TickHandler{{Name: "trim", Run: ok.Run}}},
		"no run":          {Claims: contracts.NewMemClaimer(), Prefix: "billing", Handlers: []app.TickHandler{{Name: "trim", Key: app.NoPayload}}},
		"two for a tick":  {Claims: contracts.NewMemClaimer(), Prefix: "billing", Handlers: []app.TickHandler{ok, ok}},
	} {
		if _, err := app.NewTicks(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tk, _ := app.NewTicks(app.TicksDeps{Claims: contracts.NewMemClaimer(), Prefix: "billing", Handlers: []app.TickHandler{ok}})
	if o, err := tk.Do(context.Background(), "audit", nil); err == nil || o != app.TickFailed {
		t.Errorf("a tick this process does not serve: %v, %v", o, err)
	}
}

func TestAUsageRollupTickMayAskForADaysBackfillWithinRange(t *testing.T) {
	t.Parallel()
	for payload, want := range map[string]struct {
		days int
		ok   bool
	}{"": {0, false}, `{}`: {0, false}, `{"days":null}`: {0, false}, `{"days":1}`: {1, true}, `{"days":400}`: {400, true}, `{"days":3660}`: {3660, true}} {
		days, ok, err := app.ParseRollupDays([]byte(payload))
		if err != nil || days != want.days || ok != want.ok {
			t.Errorf("%q: %d %v %v", payload, days, ok, err)
		}
	}
	for _, bad := range []string{`{"days":0}`, `{"days":-1}`, `{"days":3661}`, `{"days":"7"}`, `{"days":2.5}`, `not json`, `[1]`} {
		if _, _, err := app.ParseRollupDays([]byte(bad)); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// R112: an operator who asks for a reconcile gets one whether or not anything was booked since the last; the scheduled tick does not
// carry the flag and so keeps asking whether there is anything due.
func TestAShardPayloadMayAskForTheWorkToBeForcedAndOnlyDoesSoWhenItSaysSo(t *testing.T) {
	t.Parallel()
	for payload, want := range map[string]bool{`{"shard":1}`: false, `{"shard":1,"force":false}`: false, `{"shard":1,"kind":"full","force":true}`: true} {
		sp, err := app.ParseShard([]byte(payload), 8)
		if err != nil || sp.Force != want {
			t.Errorf("%s: force %v, err %v, want %v", payload, sp.Force, err, want)
		}
	}
	if _, err := app.ParseShard([]byte(`{"shard":1,"force":"yes"}`), 8); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a force that is not a boolean: %v", err)
	}
}
