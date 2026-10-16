package paymenthttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// api/openapi/v1.yaml is the machine-readable REST contract and the generated clients are built from it. The routes this
// door adds are marked there `x-served-by: payments`; the REST handler's own test leaves exactly those to this one, which
// holds them to the document in both directions and validates real requests and responses against it.

const specPath = "../../../../api/openapi/v1.yaml"

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(specPath)
	if err != nil {
		t.Fatalf("api/openapi/v1.yaml cannot be loaded: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("api/openapi/v1.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

// fullDoor is a door with every port, answering from the fixtures of the cases.
func fullDoor(t *testing.T) *door {
	t.Helper()
	plan := pack()
	month := domain.Monthly
	sub := pack()
	sub.Code, sub.Kind, sub.CreditAllotment, sub.Interval = "pro", domain.Subscription, nil, &month
	on := true
	sub.Entitlements = []domain.Entitlement{{Key: "sso", Kind: "flag", Bool: &on}}
	tax := domain.Money{MinorUnits: 80, Currency: "USD"}
	return newDoor(t, paymenthttp.Config{
		Catalog: catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) {
			return []domain.Plan{plan, sub}, nil
		}},
		Checkout: checkoutStub{
			start: func(context.Context, ports.CheckoutRequest) (string, error) { return "https://pay.example/s/1", nil },
			portal: func(context.Context, domain.Scope, string, string) (string, error) {
				return "https://pay.example/p/1", nil
			},
			fulfilment: func(context.Context, domain.Scope) (ports.Fulfilment, error) {
				return ports.Fulfilment{Kind: ports.FulfilmentGranted, Amount: 1000, ReceiptURL: "https://r.example/1"}, nil
			},
		},
		Subs: subsStub{get: func(_ context.Context, s domain.Scope) (domain.Sub, bool, error) {
			return domain.Sub{Scope: s, PlanCode: "pro", Status: domain.PastDue, CurrentPeriodEnd: periodEnd, GraceUntil: &graceEnd}, true, nil
		}},
		Changes: changesStub{
			change: func(context.Context, domain.Scope, string) error { return nil },
			cancel: func(context.Context, domain.Scope) error { return nil },
		},
		Recharges: rechargesStub{
			get: func(_ context.Context, s domain.Scope) (domain.AutoRecharge, bool, error) {
				return rechargeRow(s), true, nil
			},
			put: func(context.Context, domain.AutoRecharge) error { return nil },
		},
		Payments: paymentsStub{list: func(_ context.Context, s domain.Scope, _ string, _ int) ([]domain.Payment, string, error) {
			return []domain.Payment{{
				Scope: s, ProviderPaymentID: "pi_1", PlanCode: "pack_10", Kind: "one_time", Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, TaxAmount: &tax,
				CreditsGranted: 1000, Pool: "general", Status: domain.Succeeded, ReceiptURL: "https://r.example/1", CreatedAt: time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC),
			}}, "next", nil
		}},
	})
}

func TestTheDocumentAndTheDoorAgreeInBothDirections(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	d := fullDoor(t)

	routes := map[string]paymenthttp.Privilege{}
	for _, r := range d.routes {
		routes[r.Method+" "+r.Path] = r.Privilege
	}
	ops := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if by, _ := op.Extensions["x-served-by"].(string); by == "payments" {
				ops[method+" "+path] = op
			}
		}
	}
	var missingFromDoc, missingFromDoor []string
	for key := range routes {
		if _, ok := ops[key]; !ok {
			missingFromDoc = append(missingFromDoc, key)
		}
	}
	for key := range ops {
		if _, ok := routes[key]; !ok {
			missingFromDoor = append(missingFromDoor, key)
		}
	}
	sort.Strings(missingFromDoc)
	sort.Strings(missingFromDoor)
	if len(missingFromDoc) > 0 {
		t.Errorf("served by the door and not marked `x-served-by: payments` in api/openapi/v1.yaml — a generated client cannot call them: %v", missingFromDoc)
	}
	if len(missingFromDoor) > 0 {
		t.Errorf("marked `x-served-by: payments` in api/openapi/v1.yaml and not served — a generated client would call a route that does not exist: %v", missingFromDoor)
	}
	if len(routes) != 10 {
		t.Fatalf("the full door serves %d routes, want the 10 of rest-api.md's catalogue, checkout and subscription tables", len(routes))
	}
	for key, priv := range routes {
		op, found := ops[key]
		if !found {
			continue
		}
		if got, _ := op.Extensions["x-privilege"].(string); got != priv.String() {
			t.Errorf("%s needs %s and the document says %q", key, priv, got)
		}
		if op.Security == nil || len(*op.Security) == 0 {
			t.Errorf("%s is behind a service credential and the document declares no security", key)
		}
		for _, code := range []int{401, 403} {
			if op.Responses.Status(code) == nil {
				t.Errorf("%s does not document its %d", key, code)
			}
		}
		if op.OperationID == "" {
			t.Errorf("%s has no operationId: a generated client names its methods from it", key)
		}
	}
}

type specCase struct {
	name   string
	method string
	path   string
	body   string
	hdr    []string
	status int
	door   func(t *testing.T) *door // nil: the full door
}

func TestEveryOperationsRequestsAndResponsesValidateAgainstTheDocument(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	key := []string{"Idempotency-Key", "k-1"}
	failing := func(err error) func(t *testing.T) *door {
		return func(t *testing.T) *door {
			t.Helper()
			return newDoor(t, paymenthttp.Config{
				Catalog:  catalogStub{list: func(context.Context, domain.Realm, string) ([]domain.Plan, error) { return nil, err }},
				Checkout: checkoutStub{start: func(context.Context, ports.CheckoutRequest) (string, error) { return "", err }},
				Subs:     subsStub{get: func(context.Context, domain.Scope) (domain.Sub, bool, error) { return domain.Sub{}, false, err }},
				Changes: changesStub{
					change: func(context.Context, domain.Scope, string) error { return err },
					cancel: func(context.Context, domain.Scope) error { return err },
				},
			})
		}
	}
	noSub := func(t *testing.T) *door {
		t.Helper()
		return newDoor(t, paymenthttp.Config{Subs: subsStub{get: func(context.Context, domain.Scope) (domain.Sub, bool, error) { return domain.Sub{}, false, nil }}})
	}
	noRecharge := func(t *testing.T) *door {
		t.Helper()
		return newDoor(t, paymenthttp.Config{Recharges: rechargesStub{get: func(context.Context, domain.Scope) (domain.AutoRecharge, bool, error) {
			return domain.AutoRecharge{}, false, nil
		}}})
	}
	const scopeQ = "scope_kind=user&scope_id=u1"
	const scopeB = `"scope":{"kind":"user","id":"u1"}`

	cases := []specCase{
		{"plans", "GET", "/v1/plans?currency=USD", "", nil, 200, nil},
		{"plans without a currency", "GET", "/v1/plans", "", nil, 400, nil},
		{"plans in a realm the credential lacks", "GET", "/v1/plans?currency=USD&realm=other", "", nil, 403, nil},
		{"plans when the store fails", "GET", "/v1/plans?currency=USD", "", nil, 500, failing(errBoom)},
		{"checkout", "POST", "/v1/checkout", checkoutBody, key, 200, nil},
		{"checkout without a key", "POST", "/v1/checkout", checkoutBody, nil, 400, nil},
		{"checkout of an unknown price", "POST", "/v1/checkout", checkoutBody, key, 422, failing(domain.ErrUnknownPrice)},
		{"checkout through an unknown account", "POST", "/v1/checkout", checkoutBody, key, 404, failing(domain.ErrUnknownAccount)},
		{"checkout of a provider that cannot", "POST", "/v1/checkout", checkoutBody, key, 422, failing(domain.ErrUnsupported)},
		{"checkout key reused", "POST", "/v1/checkout", checkoutBody, key, 409, failing(md.ErrIdemConflict)},
		{"subscription", "GET", "/v1/subscription?" + scopeQ, "", nil, 200, nil},
		{"no subscription", "GET", "/v1/subscription?" + scopeQ, "", nil, 200, noSub},
		{"subscription change", "POST", "/v1/subscription/change", `{` + scopeB + `,"plan_code":"team"}`, nil, 202, nil},
		{"subscription change below usage", "POST", "/v1/subscription/change", `{` + scopeB + `,"plan_code":"team"}`, nil, 409, failing(domain.ErrBelowUsage)},
		{"subscription change of nothing", "POST", "/v1/subscription/change", `{` + scopeB + `,"plan_code":"team"}`, nil, 404, failing(md.ErrNotFound)},
		{"subscription cancel", "POST", "/v1/subscription/cancel", `{` + scopeB + `}`, nil, 202, nil},
		{"portal", "GET", "/v1/portal?" + scopeQ + "&return_url=https%3A%2F%2Fapp.example%2F", "", nil, 200, nil},
		{"portal without a return", "GET", "/v1/portal?" + scopeQ, "", nil, 400, nil},
		{"auto-recharge", "GET", "/v1/auto-recharge?" + scopeQ, "", nil, 200, nil},
		{"no auto-recharge", "GET", "/v1/auto-recharge?" + scopeQ, "", nil, 200, noRecharge},
		{"auto-recharge put", "PUT", "/v1/auto-recharge", putBody, nil, 200, nil},
		{"auto-recharge put refused", "PUT", "/v1/auto-recharge", strings.Replace(putBody, `"threshold":500`, `"threshold":0`, 1), nil, 400, nil},
		{"payments", "GET", "/v1/payments?" + scopeQ + "&limit=10", "", nil, 200, nil},
		{"payments bad limit", "GET", "/v1/payments?" + scopeQ + "&limit=0", "", nil, 400, nil},
		{"fulfilment", "GET", "/v1/fulfilment?" + scopeQ, "", nil, 200, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := fullDoor(t)
			if tc.door != nil {
				d = tc.door(t)
			}
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("X-Realms", "acme")
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for i := 0; i+1 < len(tc.hdr); i += 2 {
				req.Header.Set(tc.hdr[i], tc.hdr[i+1])
			}
			rec := serve(d, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			// The same request, as the document sees it.
			again := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				again.Header.Set("Content-Type", "application/json")
			}
			for i := 0; i+1 < len(tc.hdr); i += 2 {
				again.Header.Set(tc.hdr[i], tc.hdr[i+1])
			}
			route, params, err := router.FindRoute(again)
			if err != nil {
				t.Fatalf("%s %s is not in the document: %v", tc.method, tc.path, err)
			}
			opts := &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}
			in := &openapi3filter.RequestValidationInput{Request: again, PathParams: params, Route: route, Options: opts}
			if verr := openapi3filter.ValidateRequest(context.Background(), in); verr != nil && tc.status < 400 {
				t.Fatalf("the door accepted a request the document says is invalid: %v", verr)
			}
			out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: rec.Code, Header: http.Header(rec.Header()), Options: opts}
			out.SetBodyBytes(rec.Body.Bytes())
			if verr := openapi3filter.ValidateResponse(context.Background(), out); verr != nil {
				t.Fatalf("the door's %d does not match the document: %v\nbody: %s", rec.Code, verr, rec.Body.String())
			}
		})
	}

	t.Run("the cases reach every operation the door serves", func(t *testing.T) {
		t.Parallel()
		reached := map[string]bool{}
		for _, tc := range cases {
			path := tc.path
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			reached[tc.method+" "+path] = true
		}
		for _, r := range fullDoor(t).routes {
			if !reached[r.Method+" "+r.Path] {
				t.Errorf("%s %s is served and no case sends it", r.Method, r.Path)
			}
		}
	})
}
