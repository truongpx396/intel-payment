package entitlementhttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementhttp"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R105: the REST door. A batch Check answers every key it was given, in request order, and an unknown key is denied inside the
// batch and is not an error; the realm comes from the credential; the entitlements being unavailable is a 503 and never some keys
// permitted. (That the gRPC door gives the same decisions for the same input is integration/entitlement_transports_test.go.)

func TestTheDoorServesTheRoutesItHasPortsForAndNeverAnOpenOne(t *testing.T) {
	t.Parallel()
	r := newRig(t, inT1)
	got := map[string]entitlementhttp.Privilege{}
	for _, rt := range r.door.Routes() {
		got[rt.Method+" "+rt.Path] = rt.Privilege
	}
	want := map[string]entitlementhttp.Privilege{"GET /v1/entitlements": entitlementhttp.Record, "POST /v1/entitlements/check": entitlementhttp.Record}
	if len(got) != 2 || got["GET /v1/entitlements"] != want["GET /v1/entitlements"] || got["POST /v1/entitlements/check"] != want["POST /v1/entitlements/check"] {
		t.Fatalf("routes = %v; want %v: both read, so both need only `record`", got, want)
	}
	if entitlementhttp.Record.String() != "record" {
		t.Errorf("the document says `record`, the door says %q", entitlementhttp.Record)
	}
	// A door serves what it has a port for.
	only, err := entitlementhttp.New(entitlementhttp.Config{Checker: r.e, Caller: inT1})
	if err != nil || len(only.Routes()) != 1 || only.Routes()[0].Path != "/v1/entitlements/check" {
		t.Errorf("a door with only a Checker: %v, %v", only, err)
	}
	if _, err := entitlementhttp.New(entitlementhttp.Config{Entitler: r.e, Checker: r.e}); err == nil {
		t.Error("a door with no Caller could not tell whose realm a call is for, and was built")
	}
	if _, err := entitlementhttp.New(entitlementhttp.Config{Caller: inT1}); err == nil {
		t.Error("a door with no ports serves nothing, and was built")
	}
}

func TestACheckAnswersEveryKeyInOrderWithTheValuesOfTheirKinds(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	res := rg.check(t, checkBody(`{"key":"support_tier"},{"key":"no_such_capability"},{"key":"sso"},{"key":"seats","current":25},{"key":"seats"},{"key":"export"},{"key":"daily_user_credits","current":1999,"delta":2}`))
	if res.Code != http.StatusOK {
		t.Fatalf("%d %s", res.Code, res.Body)
	}
	if ct := res.Head.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := res.Head.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; an intermediary that kept an answer would deny a customer who just paid", cc)
	}
	d := res.decisions(t)
	keys := []string{"support_tier", "no_such_capability", "sso", "seats", "seats", "export", "daily_user_credits"}
	if len(d) != len(keys) {
		t.Fatalf("%d answers for %d questions: %s", len(d), len(keys), res.Body)
	}
	for i, k := range keys {
		if d[i].Key != k {
			t.Errorf("answer %d is for %q, want %q: request order", i, d[i].Key, k)
		}
	}
	if d[0].Kind != "enum" || !d[0].Allowed || d[0].Value != "silver" {
		t.Errorf("support_tier on team: %+v; want the enum member", d[0])
	}
	if d[1].Allowed || d[1].Kind != "" || d[1].Value != nil || d[1].Reason == "" || !strings.Contains(d[1].Reason, "no_such_capability") {
		t.Errorf("an unknown key: %+v; want denied inside the batch, with a reason that names it", d[1])
	}
	if d[2].Allowed || d[2].Kind != "flag" || d[2].UpgradePath != "pro" || d[2].Reason == "" {
		t.Errorf("sso on team: %+v; want refused, with pro as the way out", d[2])
	}
	if d[3].Allowed || d[3].Kind != "quota" || d[3].Limit == nil || *d[3].Limit != 25 || *d[3].Current != 25 || *d[3].Remaining != 0 || d[3].UpgradePath != "pro" ||
		!strings.Contains(d[3].Reason, "25 seats") {
		t.Errorf("the 26th seat: %+v", d[3])
	}
	if !d[4].Allowed || d[4].Limit == nil || *d[4].Limit != 25 || d[4].Current != nil || d[4].Remaining != nil || d[4].Value != float64(25) {
		t.Errorf("seats with no count: %+v; want granted at 25 and nothing measured", d[4])
	}
	if !d[5].Allowed || d[5].Value != true {
		t.Errorf("export (an override): %+v", d[5])
	}
	if d[6].Allowed || *d[6].Remaining != 1 || *d[6].Limit != 2000 {
		t.Errorf("1999 + 2 of 2000: %+v; want refused with the room it found", d[6])
	}
	// What was not measured is absent from the body, not zero.
	var raw struct{ Data []map[string]any }
	mustJSON(t, res.Body, &raw)
	for _, k := range []string{"current", "remaining"} {
		if _, present := raw.Data[4][k]; present {
			t.Errorf("an unmeasured quota carries %q: absent and zero are different answers", k)
		}
	}
}

// -1 is a number on the wire. A client that read "-1" as a string, or 0, or omitted it, would treat an unlimited plan as a refusal.
func TestUnlimitedIsTheNumberMinusOneOnTheWire(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	if err := rg.mem.PutOverride(t.Context(), domain.Grant{Scope: scopeDemo, Key: "seats", Value: domain.UnlimitedQuota(), Source: domain.FromOverride, Reason: "negotiated"}, "alice"); err != nil {
		t.Fatal(err)
	}
	res := rg.check(t, checkBody(`{"key":"seats"},{"key":"seats","current":1000000}`))
	for _, want := range []string{`"value":-1`, `"limit":-1`, `"remaining":-1`} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("the body lacks %s:\n%s", want, res.Body)
		}
	}
	if strings.Contains(res.Body, `"-1"`) {
		t.Errorf("unlimited went out as a string:\n%s", res.Body)
	}
	for _, d := range res.decisions(t) {
		if !d.Allowed {
			t.Errorf("unlimited refused: %+v", d)
		}
	}
	grants := rg.do(t, http.MethodGet, "/v1/entitlements?scope_kind=org&scope_id=o1", "")
	if !strings.Contains(grants.Body, `"key":"seats","kind":"quota","value":-1,"source":"override"`) {
		t.Errorf("the grant of an unlimited override:\n%s", grants.Body)
	}
}

// The realm is the credential's: authenticated, never asserted.
func TestTheRealmComesFromTheCredentialAndIsNeverAssertedByTheBody(t *testing.T) {
	t.Parallel()
	body := func(realm string) string {
		return `{"scope":{"kind":"org","id":"o1"` + realm + `},"checks":[{"key":"seats"}]}`
	}
	t1 := newRig(t, inT1)
	if d := t1.check(t, body("")).decisions(t); len(d) != 1 || *d[0].Limit != 25 {
		t.Errorf("a credential for t1, a scope naming no realm: %+v; want t1's team plan (25)", d)
	}
	if res := t1.check(t, body(`,"realm":"t1"`)); res.Code != 200 {
		t.Errorf("the credential's own realm named: %d %s", res.Code, res.Body)
	}
	res := t1.check(t, body(`,"realm":"other"`))
	if code, _ := res.errorCode(t); res.Code != http.StatusForbidden || code != "realm_mismatch" {
		t.Errorf("a realm outside the credential: %d %s; want 403 realm_mismatch", res.Code, res.Body)
	}
	if strings.Contains(res.Body, "5000") {
		t.Error("another product's plan leaked into a refusal")
	}
	// A credential for two realms must be told which: a realm is never guessed.
	both := newRig(t, credential{realms: []md.Realm{"t1", "other"}})
	if res := both.check(t, body("")); res.Code != http.StatusBadRequest {
		t.Errorf("a credential spanning two realms and a scope naming none: %d; want 400", res.Code)
	}
	if d := both.check(t, body(`,"realm":"other"`)).decisions(t); len(d) != 1 || *d[0].Limit != 5000 {
		t.Errorf("the other realm, named: %+v; want its own plan (5000), not t1's", d)
	}
	// No credential reached the handler, or one without the privilege.
	none := newRig(t, credential{none: true}).check(t, body(""))
	if code, _ := none.errorCode(t); none.Code != http.StatusUnauthorized || code != "unauthenticated" || none.Head.Get("WWW-Authenticate") == "" {
		t.Errorf("no credential: %d %s %v; want 401 unauthenticated with WWW-Authenticate", none.Code, none.Body, none.Head)
	}
	forbid := newRig(t, credential{noRecord: true}).check(t, body(""))
	if code, _ := forbid.errorCode(t); forbid.Code != http.StatusForbidden || code != "forbidden" {
		t.Errorf("no privilege: %d %s; want 403 forbidden", forbid.Code, forbid.Body)
	}
}

func TestARequestThatIsNotAQuestionIsA400AndAnswersNothing(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	many := strings.TrimSuffix(strings.Repeat(`{"key":"sso"},`, domain.MaxChecks+1), ",")
	for name, body := range map[string]string{
		"an empty body":         ``,
		"not JSON":              `<scope/>`,
		"two values":            checkBody(`{"key":"sso"}`) + checkBody(`{"key":"sso"}`),
		"an unknown field":      `{` + scopeJSON + `,"checks":[{"key":"sso"}],"realm":"t1"}`,
		"a typo in a check":     checkBody(`{"key":"sso","curent":3}`),
		"no scope":              `{"checks":[{"key":"sso"}]}`,
		"no checks":             `{` + scopeJSON + `}`,
		"empty checks":          checkBody(``),
		"too many checks":       checkBody(many),
		"an empty key":          checkBody(`{"key":""}`),
		"a negative count":      checkBody(`{"key":"seats","current":-1}`),
		"a negative delta":      checkBody(`{"key":"seats","current":1,"delta":-1}`),
		"a delta with no count": checkBody(`{"key":"seats","delta":1}`),
		"a fractional count":    checkBody(`{"key":"seats","current":1.5}`),
		"a string count":        checkBody(`{"key":"seats","current":"3"}`),
		"a count too large":     checkBody(`{"key":"seats","current":9223372036854775808}`),
		"a scope with no id":    `{"scope":{"kind":"org"},"checks":[{"key":"sso"}]}`,
	} {
		res := rg.check(t, body)
		code, _ := res.errorCode(t)
		if res.Code != http.StatusBadRequest || code != "invalid_request" || strings.Contains(res.Body, `"data"`) {
			t.Errorf("%s: %d %s; want 400 invalid_request and no answers", name, res.Code, res.Body)
		}
	}
	// The most there may be is fine.
	ok := strings.TrimSuffix(strings.Repeat(`{"key":"sso"},`, domain.MaxChecks), ",")
	if res := newRig(t, inT1, func(c *entitlementhttp.Config) { c.MaxBody = 1 << 20 }).check(t, checkBody(ok)); res.Code != 200 || len(res.decisions(t)) != domain.MaxChecks {
		t.Errorf("%d checks, the most there may be: %d", domain.MaxChecks, res.Code)
	}
	// A body over the limit is refused before it is read in full.
	big := checkBody(strings.Repeat(`{"key":"sso"},`, 1000) + `{"key":"sso"}`)
	if res := rg.check(t, big); res.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the cap: %d; want 413", res.Code)
	}
}

// Fail closed, and say nothing that names the topology: the entitlements being unavailable is 503 with fixed text and NO
// decisions; anything the door does not name is a 500 with fixed text; the cause goes to the operator's log.
func TestAnOutageIsA503WithFixedTextAndNoDecisionsAndTheCauseIsLogged(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	rg.faults.BreakWith(errBoom)
	for name, res := range map[string]response{
		"check":   rg.check(t, checkBody(`{"key":"seats"},{"key":"sso"}`)),
		"resolve": rg.do(t, http.MethodGet, "/v1/entitlements?scope_kind=org&scope_id=o1", ""),
	} {
		code, msg := res.errorCode(t)
		if res.Code != http.StatusServiceUnavailable || code != "unavailable" {
			t.Errorf("%s: %d %s; want 503 unavailable", name, res.Code, res.Body)
		}
		if strings.Contains(res.Body, "10.1.2.3") || strings.Contains(res.Body, "refused") || strings.Contains(msg, "dial") {
			t.Errorf("%s: the body names the topology: %s", name, res.Body)
		}
		if strings.Contains(res.Body, `"data"`) {
			t.Errorf("%s: an unavailable answer came with decisions: %s", name, res.Body)
		}
	}
	if !strings.Contains(rg.log.String(), "10.1.2.3") {
		t.Errorf("the operator's log lacks the cause: %q", rg.log.String())
	}

	unknown := newRig(t, inT1, func(c *entitlementhttp.Config) { c.Checker = brokenChecker{errBoom} })
	res := unknown.check(t, checkBody(`{"key":"sso"}`))
	if code, msg := res.errorCode(t); res.Code != http.StatusInternalServerError || code != "internal" || msg != "internal error" || strings.Contains(res.Body, "10.1.2.3") {
		t.Errorf("an error the door does not name: %d %s; want a 500 with fixed text", res.Code, res.Body)
	}
	if !strings.Contains(unknown.log.String(), "10.1.2.3") {
		t.Errorf("the cause of a 500 is not in the log: %q", unknown.log.String())
	}
}

func TestResolveListsEveryGrantWithItsProvenance(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	res := rg.do(t, http.MethodGet, "/v1/entitlements?scope_kind=org&scope_id=o1", "")
	if res.Code != http.StatusOK || res.Head.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", res.Code, res.Head)
	}
	want := `{"data":[` +
		`{"key":"daily_user_credits","kind":"quota","value":2000,"source":"subscription","reason":""},` +
		`{"key":"export","kind":"flag","value":true,"source":"override","expires_at":"2026-12-04T12:00:00Z","reason":"launch partner"},` +
		`{"key":"seats","kind":"quota","value":25,"source":"subscription","reason":""},` +
		`{"key":"support_tier","kind":"enum","value":"silver","source":"subscription","reason":""}]}`
	if strings.TrimSpace(res.Body) != want {
		t.Errorf("grants =\n%s\nwant\n%s", res.Body, want)
	}
	// Another product's scope with the same id is its own picture.
	other := rg.do(t, http.MethodGet, "/v1/entitlements?scope_kind=org&scope_id=o1&scope_realm=t1", "")
	if other.Body != res.Body {
		t.Errorf("naming the credential's own realm changed the answer")
	}
	// A scope that holds nothing is an empty list, not an error and not null.
	if res := rg.do(t, http.MethodGet, "/v1/entitlements?scope_kind=org&scope_id=nobody", ""); res.Code != 200 || strings.TrimSpace(res.Body) != `{"data":[]}` {
		t.Errorf("a scope with no plan and no free tier: %d %s; want an empty list", res.Code, res.Body)
	}
	for name, c := range map[string]struct{ target, names string }{
		"no scope_kind": {"/v1/entitlements?scope_id=o1", "scope_kind"}, "no scope_id": {"/v1/entitlements?scope_kind=org", "scope_id"},
		"nothing": {"/v1/entitlements", "scope_kind"},
	} {
		res := rg.do(t, http.MethodGet, c.target, "")
		if _, msg := res.errorCode(t); res.Code != http.StatusBadRequest || !strings.Contains(msg, c.names) {
			t.Errorf("%s: %d %q; want 400 naming %s: a caller fixes the parameter it is told about", name, res.Code, msg, c.names)
		}
	}
	if res := rg.do(t, http.MethodGet, "/v1/entitlements?scope_kind=org&scope_id=o1&scope_realm=other", ""); res.Code != http.StatusForbidden {
		t.Errorf("a realm outside the credential: %d; want 403", res.Code)
	}
}

// The caller's own cancellation wins over whatever the call made of it: a caller that went away is not an INTERNAL, and an
// operator is not paged for it.
func TestACallerWhoWentAwayIsNotAnInternalError(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1, func(c *entitlementhttp.Config) { c.Checker = brokenChecker{errBoom} })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/entitlements/check", strings.NewReader(checkBody(`{"key":"sso"}`)))
	rec := httptest.NewRecorder()
	rg.h.ServeHTTP(rec, req)
	if code, _ := (response{rec.Code, rec.Header(), rec.Body.String()}).errorCode(t); rec.Code != 499 || code != "cancelled" {
		t.Errorf("a cancelled call: %d %s; want 499 cancelled", rec.Code, rec.Body)
	}
	if strings.Contains(rg.log.String(), "10.1.2.3") {
		t.Error("a caller going away was logged as the service failing")
	}
}
