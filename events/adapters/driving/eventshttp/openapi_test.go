package eventshttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/truongpx396/intel-payment/events/adapters/driving/eventshttp"
)

// api/openapi/v1.yaml is the machine-readable REST contract. The route this door adds is marked there `x-served-by: events`; the REST
// handler's own test leaves exactly that to this one, which holds it to the document in both directions and validates real exchanges.

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
	r := newRig(t, caller{realms: []string{"acme"}}, 0)
	routes := map[string]string{}
	for _, rt := range r.door.Routes() {
		routes[rt.Method+" "+rt.Path] = rt.Privilege.String()
	}
	ops := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if by, _ := op.Extensions["x-served-by"].(string); by == "events" {
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
	if len(notInDoc) > 0 || len(notServed) > 0 {
		t.Fatalf("served and not marked `x-served-by: events`: %v; marked and not served: %v", notInDoc, notServed)
	}
	for key, priv := range routes {
		op := ops[key]
		if got, _ := op.Extensions["x-privilege"].(string); got != priv {
			t.Errorf("%s needs %s and the document says %q", key, priv, got)
		}
		if op.Security == nil || len(*op.Security) == 0 || op.OperationID == "" {
			t.Errorf("%s: security or operationId missing", key)
		}
		for _, code := range []int{400, 401, 403, 500} {
			if op.Responses.Status(code) == nil {
				t.Errorf("%s does not document its %d", key, code)
			}
		}
	}
}

func TestEveryExchangeValidatesAgainstTheDocument(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, target string
		status       int
		c            caller
		n            int
	}{
		{"a page", "/v1/events?limit=3", 200, caller{realms: []string{"acme"}}, 8},
		{"a page after a cursor, of some types", "/v1/events?types=credits.low,payment.succeeded&limit=100", 200, caller{realms: []string{"acme", "beta"}}, 8},
		{"nothing yet", "/v1/events", 200, caller{realms: []string{"acme"}}, 0},
		{"a bad cursor", "/v1/events?after=nope", 400, caller{realms: []string{"acme"}}, 1},
		{"no credential", "/v1/events", 401, caller{err: eventshttp.ErrUnauthenticated}, 1},
		{"a realm it does not cover", "/v1/events?realm=other", 403, caller{realms: []string{"acme"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, tc.c, tc.n)
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			rec := httptest.NewRecorder()
			r.door.Routes()[0].Handler(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			route, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatal(err)
			}
			in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route, Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
			if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil && tc.status < 400 {
				t.Fatalf("the request does not validate: %v", err)
			}
			out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: rec.Code, Header: rec.Header(), Options: &openapi3filter.Options{IncludeResponseStatus: true}}
			out.SetBodyBytes(rec.Body.Bytes())
			if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
				t.Fatalf("the response does not validate against the document: %v\n%s", err, rec.Body.String())
			}
		})
	}
}
