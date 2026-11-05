package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/app"
	"github.com/truongpx396/intel-payment/events/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R115: the log records what the engine announces, once, and tells a host nothing it should not know.

type recRig struct {
	rec   *app.Recorder
	store *store
	met   *metrics
	bus   *bus
	clock *contracts.Clock
}

func newRecRig(t *testing.T) *recRig {
	t.Helper()
	clock := contracts.NewClock(epoch)
	r := &recRig{store: newStore(clock), met: newMetrics(), bus: &bus{}, clock: clock}
	rec, err := app.NewRecorder(app.RecorderDeps{Store: r.store, Bus: r.bus, Metrics: r.met, Prefix: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	r.rec = rec
	return r
}

func (r *recRig) feed(t *testing.T, realm string) []events.Event {
	t.Helper()
	out, err := r.store.Feed(context.Background(), events.FeedQuery{Realms: []string{realm}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func msg(subject, id, payload string) ports.Message {
	return ports.Message{Subject: subject, ID: id, Payload: []byte(payload), Attempts: 1}
}

func jsonEq(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	_ = json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("data %s, want %s", gb, wb)
	}
}

func TestEachAnnouncementBecomesTheDocumentedEventWithOnlyTheFieldsItNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, subject, payload string
		typ                    string
		data                   string
	}{
		{"low balance", "billing.balance.low.acme/organization:org_1",
			`{"realm":"acme","scope":"acme/organization:org_1","watch":"general:5000000","seq":1843}`, events.CreditsLow, `{"pool":"general","threshold":5000000,"seq":1843}`},
		{"warning", "billing.warn.acme/organization:org_1",
			`{"realm":"acme","scope":"acme/organization:org_1","limit_name":"user_daily","subject":"u1","cap":500,"window":"daily"}`, events.CreditsWarning, `{"limit":"user_daily","subject":"u1","cap":500,"window":"daily"}`},
		{"warning with no subject", "billing.warn.acme/organization:org_1",
			`{"realm":"acme","scope":"acme/organization:org_1","limit_name":"scope_balance","subject":"","cap":0,"window":"balance"}`, events.CreditsWarning, `{"limit":"scope_balance","cap":0,"window":"balance"}`},
		{"blocked", "billing.blocked.acme/organization:org_1",
			`{"realm":"acme","scope":"acme/organization:org_1","limit_name":"scope_balance","deny_code":"payment_required"}`, events.CreditsBlocked, `{"limit":"scope_balance","deny_code":"payment_required"}`},
		{"payment succeeded", "billing.payment.acme/organization:org_1",
			`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"succeeded","payment":"pi_1","amount":1000,"currency":"USD","receipt_url":"https://r/1","billing_email":"a@b.c","card":"4242"}`,
			events.PaymentSucceeded, `{"payment_id":"pi_1","amount":1000,"currency":"USD","receipt_url":"https://r/1"}`},
		{"payment failed", "billing.payment.acme/organization:org_1",
			`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"failed","payment":"pi_2","amount":1000,"currency":"USD","reason":"charge_declined"}`,
			events.PaymentFailed, `{"payment_id":"pi_2","amount":1000,"currency":"USD","failure_reason":"charge_declined"}`},
		{"refund", "billing.payment.acme/organization:org_1",
			`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"refunded","payment":"pi_1","amount":400,"currency":"USD"}`, events.PaymentRefunded, `{"payment_id":"pi_1","amount":400,"currency":"USD"}`},
		{"dispute", "billing.payment.acme/organization:org_1",
			`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"disputed","payment":"pi_1","amount":1000,"currency":"USD"}`, events.PaymentDisputed, `{"payment_id":"pi_1","amount":1000,"currency":"USD"}`},
		{"chargeback", "billing.payment.acme/organization:org_1",
			`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"charged_back","payment":"pi_1","amount":1000,"currency":"USD"}`, events.PaymentChargedBack, `{"payment_id":"pi_1","amount":1000,"currency":"USD"}`},
		{"entitlements", "billing.entitlement.acme/organization:org_1",
			`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"reason":"subscription"}`, events.EntitlementsChanged, `{"reason":"subscription"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecRig(t)
			subject := tokenSubject(tc.subject)
			if err := r.rec.Handle(context.Background(), msg(subject, "1-0", tc.payload)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := r.feed(t, "acme")
			if len(got) != 1 || got[0].Type != tc.typ || got[0].Realm != "acme" || got[0].ScopeKind != "organization" || got[0].ScopeID != "org_1" {
				t.Fatalf("recorded %+v", got)
			}
			jsonEq(t, got[0].Data, tc.data)
			for _, banned := range []string{"billing_email", "a@b.c", "card", "4242", "secret"} {
				if strings.Contains(string(got[0].Data), banned) {
					t.Fatalf("the event leaks %q: %s", banned, got[0].Data)
				}
			}
			if r.met.count(events.MetricRecorded, ports.Labels{"type": tc.typ}) != 1 {
				t.Fatalf("not counted")
			}
		})
	}
}

// tokenSubject is the subject the engine publishes on: <prefix>.<kind>.<scope token>. The token is the scope's tag with the characters a
// subject reserves (. * > whitespace %) percent-encoded, so the tags used here, which have none, are their own tokens.
func tokenSubject(s string) string { return s }

func TestTheSameAnnouncementDeliveredAgainIsOneEvent(t *testing.T) {
	t.Parallel()
	r := newRecRig(t)
	m := msg(tokenSubject("billing.balance.low.acme/organization:org_1"), "7-0", `{"realm":"acme","scope":"acme/organization:org_1","watch":"general:100","seq":4}`)
	for range 3 {
		if err := r.rec.Handle(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.feed(t, "acme"); len(got) != 1 {
		t.Fatalf("%d events from one bus message delivered three times", len(got))
	}
	if r.met.count(events.MetricRecorded, ports.Labels{"type": events.CreditsLow}) != 1 {
		t.Fatal("counted more than once")
	}
}

func TestTheSameAnnouncementSweptByTwoRecordersAtOnceIsOneEvent(t *testing.T) {
	t.Parallel()
	r := newRecRig(t)
	m := msg(tokenSubject("billing.blocked.acme/organization:org_1"), "9-0", `{"realm":"acme","scope":"acme/organization:org_1","limit_name":"l","deny_code":"d"}`)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.rec.Handle(context.Background(), m); err != nil {
				t.Errorf("Handle: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := r.feed(t, "acme"); len(got) != 1 {
		t.Fatalf("%d events", len(got))
	}
}

// A processor that retries after a crash announces one payment twice, on two bus messages; a host must not be told twice with two ids.
// But a refusal repeated in a later window is a new fact, even if its words are the same.
func TestOnePaymentAnnouncedTwiceIsOneEventAndOneRefusalInTwoWindowsIsTwo(t *testing.T) {
	t.Parallel()
	r := newRecRig(t)
	pay := `{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"succeeded","payment":"pi_1","amount":1000,"currency":"USD"}`
	subject := tokenSubject("billing.payment.acme/organization:org_1")
	for i, id := range []string{"1-0", "2-0"} {
		if err := r.rec.Handle(context.Background(), msg(subject, id, pay)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	// A cumulative refund of a different amount is another fact.
	if err := r.rec.Handle(context.Background(), msg(subject, "3-0", strings.Replace(pay, `"succeeded"`, `"refunded"`, 1))); err != nil {
		t.Fatal(err)
	}
	if got := r.feed(t, "acme"); len(got) != 2 {
		t.Fatalf("%d events: one payment announced twice is one, and its refund is another", len(got))
	}
	blocked := msg(tokenSubject("billing.blocked.acme/organization:org_1"), "4-0", `{"realm":"acme","scope":"acme/organization:org_1","limit_name":"l","deny_code":"d"}`)
	again := blocked
	again.ID = "5-0"
	_ = r.rec.Handle(context.Background(), blocked)
	_ = r.rec.Handle(context.Background(), again)
	if got := r.feed(t, "acme"); len(got) != 4 {
		t.Fatalf("%d events: the same refusal in two windows is two", len(got))
	}
}

func TestAnAnnouncementItCannotReadIsAnErrorSoTheBusParksItAndRecordsNothing(t *testing.T) {
	t.Parallel()
	good := `{"realm":"acme","scope":"acme/organization:org_1","watch":"general:100","seq":4}`
	for name, tc := range map[string]struct{ subject, payload string }{
		"not json":                     {"billing.balance.low.acme/organization:org_1", `not json`},
		"a realm that is not a realm":  {"billing.balance.low.acme/organization:org_1", `{"realm":"ACME CORP","scope":"ACME CORP/organization:org_1","watch":"general:100"}`},
		"no realm":                     {"billing.balance.low.acme/organization:org_1", `{"scope":"acme/organization:org_1","watch":"general:100"}`},
		"the realm and the tag differ": {"billing.balance.low.acme/organization:org_1", `{"realm":"beta","scope":"acme/organization:org_1","watch":"general:100"}`},
		"the subject names another":    {"billing.balance.low.acme/organization:org_2", good},
		"a tag that is not a tag":      {"billing.balance.low.x", `{"realm":"acme","scope":"nonsense","watch":"general:100"}`},
		"a watch that is not a watch":  {"billing.balance.low.acme/organization:org_1", `{"realm":"acme","scope":"acme/organization:org_1","watch":"general"}`},
		"a threshold that is not":      {"billing.balance.low.acme/organization:org_1", `{"realm":"acme","scope":"acme/organization:org_1","watch":"general:lots"}`},
		"a subject it does not read":   {"billing.mystery.acme/organization:org_1", good},
		"another prefix":               {"other.balance.low.acme/organization:org_1", good},
		"a payment of no scope":        {"billing.payment.acme/organization:org_1", `{"realm":"acme","scope":{"kind":"","id":""},"event":"succeeded"}`},
		"a payment on another subject": {"billing.payment.acme/organization:org_9", `{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":"succeeded","payment":"p","amount":1,"currency":"USD"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRecRig(t)
			err := r.rec.Handle(context.Background(), msg(tc.subject, "1-0", tc.payload))
			if err == nil || !errors.Is(err, events.ErrInvalid) {
				t.Fatalf("got %v: an unreadable announcement must be an error, so the bus delivers it again and then parks it", err)
			}
			if got := r.feed(t, "acme"); len(got) != 0 {
				t.Fatalf("recorded %+v", got)
			}
		})
	}
}

func TestAPaymentEventTheCatalogueDoesNotEmitYetIsAcknowledgedNotParked(t *testing.T) {
	t.Parallel()
	r := newRecRig(t)
	for _, ev := range []string{"renewed", "canceled", "something_new"} {
		p := fmt.Sprintf(`{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"event":%q,"payment":"p","amount":1,"currency":"USD"}`, ev)
		if err := r.rec.Handle(context.Background(), msg(tokenSubject("billing.payment.acme/organization:org_1"), "1-0", p)); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
	}
	if got := r.feed(t, "acme"); len(got) != 0 {
		t.Fatalf("recorded %+v", got)
	}
}

func TestAFailureToRecordIsReturnedSoTheAnnouncementIsDeliveredAgain(t *testing.T) {
	t.Parallel()
	clock := contracts.NewClock(epoch)
	st := &failingStore{MemStore: contracts.NewMemStore(clock), failures: 1}
	rec, err := app.NewRecorder(app.RecorderDeps{Store: st, Bus: &bus{}, Prefix: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	m := msg(tokenSubject("billing.entitlement.acme/organization:org_1"), "1-0", `{"realm":"acme","scope":{"kind":"organization","id":"org_1"},"reason":"override"}`)
	if err := rec.Handle(context.Background(), m); err == nil {
		t.Fatal("an announcement that was not recorded was acknowledged")
	}
	if err := rec.Handle(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Feed(context.Background(), events.FeedQuery{Realms: []string{"acme"}, Limit: 10}); len(got) != 1 {
		t.Fatalf("%d events", len(got))
	}
}

type failingStore struct {
	*contracts.MemStore
	mu       sync.Mutex
	failures int
}

func (s *failingStore) Append(ctx context.Context, e events.Event, src string) (events.Event, bool, error) {
	s.mu.Lock()
	if s.failures > 0 {
		s.failures--
		s.mu.Unlock()
		return events.Event{}, false, errors.New("connection refused")
	}
	s.mu.Unlock()
	return s.MemStore.Append(ctx, e, src)
}

func TestTheRecorderJoinsEveryAnnouncementUnderItsOwnGroup(t *testing.T) {
	t.Parallel()
	r := newRecRig(t)
	sub, err := r.rec.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	want := []string{"billing.balance.low.> as events-recorder", "billing.warn.> as events-recorder", "billing.blocked.> as events-recorder",
		"billing.payment.> as events-recorder", "billing.entitlement.> as events-recorder"}
	if fmt.Sprint(r.bus.joined) != fmt.Sprint(want) {
		t.Fatalf("joined %v", r.bus.joined)
	}
	r.bus.failOn = "billing.payment.>"
	r.bus.joined = nil
	if _, err := r.rec.Subscribe(context.Background()); err == nil || !strings.Contains(err.Error(), "billing.payment.>") {
		t.Fatalf("a recorder that cannot join every subject must say which: %v", err)
	}
}

func TestARecorderWithoutWhatItNeedsIsRefused(t *testing.T) {
	t.Parallel()
	clock := contracts.NewClock(epoch)
	for name, d := range map[string]app.RecorderDeps{
		"no store": {Bus: &bus{}, Prefix: "billing"}, "no bus": {Store: contracts.NewMemStore(clock), Prefix: "billing"}, "no prefix": {Store: contracts.NewMemStore(clock), Bus: &bus{}},
	} {
		if _, err := app.NewRecorder(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
