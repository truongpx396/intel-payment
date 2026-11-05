package app_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/app"
	"github.com/truongpx396/intel-payment/events/contracts"
)

// R115: "The feed returns every event after a cursor exactly once, in id order, across pages."

func feedRig(t *testing.T, n int) (*app.Feed, []events.Event) {
	t.Helper()
	clock := contracts.NewClock(epoch)
	st := contracts.NewMemStore(clock)
	var all []events.Event
	for i := range n {
		e, _, err := st.Append(context.Background(), events.Event{Realm: "acme", Type: events.CreditsLow, ScopeKind: "k", ScopeID: "i", Data: []byte(`{}`)}, fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, e)
	}
	f, err := app.NewFeed(st)
	if err != nil {
		t.Fatal(err)
	}
	return f, all
}

func TestAConsumerThatResumesFromTheLastIdSeesEveryEventExactlyOnceInOrder(t *testing.T) {
	t.Parallel()
	f, all := feedRig(t, 23)
	var got []string
	cursor := ""
	for range 10 {
		p, err := f.Page(context.Background(), []string{"acme"}, cursor, nil, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range p.Events {
			got = append(got, e.ID)
		}
		if len(p.Events) == 0 {
			if p.Next != cursor {
				t.Fatalf("an empty page moved the cursor from %q to %q", cursor, p.Next)
			}
			break
		}
		if p.Next != p.Events[len(p.Events)-1].ID {
			t.Fatalf("next %q is not the last id of the page", p.Next)
		}
		cursor = p.Next
	}
	if len(got) != len(all) {
		t.Fatalf("%d events, want %d", len(got), len(all))
	}
	for i := range all {
		if got[i] != all[i].ID {
			t.Fatalf("event %d is %s, want %s", i, got[i], all[i].ID)
		}
	}
}

func TestTheDefaultLimitIsAHundredAndAnEmptyFirstPageHasNoCursor(t *testing.T) {
	t.Parallel()
	f, _ := feedRig(t, 130)
	p, err := f.Page(context.Background(), []string{"acme"}, "", nil, 0)
	if err != nil || len(p.Events) != app.DefaultFeedLimit {
		t.Fatalf("%d events, %v", len(p.Events), err)
	}
	empty, _ := feedRig(t, 0)
	if p, err := empty.Page(context.Background(), []string{"acme"}, "", nil, 0); err != nil || len(p.Events) != 0 || p.Next != "" {
		t.Fatalf("%+v, %v", p, err)
	}
}

func TestARequestTheFeedCannotAnswerIsRefusedWholeNotTrimmed(t *testing.T) {
	t.Parallel()
	f, all := feedRig(t, 3)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"no realm":              func() error { _, err := f.Page(ctx, nil, "", nil, 10); return err },
		"a cursor that is not":  func() error { _, err := f.Page(ctx, []string{"acme"}, "not-an-id", nil, 10); return err },
		"a negative limit":      func() error { _, err := f.Page(ctx, []string{"acme"}, "", nil, -1); return err },
		"a limit over the max":  func() error { _, err := f.Page(ctx, []string{"acme"}, "", nil, app.MaxFeedLimit+1); return err },
		"a type that is not":    func() error { _, err := f.Page(ctx, []string{"acme"}, "", []string{"Credits Low"}, 10); return err },
		"a type with a bad dot": func() error { _, err := f.Page(ctx, []string{"acme"}, "", []string{"credits."}, 10); return err },
		"an empty type":         func() error { _, err := f.Page(ctx, []string{"acme"}, "", []string{""}, 10); return err },
		"too many types": func() error {
			ts := make([]string, 21)
			for i := range ts {
				ts[i] = "a.b"
			}
			_, err := f.Page(ctx, []string{"acme"}, "", ts, 10)
			return err
		},
	} {
		if err := call(); !errors.Is(err, events.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if p, err := f.Page(ctx, []string{"acme"}, all[0].ID, []string{events.CreditsLow, "a.type.that.is.not.emitted.yet"}, app.MaxFeedLimit); err != nil || len(p.Events) != 2 {
		t.Errorf("a well-formed type this deployment does not emit is not an error: %v, %d", err, len(p.Events))
	}
	if _, err := app.NewFeed(nil); err == nil {
		t.Error("a feed with no store")
	}
}
