package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// Seeder is how a test makes the endpoints a store owes deliveries to: they are realm configuration, written through the database.
type Seeder interface {
	// AddEndpoint writes the endpoint and returns the id the store will report for it in a Due (a database keys it itself).
	AddEndpoint(ctx context.Context, ep events.Endpoint) (id string, err error)
	SetActive(ctx context.Context, id string, active bool) error
}

// Backend is a store under test and the way to seed it.
type Backend struct {
	Store events.Store
	Seed  Seeder
}

// StoreFactory builds a fresh, empty store that reads time from the clock it is given.
type StoreFactory func(tb testing.TB, clock *Clock) *Backend

// Case is one named obligation of the log.
type Case struct {
	Name string
	Run  func(tb testing.TB, f StoreFactory)
}

// The names of the cases, so a test that plants a defect can say which case must catch it.
const (
	CaseAppendOnce       = "an event is recorded once per source, however often the source is seen"
	CaseAppendTogether   = "the same source recorded from sixteen places at once is one event"
	CaseIDsIncrease      = "ids sort in the order events were recorded"
	CaseKeepsWhatItGot   = "an event keeps its realm, type, scope and data, and is stamped by the store"
	CaseFeedExactlyOnce  = "the feed returns every event after a cursor exactly once, in id order, across pages"
	CaseFeedWhileWriting = "a consumer paging while events are being recorded misses none and sees none twice"
	CaseFeedScoped       = "the feed returns only the realms and types asked for"
	CaseFanOut           = "an endpoint is owed only its own realm's events, of the types it subscribed to, while it is active"
	CaseDueIsComplete    = "a due delivery carries the event and the endpoint it goes to"
	CaseLease            = "a claim leases: no one else gets the delivery until the lease lapses"
	CaseDeliveredFinal   = "a delivered delivery is never owed again"
	CaseFailureSchedules = "a failed delivery is owed again at the time it was given, and counts its attempts"
	CaseDeadIsCounted    = "a dead delivery is never owed again, is counted, and only a dead one can be re-driven"
	CaseExpireStale      = "a delivery still pending past the window is ended, per realm; recent and settled ones are untouched"
	CasePurge            = "purge removes old events that nothing is owed for, and only those"
)

// Cases are the obligations of an events.Store (outbound-events.md).
func Cases() []Case {
	return []Case{
		{CaseAppendOnce, appendOnce}, {CaseAppendTogether, appendTogether}, {CaseIDsIncrease, idsIncrease}, {CaseKeepsWhatItGot, keepsWhatItGot},
		{CaseFeedExactlyOnce, feedExactlyOnce}, {CaseFeedWhileWriting, feedWhileWriting}, {CaseFeedScoped, feedScoped}, {CaseFanOut, fanOut},
		{CaseDueIsComplete, dueIsComplete}, {CaseLease, lease}, {CaseDeliveredFinal, deliveredFinal}, {CaseFailureSchedules, failureSchedules},
		{CaseDeadIsCounted, deadIsCounted}, {CaseExpireStale, expireStale}, {CasePurge, purge},
	}
}

// StoreContract runs every case, each against a store of its own, in parallel.
func StoreContract(t *testing.T, f StoreFactory) {
	t.Helper()
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

var epoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

type world struct {
	b     *Backend
	clock *Clock
	ids   map[string]string // endpoint name → the id the store reports
	names map[string]string // and back
}

func newWorld(tb testing.TB, f StoreFactory) *world {
	tb.Helper()
	clock := NewClock(epoch)
	return &world{b: f(tb, clock), clock: clock, ids: map[string]string{}, names: map[string]string{}}
}

func (w *world) endpoint(tb testing.TB, id, realm string, active bool, types ...string) events.Endpoint {
	tb.Helper()
	ep := events.Endpoint{ID: id, Realm: realm, URL: "https://hooks.example.com/" + id, SecretRef: "env:SECRET_" + id, Types: types, Active: active}
	got, err := w.b.Seed.AddEndpoint(context.Background(), ep)
	if err != nil {
		tb.Fatalf("seed endpoint %s: %v", id, err)
	}
	w.ids[id], w.names[got] = got, id
	return ep
}

func ev(realm, typ string, n int) events.Event {
	return events.Event{Realm: realm, Type: typ, ScopeKind: "workspace", ScopeID: fmt.Sprintf("w-%d", n), Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}
}

func (w *world) append(tb testing.TB, e events.Event, source string) events.Event {
	tb.Helper()
	stored, created, err := w.b.Store.Append(context.Background(), e, source)
	if err != nil || !created || stored.ID == "" {
		tb.Fatalf("Append %s: id %q created %v: %v", source, stored.ID, created, err)
	}
	return stored
}

func (w *world) feed(tb testing.TB, q events.FeedQuery) []events.Event {
	tb.Helper()
	out, err := w.b.Store.Feed(context.Background(), q)
	if err != nil {
		tb.Fatalf("Feed: %v", err)
	}
	return out
}

func (w *world) claim(tb testing.TB, limit int) []events.Due {
	tb.Helper()
	out, err := w.b.Store.Claim(context.Background(), w.clock.Now(), time.Minute, limit)
	if err != nil {
		tb.Fatalf("Claim: %v", err)
	}
	return out
}

func ids(es []events.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}

func appendOnce(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	first := w.append(tb, ev("acme", events.CreditsLow, 1), "bus-1")
	again, created, err := w.b.Store.Append(context.Background(), ev("acme", events.CreditsLow, 2), "bus-1")
	if err != nil || created || again.ID != first.ID {
		tb.Fatalf("the same source again: created %v, id %s then %s, %v", created, first.ID, again.ID, err)
	}
	if string(again.Data) != string(first.Data) {
		tb.Fatalf("the second recording rewrote the first: %s then %s", first.Data, again.Data)
	}
	if got := w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, Limit: 10}); len(got) != 1 {
		tb.Fatalf("%d events, want 1", len(got))
	}
	if got := w.claim(tb, 10); len(got) != 1 {
		tb.Fatalf("%d deliveries owed for one event and one endpoint, want 1", len(got))
	}
}

func appendTogether(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
		seen    = map[string]bool{}
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, c, err := w.b.Store.Append(context.Background(), ev("acme", events.CreditsLow, 1), "bus-7")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				tb.Errorf("Append: %v", err)
				return
			}
			if c {
				created++
			}
			seen[e.ID] = true
		}()
	}
	wg.Wait()
	if created != 1 || len(seen) != 1 {
		tb.Fatalf("%d concurrent recordings of one source: %d created it, %d events exist", 16, created, len(seen))
	}
}

func idsIncrease(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	var order []string
	for i := range 60 { // the clock does not move: every id is in the same millisecond
		order = append(order, w.append(tb, ev("acme", events.CreditsLow, i), fmt.Sprintf("s%d", i)).ID)
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			tb.Fatalf("event %d (%s) does not sort after event %d (%s): a cursor would skip it", i, order[i], i-1, order[i-1])
		}
	}
	if got := ids(w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, Limit: 100})); fmt.Sprint(got) != fmt.Sprint(order) {
		tb.Fatalf("the feed is not in recorded order:\n%v\n%v", got, order)
	}
}

func keepsWhatItGot(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	in := events.Event{Realm: "acme", Type: events.PaymentSucceeded, ScopeKind: "organization", ScopeID: "org_1", Data: json.RawMessage(`{"amount":1000,"currency":"USD","payment_id":"pi_1"}`)}
	w.clock.Advance(1500 * time.Millisecond)
	stored := w.append(tb, in, "s")
	got := w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, Limit: 5})
	if len(got) != 1 {
		tb.Fatalf("%d events", len(got))
	}
	for name, e := range map[string]events.Event{"as returned by Append": stored, "as read from the feed": got[0]} {
		if e.Realm != in.Realm || e.Type != in.Type || e.ScopeKind != in.ScopeKind || e.ScopeID != in.ScopeID || !sameJSON(e.Data, in.Data) {
			tb.Fatalf("%s: %+v", name, e)
		}
		if !events.ValidID(e.ID) || !e.CreatedAt.Equal(w.clock.Now()) {
			tb.Fatalf("%s: id %q, created %v (now %v): the store stamps both", name, e.ID, e.CreatedAt, w.clock.Now())
		}
	}
}

func feedExactlyOnce(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	var all []string
	for i := range 25 {
		all = append(all, w.append(tb, ev("acme", events.CreditsLow, i), fmt.Sprintf("s%d", i)).ID)
	}
	var got []string
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		page := w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, After: cursor, Limit: 4})
		if len(page) > 4 {
			tb.Fatalf("a page of %d with a limit of 4", len(page))
		}
		if len(page) == 0 {
			break
		}
		got = append(got, ids(page)...)
		cursor = page[len(page)-1].ID
	}
	if fmt.Sprint(got) != fmt.Sprint(all) {
		tb.Fatalf("paging 4 at a time returned %d events, want the 25 recorded, once each and in order:\n%v\n%v", len(got), got, all)
	}
	if rest := w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, After: all[len(all)-1], Limit: 4}); len(rest) != 0 {
		tb.Fatalf("%d events after the last one", len(rest))
	}
	if from := w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, After: all[9], Limit: 100}); len(from) != 15 || from[0].ID != all[10] {
		tb.Fatalf("after event 9: %d events starting %v, want the 15 after it (the cursor is exclusive)", len(from), ids(from))
	}
}

func feedWhileWriting(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	const writers, each = 4, 25
	// A consumer already holds a cursor when the writers start, so every event recorded from here on must sort after it. A store that
	// issues ids without looking at the last puts about half of them behind the cursor — whatever the timing of the consumer's reads, so
	// this is a certainty and not a race that has to be lost.
	first := w.append(tb, ev("acme", events.CreditsLow, -1), "first")
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, _, err := w.b.Store.Append(context.Background(), ev("acme", events.CreditsLow, g*each+i), fmt.Sprintf("g%d-%d", g, i)); err != nil {
					tb.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	seen := map[string]bool{first.ID: true}
	order := []string{first.ID}
	cursor := first.ID
	finished := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		page, err := w.b.Store.Feed(context.Background(), events.FeedQuery{Realms: []string{"acme"}, After: cursor, Limit: 7})
		if err != nil {
			tb.Fatalf("Feed: %v", err)
		}
		for _, e := range page {
			if seen[e.ID] {
				tb.Fatalf("event %s was returned twice", e.ID)
			}
			seen[e.ID] = true
			order = append(order, e.ID)
			cursor = e.ID
		}
		if len(page) == 0 {
			select {
			case <-done:
				if finished {
					goto checked
				}
				finished = true // one more pass after the writers stop
			default:
			}
		}
	}
checked:
	if len(order) != 1+writers*each {
		tb.Fatalf("a consumer paging while %d events were recorded saw %d: an event became visible behind its cursor", 1+writers*each, len(order)-1)
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			tb.Fatalf("pages are not in id order at %d", i)
		}
	}
}

func feedScoped(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	a1 := w.append(tb, ev("acme", events.CreditsLow, 1), "1")
	a2 := w.append(tb, ev("acme", events.PaymentSucceeded, 2), "2")
	b1 := w.append(tb, ev("beta", events.CreditsLow, 3), "3")
	if got := ids(w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, Limit: 10})); fmt.Sprint(got) != fmt.Sprint([]string{a1.ID, a2.ID}) {
		tb.Fatalf("realm acme: %v", got)
	}
	if got := ids(w.feed(tb, events.FeedQuery{Realms: []string{"beta"}, Limit: 10})); fmt.Sprint(got) != fmt.Sprint([]string{b1.ID}) {
		tb.Fatalf("realm beta: %v", got)
	}
	if got := w.feed(tb, events.FeedQuery{Realms: []string{"acme", "beta"}, Limit: 10}); len(got) != 3 {
		tb.Fatalf("both realms: %d", len(got))
	}
	if got := ids(w.feed(tb, events.FeedQuery{Realms: []string{"acme", "beta"}, Types: []string{events.CreditsLow}, Limit: 10})); fmt.Sprint(got) != fmt.Sprint([]string{a1.ID, b1.ID}) {
		tb.Fatalf("type credits.low: %v", got)
	}
	if got := w.feed(tb, events.FeedQuery{Realms: []string{"acme"}, Types: []string{events.PaymentSucceeded, "nothing.like.it"}, Limit: 10}); len(got) != 1 || got[0].ID != a2.ID {
		tb.Fatalf("two types, one unknown: %v", ids(got))
	}
	if got := w.feed(tb, events.FeedQuery{Realms: []string{"nobody"}, Limit: 10}); len(got) != 0 {
		tb.Fatalf("a realm with no events: %d", len(got))
	}
}

func fanOut(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	w.endpoint(tb, "low-acme", "acme", true, events.CreditsLow)
	w.endpoint(tb, "pay-acme", "acme", true, events.PaymentSucceeded)
	w.endpoint(tb, "low-beta", "beta", true, events.CreditsLow)
	w.endpoint(tb, "off-acme", "acme", false, events.CreditsLow)
	w.endpoint(tb, "both-acme", "acme", true, events.CreditsLow, events.PaymentSucceeded)

	owed := func(typ, realm string) string {
		w.append(tb, ev(realm, typ, 1), fmt.Sprintf("%s-%s", typ, realm))
		var urls []string
		for _, d := range w.claim(tb, 100) {
			urls = append(urls, w.names[d.Endpoint.ID])
		}
		return fmt.Sprint(sorted(urls))
	}
	if got := owed(events.CreditsLow, "acme"); got != "[both-acme low-acme]" {
		tb.Fatalf("credits.low in acme is owed to %s, want [both-acme low-acme] (not the other realm's, not the other type's, not the switched-off one)", got)
	}
	if got := owed(events.PaymentSucceeded, "acme"); got != "[both-acme pay-acme]" {
		tb.Fatalf("payment.succeeded in acme is owed to %s", got)
	}
	if got := owed(events.CreditsLow, "beta"); got != "[low-beta]" {
		tb.Fatalf("credits.low in beta is owed to %s", got)
	}
	if got := owed(events.CreditsBlocked, "acme"); got != "[]" {
		tb.Fatalf("a type nobody subscribed to is owed to %s", got)
	}
	// Switching an endpoint on does not back-date: the events already recorded are for the feed to backfill.
	if err := w.b.Seed.SetActive(context.Background(), w.ids["off-acme"], true); err != nil {
		tb.Fatal(err)
	}
	if got := w.claim(tb, 100); len(got) != 0 {
		tb.Fatalf("switching an endpoint on made %d old events owed to it", len(got))
	}
}

func sorted(ss []string) []string {
	out := append([]string(nil), ss...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func dueIsComplete(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	ep := w.endpoint(tb, "e1", "acme", true, events.CreditsLow, events.CreditsWarning)
	in := ev("acme", events.CreditsLow, 9)
	stored := w.append(tb, in, "s")
	due := w.claim(tb, 10)
	if len(due) != 1 {
		tb.Fatalf("%d due", len(due))
	}
	d := due[0]
	if d.DeliveryID == "" || d.Event.ID != stored.ID || d.Event.Realm != "acme" || d.Event.Type != events.CreditsLow || !sameJSON(d.Event.Data, in.Data) || !d.Event.CreatedAt.Equal(epoch) {
		tb.Fatalf("the event a delivery carries: %+v", d.Event)
	}
	if d.Endpoint.URL != ep.URL || d.Endpoint.SecretRef != ep.SecretRef || d.Endpoint.Realm != "acme" || fmt.Sprint(d.Endpoint.Types) != fmt.Sprint(ep.Types) {
		tb.Fatalf("the endpoint a delivery carries: %+v", d.Endpoint)
	}
	if d.Attempts != 0 || !d.CreatedAt.Equal(epoch) {
		tb.Fatalf("attempts %d, created %v", d.Attempts, d.CreatedAt)
	}
}

func lease(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	for i := range 5 {
		w.append(tb, ev("acme", events.CreditsLow, i), fmt.Sprintf("s%d", i))
	}
	first := w.claim(tb, 3)
	if len(first) != 3 {
		tb.Fatalf("a claim of 3 returned %d", len(first))
	}
	second := w.claim(tb, 10)
	if len(second) != 2 {
		tb.Fatalf("the second claim returned %d, want the 2 nobody holds: a delivery is leased to one claimant", len(second))
	}
	held := map[string]bool{}
	for _, d := range append(first, second...) {
		if held[d.DeliveryID] {
			tb.Fatalf("delivery %s was handed out twice inside its lease", d.DeliveryID)
		}
		held[d.DeliveryID] = true
	}
	if got := w.claim(tb, 10); len(got) != 0 {
		tb.Fatalf("%d claimed with every lease live", len(got))
	}
	w.clock.Advance(59 * time.Second)
	if got := w.claim(tb, 10); len(got) != 0 {
		tb.Fatalf("%d claimed inside the lease", len(got))
	}
	w.clock.Advance(2 * time.Second) // the holders were killed
	if got := w.claim(tb, 10); len(got) != 5 {
		tb.Fatalf("%d claimed after the lease lapsed, want all 5: a killed worker's deliveries must be completed by the next", len(got))
	}
}

func deliveredFinal(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	w.append(tb, ev("acme", events.CreditsLow, 1), "s")
	d := w.claim(tb, 1)[0]
	if err := w.b.Store.Delivered(context.Background(), d.DeliveryID, 200, w.clock.Now()); err != nil {
		tb.Fatal(err)
	}
	for _, step := range []time.Duration{0, time.Hour, 100 * time.Hour} {
		w.clock.Advance(step)
		if got := w.claim(tb, 10); len(got) != 0 {
			tb.Fatalf("a delivered delivery is owed again after %v", step)
		}
	}
	if n := deadTotal(tb, w); n != 0 {
		tb.Fatalf("%d dead", n)
	}
}

func deadTotal(tb testing.TB, w *world) int64 {
	tb.Helper()
	counts, err := w.b.Store.DeadCounts(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	var n int64
	for _, c := range counts {
		n += c
	}
	return n
}

func failureSchedules(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	w.append(tb, ev("acme", events.CreditsLow, 1), "s")
	d := w.claim(tb, 1)[0]
	next := w.clock.Now().Add(5 * time.Minute)
	if err := w.b.Store.Failed(context.Background(), d.DeliveryID, events.Failure{Status: 503, Err: "unavailable", Next: next}); err != nil {
		tb.Fatal(err)
	}
	w.clock.Advance(4*time.Minute + 59*time.Second)
	if got := w.claim(tb, 10); len(got) != 0 {
		tb.Fatal("a failed delivery is owed before the time it was given")
	}
	w.clock.Advance(2 * time.Second)
	got := w.claim(tb, 10)
	if len(got) != 1 || got[0].Attempts != 1 || got[0].DeliveryID != d.DeliveryID || !got[0].CreatedAt.Equal(epoch) {
		tb.Fatalf("owed again: %+v", got)
	}
	if err := w.b.Store.Failed(context.Background(), d.DeliveryID, events.Failure{Err: "timeout", Next: w.clock.Now().Add(time.Minute)}); err != nil {
		tb.Fatal(err)
	}
	w.clock.Advance(2 * time.Minute)
	if got := w.claim(tb, 10); len(got) != 1 || got[0].Attempts != 2 {
		tb.Fatalf("after a second failure: %+v", got)
	}
	if err := w.b.Store.Failed(context.Background(), "00000000-0000-4000-8000-0000000000ff", events.Failure{}); !errors.Is(err, events.ErrNotFound) {
		tb.Fatalf("failing a delivery that does not exist: %v", err)
	}
	if err := w.b.Store.Delivered(context.Background(), "00000000-0000-4000-8000-0000000000ff", 200, w.clock.Now()); !errors.Is(err, events.ErrNotFound) {
		tb.Fatalf("delivering a delivery that does not exist: %v", err)
	}
}

func deadIsCounted(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	ctx := context.Background()
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	w.endpoint(tb, "e2", "beta", true, events.CreditsLow)
	w.append(tb, ev("acme", events.CreditsLow, 1), "a")
	w.append(tb, ev("beta", events.CreditsLow, 2), "b")
	due := w.claim(tb, 10)
	var acme events.Due
	for _, d := range due {
		if d.Event.Realm == "acme" {
			acme = d
		}
	}
	if err := w.b.Store.Failed(ctx, acme.DeliveryID, events.Failure{Status: 500, Err: "gave up", Dead: true}); err != nil {
		tb.Fatal(err)
	}
	counts, err := w.b.Store.DeadCounts(ctx)
	if err != nil || counts["acme"] != 1 || counts["beta"] != 0 {
		tb.Fatalf("dead counts %v, %v", counts, err)
	}
	w.clock.Advance(200 * time.Hour)
	for _, d := range w.claim(tb, 10) {
		if d.DeliveryID == acme.DeliveryID {
			tb.Fatal("a dead delivery is owed again")
		}
	}
	// Only a dead delivery can be re-driven; the others are not touched.
	for _, d := range due {
		if d.DeliveryID != acme.DeliveryID {
			if err := w.b.Store.Redeliver(ctx, d.DeliveryID, w.clock.Now()); !errors.Is(err, events.ErrInvalid) {
				tb.Fatalf("re-driving a delivery that is not dead: %v", err)
			}
		}
	}
	if err := w.b.Store.Redeliver(ctx, "00000000-0000-4000-8000-0000000000ff", w.clock.Now()); !errors.Is(err, events.ErrNotFound) {
		tb.Fatalf("re-driving a delivery that does not exist: %v", err)
	}
	if err := w.b.Store.Redeliver(ctx, acme.DeliveryID, w.clock.Now()); err != nil {
		tb.Fatalf("Redeliver: %v", err)
	}
	if err := w.b.Store.Redeliver(ctx, acme.DeliveryID, w.clock.Now()); !errors.Is(err, events.ErrInvalid) {
		tb.Fatalf("re-driving it twice: %v", err)
	}
	got := w.claim(tb, 10)
	found := false
	for _, d := range got {
		found = found || d.DeliveryID == acme.DeliveryID
	}
	if !found {
		tb.Fatalf("a re-driven delivery is not owed: %+v", got)
	}
	if n := deadTotal(tb, w); n != 0 {
		tb.Fatalf("%d dead after the re-drive", n)
	}
}

func expireStale(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	ctx := context.Background()
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	w.endpoint(tb, "e2", "beta", true, events.CreditsLow)
	w.append(tb, ev("acme", events.CreditsLow, 1), "old-acme")
	w.append(tb, ev("beta", events.CreditsLow, 2), "old-beta")
	settled := w.claim(tb, 10)
	for _, d := range settled {
		if d.Event.Realm == "beta" {
			if err := w.b.Store.Delivered(ctx, d.DeliveryID, 200, w.clock.Now()); err != nil {
				tb.Fatal(err)
			}
		}
	}
	w.clock.Advance(events.Window - time.Hour)
	w.append(tb, ev("acme", events.CreditsLow, 3), "recent-acme")
	if n, err := w.b.Store.ExpireStale(ctx, w.clock.Now()); err != nil || len(n) != 0 {
		tb.Fatalf("nothing is past the window yet: %v, %v", n, err)
	}
	perRealm := func(ended []events.Ended) map[string]int64 {
		out := map[string]int64{}
		for _, e := range ended {
			if w.names[e.Endpoint] == "" {
				tb.Fatalf("an ended delivery names endpoint %q, which is none of the endpoints", e.Endpoint)
			}
			out[e.Realm] += e.N
		}
		return out
	}
	w.clock.Advance(time.Hour) // the first acme delivery is now exactly Window old; the recent one is an hour old
	ended, err := w.b.Store.ExpireStale(ctx, w.clock.Now())
	if n := perRealm(ended); err != nil || n["acme"] != 1 || n["beta"] != 0 || len(ended) != 1 || w.names[ended[0].Endpoint] != "e1" {
		tb.Fatalf("expired %v, %v: want one acme delivery, of endpoint e1 (the delivered beta one and the recent acme one are not stale)", ended, err)
	}
	if again, _ := w.b.Store.ExpireStale(ctx, w.clock.Now()); len(again) != 0 {
		tb.Fatalf("expiring twice ended %v the second time", again)
	}
	if deadTotal(tb, w) != 1 {
		tb.Fatalf("%d dead, want 1", deadTotal(tb, w))
	}
}

func purge(tb testing.TB, f StoreFactory) {
	w := newWorld(tb, f)
	ctx := context.Background()
	w.endpoint(tb, "e1", "acme", true, events.CreditsLow)
	oldNoEndpoint := w.append(tb, ev("beta", events.PaymentFailed, 1), "old-quiet") // nothing owed: no endpoint of beta
	oldOwed := w.append(tb, ev("acme", events.CreditsLow, 2), "old-owed")
	due := w.claim(tb, 10)
	w.clock.Advance(40 * 24 * time.Hour)
	recent := w.append(tb, ev("beta", events.PaymentFailed, 3), "recent")
	cutoff := w.clock.Now().Add(-30 * 24 * time.Hour)
	n, err := w.b.Store.Purge(ctx, cutoff)
	if err != nil || n != 1 {
		tb.Fatalf("purged %d, %v: want only the old event nothing is owed for", n, err)
	}
	got := ids(w.feed(tb, events.FeedQuery{Realms: []string{"acme", "beta"}, Limit: 10}))
	if fmt.Sprint(got) != fmt.Sprint([]string{oldOwed.ID, recent.ID}) {
		tb.Fatalf("after the purge the feed has %v, want [%s %s] (not %s)", got, oldOwed.ID, recent.ID, oldNoEndpoint.ID)
	}
	// Once what was owed is settled, the old event goes too, and its delivery with it.
	for _, d := range due {
		if err := w.b.Store.Delivered(ctx, d.DeliveryID, 200, w.clock.Now()); err != nil {
			tb.Fatal(err)
		}
	}
	if n, err := w.b.Store.Purge(ctx, cutoff); err != nil || n != 1 {
		tb.Fatalf("purged %d, %v after the delivery settled", n, err)
	}
	if got := w.feed(tb, events.FeedQuery{Realms: []string{"acme", "beta"}, Limit: 10}); len(got) != 1 || got[0].ID != recent.ID {
		tb.Fatalf("after the second purge: %v", ids(got))
	}
}
