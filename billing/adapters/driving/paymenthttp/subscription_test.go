package paymenthttp_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

var (
	periodStart = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	graceEnd    = time.Date(2026, 11, 8, 12, 30, 0, 0, time.UTC)
)

func subDoor(t *testing.T, get func(domain.Scope) (domain.Sub, bool, error), changes changesStub) *door {
	t.Helper()
	return newDoor(t, paymenthttp.Config{
		Subs:    subsStub{get: func(_ context.Context, s domain.Scope) (domain.Sub, bool, error) { return get(s) }},
		Changes: changes,
	})
}

func TestSubscriptionNoneIsNullNotAnError(t *testing.T) {
	t.Parallel()
	d := subDoor(t, func(domain.Scope) (domain.Sub, bool, error) { return domain.Sub{}, false, nil }, changesStub{})
	got := d.call(t, "GET", "/v1/subscription?scope_kind=user&scope_id=u1", "")
	if got.Status != http.StatusOK || strings.TrimSpace(string(got.Body)) != "null" {
		t.Errorf("no subscription answered %d %s, want 200 null", got.Status, got.Body)
	}
}

func TestSubscriptionIsReadForTheScopeOfTheCredentialsRealm(t *testing.T) {
	t.Parallel()
	var asked domain.Scope
	d := subDoor(t, func(s domain.Scope) (domain.Sub, bool, error) {
		asked = s
		return domain.Sub{
			Scope: s, PlanCode: "pro", Account: "stripe-test", ProviderSubscriptionID: "sub_123", Status: domain.PastDue,
			CurrentPeriodStart: periodStart, CurrentPeriodEnd: periodEnd, CancelAtPeriodEnd: true, GraceUntil: &graceEnd,
		}, true, nil
	}, changesStub{})
	got := d.call(t, "GET", "/v1/subscription?scope_kind=user&scope_id=u1", "")
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", got.Status, got.Body)
	}
	if asked != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) {
		t.Errorf("read the subscription of %+v", asked)
	}
	var out struct {
		PlanCode          string  `json:"plan_code"`
		Status            string  `json:"status"`
		CurrentPeriodEnd  string  `json:"current_period_end"`
		CancelAtPeriodEnd bool    `json:"cancel_at_period_end"`
		GraceUntil        *string `json:"grace_until"`
	}
	got.JSON(t, &out)
	if out.PlanCode != "pro" || out.Status != "past_due" || out.CurrentPeriodEnd != "2026-11-01T00:00:00Z" || !out.CancelAtPeriodEnd ||
		out.GraceUntil == nil || *out.GraceUntil != "2026-11-08T12:30:00Z" {
		t.Errorf("the subscription came out as %+v", out)
	}
	for _, leak := range []string{"sub_123", "stripe-test", "provider"} {
		if strings.Contains(string(got.Body), leak) {
			t.Errorf("the subscription names %q: a provider's identifiers stay with the deployment", leak)
		}
	}
}

func TestSubscriptionNoGraceIsNull(t *testing.T) {
	t.Parallel()
	d := subDoor(t, func(s domain.Scope) (domain.Sub, bool, error) {
		return domain.Sub{Scope: s, PlanCode: "pro", Status: domain.Active, CurrentPeriodEnd: periodEnd}, true, nil
	}, changesStub{})
	got := d.call(t, "GET", "/v1/subscription?scope_kind=user&scope_id=u1", "")
	if !strings.Contains(string(got.Body), `"grace_until":null`) {
		t.Errorf("no grace must be an explicit null, not absent: %s", got.Body)
	}
}

func TestScopeQueryIsRequiredAndCheckedOnEveryRead(t *testing.T) {
	t.Parallel()
	d := subDoor(t, func(domain.Scope) (domain.Sub, bool, error) {
		t.Error("the store was read for a scope that was not named")
		return domain.Sub{}, false, nil
	}, changesStub{})
	for _, path := range []string{"/v1/subscription", "/v1/subscription?scope_kind=user", "/v1/subscription?scope_id=u1"} {
		if got := d.call(t, "GET", path, ""); got.Status != http.StatusBadRequest || got.ErrorCode(t) != "invalid_request" {
			t.Errorf("GET %s = %d %s, want 400 invalid_request", path, got.Status, got.Body)
		}
	}
	if got := d.call(t, "GET", "/v1/subscription?scope_kind=user&scope_id=u1&scope_realm=other", ""); got.Status != http.StatusForbidden || got.ErrorCode(t) != "realm_mismatch" {
		t.Errorf("a realm outside the credential = %d %s, want 403 realm_mismatch", got.Status, got.Body)
	}
}

// A change asks the provider and says so: the subscription's status is the processor's alone (invariant 12), so the answer is
// "accepted", never "changed".
func TestSubscriptionChangeIsAcceptedNotDoneAndCarriesTheScopeAndPlan(t *testing.T) {
	t.Parallel()
	var scope domain.Scope
	var plan string
	d := subDoor(t, nil, changesStub{change: func(_ context.Context, s domain.Scope, p string) error { scope, plan = s, p; return nil }})
	got := d.call(t, "POST", "/v1/subscription/change", `{"scope":{"kind":"user","id":"u1"},"plan_code":"team"}`)
	if got.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", got.Status, got.Body)
	}
	if scope != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) || plan != "team" {
		t.Errorf("changed %+v to %q", scope, plan)
	}
	for _, field := range []string{"status", "plan_code"} {
		if strings.Contains(string(got.Body), `"`+field+`":"active"`) {
			t.Errorf("the answer claims the subscription is now %s: the processor says that, on the webhook", field)
		}
	}
}

func TestSubscriptionChangeRefusalsAreStatusesAClientCanActOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"below usage", fmt.Errorf("%w: seats 12 > 5", domain.ErrBelowUsage), 409, "below_usage"},
		{"a plan nobody sells", domain.ErrUnknownPrice, 422, "unknown_price"},
		{"no live subscription", md.ErrNotFound, 404, "not_found"},
		{"a provider that cannot", domain.ErrUnsupported, 422, "unsupported"},
		{"anything else", errBoom, 500, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := subDoor(t, nil, changesStub{change: func(context.Context, domain.Scope, string) error { return tc.err }})
			got := d.call(t, "POST", "/v1/subscription/change", `{"scope":{"kind":"user","id":"u1"},"plan_code":"team"}`)
			if got.Status != tc.status || got.ErrorCode(t) != tc.code {
				t.Errorf("%d %s, want %d %s", got.Status, got.Body, tc.status, tc.code)
			}
		})
	}
}

func TestSubscriptionChangeNeedsAPlanAndAScopeAndNothingElse(t *testing.T) {
	t.Parallel()
	d := subDoor(t, nil, changesStub{change: func(context.Context, domain.Scope, string) error {
		t.Error("a change was asked of the provider from a malformed request")
		return nil
	}})
	for name, body := range map[string]string{
		"no plan":          `{"scope":{"kind":"user","id":"u1"}}`,
		"a blank plan":     `{"scope":{"kind":"user","id":"u1"},"plan_code":" "}`,
		"no scope":         `{"plan_code":"team"}`,
		"a price":          `{"scope":{"kind":"user","id":"u1"},"plan_code":"team","price":1}`,
		"a status":         `{"scope":{"kind":"user","id":"u1"},"plan_code":"team","status":"active"}`,
		"another realm":    `{"scope":{"kind":"user","id":"u1","realm":"other"},"plan_code":"team"}`,
		"not a JSON value": `team`,
	} {
		if got := d.call(t, "POST", "/v1/subscription/change", body); got.Status < 400 || got.Status >= 500 {
			t.Errorf("%s: %d %s, want a 4xx", name, got.Status, got.Body)
		}
	}
}

func TestSubscriptionCancelAsksForTheEndOfThePeriodAndReportsAcceptance(t *testing.T) {
	t.Parallel()
	var scope domain.Scope
	d := subDoor(t, nil, changesStub{cancel: func(_ context.Context, s domain.Scope) error { scope = s; return nil }})
	got := d.call(t, "POST", "/v1/subscription/cancel", `{"scope":{"kind":"user","id":"u1"}}`)
	if got.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", got.Status, got.Body)
	}
	if scope != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) {
		t.Errorf("cancelled %+v", scope)
	}
	if got := d.call(t, "POST", "/v1/subscription/cancel", `{"scope":{"kind":"user","id":"u1"},"immediately":true}`); got.Status != http.StatusBadRequest {
		t.Errorf("a request to cancel immediately = %d %s, want 400: the contract ends at the period's end and has no other mode", got.Status, got.Body)
	}
}

// ---------------------------------------------------------- the portal and fulfilment --

func TestPortalIsTheProvidersPageForTheScopeAndItsReturnURL(t *testing.T) {
	t.Parallel()
	var scope domain.Scope
	var account, ret string
	d := newDoor(t, paymenthttp.Config{Checkout: checkoutStub{portal: func(_ context.Context, s domain.Scope, a, r string) (string, error) {
		scope, account, ret = s, a, r
		return "https://billing.example/portal/xyz", nil
	}}})
	got := d.call(t, "GET", "/v1/portal?scope_kind=user&scope_id=u1&account=stripe-test&return_url=https%3A%2F%2Fapp.example%2Fback", "")
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", got.Status, got.Body)
	}
	var out struct {
		PortalURL string `json:"portal_url"`
	}
	got.JSON(t, &out)
	if out.PortalURL != "https://billing.example/portal/xyz" {
		t.Errorf("portal_url = %q", out.PortalURL)
	}
	if scope != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) || account != "stripe-test" || ret != "https://app.example/back" {
		t.Errorf("asked for %+v %q %q", scope, account, ret)
	}
	if got := d.call(t, "GET", "/v1/portal?scope_kind=user&scope_id=u1", ""); got.Status != http.StatusBadRequest {
		t.Errorf("a portal with no return_url = %d %s, want 400: a buyer is sent somewhere, and the caller says where", got.Status, got.Body)
	}
}

func TestPortalOfAScopeThatNeverBoughtIsNotFound(t *testing.T) {
	t.Parallel()
	d := newDoor(t, paymenthttp.Config{Checkout: checkoutStub{portal: func(context.Context, domain.Scope, string, string) (string, error) {
		return "", fmt.Errorf("%w: the scope has no customer", md.ErrNotFound)
	}}})
	if got := d.call(t, "GET", "/v1/portal?scope_kind=user&scope_id=u1&return_url=https%3A%2F%2Fa.example%2F", ""); got.Status != http.StatusNotFound || got.ErrorCode(t) != "not_found" {
		t.Errorf("= %d %s, want 404 not_found", got.Status, got.Body)
	}
}

// Fulfilment is the only sanctioned way a UI learns that credits arrived, and every kind is spelled as the contract spells it.
func TestFulfilmentReportsTheServersKindAndNothingTheClientInferred(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		f    ports.Fulfilment
		want string
	}{
		{"none", ports.Fulfilment{Kind: ports.FulfilmentNone}, `{"kind":"none"}`},
		{"processing", ports.Fulfilment{Kind: ports.FulfilmentProcessing, Since: since}, `{"kind":"processing","since":"2026-10-17T09:00:00Z"}`},
		{"granted", ports.Fulfilment{Kind: ports.FulfilmentGranted, Amount: 1000, ReceiptURL: "https://r.example/1"}, `{"kind":"granted","amount":1000,"receipt_url":"https://r.example/1"}`},
		{"failed", ports.Fulfilment{Kind: ports.FulfilmentFailed, Reason: "card_declined"}, `{"kind":"failed","reason":"card_declined"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var scope domain.Scope
			d := newDoor(t, paymenthttp.Config{Checkout: checkoutStub{fulfilment: func(_ context.Context, s domain.Scope) (ports.Fulfilment, error) {
				scope = s
				return tc.f, nil
			}}})
			got := d.call(t, "GET", "/v1/fulfilment?scope_kind=user&scope_id=u1", "")
			if got.Status != http.StatusOK || strings.TrimSpace(string(got.Body)) != tc.want {
				t.Errorf("%d %s, want 200 %s", got.Status, got.Body, tc.want)
			}
			if scope != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) {
				t.Errorf("read the fulfilment of %+v", scope)
			}
		})
	}
}
