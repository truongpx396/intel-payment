package paymenthttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
)

func credits(n int64) *domain.Credits { c := domain.Credits(n); return &c }

func pack() domain.Plan {
	return domain.Plan{
		Realm: "acme", Code: "pack_10", Name: "10 credits", Kind: domain.OneTime, CreditAllotment: credits(1000), Pool: "general",
		Active: true, SortOrder: 1, Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}, "EUR": {MinorUnits: 900, Currency: "EUR"}},
	}
}

// walk reports every object key in a decoded JSON document.
func walk(v any, keys *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			*keys = append(*keys, k)
			walk(child, keys)
		}
	case []any:
		for _, child := range x {
			walk(child, keys)
		}
	}
}

// The catalogue is the credential's realm's, in the currency asked, and a plan crosses the wire by its slug: a provider
// price id is the deployment's, and a client that learned one could name a price the catalogue did not offer it
// (payments invariant 4).
func TestPlansAreTheCredentialsRealmsInTheCurrencyAskedAndNameNoProviderPrice(t *testing.T) {
	t.Parallel()
	var gotRealm domain.Realm
	var gotCurrency string
	d := newDoor(t, paymenthttp.Config{Catalog: catalogStub{list: func(_ context.Context, r domain.Realm, cur string) ([]domain.Plan, error) {
		gotRealm, gotCurrency = r, cur
		sub := pack()
		sub.Code, sub.Kind, sub.CreditAllotment = "pro", domain.Subscription, nil
		month := domain.Monthly
		sub.Interval = &month
		on := true
		sub.Entitlements = []domain.Entitlement{{Key: "sso", Kind: "flag", Bool: &on}}
		return []domain.Plan{pack(), sub}, nil
	}}})

	got := d.call(t, "GET", "/v1/plans?currency=USD", "")
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", got.Status, got.Body)
	}
	if gotRealm != "acme" || gotCurrency != "USD" {
		t.Fatalf("the catalogue was asked for realm %q in %q, want the credential's realm acme in USD", gotRealm, gotCurrency)
	}
	var body struct {
		Data []struct {
			Code            string           `json:"code"`
			Name            string           `json:"name"`
			Kind            string           `json:"kind"`
			CreditAllotment *int64           `json:"credit_allotment"`
			Pool            string           `json:"pool"`
			Interval        *string          `json:"interval"`
			SortOrder       int              `json:"sort_order"`
			Prices          map[string]int64 `json:"prices"`
			Entitlements    []struct {
				Key   string `json:"key"`
				Kind  string `json:"kind"`
				Value any    `json:"value"`
			} `json:"entitlements"`
		} `json:"data"`
	}
	got.JSON(t, &body)
	if len(body.Data) != 2 {
		t.Fatalf("got %d plans, want 2: %s", len(body.Data), got.Body)
	}
	p := body.Data[0]
	if p.Code != "pack_10" || p.Kind != "one_time" || p.CreditAllotment == nil || *p.CreditAllotment != 1000 || p.Pool != "general" ||
		p.Prices["USD"] != 1000 || p.Prices["EUR"] != 900 || p.Interval != nil || len(p.Entitlements) != 0 {
		t.Errorf("the pack came out as %+v", p)
	}
	s := body.Data[1]
	if s.Kind != "subscription" || s.CreditAllotment != nil || s.Interval == nil || *s.Interval != "month" ||
		len(s.Entitlements) != 1 || s.Entitlements[0].Key != "sso" || s.Entitlements[0].Kind != "flag" || s.Entitlements[0].Value != true {
		t.Errorf("the subscription came out as %+v", s)
	}

	var doc any
	got.JSON(t, &doc)
	var keys []string
	walk(doc, &keys)
	for _, k := range keys {
		if strings.Contains(k, "provider") || strings.Contains(k, "price_id") {
			t.Errorf("the catalogue names %q: a provider's price id never crosses to a client", k)
		}
	}
}

func TestPlansAnEmptyCatalogueIsAnEmptyListNotNull(t *testing.T) {
	t.Parallel()
	d := newDoor(t, paymenthttp.Config{Catalog: catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) { return nil, nil }}})
	got := d.call(t, "GET", "/v1/plans?currency=USD", "")
	if got.Status != http.StatusOK || strings.TrimSpace(string(got.Body)) != `{"data":[]}` {
		t.Fatalf("an empty catalogue answered %d %s, want 200 {\"data\":[]}", got.Status, got.Body)
	}
}

func TestPlansTheCurrencyIsRequiredBecauseNoneIsGuessed(t *testing.T) {
	t.Parallel()
	d := newDoor(t, paymenthttp.Config{Catalog: catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) {
		t.Error("the catalogue was asked without a currency")
		return nil, nil
	}}})
	for _, path := range []string{"/v1/plans", "/v1/plans?currency=", "/v1/plans?currency=usd", "/v1/plans?currency=US"} {
		if got := d.call(t, "GET", path, ""); got.Status != http.StatusBadRequest || got.ErrorCode(t) != "invalid_request" {
			t.Errorf("GET %s = %d %s, want 400 invalid_request", path, got.Status, got.Body)
		}
	}
}

// A credential that spans several realms names the one it means; one that does not cover it is refused, and the catalogue
// is not asked.
func TestPlansTheRealmComesFromTheCredentialAndNeverFromTheCaller(t *testing.T) {
	t.Parallel()
	var asked []domain.Realm
	d := newDoor(t, paymenthttp.Config{Catalog: catalogStub{list: func(_ context.Context, r domain.Realm, _ string) ([]domain.Plan, error) {
		asked = append(asked, r)
		return nil, nil
	}}})
	if got := d.call(t, "GET", "/v1/plans?currency=USD&realm=other", ""); got.Status != http.StatusForbidden || got.ErrorCode(t) != "realm_mismatch" {
		t.Errorf("a realm the credential does not cover = %d %s, want 403 realm_mismatch", got.Status, got.Body)
	}
	if got := d.call(t, "GET", "/v1/plans?currency=USD", "", "X-Realms", "acme+beta"); got.Status != http.StatusBadRequest || got.ErrorCode(t) != "invalid_request" {
		t.Errorf("two realms and none named = %d %s, want 400 invalid_request", got.Status, got.Body)
	}
	if got := d.call(t, "GET", "/v1/plans?currency=USD&realm=beta", "", "X-Realms", "acme+beta"); got.Status != http.StatusOK {
		t.Errorf("the second of two realms, named = %d %s, want 200", got.Status, got.Body)
	}
	if len(asked) != 1 || asked[0] != "beta" {
		t.Errorf("the catalogue was asked for %v, want only beta", asked)
	}
}

// What the catalogue's failure says names a topology, and goes to the operator's log whole: the client gets fixed text.
func TestPlansAFailureAnswersFixedTextAndLogsTheCause(t *testing.T) {
	t.Parallel()
	log := &logSink{}
	d := newDoor(t, paymenthttp.Config{Logger: log.logger(), Catalog: catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) {
		return nil, errBoom
	}}})
	got := d.call(t, "GET", "/v1/plans?currency=USD", "")
	if got.Status != http.StatusInternalServerError || got.ErrorCode(t) != "internal" {
		t.Fatalf("a store failure = %d %s, want 500 internal", got.Status, got.Body)
	}
	if strings.Contains(string(got.Body), "10.0.0.7") {
		t.Errorf("the response leaks the topology: %s", got.Body)
	}
	if !strings.Contains(log.String(), "10.0.0.7") {
		t.Errorf("the operator's log lacks the cause: %q", log.String())
	}
}

// A route whose port is absent is not registered: the deployment does not serve it.
func TestOnlyTheRoutesOfThePortsGivenAreRegistered(t *testing.T) {
	t.Parallel()
	d := newDoor(t, paymenthttp.Config{Catalog: catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) { return nil, nil }}})
	var got []string
	for _, r := range d.routes {
		got = append(got, r.Method+" "+r.Path)
	}
	if b, _ := json.Marshal(got); string(b) != `["GET /v1/plans"]` {
		t.Errorf("a catalogue alone registers %s, want only GET /v1/plans", b)
	}
	if got := d.call(t, "POST", "/v1/checkout", `{}`); got.Status != http.StatusMethodNotAllowed && got.Status != http.StatusNotFound {
		t.Errorf("a checkout nobody serves answered %d", got.Status)
	}
}

func TestNewRefusesWhatWouldServeMoneyToAnyone(t *testing.T) {
	t.Parallel()
	if _, err := paymenthttp.New(paymenthttp.Config{Catalog: catalogStub{}}); err == nil {
		t.Error("a door with no Caller was built: it could not tell whose realm a call is for")
	}
	if _, err := paymenthttp.New(paymenthttp.Config{Caller: caller{}}); err == nil {
		t.Error("a door that serves nothing was built: that is a wiring mistake, and one that says nothing")
	}
}

// anonymous is a Caller for a call no credential reached: a wiring slip, which must refuse and never serve.
type anonymous struct{}

func (anonymous) Resolve(*http.Request, domain.Scope) (domain.Scope, error) {
	return domain.Scope{}, paymenthttp.ErrUnauthenticated
}

func TestACallNoCredentialReachedIsRefusedBeforeAnyStoreIsAsked(t *testing.T) {
	t.Parallel()
	d := newDoor(t, paymenthttp.Config{Caller: anonymous{}, Catalog: catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) {
		t.Error("the catalogue was asked for a caller nobody authenticated")
		return nil, nil
	}}})
	got := d.call(t, "GET", "/v1/plans?currency=USD", "")
	if got.Status != http.StatusUnauthorized || got.ErrorCode(t) != "unauthenticated" || got.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("= %d %s %q, want 401 unauthenticated with WWW-Authenticate: Bearer", got.Status, got.Body, got.Header.Get("WWW-Authenticate"))
	}
}
