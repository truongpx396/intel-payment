package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/app"
	"github.com/truongpx396/intel-payment/events/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R115 (outbound-events.md § Contract-test obligations), at the use case: signed, retried on the schedule, dead after the window with a
// page, the same id every time, only what was subscribed to.

type dispRig struct {
	d     *app.Dispatcher
	store *store
	send  *sender
	met   *metrics
	clock *contracts.Clock
}

func newDispRig(t *testing.T, sec secrets) *dispRig {
	t.Helper()
	clock := contracts.NewClock(epoch)
	r := &dispRig{store: newStore(clock), send: &sender{clock: clock}, met: newMetrics(), clock: clock}
	d, err := app.NewDispatcher(app.DispatcherDeps{
		Store: r.store, Sender: r.send, Secrets: sec, Clock: clock, Metrics: r.met, Jitter: func() float64 { return 0.5 }, // no jitter
	})
	if err != nil {
		t.Fatal(err)
	}
	r.d = d
	return r
}

func (r *dispRig) endpoint(t *testing.T, id, realm, secretRef string, types ...string) {
	t.Helper()
	if _, err := r.store.AddEndpoint(context.Background(), events.Endpoint{ID: id, Realm: realm, URL: "https://hooks.example.com/" + id, SecretRef: secretRef, Types: types, Active: true}); err != nil {
		t.Fatal(err)
	}
}

func (r *dispRig) record(t *testing.T, realm, typ, source string) events.Event {
	t.Helper()
	e, _, err := r.store.Append(context.Background(), events.Event{Realm: realm, Type: typ, ScopeKind: "organization", ScopeID: "org_1", Data: []byte(`{"pool":"general"}`)}, source)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (r *dispRig) tick(t *testing.T) app.TickResult {
	t.Helper()
	res, err := r.d.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	return res
}

func TestADeliveryIsSignedWithTheEndpointsSecretAndVerifiesOnlyWithThat(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "secret-one", "env:TWO": "secret-two"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	r.endpoint(t, "two", "acme", "env:TWO", events.CreditsLow)
	e := r.record(t, "acme", events.CreditsLow, "s1")
	if res := r.tick(t); res.Delivered != 2 {
		t.Fatalf("delivered %d, want 2", res.Delivered)
	}
	for _, req := range r.send.sent() {
		own := map[string]string{"https://hooks.example.com/one": "secret-one", "https://hooks.example.com/two": "secret-two"}[req.URL]
		other := map[string]string{"https://hooks.example.com/one": "secret-two", "https://hooks.example.com/two": "secret-one"}[req.URL]
		if err := events.Verify(req.Header[events.HeaderSignature], req.Body, []byte(own), r.clock.Now(), 5*time.Minute); err != nil {
			t.Errorf("%s: its own secret does not verify: %v", req.URL, err)
		}
		if err := events.Verify(req.Header[events.HeaderSignature], req.Body, []byte(other), r.clock.Now(), 5*time.Minute); !errors.Is(err, events.ErrBadSignature) {
			t.Errorf("%s: another endpoint's secret verified: %v", req.URL, err)
		}
		if req.Header[events.HeaderEventID] != e.ID || req.Header["Content-Type"] != "application/json" {
			t.Errorf("%s: headers %v", req.URL, req.Header)
		}
	}
}

func TestARotationCarriesOneSignaturePerSecret(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:NEW": "new", "env:OLD": "old"})
	r.endpoint(t, "rot", "acme", "env:NEW, env:OLD", events.CreditsLow)
	r.record(t, "acme", events.CreditsLow, "s1")
	r.tick(t)
	req := r.send.sent()[0]
	if strings.Count(req.Header[events.HeaderSignature], "v1=") != 2 {
		t.Fatalf("%q", req.Header[events.HeaderSignature])
	}
	for _, s := range []string{"new", "old"} {
		if err := events.Verify(req.Header[events.HeaderSignature], req.Body, []byte(s), r.clock.Now(), time.Minute); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestTheSameEventDeliveredTwiceCarriesTheSameIdAndBody(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	e := r.record(t, "acme", events.CreditsLow, "s1")
	r.send.status = func(string) (int, error) { return 503, nil }
	r.tick(t)
	r.clock.Advance(time.Minute)
	r.tick(t) // the retry, a minute later
	got := r.send.sent()
	if len(got) != 2 {
		t.Fatalf("%d sends, want the attempt and its retry", len(got))
	}
	if got[0].Header[events.HeaderEventID] != e.ID || got[1].Header[events.HeaderEventID] != e.ID || string(got[0].Body) != string(got[1].Body) {
		t.Fatalf("a retry must be the same event: the host's dedup key is its id\n%v\n%v", got[0].Header, got[1].Header)
	}
	// The signature is fresh (its timestamp is the retry's), over the same body.
	if got[0].Header[events.HeaderSignature] == got[1].Header[events.HeaderSignature] {
		t.Error("a retry reused a stale signature: the host would refuse it for its age")
	}
}

func TestAnEndpointReceivesOnlyTheTypesItSubscribedToAndOnlyItsRealmsEvents(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:A": "a", "env:B": "b"})
	r.endpoint(t, "a", "acme", "env:A", events.CreditsLow)
	r.endpoint(t, "b", "beta", "env:B", events.CreditsLow, events.PaymentSucceeded)
	r.record(t, "acme", events.CreditsLow, "1")
	r.record(t, "acme", events.PaymentSucceeded, "2")
	r.record(t, "beta", events.CreditsLow, "3")
	r.record(t, "other", events.CreditsLow, "4")
	r.tick(t)
	var urls []string
	for _, req := range r.send.sent() {
		urls = append(urls, req.URL+" "+req.Header[events.HeaderEventID][:0]+fmt.Sprint(strings.Contains(string(req.Body), `"realm":"acme"`)))
	}
	if len(urls) != 2 {
		t.Fatalf("%d sends %v, want one to a (credits.low in acme) and one to b (credits.low in beta)", len(urls), urls)
	}
	for _, req := range r.send.sent() {
		isAcme := strings.Contains(string(req.Body), `"realm":"acme"`)
		if (req.URL == "https://hooks.example.com/a") != isAcme {
			t.Errorf("%s received %s", req.URL, req.Body)
		}
	}
}

// R115: "A 5xx endpoint is retried on the backoff schedule then marked dead after the window, with an alert."
func TestAnEndpointThatAnswers5xxIsRetriedOnTheScheduleThenMarkedDeadAndPaged(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	r.record(t, "acme", events.CreditsLow, "s1")
	r.send.status = func(string) (int, error) { return 502, nil }

	// The attempts are at 0, 1 m, 6 m, 36 m, 2 h 36 m, then every 6 h through 72 h.
	want := []time.Duration{0, time.Minute, 6 * time.Minute, 36 * time.Minute, 156 * time.Minute}
	for h := 156*time.Minute + 6*time.Hour; h <= events.Window; h += 6 * time.Hour {
		want = append(want, h)
	}
	for i, at := range want {
		r.clock.Advance(epoch.Add(at).Sub(r.clock.Now()))
		before := len(r.send.sent())
		res := r.tick(t)
		if len(r.send.sent()) != before+1 {
			t.Fatalf("attempt %d at +%v: %d sends", i, at, len(r.send.sent())-before)
		}
		// Not owed again before the schedule says so.
		if i+1 < len(want) {
			r.clock.Advance(epoch.Add(want[i+1]).Sub(r.clock.Now()) - time.Second)
			if extra := r.tick(t); extra != (app.TickResult{}) || len(r.send.sent()) != before+1 {
				t.Fatalf("attempt %d: sent again a second before it was due: %+v", i, extra)
			}
		} else if res.Dead != 1 {
			t.Fatalf("the last attempt at +%v: %+v, want it dead", at, res)
		}
	}
	if got := len(r.send.sent()); got != len(want) {
		t.Fatalf("%d attempts, want %d", got, len(want))
	}
	if n := r.met.count(events.MetricDeliveryFailed, ports.Labels{"realm": "acme", "endpoint": "one"}); n != 1 {
		t.Fatalf("events_delivery_failed_total = %d, want 1 (the page)", n)
	}
	if n := r.met.count(events.MetricDelivery, ports.Labels{"outcome": "retry"}); n != int64(len(want)-1) {
		t.Fatalf("%d retries counted, want %d", n, len(want)-1)
	}
	r.clock.Advance(200 * time.Hour)
	before := len(r.send.sent())
	r.tick(t)
	if len(r.send.sent()) != before {
		t.Fatal("a dead delivery was sent again")
	}
	if err := r.d.ExportGauges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.met.gauges[key(events.MetricDeadDeliveries, ports.Labels{"realm": "acme"})] != 1 {
		t.Fatal("the dead-deliveries gauge does not show it")
	}
}

func TestA2xxIsDeliveredOnceAndEverythingElseIsRetriedNotDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		err    error
		ok     bool
	}{
		{"200", 200, nil, true}, {"201", 201, nil, true}, {"204", 204, nil, true}, {"299", 299, nil, true},
		{"a redirect is not followed and is a failure", 302, nil, false}, {"a 3xx", 307, nil, false}, {"400", 400, nil, false}, {"401", 401, nil, false},
		{"404", 404, nil, false}, {"410", 410, nil, false}, {"429", 429, nil, false}, {"500", 500, nil, false}, {"503", 503, nil, false},
		{"1xx", 100, nil, false}, {"a timeout", 0, context.DeadlineExceeded, false}, {"a refused address", 0, events.ErrForbiddenAddress, false},
		{"a connection error", 0, errors.New("connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDispRig(t, secrets{"env:ONE": "s"})
			r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
			r.record(t, "acme", events.CreditsLow, "s1")
			r.send.status = func(string) (int, error) { return tc.status, tc.err }
			res := r.tick(t)
			if tc.ok != (res.Delivered == 1) || tc.ok == (res.Retried == 1) {
				t.Fatalf("%+v", res)
			}
			r.clock.Advance(30 * 24 * time.Hour)
			r.send.status = func(string) (int, error) { return 200, nil }
			again := r.tick(t)
			if tc.ok && len(r.send.sent()) != 1 {
				t.Fatalf("a delivered event was sent again")
			}
			if !tc.ok && (again.Delivered != 0 && len(r.send.sent()) != 2) {
				t.Fatalf("a failed delivery within the window was not retried: %+v", again)
			}
		})
	}
}

func TestASigningSecretThatCannotBeResolvedSendsNothingUnsigned(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:KNOWN": "k"})
	r.endpoint(t, "one", "acme", "env:MISSING", events.CreditsLow)
	r.endpoint(t, "two", "acme", "env:KNOWN, env:MISSING", events.CreditsLow) // one secret of a rotation is gone: still nothing unsigned
	r.record(t, "acme", events.CreditsLow, "s1")
	res := r.tick(t)
	if len(r.send.sent()) != 0 || res.Retried != 2 {
		t.Fatalf("sent %d, %+v: with a secret that cannot be resolved nothing goes out, and it is retried", len(r.send.sent()), res)
	}
}

func TestAWorkerKilledAfterSendingSendsAgainWithTheSameIdOnceItsLeaseLapses(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	e := r.record(t, "acme", events.CreditsLow, "s1")
	// A claim whose holder died before recording the outcome.
	if due, err := r.store.Claim(context.Background(), r.clock.Now(), app.DefaultLease, 10); err != nil || len(due) != 1 {
		t.Fatalf("%v %v", due, err)
	}
	if res := r.tick(t); res != (app.TickResult{}) || len(r.send.sent()) != 0 {
		t.Fatalf("a delivery under a live lease was sent: %+v", res)
	}
	r.clock.Advance(app.DefaultLease + time.Second)
	if res := r.tick(t); res.Delivered != 1 || r.send.sent()[0].Header[events.HeaderEventID] != e.ID {
		t.Fatalf("after the lease: %+v", res)
	}
}

func TestEightTicksAtOnceSendEachDeliveryOnce(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	for i := range 20 {
		r.record(t, "acme", events.CreditsLow, fmt.Sprintf("s%d", i))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.d.Tick(context.Background()); err != nil {
				t.Errorf("Tick: %v", err)
			}
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for _, req := range r.send.sent() {
		seen[req.Header[events.HeaderEventID]]++
	}
	if len(seen) != 20 {
		t.Fatalf("%d events sent, want 20", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s was sent %d times by concurrent ticks", id, n)
		}
	}
}

func TestATickWithNothingDueWritesNothing(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	if res := r.tick(t); res != (app.TickResult{}) {
		t.Fatalf("%+v", res)
	}
	r.record(t, "acme", events.CreditsLow, "s1")
	r.tick(t)
	delivered, sent := r.store.wrote("delivered"), len(r.send.sent())
	for range 3 {
		r.clock.Advance(time.Hour)
		r.tick(t)
	}
	if r.store.wrote("delivered") != delivered || r.store.wrote("failed") != 0 || len(r.send.sent()) != sent {
		t.Fatal("an idle tick wrote or sent something")
	}
}

func TestADeliveryStillPendingPastTheWindowIsEndedCountedAndPaged(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	r.record(t, "acme", events.CreditsLow, "s1")
	if err := r.store.SetActive(context.Background(), "one", false); err != nil { // switched off: it keeps what it is owed
		t.Fatal(err)
	}
	r.clock.Advance(events.Window)
	res := r.tick(t)
	if res.Dead != 1 || r.met.count(events.MetricDeliveryFailed, ports.Labels{"realm": "acme", "endpoint": "one"}) != 1 || len(r.send.sent()) != 0 {
		t.Fatalf("%+v, paged %d, sent %d", res, r.met.count(events.MetricDeliveryFailed, ports.Labels{"realm": "acme", "endpoint": "one"}), len(r.send.sent()))
	}
}

func TestAnOperatorCanReDriveADeadDeliveryAndItIsSentAgain(t *testing.T) {
	t.Parallel()
	r := newDispRig(t, secrets{"env:ONE": "s"})
	r.endpoint(t, "one", "acme", "env:ONE", events.CreditsLow)
	r.record(t, "acme", events.CreditsLow, "s1")
	due, _ := r.store.Claim(context.Background(), r.clock.Now(), time.Minute, 1)
	if err := r.store.Failed(context.Background(), due[0].DeliveryID, events.Failure{Dead: true, Err: "gave up"}); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	if res := r.tick(t); res != (app.TickResult{}) {
		t.Fatalf("a dead delivery was sent: %+v", res)
	}
	if err := r.d.Redeliver(context.Background(), due[0].DeliveryID); err != nil {
		t.Fatal(err)
	}
	if res := r.tick(t); res.Delivered != 1 {
		t.Fatalf("%+v", res)
	}
	if err := r.d.Redeliver(context.Background(), due[0].DeliveryID); !errors.Is(err, events.ErrInvalid) {
		t.Fatalf("re-driving a delivered one: %v", err)
	}
}

func TestADispatcherWithoutWhatItNeedsIsRefused(t *testing.T) {
	t.Parallel()
	clock := contracts.NewClock(epoch)
	ok := app.DispatcherDeps{Store: contracts.NewMemStore(clock), Sender: &sender{clock: clock}, Secrets: secrets{}, Clock: clock}
	for name, mod := range map[string]func(*app.DispatcherDeps){
		"no store": func(d *app.DispatcherDeps) { d.Store = nil }, "no sender": func(d *app.DispatcherDeps) { d.Sender = nil },
		"no secrets": func(d *app.DispatcherDeps) { d.Secrets = nil }, "no clock": func(d *app.DispatcherDeps) { d.Clock = nil },
		"negative lease": func(d *app.DispatcherDeps) { d.Lease = -1 }, "negative batch": func(d *app.DispatcherDeps) { d.Batch = -1 },
	} {
		d := ok
		mod(&d)
		if _, err := app.NewDispatcher(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
