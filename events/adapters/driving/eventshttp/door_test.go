package eventshttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/adapters/driving/eventshttp"
	"github.com/truongpx396/intel-payment/events/app"
	"github.com/truongpx396/intel-payment/events/contracts"
)

// R115: the feed returns every event after a cursor exactly once, in id order, across pages — over HTTP, scoped to the credential.

var epoch = time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)

// caller is a credential: the realms it covers, or why it has none.
type caller struct {
	realms []string
	any    bool
	err    error
}

func (c caller) Realms(_ context.Context, requested string) ([]string, error) {
	switch {
	case c.err != nil:
		return nil, c.err
	case requested != "":
		for _, r := range c.realms {
			if r == requested {
				return []string{requested}, nil
			}
		}
		if c.any {
			return []string{requested}, nil
		}
		return nil, eventshttp.ErrRealmMismatch
	case c.any:
		return nil, eventshttp.ErrRealmRequired
	}
	return c.realms, nil
}

type rig struct {
	door  *eventshttp.Door
	store *contracts.MemStore
	ids   []string
}

func newRig(t *testing.T, c caller, n int) *rig {
	t.Helper()
	st := contracts.NewMemStore(contracts.NewClock(epoch))
	r := &rig{store: st}
	for i := range n {
		realm, typ := "acme", events.CreditsLow
		if i%3 == 1 {
			typ = events.PaymentSucceeded
		}
		if i%5 == 4 {
			realm = "beta"
		}
		e, _, err := st.Append(context.Background(), events.Event{Realm: realm, Type: typ, ScopeKind: "organization", ScopeID: fmt.Sprintf("o%d", i), Data: []byte(fmt.Sprintf(`{"n":%d}`, i))}, fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatal(err)
		}
		r.ids = append(r.ids, e.ID)
	}
	feed, err := app.NewFeed(st)
	if err != nil {
		t.Fatal(err)
	}
	r.door, err = eventshttp.New(eventshttp.Config{Feed: feed, Caller: c})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type page struct {
	Data []struct {
		ID, Type, Realm string
		Scope           struct{ Kind, ID string }
		Data            map[string]any
		APIVersion      string `json:"api_version"`
	}
	Next *string
}

func (r *rig) get(t *testing.T, target string) (int, page, http.Header, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.door.Routes()[0].Handler(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var p page
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p, rec.Header(), rec.Body.String()
}

func TestTheFeedIsOneRouteForTheRecordPrivilege(t *testing.T) {
	t.Parallel()
	r := newRig(t, caller{realms: []string{"acme"}}, 0)
	rs := r.door.Routes()
	if len(rs) != 1 || rs[0].Method != http.MethodGet || rs[0].Path != "/v1/events" || rs[0].Privilege != eventshttp.Record || rs[0].Privilege.String() != "record" {
		t.Fatalf("%+v", rs)
	}
}

func TestAConsumerPagingByNextSeesEveryEventOfItsRealmsOnceInIDOrder(t *testing.T) {
	t.Parallel()
	r := newRig(t, caller{realms: []string{"acme", "beta"}}, 31)
	var got []string
	target := "/v1/events?limit=4"
	for range 20 {
		code, p, hdr, body := r.get(t, target)
		if code != 200 || hdr.Get("Cache-Control") != "no-store" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
			t.Fatalf("%d %v %s", code, hdr, body)
		}
		for _, e := range p.Data {
			got = append(got, e.ID)
			if e.APIVersion != events.APIVersion || e.Scope.Kind != "organization" {
				t.Fatalf("envelope %+v", e)
			}
		}
		if len(p.Data) == 0 {
			break
		}
		target = "/v1/events?limit=4&after=" + *p.Next
	}
	if fmt.Sprint(got) != fmt.Sprint(r.ids) {
		t.Fatalf("paged %d events:\n%v\nrecorded %d:\n%v", len(got), got, len(r.ids), r.ids)
	}
}

func TestADeliveryAndTheFeedCarryTheSameBytesForTheSameEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t, caller{realms: []string{"acme"}}, 1)
	_, _, _, body := r.get(t, "/v1/events")
	evs, _ := r.store.Feed(context.Background(), events.FeedQuery{Realms: []string{"acme"}, Limit: 1})
	want, _ := evs[0].Body()
	if !strings.Contains(body, string(want)) {
		t.Fatalf("the feed's envelope is not the webhook's:\n%s\n%s", body, want)
	}
}

func TestTheFeedIsScopedToTheCredentialsRealmsAndMayBeNarrowedNotWidened(t *testing.T) {
	t.Parallel()
	r := newRig(t, caller{realms: []string{"acme"}}, 15)
	_, p, _, _ := r.get(t, "/v1/events?limit=500")
	for _, e := range p.Data {
		if e.Realm != "acme" {
			t.Fatalf("a credential for acme was served %s's event %s", e.Realm, e.ID)
		}
	}
	if code, _, _, _ := r.get(t, "/v1/events?realm=beta"); code != 403 {
		t.Fatalf("a realm outside the credential: %d", code)
	}
	multi := newRig(t, caller{realms: []string{"acme", "beta"}}, 15)
	_, both, _, _ := multi.get(t, "/v1/events?limit=500")
	_, only, _, _ := multi.get(t, "/v1/events?limit=500&realm=beta")
	if len(only.Data) == 0 || len(only.Data) >= len(both.Data) {
		t.Fatalf("narrowing to one realm: %d of %d", len(only.Data), len(both.Data))
	}
	for _, e := range only.Data {
		if e.Realm != "beta" {
			t.Fatal("narrowed to beta and served another realm")
		}
	}
	every := newRig(t, caller{any: true}, 5)
	if code, _, _, _ := every.get(t, "/v1/events"); code != 400 {
		t.Fatalf("a credential for every realm must name one: %d", code)
	}
	if code, p, _, _ := every.get(t, "/v1/events?realm=acme&limit=500"); code != 200 || len(p.Data) == 0 {
		t.Fatalf("%d %d", code, len(p.Data))
	}
}

func TestTheTypesFilterReturnsOnlyThoseTypes(t *testing.T) {
	t.Parallel()
	r := newRig(t, caller{realms: []string{"acme", "beta"}}, 20)
	_, p, _, _ := r.get(t, "/v1/events?limit=500&types=payment.succeeded")
	if len(p.Data) == 0 {
		t.Fatal("none")
	}
	for _, e := range p.Data {
		if e.Type != events.PaymentSucceeded {
			t.Fatalf("type %s", e.Type)
		}
	}
	_, both, _, _ := r.get(t, "/v1/events?limit=500&types=payment.succeeded,credits.low")
	_, all, _, _ := r.get(t, "/v1/events?limit=500")
	if len(both.Data) != len(all.Data) {
		t.Fatalf("%d of %d", len(both.Data), len(all.Data))
	}
}

func TestAnEmptyPageKeepsTheCallersPlaceAndAnEmptyLogHasNoCursor(t *testing.T) {
	t.Parallel()
	r := newRig(t, caller{realms: []string{"acme"}}, 3)
	_, p, _, _ := r.get(t, "/v1/events?limit=500")
	last := *p.Next
	code, empty, _, body := r.get(t, "/v1/events?after="+last)
	if code != 200 || len(empty.Data) != 0 || empty.Next == nil || *empty.Next != last || !strings.Contains(body, `"data":[]`) {
		t.Fatalf("%d %s", code, body)
	}
	none := newRig(t, caller{realms: []string{"acme"}}, 0)
	if code, p, _, body := none.get(t, "/v1/events"); code != 200 || p.Next != nil || !strings.Contains(body, `"next":null`) || !strings.Contains(body, `"data":[]`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestARequestTheDoorCannotAnswerIsRefusedWithTheDocumentedStatus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		c      caller
		target string
		status int
		code   string
	}{
		"a cursor that is not an id":  {caller{realms: []string{"acme"}}, "/v1/events?after=nope", 400, "invalid_request"},
		"a limit that is not":         {caller{realms: []string{"acme"}}, "/v1/events?limit=many", 400, "invalid_request"},
		"a limit of zero":             {caller{realms: []string{"acme"}}, "/v1/events?limit=0", 400, "invalid_request"},
		"a negative limit":            {caller{realms: []string{"acme"}}, "/v1/events?limit=-1", 400, "invalid_request"},
		"a limit over the max":        {caller{realms: []string{"acme"}}, "/v1/events?limit=501", 400, "invalid_request"},
		"a type that is not":          {caller{realms: []string{"acme"}}, "/v1/events?types=Credits%20Low", 400, "invalid_request"},
		"no credential":               {caller{err: eventshttp.ErrUnauthenticated}, "/v1/events", 401, "unauthenticated"},
		"a credential without record": {caller{err: eventshttp.ErrForbidden}, "/v1/events", 403, "forbidden"},
		"a realm it does not cover":   {caller{realms: []string{"acme"}}, "/v1/events?realm=other", 403, "realm_mismatch"},
		"a failure nobody named":      {caller{err: errors.New("db password is hunter2")}, "/v1/events", 500, "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.c, 2)
			code, _, hdr, body := r.get(t, tc.target)
			if code != tc.status || !strings.Contains(body, `"code":"`+tc.code+`"`) || hdr.Get("Cache-Control") != "no-store" {
				t.Fatalf("%d %s", code, body)
			}
			if strings.Contains(body, "hunter2") {
				t.Fatalf("the cause reached the caller: %s", body)
			}
		})
	}
}

func TestADoorWithoutWhatItNeedsIsRefused(t *testing.T) {
	t.Parallel()
	feed, _ := app.NewFeed(contracts.NewMemStore(contracts.NewClock(epoch)))
	if _, err := eventshttp.New(eventshttp.Config{Caller: caller{}}); err == nil {
		t.Error("no feed")
	}
	if _, err := eventshttp.New(eventshttp.Config{Feed: feed}); err == nil {
		t.Error("no caller: the door could not tell whose events to serve")
	}
}
