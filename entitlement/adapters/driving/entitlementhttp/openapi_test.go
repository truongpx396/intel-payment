package entitlementhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/truongpx396/intel-payment/entitlement/domain"
)

// api/openapi/v1.yaml is the machine-readable REST contract, and the generated clients are built from it. The routes this door adds
// are marked there `x-served-by: entitlement`; the REST handler's own test leaves exactly those to this one, which holds them to the
// document in both directions and validates real requests and responses against it.

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

func TestTheDocumentAndTheDoorAgreeInBothDirections(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	rg := newRig(t, inT1)

	routes := map[string]string{}
	for _, r := range rg.door.Routes() {
		routes[r.Method+" "+r.Path] = r.Privilege.String()
	}
	ops := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if by, _ := op.Extensions["x-served-by"].(string); by == "entitlement" {
				ops[method+" "+path] = op
			}
		}
	}
	var notInDoc, notServed []string
	for k := range routes {
		if _, ok := ops[k]; !ok {
			notInDoc = append(notInDoc, k)
		}
	}
	for k := range ops {
		if _, ok := routes[k]; !ok {
			notServed = append(notServed, k)
		}
	}
	sort.Strings(notInDoc)
	sort.Strings(notServed)
	if len(notInDoc) > 0 {
		t.Errorf("served by the door and not marked `x-served-by: entitlement` in api/openapi/v1.yaml — a generated client cannot call them: %v", notInDoc)
	}
	if len(notServed) > 0 {
		t.Errorf("marked `x-served-by: entitlement` in api/openapi/v1.yaml and not served — a generated client would call a route that does not exist: %v", notServed)
	}
	if len(routes) != 2 {
		t.Fatalf("the full door serves %d routes, want the 2 of rest-api.md's Entitlements table", len(routes))
	}
	for key, priv := range routes {
		op, found := ops[key]
		if !found {
			continue
		}
		if got, _ := op.Extensions["x-privilege"].(string); got != priv {
			t.Errorf("%s needs %s and the document says %q", key, priv, got)
		}
		if op.Security == nil || len(*op.Security) == 0 {
			t.Errorf("%s is behind a service credential and the document declares no security", key)
		}
		for _, code := range []int{400, 401, 403, 503} {
			if op.Responses.Status(code) == nil {
				t.Errorf("%s does not document its %d", key, code)
			}
		}
		if op.OperationID == "" {
			t.Errorf("%s has no operationId: a generated client names its methods from it", key)
		}
	}
}

func TestEveryOperationsRequestsAndResponsesValidateAgainstTheDocument(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	many := strings.TrimSuffix(strings.Repeat(`{"key":"sso"},`, domain.MaxChecks+1), ",")
	type spec struct {
		name, method, target, body string
		status                     int
		rig                        func(t *testing.T) *rig
	}
	down := func(t *testing.T) *rig { r := newRig(t, inT1); r.faults.BreakWith(errBoom); return r }
	as := func(c credential) func(t *testing.T) *rig { return func(t *testing.T) *rig { return newRig(t, c) } }
	const q = "/v1/entitlements?scope_kind=org&scope_id=o1"
	cases := []spec{
		{"grants", "GET", q, "", 200, nil},
		{"no grants", "GET", "/v1/entitlements?scope_kind=org&scope_id=nobody", "", 200, nil},
		{"grants without a scope", "GET", "/v1/entitlements", "", 400, nil},
		{"grants of a realm outside the credential", "GET", q + "&scope_realm=other", "", 403, nil},
		{"grants with no credential", "GET", q, "", 401, as(credential{none: true})},
		{"grants while the store is down", "GET", q, "", 503, down},
		{"a mixed batch", "POST", "/v1/entitlements/check", checkBody(`{"key":"support_tier"},{"key":"nope"},{"key":"sso"},{"key":"seats","current":25},{"key":"seats"},{"key":"export"}`), 200, nil},
		{"a batch of one unknown key", "POST", "/v1/entitlements/check", checkBody(`{"key":"nope"}`), 200, nil},
		{"unlimited", "POST", "/v1/entitlements/check", checkBody(`{"key":"seats","current":1000000}`), 200, func(t *testing.T) *rig {
			r := newRig(t, inT1)
			g := domain.Grant{Scope: scopeDemo, Key: "seats", Value: domain.UnlimitedQuota(), Source: domain.FromOverride, Reason: "negotiated"}
			if err := r.mem.PutOverride(t.Context(), g, "alice"); err != nil {
				t.Fatal(err)
			}
			return r
		}},
		{"a batch of nothing", "POST", "/v1/entitlements/check", checkBody(``), 400, nil},
		{"a batch of too many", "POST", "/v1/entitlements/check", checkBody(many), 400, nil},
		{"a body that is not JSON", "POST", "/v1/entitlements/check", `nope`, 400, nil},
		{"a realm outside the credential", "POST", "/v1/entitlements/check", `{"scope":{"kind":"org","id":"o1","realm":"other"},"checks":[{"key":"sso"}]}`, 403, nil},
		{"no credential", "POST", "/v1/entitlements/check", checkBody(`{"key":"sso"}`), 401, as(credential{none: true})},
		{"the store is down", "POST", "/v1/entitlements/check", checkBody(`{"key":"sso"}`), 503, down},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rg *rig
			if tc.rig != nil {
				rg = tc.rig(t)
			} else {
				rg = newRig(t, inT1)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rg.h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			again := httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, strings.NewReader(tc.body))
			if tc.body != "" {
				again.Header.Set("Content-Type", "application/json")
			}
			route, params, err := router.FindRoute(again)
			if err != nil {
				t.Fatalf("%s %s is not in the document: %v", tc.method, tc.target, err)
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
}
