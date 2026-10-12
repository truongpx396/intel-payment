package resthandler_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/contracts"
)

// The authorization matrix (R065): privilege × route → allowed, or 403 forbidden; no credential → 401. It is
// generated from the PRODUCTION route table, and compared with a table written out by hand, in both
// directions: a route added with no row here fails, and so does a row for a route that is gone. A new route
// cannot ship with an authorization nobody decided.
var wantPrivilege = map[string]auth.Privilege{
	"POST /v1/admit":    auth.Record,
	"POST /v1/record":   auth.Record,
	"POST /v1/grant":    auth.Grant,
	"POST /v1/transfer": auth.Grant,
	"GET /v1/balance":   auth.Record,

	"GET /v1/ledger":               auth.Record,
	"GET /v1/transfers/{idem_key}": auth.Grant,
}

func productionTable(t *testing.T) *resthandler.Registry {
	t.Helper()
	stub := &stubMeter{}
	reg, err := resthandler.BuildRegistry(resthandler.Config{
		Meter: stub, Balance: stub, Clock: contracts.NewFakeClock(epoch), Ledger: &stubReads{}, Transfers: &stubReads{},
		Auth: auth.AuthenticatorFunc(func(context.Context, auth.Credential) (auth.Principal, error) {
			return auth.Principal{}, auth.ErrUnauthenticated
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestTheProductionRouteTableIsValidAndEveryRouteHasAnAuthorizationRow(t *testing.T) {
	t.Parallel()
	reg := productionTable(t)
	if err := reg.Validate(); err != nil {
		t.Fatalf("the production table breaks the rules of the three surfaces: %v", err)
	}
	have := map[string]bool{}
	for _, r := range reg.Routes() {
		if r.Surface != resthandler.V1 {
			continue
		}
		key := r.Method + " " + r.Path
		have[key] = true
		want, ok := wantPrivilege[key]
		if !ok {
			t.Errorf("%s is registered with privilege %s and has no row in the authorization matrix: decide who may call it, and add the row", key, r.Privilege)
		} else if r.Privilege != want {
			t.Errorf("%s needs %s, the matrix says %s", key, r.Privilege, want)
		}
	}
	var gone []string
	for key := range wantPrivilege {
		if !have[key] {
			gone = append(gone, key)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("the matrix has rows for routes that are not registered: %v", gone)
	}
}

// credentials of every privilege set, as bearer tokens and as client certificates: the same matrix for both.
type who struct {
	name  string
	privs auth.Privilege
}

var callers = []who{{"record", auth.Record}, {"grant", auth.Grant}, {"admin", auth.Admin}, {"record+grant", auth.Record | auth.Grant}, {"all three", auth.Record | auth.Grant | auth.Admin}}

func matrixHandler(t *testing.T) http.Handler {
	t.Helper()
	var tokenSpec, certSpec []string
	for _, c := range callers {
		id := strings.ReplaceAll(c.name, "+", "-")
		id = strings.ReplaceAll(id, " ", "-")
		tokenSpec = append(tokenSpec, id+":"+auth.HashToken("tok-"+id)+":acme:"+c.privs.String())
		certSpec = append(certSpec, id+":"+id+".internal:acme:"+c.privs.String())
	}
	bearer, err := auth.ParseTokens(strings.Join(tokenSpec, ","))
	if err != nil {
		t.Fatal(err)
	}
	certs, err := auth.ParseCertSubjects(strings.Join(certSpec, ","))
	if err != nil {
		t.Fatal(err)
	}
	// Either credential: the service runs one mode at a time, and the matrix holds for each.
	stub := &stubMeter{}
	reg, err := resthandler.BuildRegistry(resthandler.Config{Meter: stub, Balance: stub, Clock: contracts.NewFakeClock(epoch), Ledger: &stubReads{}, Transfers: &stubReads{}, Auth: bearer})
	if err != nil {
		t.Fatal(err)
	}
	h, err := reg.Handler(resthandler.Options{Auth: auth.AuthenticatorFunc(func(ctx context.Context, c auth.Credential) (auth.Principal, error) {
		if len(c.PeerCertificates) > 0 {
			return certs.Authenticate(ctx, c)
		}
		return bearer.Authenticate(ctx, c)
	}), Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEveryRouteRefusesTheWrongPrivilegeAndTheNoCredential(t *testing.T) {
	t.Parallel()
	h := matrixHandler(t)
	reg := productionTable(t)
	for _, route := range reg.Routes() {
		if route.Surface != resthandler.V1 {
			continue
		}
		path := strings.NewReplacer("{idem_key}", "k-1", "{name}", "x", "{id}", "x").Replace(route.Path)
		for _, c := range callers {
			id := strings.ReplaceAll(strings.ReplaceAll(c.name, "+", "-"), " ", "-")
			for _, mode := range []string{"bearer", "certificate"} {
				t.Run(route.Method+" "+route.Path+"/"+c.name+"/"+mode, func(t *testing.T) {
					t.Parallel()
					req := httptest.NewRequest(route.Method, path, strings.NewReader(`{}`))
					req.Header.Set("Idempotency-Key", "k-1")
					if mode == "bearer" {
						req.Header.Set("Authorization", "Bearer tok-"+id)
					} else {
						req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: id + ".internal"}}}}
					}
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					allowed := c.privs.Has(route.Privilege)
					if allowed && (rec.Code == 401 || rec.Code == 403) {
						t.Fatalf("%s holds %s and was refused: %d %s", c.name, route.Privilege, rec.Code, rec.Body.String())
					}
					if !allowed && (rec.Code != 403 || decodeErr(t, rec).Error.Code != "forbidden") {
						t.Fatalf("%s lacks %s and got %d %s", c.name, route.Privilege, rec.Code, rec.Body.String())
					}
				})
			}
		}
		t.Run(route.Method+" "+route.Path+"/no credential", func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(route.Method, path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 401 || decodeErr(t, rec).Error.Code != "unauthenticated" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Realm binding in the matrix: a credential bound to acme cannot reach another realm on ANY route that
// takes a scope, by token or by certificate.
func TestNoCredentialCrossesIntoAnotherRealm(t *testing.T) {
	t.Parallel()
	h := matrixHandler(t)
	foreign := map[string]string{
		"POST /v1/admit":    `{"scope":{"realm":"globex","kind":"workspace","id":"w1"},"resource":"llm.chat","max_cost":1}`,
		"POST /v1/record":   `{"scope":{"realm":"globex","kind":"workspace","id":"w1"},"rate_key":"gpt","occurred_at":"2026-09-29T12:00:00Z","quantities":[{"unit":"in","amount":1}]}`,
		"POST /v1/grant":    `{"scope":{"realm":"globex","kind":"workspace","id":"w1"},"amount":1,"reason":"x"}`,
		"POST /v1/transfer": `{"from":{"realm":"globex","kind":"workspace","id":"w1"},"to":{"realm":"globex","kind":"workspace","id":"w2"},"amount":1,"reason":"x"}`,
	}
	for key, body := range foreign {
		method, path, _ := strings.Cut(key, " ")
		for _, mode := range []string{"bearer", "certificate"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				t.Parallel()
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Idempotency-Key", "k-1")
				if mode == "bearer" {
					req.Header.Set("Authorization", "Bearer tok-record-grant")
				} else {
					req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: "record-grant.internal"}}}}
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
