package payments_test

import (
	"bytes"
	"context"
	"errors"

	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/ports"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// R097c: what payment-worker does for payments, apart from the processes around it. Four things run for as long as it
// does, and each is held to the same few rules: it runs at once on start (a restart is not an interval of silence), it
// repeats, it never runs over itself, a failure is logged and tried again and does not stop its neighbours, and it stops
// with the context. The low-balance subscription is the fifth, and its failure to start is the one fatal thing.

const wait = 5 * time.Second

type processor struct {
	ticks, running, most atomic.Int64
	fail                 atomic.Bool
	hold                 time.Duration
	settle               time.Duration // work that outlives the cancellation, as a database call that is already on the wire does
	sawCancel, finished  atomic.Bool
}

func (p *processor) Tick(ctx context.Context) error {
	p.ticks.Add(1)
	n := p.running.Add(1)
	defer p.running.Add(-1)
	for {
		m := p.most.Load()
		if n <= m || p.most.CompareAndSwap(m, n) {
			break
		}
	}
	if p.hold > 0 {
		select {
		case <-time.After(p.hold):
		case <-ctx.Done():
			p.sawCancel.Store(true)
			time.Sleep(p.settle)
		}
	}
	p.finished.Store(true)
	if p.fail.Load() {
		return errors.New("the database is down")
	}
	return nil
}

type ingress struct {
	retries, running, most atomic.Int64
	fail                   atomic.Bool
}

func (g *ingress) Handle(context.Context, string, string, []byte, http.Header) ports.IngressResult {
	return ports.IngressResult{Status: http.StatusOK}
}

func (g *ingress) Retry(context.Context) error {
	g.retries.Add(1)
	n := g.running.Add(1)
	defer g.running.Add(-1)
	for {
		m := g.most.Load()
		if n <= m || g.most.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	if g.fail.Load() {
		return errors.New("the verifier is down")
	}
	return nil
}

type inbox struct {
	mu    sync.Mutex
	cuts  []time.Time
	fail  atomic.Bool
	calls atomic.Int64
}

func (i *inbox) Purge(_ context.Context, olderThan time.Time) (int, error) {
	i.calls.Add(1)
	i.mu.Lock()
	i.cuts = append(i.cuts, olderThan)
	i.mu.Unlock()
	if i.fail.Load() {
		return 0, errors.New("the purge failed")
	}
	return 3, nil
}

func (i *inbox) cutoffs() []time.Time {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]time.Time(nil), i.cuts...)
}

type recharger struct {
	mu   sync.Mutex
	got  []string
	fail error
}

func (r *recharger) Handle(_ context.Context, subject string, payload []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, subject+"|"+string(payload))
	return r.fail
}

type subscribed struct {
	subject, group string
	h              mports.Handler
}

type bus struct {
	mu     sync.Mutex
	subs   []subscribed
	err    error
	closed atomic.Int64
}

func (b *bus) Publish(context.Context, string, []byte) error { return nil }
func (b *bus) Subscribe(_ context.Context, subject, group string, h mports.Handler) (mports.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	b.subs = append(b.subs, subscribed{subject, group, h})
	return closer{&b.closed}, nil
}
func (b *bus) Pending(context.Context, string, string) (int64, time.Duration, error) {
	return 0, 0, nil
}
func (b *bus) Trim(context.Context, string, mports.RetentionPolicy) error { return nil }

func (b *bus) subscriptions() []subscribed {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]subscribed(nil), b.subs...)
}

type closer struct{ n *atomic.Int64 }

func (c closer) Close() error { c.n.Add(1); return nil }

type clock struct{ at time.Time }

func (c clock) Now() time.Time { return c.at }

var epoch = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// rig is a worker's payment loops over doubles.
type rig struct {
	proc *processor
	ing  *ingress
	box  *inbox
	rech *recharger
	bus  *bus
	logs *syncBuffer
	deps payments.LoopsDeps
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newRig() *rig {
	r := &rig{proc: &processor{}, ing: &ingress{}, box: &inbox{}, rech: &recharger{}, bus: &bus{}, logs: &syncBuffer{}}
	r.deps = payments.LoopsDeps{
		Processor: r.proc, Ingress: r.ing, Inbox: r.box, Recharge: r.rech, Bus: r.bus, Clock: clock{epoch},
		Log:  slog.New(slog.NewTextHandler(r.logs, nil)),
		Tick: 5 * time.Millisecond, Retention: 90 * 24 * time.Hour, PurgeEvery: 5 * time.Millisecond,
	}
	return r
}

// run starts the loops and returns a function that stops them and returns what Run returned.
func (r *rig) run(t *testing.T) (stop func() error) {
	t.Helper()
	l, err := payments.NewLoops(r.deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	var once sync.Once
	var out error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case out = <-done:
			case <-time.After(wait):
				t.Error("Run did not return after its context was cancelled")
			}
		})
		return out
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func TestEachStageRunsAtOnceOnStartAndThenRepeats(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.deps.Tick, r.deps.PurgeEvery = time.Hour, time.Hour // an interval that would pass no tick in the test
	stop := r.run(t)
	eventually(t, "the first processor tick, retry and purge, without waiting an interval", func() bool {
		return r.proc.ticks.Load() >= 1 && r.ing.retries.Load() >= 1 && r.box.calls.Load() >= 1
	})
	_ = stop()

	r = newRig()
	stop = r.run(t)
	eventually(t, "three ticks, retries and purges", func() bool {
		return r.proc.ticks.Load() >= 3 && r.ing.retries.Load() >= 3 && r.box.calls.Load() >= 3
	})
	_ = stop()
}

func TestAFailingStageIsLoggedAndTriedAgainAndStopsNoOtherStage(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.proc.fail.Store(true)
	r.ing.fail.Store(true)
	r.box.fail.Store(true)
	stop := r.run(t)
	eventually(t, "every stage to run three times though each fails", func() bool {
		return r.proc.ticks.Load() >= 3 && r.ing.retries.Load() >= 3 && r.box.calls.Load() >= 3
	})
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v: a stage that fails is tried again, it does not end the worker", err)
	}
	for _, want := range []string{"the database is down", "the verifier is down", "the purge failed"} {
		if !bytes.Contains([]byte(r.logs.String()), []byte(want)) {
			t.Errorf("the log does not say %q, so an operator cannot see why a payment is not fulfilled:\n%s", want, r.logs)
		}
	}
}

func TestNoStageRunsOverItself(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.proc.hold = 25 * time.Millisecond // slower than the interval
	stop := r.run(t)
	eventually(t, "several ticks", func() bool { return r.proc.ticks.Load() >= 3 && r.ing.retries.Load() >= 5 })
	_ = stop()
	if r.proc.most.Load() != 1 || r.ing.most.Load() != 1 {
		t.Fatalf("at most %d ticks and %d retries ran at once, want 1 of each: a slow database would pile up work on itself", r.proc.most.Load(), r.ing.most.Load())
	}
}

func TestPurgingKeepsItsOwnPaceAndDefaultsToHourly(t *testing.T) {
	t.Parallel()
	// A purge that is faster than the tick, with a tick that never comes again after the first.
	r := newRig()
	r.deps.Tick, r.deps.PurgeEvery = time.Hour, 5*time.Millisecond
	stop := r.run(t)
	eventually(t, "three purges", func() bool { return r.box.calls.Load() >= 3 })
	_ = stop()
	if r.proc.ticks.Load() != 1 {
		t.Fatalf("%d processor ticks: the tick interval is the purge's", r.proc.ticks.Load())
	}

	// A purge that is slower than the tick, and one left unset (hourly), while the inbox is worked every 5 ms.
	for name, every := range map[string]time.Duration{"an hour": time.Hour, "unset": 0} {
		r := newRig()
		r.deps.PurgeEvery = every
		stop := r.run(t)
		eventually(t, name+": ten ticks", func() bool { return r.proc.ticks.Load() >= 10 })
		_ = stop()
		if n := r.box.calls.Load(); n != 1 {
			t.Fatalf("%s: %d purges while the inbox was worked ten times, want the one at start: the purge is not the tick's, and unset is hourly", name, n)
		}
	}
}

func TestProcessedEventsPastRetentionArePurgedAsOfNow(t *testing.T) {
	t.Parallel()
	r := newRig()
	stop := r.run(t)
	eventually(t, "two purges", func() bool { return len(r.box.cutoffs()) >= 2 })
	_ = stop()
	want := epoch.Add(-90 * 24 * time.Hour)
	for _, got := range r.box.cutoffs() {
		if !got.Equal(want) {
			t.Fatalf("purged events older than %s, want %s: retention counted from the wrong instant", got, want)
		}
	}
}

func TestLowBalanceMessagesGoToAutoTopUpAndAFailureIsRedelivered(t *testing.T) {
	t.Parallel()
	r := newRig()
	stop := r.run(t)
	eventually(t, "the subscription", func() bool { return len(r.bus.subscriptions()) == 1 })
	s := r.bus.subscriptions()[0]
	if s.subject != "billing.balance.low.>" || s.group != "payment-autorecharge" {
		t.Fatalf("subscribed to %q as %q, want billing.balance.low.> as payment-autorecharge: a group that changed between deploys would deliver every message twice", s.subject, s.group)
	}
	if err := s.h(context.Background(), mports.Message{Subject: "billing.balance.low.acme%2Fuser:1", Payload: []byte(`{"x":1}`)}); err != nil {
		t.Fatal(err)
	}
	r.rech.mu.Lock()
	got := append([]string(nil), r.rech.got...)
	r.rech.fail = errors.New("the balance could not be read")
	r.rech.mu.Unlock()
	if len(got) != 1 || got[0] != `billing.balance.low.acme%2Fuser:1|{"x":1}` {
		t.Fatalf("the handler was given %v", got)
	}
	if err := s.h(context.Background(), mports.Message{Subject: "billing.balance.low.x", Payload: []byte(`{}`)}); err == nil {
		t.Fatal("a handler that failed acknowledged the message: the bus would never deliver it again and a customer would run dry")
	}
	_ = stop()
	if r.bus.closed.Load() != 1 {
		t.Fatalf("the subscription was closed %d times on shutdown, want 1", r.bus.closed.Load())
	}
}

func TestTheSubscriptionFollowsTheConfiguredPrefix(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.deps.SubjectPrefix = "acme"
	stop := r.run(t)
	eventually(t, "the subscription", func() bool { return len(r.bus.subscriptions()) == 1 })
	_ = stop()
	if got := r.bus.subscriptions()[0].subject; got != "acme.balance.low.>" {
		t.Fatalf("subscribed to %q, want acme.balance.low.>", got)
	}
}

func TestAWorkerThatCannotSubscribeDoesNotStartTheRest(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.bus.err = errors.New("redis: connection refused")
	l, err := payments.NewLoops(r.deps)
	if err != nil {
		t.Fatal(err)
	}
	err = l.Run(context.Background())
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("billing.balance.low.>")) {
		t.Fatalf("Run returned %v, want an error naming the subject it could not join: a worker that runs without auto top-up is one nobody knows is not topping up", err)
	}
	time.Sleep(20 * time.Millisecond)
	if r.proc.ticks.Load() != 0 || r.ing.retries.Load() != 0 {
		t.Fatalf("the loops started (%d ticks, %d retries) though the worker was about to fail", r.proc.ticks.Load(), r.ing.retries.Load())
	}
}

func TestStoppingCancelsTheStageInHandAndWaitsForIt(t *testing.T) {
	t.Parallel()
	r := newRig()
	r.proc.hold, r.proc.settle = time.Minute, 60*time.Millisecond // a tick that only the context can end, and then needs a moment
	stop := r.run(t)
	eventually(t, "a tick in progress", func() bool { return r.proc.running.Load() == 1 })
	if err := stop(); err != nil {
		t.Fatalf("Run returned %v after a clean stop", err)
	}
	if !r.proc.sawCancel.Load() {
		t.Fatal("the tick in hand never saw its context cancelled")
	}
	if !r.proc.finished.Load() || r.proc.running.Load() != 0 {
		t.Fatalf("Run returned while the tick in hand was still working (finished %v, running %d): a stop that does not wait abandons a payment half fulfilled", r.proc.finished.Load(), r.proc.running.Load())
	}
}

func TestLoopsAreBuiltOnlyWithWhatTheyNeed(t *testing.T) {
	t.Parallel()
	ok := newRig().deps
	if _, err := payments.NewLoops(ok); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*payments.LoopsDeps){
		"no processor":     func(d *payments.LoopsDeps) { d.Processor = nil },
		"no ingress":       func(d *payments.LoopsDeps) { d.Ingress = nil },
		"no inbox":         func(d *payments.LoopsDeps) { d.Inbox = nil },
		"no auto top-up":   func(d *payments.LoopsDeps) { d.Recharge = nil },
		"no bus":           func(d *payments.LoopsDeps) { d.Bus = nil },
		"no clock":         func(d *payments.LoopsDeps) { d.Clock = nil },
		"no logger":        func(d *payments.LoopsDeps) { d.Log = nil },
		"no tick":          func(d *payments.LoopsDeps) { d.Tick = 0 },
		"a negative tick":  func(d *payments.LoopsDeps) { d.Tick = -time.Second },
		"no retention":     func(d *payments.LoopsDeps) { d.Retention = 0 },
		"a negative purge": func(d *payments.LoopsDeps) { d.PurgeEvery = -time.Second },
	} {
		d := ok
		mutate(&d)
		if _, err := payments.NewLoops(d); err == nil {
			t.Errorf("%s: the loops were built", name)
		}
	}
	d := ok
	d.PurgeEvery = 0
	if _, err := payments.NewLoops(d); err != nil {
		t.Errorf("a purge interval left unset must default (hourly), got %v", err)
	}
}
