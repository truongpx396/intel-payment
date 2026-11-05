package events_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	eventsd "github.com/truongpx396/intel-payment/cmd/internal/events"
	"github.com/truongpx396/intel-payment/events/adapters/driving/eventshttp"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R115: the realm is the credential's. The feed is read for every realm the credential covers, narrowed (never widened) by ?realm=, and a
// credential that covers every realm must say which.

func asPrincipal(p auth.Principal) context.Context {
	return auth.WithPrincipal(context.Background(), p)
}

func TestAFeedIsReadForEveryRealmTheCredentialCoversAndNoOther(t *testing.T) {
	t.Parallel()
	c := eventsd.CredentialRealms{}
	got, err := c.Realms(asPrincipal(auth.Principal{CallerID: "host", Realms: []domain.Realm{"zeta", "acme", "acme", ""}, Privileges: auth.Record}), "")
	if err != nil || !slices.Equal(got, []string{"acme", "default", "zeta"}) {
		t.Fatalf("%v, %v", got, err)
	}
	one, err := c.Realms(asPrincipal(auth.Principal{Realms: []domain.Realm{"acme", "beta"}, Privileges: auth.Record}), "beta")
	if err != nil || !slices.Equal(one, []string{"beta"}) {
		t.Fatalf("narrowed: %v, %v", one, err)
	}
}

func TestARealmOutsideTheCredentialIsAMismatchWhateverItLooksLike(t *testing.T) {
	t.Parallel()
	c := eventsd.CredentialRealms{}
	p := auth.Principal{Realms: []domain.Realm{"acme"}, Privileges: auth.Record}
	for _, realm := range []string{"beta", "ACME", "acme ", "not a realm", "../acme"} {
		if _, err := c.Realms(asPrincipal(p), realm); !errors.Is(err, eventshttp.ErrRealmMismatch) {
			t.Errorf("%q: %v", realm, err)
		}
	}
}

func TestACredentialThatCoversEveryRealmMustSayWhichAndMayThenReadAny(t *testing.T) {
	t.Parallel()
	c := eventsd.CredentialRealms{}
	p := auth.Principal{AnyRealm: true, Privileges: auth.Record}
	if _, err := c.Realms(asPrincipal(p), ""); !errors.Is(err, eventshttp.ErrRealmRequired) {
		t.Fatalf("%v", err)
	}
	if got, err := c.Realms(asPrincipal(p), "acme"); err != nil || !slices.Equal(got, []string{"acme"}) {
		t.Fatalf("%v, %v", got, err)
	}
}

func TestACallNoCredentialReachedOrWithoutRecordIsRefused(t *testing.T) {
	t.Parallel()
	c := eventsd.CredentialRealms{}
	if _, err := c.Realms(context.Background(), ""); !errors.Is(err, eventshttp.ErrUnauthenticated) {
		t.Fatalf("no credential: %v", err)
	}
	if _, err := c.Realms(asPrincipal(auth.Principal{Realms: []domain.Realm{"acme"}}), ""); !errors.Is(err, eventshttp.ErrForbidden) {
		t.Fatalf("no privilege: %v", err)
	}
	if _, err := c.Realms(asPrincipal(auth.Principal{Realms: []domain.Realm{"acme"}, Privileges: auth.Grant}), ""); !errors.Is(err, eventshttp.ErrForbidden) {
		t.Fatalf("grant without record: %v", err)
	}
	// A credential with no realm can do nothing, never everything.
	if got, err := c.Realms(asPrincipal(auth.Principal{Privileges: auth.Record}), ""); err != nil || len(got) != 0 {
		t.Fatalf("a credential with no realm: %v, %v", got, err)
	}
}

func TestTheFeedRoutesAreMountedForRecordOnly(t *testing.T) {
	t.Parallel()
	routes, err := eventsd.FeedRoutes(nil, nil, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Method != "GET" || routes[0].Path != "/v1/events" || routes[0].Privilege != auth.Record {
		t.Fatalf("%+v", routes)
	}
}
