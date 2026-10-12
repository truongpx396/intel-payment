package resthandler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// api/openapi/v1.yaml is the machine-readable REST contract, and the TypeScript and Python clients are generated
// from it. A document that disagrees with the handler is a client that compiles against an API that is not
// there, so the two are held together three ways: the routes (both directions, with their privileges), the
// document's own validity, and real requests through the real handler validated — request and response —
// against the document.

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

func privilegeName(p auth.Privilege) string { return p.String() }

// R063b: every registered route is in the document and every path in it is registered, so a route added without
// a document entry fails — and so does an entry for a route that is gone.
func TestTheDocumentAndTheRouteTableAgreeInBothDirections(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	reg := productionTable(t)

	routes := map[string]auth.Privilege{}
	for _, r := range reg.Routes() {
		routes[r.Method+" "+r.Path] = r.Privilege
	}
	ops := map[string]*openapi3.Operation{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			ops[method+" "+path] = op
		}
	}

	var missingFromDoc, missingFromTable []string
	for key := range routes {
		if _, ok := ops[key]; !ok {
			missingFromDoc = append(missingFromDoc, key)
		}
	}
	for key := range ops {
		if _, ok := routes[key]; !ok {
			missingFromTable = append(missingFromTable, key)
		}
	}
	sort.Strings(missingFromDoc)
	sort.Strings(missingFromTable)
	if len(missingFromDoc) > 0 {
		t.Errorf("registered but not in api/openapi/v1.yaml — a client generated from the document cannot call them: %v", missingFromDoc)
	}
	if len(missingFromTable) > 0 {
		t.Errorf("in api/openapi/v1.yaml but not registered — a generated client would call a route that does not exist: %v", missingFromTable)
	}
	if len(routes) < 15 {
		t.Fatalf("the production table has only %d routes: is the check reading it?", len(routes))
	}

	ids := map[string]string{}
	for key, op := range ops {
		if op.OperationID == "" {
			t.Errorf("%s has no operationId: a generated client names its methods from it", key)
		} else if other, dup := ids[op.OperationID]; dup {
			t.Errorf("operationId %q is on both %s and %s", op.OperationID, other, key)
		}
		ids[op.OperationID] = key

		priv, registered := routes[key]
		if !registered {
			continue
		}
		got, _ := op.Extensions["x-privilege"].(string)
		switch {
		case priv != 0 && got != privilegeName(priv):
			t.Errorf("%s needs %s and the document says %q", key, priv, got)
		case priv == 0 && got != "":
			t.Errorf("%s needs no privilege and the document says %q", key, got)
		}
		if priv != 0 {
			if op.Security == nil || len(*op.Security) == 0 {
				t.Errorf("%s is behind a service credential and the document declares no security", key)
			}
			for _, code := range []int{401, 403} {
				if op.Responses.Status(code) == nil {
					t.Errorf("%s does not document its %d", key, code)
				}
			}
		}
	}
}

// A request through the real handler, and what comes back, validated against the document. Each case names the
// shape it pins; between them they cover every operation and the error shapes a client must handle.
type specCase struct {
	name   string
	token  string
	method string
	path   string
	body   string
	hdr    []string
	status int
	setup  func(*stubMeter, *stubReads, *stubCredits, *stubAdmin)
}

// notReady is the readiness stub the 503 case swaps in.
var notReady = &stubReady{report: ports.ReadyReport{Migrations: "behind", Shards: 256, Problems: []string{"2 migrations are not applied"}}}

func fullHandler(t *testing.T, name string, m *stubMeter, r *stubReads, c *stubCredits, a *stubAdmin) *api {
	t.Helper()
	var ready ports.Readiness = &stubReady{report: ports.ReadyReport{Migrations: "head", Shards: 256}}
	if strings.Contains(name, "not ready") {
		ready = notReady
	}
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, "# HELP metering_admit_denied_total Admissions refused.\n# TYPE metering_admit_denied_total counter\n")
	})
	return newAPI(t, m, apiOpts{ledger: r, transfers: r, credits: c, admin: a, ready: ready, metrics: metrics})
}

func TestEveryOperationsRequestsAndResponsesValidateAgainstTheDocument(t *testing.T) {
	t.Parallel()
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	bal := domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}
	daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, TZ: "UTC", WarnAt: 0.8, DenyCode: domain.DenyLimitReached}
	key := []string{"Idempotency-Key", "k-1"}
	limitPut := `{"actor":"alice","reason":"free tier","subject_kind":"user","unit":"credit","max":10,"window":"daily","tz":"UTC","warn_at":0.8,"deny_code":"limit_reached","sort_order":20}`

	cases := []specCase{
		{"admit allowed", "all", "POST", "/v1/admit", admitRoute.body, nil, 200, nil},
		{"admit allowed with a warning", "all", "POST", "/v1/admit", admitRoute.body, nil, 200, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.admit = func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return domain.Admission{Allowed: true, Warning: &daily, Headroom: 12}, nil
			}
		}},
		{"admit refused: balance", "all", "POST", "/v1/admit", admitRoute.body, nil, 402, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.admit = func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return domain.Admission{Exceeded: &bal}, nil
			}
		}},
		{"admit refused: a window", "all", "POST", "/v1/admit", admitRoute.body, nil, 409, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.admit = func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return domain.Admission{Exceeded: &daily}, nil
			}
		}},
		{"admit refused: blocked", "all", "POST", "/v1/admit", admitRoute.body, nil, 403, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.admit = func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return domain.Admission{Blocked: true, Exceeded: &domain.Limit{Name: "account_blocked", DenyCode: domain.DenyAccountBlocked}}, nil
			}
		}},
		{"record", "all", "POST", "/v1/record", recordRoute.body, key, 200, nil},
		{"record deferred", "all", "POST", "/v1/record", recordRoute.body, key, 202, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.record = func(_ context.Context, e domain.Event) (domain.Receipt, error) {
				return domain.Receipt{IdemKey: e.IdemKey, Applied: true, Deferred: true, RateCardVersion: "v1"}, nil
			}
		}},
		{"record unpriceable", "all", "POST", "/v1/record", recordRoute.body, key, 422, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.record = func(context.Context, domain.Event) (domain.Receipt, error) {
				return domain.Receipt{}, domain.ErrUnpriceable
			}
		}},
		{"record idempotency conflict", "all", "POST", "/v1/record", recordRoute.body, key, 409, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.record = func(context.Context, domain.Event) (domain.Receipt, error) {
				return domain.Receipt{}, domain.ErrIdemConflict
			}
		}},
		{"record without a key", "all", "POST", "/v1/record", recordRoute.body, nil, 400, nil},
		{"grant", "all", "POST", "/v1/grant", grantRoute.body, key, 200, nil},
		{"transfer", "all", "POST", "/v1/transfer", transferRoute.body, key, 200, nil},
		{"transfer overdraw", "all", "POST", "/v1/transfer", transferRoute.body, key, 409, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.transfer = func(context.Context, domain.Transfer) (domain.TransferReceipt, error) {
				return domain.TransferReceipt{}, domain.ErrInsufficient
			}
		}},
		{"balance", "record", "GET", "/v1/balance?scope_kind=workspace&scope_id=w1", "", nil, 200, nil},
		{"ledger", "record", "GET", "/v1/ledger?scope_kind=workspace&scope_id=w1&limit=2", "", nil, 200, func(_ *stubMeter, r *stubReads, _ *stubCredits, _ *stubAdmin) {
			r.rows = func(_ context.Context, q ports.LedgerQuery) (ports.LedgerPage, error) {
				return ports.LedgerPage{Next: "cur", Rows: fullSnapshot(ports.SnapshotQuery{Scope: q.Scope}).Ledger}, nil
			}
		}},
		{"credits", "record", "GET", "/v1/credits?scope_kind=workspace&scope_id=w1", "", nil, 200, func(_ *stubMeter, _ *stubReads, c *stubCredits, _ *stubAdmin) {
			c.fn = func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
				return fullSnapshot(q), nil
			}
		}},
		{"transfer status", "grant", "GET", "/v1/transfers/alloc-1", "", nil, 200, func(_ *stubMeter, r *stubReads, _ *stubCredits, _ *stubAdmin) {
			settled := mustTime("2026-09-29T12:00:01Z")
			r.xfer = func(_ context.Context, realm domain.Realm, k string) (ports.TransferRecord, bool, error) {
				return ports.TransferRecord{Realm: realm, IdemKey: k, From: domain.Scope{Realm: realm, Kind: "organization", ID: "o"}, To: domain.Scope{Realm: realm, Kind: "workspace", ID: "w"},
					FromPool: "promo", ToPool: "general", Amount: 5, Status: "settled", CreatedAt: mustTime("2026-09-29T12:00:00Z"), SettledAt: &settled}, true, nil
			}
		}},
		{"transfer status not yet booked", "grant", "GET", "/v1/transfers/alloc-1", "", nil, 404, nil},
		{"rate cards", "admin", "GET", "/v1/admin/rate-cards?realm=acme", "", nil, 200, nil},
		{"publish a card", "admin", "POST", "/v1/admin/rate-cards?realm=acme", cardBody, nil, 201, nil},
		{"publish a card twice", "admin", "POST", "/v1/admin/rate-cards?realm=acme", cardBody, nil, 409, func(_ *stubMeter, _ *stubReads, _ *stubCredits, a *stubAdmin) { a.err = domain.ErrAlreadyExists }},
		{"get a limit", "admin", "GET", "/v1/admin/limits/user_daily?realm=acme", "", nil, 200, nil},
		{"put a limit: created", "admin", "PUT", "/v1/admin/limits/user_daily?realm=acme", limitPut, nil, 201, func(_ *stubMeter, _ *stubReads, _ *stubCredits, a *stubAdmin) { a.created = true }},
		{"put a limit: updated", "admin", "PUT", "/v1/admin/limits/user_daily?realm=acme", limitPut, nil, 200, nil},
		{"delete a limit", "admin", "DELETE", "/v1/admin/limits/user_daily?realm=acme&actor=alice&reason=retired", "", nil, 204, nil},
		{"get a limit that is not there", "admin", "GET", "/v1/admin/limits/nope?realm=acme", "", nil, 404, func(_ *stubMeter, _ *stubReads, _ *stubCredits, a *stubAdmin) { a.err = domain.ErrNotFound }},
		{"get a pool", "admin", "GET", "/v1/admin/pools/gpu?realm=acme", "", nil, 200, nil},
		{"put a pool", "admin", "PUT", "/v1/admin/pools/gpu?realm=acme", `{"actor":"alice","reason":"gpu","priority":5,"applies_to":["gpu.job"],"description":"gpu"}`, nil, 201, func(_ *stubMeter, _ *stubReads, _ *stubCredits, a *stubAdmin) { a.created = true }},
		{"liveness", "", "GET", "/healthz", "", nil, 200, nil},
		{"readiness", "", "GET", "/readyz", "", nil, 200, nil},
		{"readiness: not ready", "", "GET", "/readyz", "", nil, 503, nil},
		{"metrics", "", "GET", "/metrics", "", nil, 200, nil},
		{"no credential", "", "POST", "/v1/admit", admitRoute.body, nil, 401, nil},
		{"the wrong privilege", "record", "POST", "/v1/grant", grantRoute.body, key, 403, nil},
		{"an internal error", "all", "POST", "/v1/admit", admitRoute.body, nil, 500, func(m *stubMeter, _ *stubReads, _ *stubCredits, _ *stubAdmin) {
			m.admit = func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return domain.Admission{}, io.ErrUnexpectedEOF
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, r, c, a := &stubMeter{}, &stubReads{}, &stubCredits{}, &stubAdmin{}
			if tc.setup != nil {
				tc.setup(m, r, c, a)
			}
			h := fullHandler(t, tc.name, m, r, c, a)
			rec := h.call(tc.token, tc.method, tc.path, tc.body, tc.hdr...)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}

			// The same request, as the document sees it.
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			for i := 0; i+1 < len(tc.hdr); i += 2 {
				req.Header.Set(tc.hdr[i], tc.hdr[i+1])
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			route, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("%s %s is not in the document: %v", tc.method, tc.path, err)
			}
			opts := &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}
			in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route, Options: opts}
			// A request the document calls invalid must be one the handler also refused.
			if verr := openapi3filter.ValidateRequest(context.Background(), in); verr != nil && tc.status < 400 {
				t.Fatalf("the handler accepted a request the document says is invalid: %v", verr)
			}
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in, Status: rec.Code, Header: http.Header(rec.Header()), Options: opts,
			}
			out.SetBodyBytes(rec.Body.Bytes())
			if verr := openapi3filter.ValidateResponse(context.Background(), out); verr != nil {
				t.Fatalf("the handler's %d does not match the document: %v\nbody: %s", rec.Code, verr, rec.Body.String())
			}
		})
	}

	// The cases must reach every operation, or a new one is documented and never checked.
	t.Run("the cases reach every operation of the document", func(t *testing.T) {
		t.Parallel()
		reached := map[string]bool{}
		for _, tc := range cases {
			path := tc.path
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			for p, item := range doc.Paths.Map() {
				if tmplMatches(p, path) {
					for method := range item.Operations() {
						if method == tc.method {
							reached[method+" "+p] = true
						}
					}
				}
			}
		}
		for p, item := range doc.Paths.Map() {
			for method := range item.Operations() {
				if !reached[method+" "+p] {
					t.Errorf("%s %s is documented and no case sends it", method, p)
				}
			}
		}
	})
}

// tmplMatches reports whether a concrete path fits an OpenAPI path template.
func tmplMatches(tmpl, path string) bool {
	a, b := strings.Split(tmpl, "/"), strings.Split(path, "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.HasPrefix(a[i], "{") {
			continue
		}
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
