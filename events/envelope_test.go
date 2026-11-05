package events_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
)

// R115: "The same event delivered twice carries the same id" — and the same bytes, whoever asks.

func sampleEvent() events.Event {
	return events.Event{
		ID: "0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9d0e", Realm: "my-product", Type: events.CreditsLow, ScopeKind: "organization", ScopeID: "org_1",
		Data: json.RawMessage(`{"pool":"general","threshold":5000000,"seq":1843}`), CreatedAt: time.Date(2026, 9, 28, 10, 15, 42, 103_000_000, time.UTC),
	}
}

func TestTheEnvelopeIsTheDocumentedShapeAndAPureFunctionOfTheEvent(t *testing.T) {
	t.Parallel()
	a, err := sampleEvent().Body()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := sampleEvent().Body()
	if string(a) != string(b) {
		t.Fatalf("the same event is two different bodies:\n%s\n%s", a, b)
	}
	want := `{"id":"0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9d0e","type":"credits.low","api_version":"2026-09-01","created_at":"2026-09-28T10:15:42.103Z",` +
		`"realm":"my-product","scope":{"kind":"organization","id":"org_1"},"data":{"pool":"general","threshold":5000000,"seq":1843}}`
	if string(a) != want {
		t.Fatalf("envelope:\n%s\nwant\n%s", a, want)
	}
}

func TestAnEventAboutNoScopeHasNoScopeAndNoDataIsAnEmptyObject(t *testing.T) {
	t.Parallel()
	e := sampleEvent()
	e.ScopeKind, e.ScopeID, e.Data = "", "", nil
	body, err := e.Body()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"scope"`) || !strings.Contains(string(body), `"data":{}`) {
		t.Fatalf("%s", body)
	}
}

func TestAnEventMissingItsIdentityHasNoEnvelope(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*events.Event){"no id": func(e *events.Event) { e.ID = "" }, "no type": func(e *events.Event) { e.Type = "" }, "no realm": func(e *events.Event) { e.Realm = "" }} {
		e := sampleEvent()
		mod(&e)
		if _, err := e.Body(); err == nil {
			t.Errorf("%s: an envelope was made", name)
		}
	}
}

func TestAnEndpointSubscribesToItsOwnRealmsTypesAndOnlyWhileActive(t *testing.T) {
	t.Parallel()
	ep := events.Endpoint{Realm: "acme", Types: []string{events.CreditsLow, events.PaymentSucceeded}, Active: true}
	for _, tc := range []struct {
		realm, typ string
		active     bool
		want       bool
	}{
		{"acme", events.CreditsLow, true, true}, {"acme", events.PaymentSucceeded, true, true}, {"acme", events.CreditsBlocked, true, false},
		{"other", events.CreditsLow, true, false}, {"acme", events.CreditsLow, false, false}, {"acme", "", true, false}, {"", events.CreditsLow, true, false},
	} {
		e := ep
		e.Active = tc.active
		if got := e.Subscribes(tc.realm, tc.typ); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc, got, tc.want)
		}
	}
}
