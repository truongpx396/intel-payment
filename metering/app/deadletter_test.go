package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R111 (FR-043): a message that exhausts its delivery budget is parked, never dropped, and the operator is told once. Redelivery
// below the cap is the Bus's; what is tested here is what happens at the cap, and that nothing but an operator's replay ever
// brings a parked message back.

const dlPrefix = "billing"

type dlBus struct {
	ports.Bus
	mu        sync.Mutex
	published []dlPublished
	fail      error
	joined    []string
}

type dlPublished struct {
	Subject string
	Payload []byte
}

func (b *dlBus) Publish(_ context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		err := b.fail
		b.fail = nil
		return err
	}
	b.published = append(b.published, dlPublished{subject, append([]byte(nil), payload...)})
	return nil
}

func (b *dlBus) Subscribe(_ context.Context, subject, group string, _ ports.Handler) (ports.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.joined = append(b.joined, subject+" as "+group)
	return nopSub{}, nil
}

func (b *dlBus) sent() []dlPublished {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]dlPublished(nil), b.published...)
}

func (b *dlBus) failNext(err error) {
	b.mu.Lock()
	b.fail = err
	b.mu.Unlock()
}

type nopSub struct{}

func (nopSub) Close() error { return nil }

type dlMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64
}

func newDLMetrics() *dlMetrics {
	return &dlMetrics{counts: map[string]int64{}, gauges: map[string]int64{}}
}

func (m *dlMetrics) Count(name string, n int64, l ports.Labels) {
	m.mu.Lock()
	m.counts[name+"{"+l["realm"]+"}"] += n
	m.mu.Unlock()
}

func (m *dlMetrics) Gauge(name string, v int64, l ports.Labels) {
	m.mu.Lock()
	m.gauges[name+"{"+l["realm"]+"}"] = v
	m.mu.Unlock()
}
func (*dlMetrics) Observe(string, float64, ports.Labels) {}

func (m *dlMetrics) pages(realm string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[ports.MetricDeadLettered+"{"+realm+"}"]
}

func (m *dlMetrics) totalPages() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, v := range m.counts {
		n += v
	}
	return n
}

func (m *dlMetrics) open(realm string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.gauges[ports.MetricDeadLetters+"{"+realm+"}"]
	return v, ok
}

// flakyStore fails Park a given number of times, as a database that is down does.
type flakyStore struct {
	*contracts.MemDeadLetters
	mu       sync.Mutex
	failures int
}

func (s *flakyStore) Park(ctx context.Context, l ports.DeadLetter) (ports.DeadLetter, bool, error) {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return ports.DeadLetter{}, false, errors.New("connection refused")
	}
	s.mu.Unlock()
	return s.MemDeadLetters.Park(ctx, l)
}

type dlRig struct {
	dl    *app.DeadLetters
	store *flakyStore
	clock *contracts.FakeClock
	bus   *dlBus
	met   *dlMetrics
}

func newDLRig(t *testing.T) *dlRig {
	t.Helper()
	clock := contracts.NewFakeClock(epoch)
	r := &dlRig{store: &flakyStore{MemDeadLetters: contracts.NewMemDeadLetters(clock)}, clock: clock, bus: &dlBus{}, met: newDLMetrics()}
	dl, err := app.NewDeadLetters(app.DeadLetterDeps{Store: r.store, Bus: r.bus, Metrics: r.met, Prefix: dlPrefix})
	if err != nil {
		t.Fatal(err)
	}
	r.dl = dl
	return r
}

// envelope is the message a Bus adapter publishes to <prefix>.dlq.<subject> when a message has used its attempts.
func envelope(subject, sourceID string, attempts int, payload []byte) ports.Message {
	body, err := json.Marshal(struct {
		Subject  string `json:"subject"`
		ID       string `json:"id"`
		Attempts int    `json:"attempts"`
		Error    string `json:"error"`
		Payload  []byte `json:"payload"`
	}{subject, sourceID, attempts, "handler failed", payload})
	if err != nil {
		panic(err)
	}
	return ports.Message{Subject: dlPrefix + ".dlq." + subject, ID: "dlq-" + sourceID, Payload: body, Attempts: 1}
}

func mustHandle(t *testing.T, dl *app.DeadLetters, m ports.Message) {
	t.Helper()
	if err := dl.Handle(context.Background(), m); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func letterOf(t *testing.T, r *dlRig, subject, sourceID string) ports.DeadLetter {
	t.Helper()
	l, created, err := r.store.Park(context.Background(), ports.DeadLetter{Subject: subject, SourceID: sourceID, Realm: "probe"})
	if err != nil || created {
		t.Fatalf("looking up %s/%s: created %v, %v", subject, sourceID, created, err)
	}
	return l
}

func TestAMessageThatUsedItsAttemptsIsParkedWholeAndPagesOnce(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	payload := []byte(`{"realm":"acme","scope":"acme/workspace:w1","event":"succeeded"}`)
	mustHandle(t, r.dl, envelope("billing.payment.acme%2Fworkspace:w1", "1700000000000-3", 5, payload))

	l := letterOf(t, r, "billing.payment.acme%2Fworkspace:w1", "1700000000000-3")
	if l.Realm != "acme" || l.Attempts != 5 || l.LastError != "handler failed" || !bytes.Equal(l.Payload, payload) || l.ReplayedAt != nil || !l.ParkedAt.Equal(epoch) {
		t.Fatalf("the parked letter is the original message and what became of it: %+v", l)
	}
	if got := r.met.pages("acme"); got != 1 {
		t.Fatalf("%d pages, want 1", got)
	}
	if sent := r.bus.sent(); len(sent) != 0 {
		t.Fatalf("parking delivered the message again: %v", sent)
	}
}

func TestTheSameEnvelopeDeliveredAgainParksNothingAndPagesNothing(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	m := envelope("billing.payment.t1", "5-0", 5, []byte(`{"realm":"acme"}`))
	for range 3 {
		mustHandle(t, r.dl, m)
	}
	if got := r.met.pages("acme"); got != 1 {
		t.Fatalf("three deliveries of one envelope paged %d times, want 1", got)
	}
	if open, _ := r.store.Open(context.Background()); open["acme"] != 1 {
		t.Fatalf("open letters %v, want one", open)
	}
}

func TestTheSameEnvelopeSweptByTwoSweepersAtOncePagesOnce(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	m := envelope("billing.payment.t1", "5-0", 5, []byte(`{"realm":"acme"}`))
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.dl.Handle(context.Background(), m); err != nil {
				t.Errorf("Handle: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := r.met.pages("acme"); got != 1 {
		t.Fatalf("sixteen concurrent sweeps paged %d times, want 1", got)
	}
}

func TestTheRealmIsTheOneThePayloadNamesAndAMessageThatNamesNoneIsGlobal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{"named", `{"realm":"acme"}`, "acme"},
		{"a tick", `{"shard":3}`, ports.GlobalRealm},
		{"not json", `not json at all`, ports.GlobalRealm},
		{"empty", ``, ports.GlobalRealm},
		{"a realm that is not a realm", `{"realm":"ACME CORP/../x"}`, ports.GlobalRealm},
		{"a realm that is not a string", `{"realm":5}`, ports.GlobalRealm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDLRig(t)
			mustHandle(t, r.dl, envelope("billing.x.y", "1-0", 5, []byte(tc.payload)))
			if got := letterOf(t, r, "billing.x.y", "1-0").Realm; got != tc.want {
				t.Fatalf("realm %q, want %q", got, tc.want)
			}
			if r.met.pages(tc.want) != 1 {
				t.Fatalf("the page does not carry the realm %q", tc.want)
			}
		})
	}
}

func TestAnEnvelopeNobodyCanReadIsParkedAsItArrivedNotDropped(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	raw := []byte{0xff, 0x00, '{', 'n', 'o', 't', ' ', 'a', 'n', ' ', 'e', 'n', 'v'}
	mustHandle(t, r.dl, ports.Message{Subject: dlPrefix + ".dlq.billing.x", ID: "9-9", Payload: raw, Attempts: 2})

	l := letterOf(t, r, dlPrefix+".dlq.billing.x", "9-9")
	if !bytes.Equal(l.Payload, raw) || l.Realm != ports.GlobalRealm || l.LastError == "" {
		t.Fatalf("an unreadable envelope must be kept as it arrived, with a reason: %+v", l)
	}
	if r.met.totalPages() != 1 {
		t.Fatalf("an unreadable envelope paged %d times, want 1", r.met.totalPages())
	}
}

func TestAFailureToParkIsReturnedSoTheBusDeliversAgainAndPagesOnceWhenItLands(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	r.store.failures = 2
	m := envelope("billing.payment.t1", "5-0", 5, []byte(`{"realm":"acme"}`))
	for range 2 {
		if err := r.dl.Handle(context.Background(), m); err == nil {
			t.Fatal("a letter that could not be parked was acknowledged: the bus would drop it")
		}
	}
	if r.met.totalPages() != 0 {
		t.Fatalf("paged %d times for a letter that is not parked", r.met.totalPages())
	}
	mustHandle(t, r.dl, m)
	mustHandle(t, r.dl, m)
	if got := r.met.pages("acme"); got != 1 {
		t.Fatalf("%d pages after the store recovered, want 1", got)
	}
}

func TestReplayPublishesTheOriginalPayloadToTheOriginalSubjectOnce(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	payload := []byte(`{"realm":"acme","watch":"general:5000"}`)
	mustHandle(t, r.dl, envelope("billing.balance.low.acme%2Fworkspace:w1", "8-0", 5, payload))
	id := letterOf(t, r, "billing.balance.low.acme%2Fworkspace:w1", "8-0").ID

	out, l, err := r.dl.Replay(context.Background(), id)
	if err != nil || out != app.Replayed || l.ReplayedAt == nil {
		t.Fatalf("Replay: %v, %v, replayed at %v", out, err, l.ReplayedAt)
	}
	sent := r.bus.sent()
	if len(sent) != 1 || sent[0].Subject != "billing.balance.low.acme%2Fworkspace:w1" || !bytes.Equal(sent[0].Payload, payload) {
		t.Fatalf("a replay re-publishes the original message to the subject it failed on: %+v", sent)
	}
	if out, _, err := r.dl.Replay(context.Background(), id); err != nil || out != app.AlreadyReplayed {
		t.Fatalf("a second replay: %v, %v", out, err)
	}
	if len(r.bus.sent()) != 1 {
		t.Fatalf("a second replay published again: %d publishes", len(r.bus.sent()))
	}
}

func TestSixteenReplaysAtOncePublishOnce(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	mustHandle(t, r.dl, envelope("billing.reconcile.tick", "3-0", 5, []byte(`{"shard":1}`)))
	id := letterOf(t, r, "billing.reconcile.tick", "3-0").ID

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		outs = map[app.ReplayOutcome]int{}
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, _, err := r.dl.Replay(context.Background(), id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Replay: %v", err)
			}
			outs[out]++
		}()
	}
	wg.Wait()
	if outs[app.Replayed] != 1 || len(r.bus.sent()) != 1 {
		t.Fatalf("outcomes %v, %d publishes: exactly one replay publishes", outs, len(r.bus.sent()))
	}
}

func TestAReplayWhosePublishFailedLeavesTheLetterParkedAndTheNextOneTriesAgain(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	mustHandle(t, r.dl, envelope("billing.payment.t1", "5-0", 5, []byte(`{"realm":"acme"}`)))
	id := letterOf(t, r, "billing.payment.t1", "5-0").ID

	r.bus.failNext(errors.New("bus unavailable"))
	if _, _, err := r.dl.Replay(context.Background(), id); err == nil {
		t.Fatal("a replay that published nothing reported success")
	}
	if l := letterOf(t, r, "billing.payment.t1", "5-0"); l.ReplayedAt != nil {
		t.Fatal("a failed publish marked the letter replayed: it is neither parked nor pending")
	}
	if open, _ := r.store.Open(context.Background()); open["acme"] != 1 {
		t.Fatalf("open %v: the letter must still be parked", open)
	}
	out, _, err := r.dl.Replay(context.Background(), id) // at once, without waiting out a lease
	if err != nil || out != app.Replayed || len(r.bus.sent()) != 1 {
		t.Fatalf("the retry: %v, %v, %d publishes", out, err, len(r.bus.sent()))
	}
}

func TestAReplayKilledAfterClaimingIsCompletedByTheNextOnceItsLeaseLapses(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	mustHandle(t, r.dl, envelope("billing.payment.t1", "5-0", 5, []byte(`{"realm":"acme"}`)))
	id := letterOf(t, r, "billing.payment.t1", "5-0").ID

	// The first replay claimed the letter and was killed before it published.
	if _, claimed, err := r.store.ClaimReplay(context.Background(), id, app.DefaultReplayLease); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	out, _, err := r.dl.Replay(context.Background(), id)
	if err != nil || out != app.ReplayInProgress || len(r.bus.sent()) != 0 {
		t.Fatalf("while the claim is live: %v, %v, %d publishes", out, err, len(r.bus.sent()))
	}
	r.clock.Advance(app.DefaultReplayLease + time.Second)
	out, _, err = r.dl.Replay(context.Background(), id)
	if err != nil || out != app.Replayed || len(r.bus.sent()) != 1 {
		t.Fatalf("after the lease: %v, %v, %d publishes: a replay killed midway must be completed by the next", out, err, len(r.bus.sent()))
	}
}

func TestALetterThatIsNotAnEnvelopeCannotBeReplayedAndIsKept(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	raw := []byte("garbage")
	mustHandle(t, r.dl, ports.Message{Subject: dlPrefix + ".dlq.billing.x", ID: "9-9", Payload: raw})
	id := letterOf(t, r, dlPrefix+".dlq.billing.x", "9-9").ID

	if _, _, err := r.dl.Replay(context.Background(), id); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("replaying a letter with no subject to go back to: %v", err)
	}
	if len(r.bus.sent()) != 0 {
		t.Fatalf("it was published anyway, to %v: onto the dead-letter subject, to be parked again", r.bus.sent())
	}
	if l := letterOf(t, r, dlPrefix+".dlq.billing.x", "9-9"); !bytes.Equal(l.Payload, raw) || l.ReplayedAt != nil {
		t.Fatalf("the letter was disturbed: %+v", l)
	}
}

func TestReplayOfALetterThatDoesNotExistIsNotFound(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	if _, _, err := r.dl.Replay(context.Background(), "00000000-0000-4000-8000-0000000000ff"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestTheOpenGaugeIsReadFromTheStoreAndFallsToZeroWhenTheLastLetterIsReplayed(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	mustHandle(t, r.dl, envelope("billing.payment.t1", "1-0", 5, []byte(`{"realm":"acme"}`)))
	mustHandle(t, r.dl, envelope("billing.payment.t1", "2-0", 5, []byte(`{"realm":"acme"}`)))
	mustHandle(t, r.dl, envelope("billing.payment.t2", "3-0", 5, []byte(`{"realm":"beta"}`)))
	if err := r.dl.ExportGauges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a, _ := r.met.open("acme"); a != 2 {
		t.Fatalf("acme gauge %d, want 2", a)
	}
	if b, _ := r.met.open("beta"); b != 1 {
		t.Fatalf("beta gauge %d, want 1", b)
	}
	if _, _, err := r.dl.Replay(context.Background(), letterOf(t, r, "billing.payment.t2", "3-0").ID); err != nil {
		t.Fatal(err)
	}
	if err := r.dl.ExportGauges(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, ok := r.met.open("beta"); !ok || b != 0 {
		t.Fatalf("beta gauge %d (exported: %v): a page that cannot clear stays lit", b, ok)
	}
}

func TestTheSweeperJoinsTheDeadLetterSubjectsUnderItsOwnGroup(t *testing.T) {
	t.Parallel()
	r := newDLRig(t)
	if _, err := r.dl.Subscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.bus.joined) != 1 || r.bus.joined[0] != "billing.dlq.> as "+app.DeadLetterGroup {
		t.Fatalf("joined %v", r.bus.joined)
	}
}

func TestADeadLetterSweeperWithoutWhatItNeedsIsRefused(t *testing.T) {
	t.Parallel()
	clock := contracts.NewFakeClock(epoch)
	store := contracts.NewMemDeadLetters(clock)
	for name, d := range map[string]app.DeadLetterDeps{
		"no store":    {Bus: &dlBus{}, Prefix: dlPrefix},
		"no bus":      {Store: store, Prefix: dlPrefix},
		"no prefix":   {Store: store, Bus: &dlBus{}},
		"bad lease":   {Store: store, Bus: &dlBus{}, Prefix: dlPrefix, Lease: -time.Second},
		"blank space": {Store: store, Bus: &dlBus{}, Prefix: "  "},
	} {
		if _, err := app.NewDeadLetters(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The property (R111): whatever order failures, duplicate deliveries and replays arrive in — including a replay killed midway, a
// publish that fails and replays racing each other — every message is in exactly one of pending and parked; a parked message is
// never delivered again except by a replay, and a replay delivers it once; the page fires once per parking; nothing is deleted.
func TestWhateverTheInterleavingAMessageIsInExactlyOneOfPendingAndParked(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 300; seed++ {
		runDeadLetterModel(t, seed)
		if t.Failed() {
			t.Fatalf("seed %d", seed)
		}
	}
}

type dlItem struct {
	subject   string
	realm     string
	inc       int       // the incarnation: each replay publishes a new message, which gets a new bus id when it fails
	parked    bool      // false = pending: on the bus, not yet failed
	claimedAt time.Time // a replay claimed it at this instant and was killed before it published; zero = no claim
	letters   []string
	replays   int
}

func (it *dlItem) source() string { return fmt.Sprintf("%s-%d", it.realm, it.inc) }

func runDeadLetterModel(t *testing.T, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	r := newDLRig(t)
	ctx := context.Background()
	items := make([]*dlItem, 5)
	for i := range items {
		items[i] = &dlItem{subject: fmt.Sprintf("billing.payment.t%d", i), realm: fmt.Sprintf("r%d", i)}
	}
	payloadOf := func(it *dlItem, inc int) []byte { return fmt.Appendf(nil, `{"realm":%q,"inc":%d}`, it.realm, inc) }
	pages, failures := int64(0), 0
	lease := app.DefaultReplayLease
	// live: a replay killed midway still holds the letter. Its claim lapses once the lease has run.
	live := func(it *dlItem) bool {
		return !it.claimedAt.IsZero() && r.clock.Now().Before(it.claimedAt.Add(lease))
	}
	current := func(it *dlItem) string { return it.letters[len(it.letters)-1] }

	check := func(step string) {
		t.Helper()
		open, err := r.store.Open(ctx)
		if err != nil {
			t.Fatalf("seed %d after %s: %v", seed, step, err)
		}
		for _, it := range items {
			want := int64(0)
			if it.parked {
				want = 1
			}
			if open[it.realm] != want {
				t.Fatalf("seed %d after %s: %s has %d parked letters, its model says %d: a message must be in exactly one of pending and parked", seed, step, it.subject, open[it.realm], want)
			}
			pub := 0
			for _, p := range r.bus.sent() {
				if p.Subject == it.subject {
					pub++
				}
			}
			if pub != it.replays {
				t.Fatalf("seed %d after %s: %s was delivered again %d times but replayed %d: only a replay brings a parked message back, and once", seed, step, it.subject, pub, it.replays)
			}
			for _, id := range it.letters {
				if _, err := r.store.Get(ctx, id); err != nil {
					t.Fatalf("seed %d after %s: letter %s was lost: %v", seed, step, id, err)
				}
			}
		}
		if got := r.met.totalPages(); got != pages {
			t.Fatalf("seed %d after %s: %d pages for %d parkings", seed, step, got, failures)
		}
	}

	for step := range 60 {
		it := items[rng.IntN(len(items))]
		switch op := rng.IntN(7); {
		case op == 0 && !it.parked: // the message used its attempts
			if err := r.dl.Handle(ctx, envelope(it.subject, it.source(), 5, payloadOf(it, it.inc))); err != nil {
				t.Fatalf("seed %d: Handle: %v", seed, err)
			}
			it.parked, failures, pages = true, failures+1, pages+1
			it.letters = append(it.letters, letterOf(t, r, it.subject, it.source()).ID)
		case op == 1 && len(it.letters) > 0: // the bus delivers an envelope it already delivered
			inc := rng.IntN(len(it.letters))
			if err := r.dl.Handle(ctx, envelope(it.subject, fmt.Sprintf("%s-%d", it.realm, inc), 5, payloadOf(it, inc))); err != nil {
				t.Fatalf("seed %d: Handle: %v", seed, err)
			}
		case op == 2 && it.parked: // an operator replays it
			out, _, err := r.dl.Replay(ctx, current(it))
			if live(it) {
				if err != nil || out != app.ReplayInProgress {
					t.Fatalf("seed %d: a claim is live: %v %v", seed, out, err)
				}
				break
			}
			if err != nil || out != app.Replayed {
				t.Fatalf("seed %d: Replay of a parked letter: %v %v", seed, out, err)
			}
			it.parked, it.claimedAt, it.replays, it.inc = false, time.Time{}, it.replays+1, it.inc+1
		case op == 3 && len(it.letters) > 0: // a letter that was replayed is replayed again
			old := it.letters[rng.IntN(len(it.letters))]
			if it.parked && old == current(it) {
				break
			}
			out, _, err := r.dl.Replay(ctx, old)
			if err != nil || out != app.AlreadyReplayed {
				t.Fatalf("seed %d: Replay of a letter already replayed: %v %v", seed, out, err)
			}
		case op == 4 && it.parked && !live(it): // operators racing
			var wg sync.WaitGroup
			var mu sync.Mutex
			won := 0
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					out, _, err := r.dl.Replay(ctx, current(it))
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						t.Errorf("seed %d: racing Replay: %v", seed, err)
					}
					if out == app.Replayed {
						won++
					}
				}()
			}
			wg.Wait()
			if won != 1 {
				t.Fatalf("seed %d: %d of 4 racing replays published, want exactly 1", seed, won)
			}
			it.parked, it.claimedAt, it.replays, it.inc = false, time.Time{}, it.replays+1, it.inc+1
		case op == 5 && it.parked && !live(it): // a replay claims it and is killed before it publishes
			if _, ok, err := r.store.ClaimReplay(ctx, current(it), lease); err != nil || !ok {
				t.Fatalf("seed %d: claim: %v %v", seed, ok, err)
			}
			it.claimedAt = r.clock.Now()
		case op == 6: // time passes; a publish fails now and then
			r.clock.Advance(time.Duration(rng.IntN(90)) * time.Second)
			if it.parked && !live(it) && rng.IntN(2) == 0 {
				r.bus.failNext(errors.New("bus unavailable"))
				if _, _, err := r.dl.Replay(ctx, current(it)); err == nil {
					t.Fatalf("seed %d: a replay that published nothing reported success", seed)
				}
				it.claimedAt = time.Time{} // a failed publish gives the claim back
			}
		}
		check(fmt.Sprintf("step %d", step))
	}
}
