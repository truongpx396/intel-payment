package resthandler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// "Realm resolved from the credential, never from the body; money as integer minor units; credits as signed
// int64; timestamps ISO-8601 UTC." A request the contract calls invalid is refused at the edge, by name,
// and never reaches the engine.

func TestNoCredentialIs401OnEveryRoute(t *testing.T) {
	t.Parallel()
	for _, r := range allRoutes {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{}
			a := newAPI(t, stub, apiOpts{})
			for _, token := range []string{"", "nope"} {
				rec := a.do(token, r)
				if rec.Code != 401 || decodeErr(t, rec).Error.Code != "unauthenticated" {
					t.Fatalf("token %q: %d %s", token, rec.Code, rec.Body.String())
				}
			}
			if stub.calls.Load() != 0 {
				t.Fatal("the engine was reached without a credential")
			}
		})
	}
}

func TestPrivilegesAreSeparatedAtTheRoute(t *testing.T) {
	t.Parallel()
	allowed := map[string][]string{
		"record": {"admit", "record", "balance"},
		"grant":  {"grant", "transfer"},
		"all":    {"admit", "record", "balance", "grant", "transfer"},
	}
	for token, ok := range allowed {
		for _, r := range allRoutes {
			permitted := false
			for _, n := range ok {
				permitted = permitted || n == r.name
			}
			t.Run(token+"/"+r.name, func(t *testing.T) {
				t.Parallel()
				stub := &stubMeter{}
				rec := newAPI(t, stub, apiOpts{}).do(token, r)
				if permitted {
					if rec.Code >= 400 || stub.calls.Load() != 1 {
						t.Fatalf("must be allowed: %d %s (engine calls %d)", rec.Code, rec.Body.String(), stub.calls.Load())
					}
					return
				}
				if rec.Code != 403 || decodeErr(t, rec).Error.Code != "forbidden" || stub.calls.Load() != 0 {
					t.Fatalf("must be refused at the route: %d %s (engine calls %d)", rec.Code, rec.Body.String(), stub.calls.Load())
				}
			})
		}
	}
}

func TestTheRealmComesFromTheCredentialNotTheBody(t *testing.T) {
	t.Parallel()
	body := func(realm string) string {
		return `{"scope":{"realm":"` + realm + `","kind":"workspace","id":"w1"},"resource":"llm.chat","max_cost":10}`
	}
	t.Run("a realm outside the credential is 403 realm_mismatch and never reaches the engine", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		rec := newAPI(t, stub, apiOpts{}).call("all", "POST", "/v1/admit", body("globex"))
		if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" || stub.calls.Load() != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("an omitted realm is the credential's only realm", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		if rec := newAPI(t, stub, apiOpts{}).call("all", "POST", "/v1/admit", `{"scope":`+scopeJSON+`,"resource":"llm.chat","max_cost":10}`); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if stub.scope.Realm != "acme" {
			t.Fatalf("the engine saw realm %q", stub.scope.Realm)
		}
	})
	t.Run("two realms and none named: refused, not guessed", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		rec := newAPI(t, stub, apiOpts{}).call("two", "POST", "/v1/admit", `{"scope":`+scopeJSON+`,"resource":"llm.chat","max_cost":10}`)
		if rec.Code != 400 || decodeErr(t, rec).Error.Code != "invalid_request" || stub.calls.Load() != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a realm in the credential's set is served", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		if rec := newAPI(t, stub, apiOpts{}).call("two", "POST", "/v1/admit", body("globex")); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if stub.scope.Realm != "globex" {
			t.Fatalf("the engine saw realm %q", stub.scope.Realm)
		}
	})
	t.Run("both ends of a transfer", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		a := newAPI(t, stub, apiOpts{})
		for _, side := range []string{
			`{"from":{"realm":"globex","kind":"workspace","id":"w1"},"to":` + scope2 + `,"amount":5,"reason":"x"}`,
			`{"from":` + scopeJSON + `,"to":{"realm":"globex","kind":"workspace","id":"w2"},"amount":5,"reason":"x"}`,
		} {
			rec := a.call("all", "POST", "/v1/transfer", side, "Idempotency-Key", "k")
			if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		}
		if stub.calls.Load() != 0 {
			t.Fatal("a foreign realm reached the engine")
		}
	})
	t.Run("a balance read names its realm in the query", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		a := newAPI(t, stub, apiOpts{})
		rec := a.call("all", "GET", "/v1/balance?scope_realm=globex&scope_kind=workspace&scope_id=w1", "")
		if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" || stub.calls.Load() != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if rec := a.call("two", "GET", "/v1/balance?scope_realm=globex&scope_kind=workspace&scope_id=w1", ""); rec.Code != 200 || stub.scope.Realm != "globex" {
			t.Fatalf("%d %s realm=%q", rec.Code, rec.Body.String(), stub.scope.Realm)
		}
	})
}

// A request the contract calls invalid is a 400 invalid_request that names what is wrong, and the engine
// is not reached. Money is an integer: a fraction, an exponent or a string is not an amount.
func TestAMalformedRequestIsRefusedAtTheEdge(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		r     route
		body  string // replaces the route's body when set
		query string // replaces the route's path when set
		names string // the message names this
		code  string
		http  int
	}{
		{"malformed JSON", grantRoute, `{"scope":`, "", "", "invalid_request", 400},
		{"an empty body", grantRoute, ``, "", "", "invalid_request", 400},
		{"JSON null", grantRoute, `null`, "", "", "invalid_request", 400},
		{"an array", grantRoute, `[]`, "", "", "invalid_request", 400},
		{"trailing garbage after the object", grantRoute, `{"scope":` + scopeJSON + `,"amount":1,"reason":"x"} {"x":1}`, "", "", "invalid_request", 400},
		{"an unknown field is a typo that would silently not apply", admitRoute, `{"scope":` + scopeJSON + `,"resource":"llm.chat","maxcost":10}`, "", "maxcost", "invalid_request", 400},
		{"an amount that is a string", grantRoute, `{"scope":` + scopeJSON + `,"amount":"100","reason":"x"}`, "", "amount", "invalid_request", 400},
		{"an amount with a fraction", grantRoute, `{"scope":` + scopeJSON + `,"amount":1.5,"reason":"x"}`, "", "amount", "invalid_request", 400},
		{"an amount in exponent form", grantRoute, `{"scope":` + scopeJSON + `,"amount":1e2,"reason":"x"}`, "", "amount", "invalid_request", 400},
		{"an amount past int64", grantRoute, `{"scope":` + scopeJSON + `,"amount":9223372036854775808,"reason":"x"}`, "", "amount", "invalid_request", 400},
		{"max_cost with a fraction", admitRoute, `{"scope":` + scopeJSON + `,"resource":"llm.chat","max_cost":0.5}`, "", "max_cost", "invalid_request", 400},
		{"a quantity with a fraction", recordRoute, `{"scope":` + scopeJSON + `,"rate_key":"gpt","occurred_at":"2026-09-29T12:00:00Z","quantities":[{"unit":"in","amount":1.5}]}`, "", "amount", "invalid_request", 400},
		{"no scope", admitRoute, `{"resource":"llm.chat","max_cost":10}`, "", "scope", "invalid_request", 400},
		{"no occurred_at on a record is never a default", recordRoute, `{"scope":` + scopeJSON + `,"rate_key":"gpt","quantities":[{"unit":"in","amount":1}]}`, "", "occurred_at", "invalid_request", 400},
		{"an occurred_at that is not a timestamp", recordRoute, `{"scope":` + scopeJSON + `,"rate_key":"gpt","occurred_at":"yesterday","quantities":[{"unit":"in","amount":1}]}`, "", "occurred_at", "invalid_request", 400},
		{"an occurred_at with no zone is ambiguous", recordRoute, `{"scope":` + scopeJSON + `,"rate_key":"gpt","occurred_at":"2026-09-29T12:00:00","quantities":[{"unit":"in","amount":1}]}`, "", "occurred_at", "invalid_request", 400},
		{"an expires_at that is not a timestamp", grantRoute, `{"scope":` + scopeJSON + `,"amount":1,"reason":"x","expires_at":"soon"}`, "", "expires_at", "invalid_request", 400},
		{"a limit with no window is not a balance by default", admitRoute, `{"scope":` + scopeJSON + `,"resource":"llm.chat","limits":[{"name":"x","unit":"credit","deny_code":"limit_reached"}]}`, "", "window", "invalid_request", 400},
		{"a limit with an unknown window", admitRoute, `{"scope":` + scopeJSON + `,"resource":"llm.chat","limits":[{"name":"x","unit":"credit","window":"fortnightly","deny_code":"limit_reached"}]}`, "", "window", "invalid_request", 400},
		{"a rolling duration that is not whole seconds", admitRoute, `{"scope":` + scopeJSON + `,"resource":"llm.chat","limits":[{"name":"x","unit":"credit","window":"rolling","dur_seconds":1.5,"deny_code":"limit_reached"}]}`, "", "dur_seconds", "invalid_request", 400},
		{"a balance read with no scope_kind", balanceRoute, ``, "/v1/balance?scope_id=w1", "scope_kind", "invalid_request", 400},
		{"a balance read with no scope_id", balanceRoute, ``, "/v1/balance?scope_kind=workspace", "scope_id", "invalid_request", 400},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := row.r
			if row.query != "" {
				r.path = row.query
			} else {
				r.body = row.body
			}
			stub := &stubMeter{}
			rec := newAPI(t, stub, apiOpts{}).do("all", r)
			if rec.Code != row.http {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, row.http, rec.Body.String())
			}
			e := decodeErr(t, rec)
			if e.Error.Code != row.code {
				t.Fatalf("code = %q", e.Error.Code)
			}
			if row.names != "" && !strings.Contains(e.Error.Message, row.names) {
				t.Fatalf("the message should name %q: %q", row.names, e.Error.Message)
			}
			if stub.calls.Load() != 0 {
				t.Fatal("the engine was reached with a request the contract says is invalid")
			}
		})
	}
}

func TestABodyOverTheCapIs413AndNeverReachesTheEngine(t *testing.T) {
	t.Parallel()
	stub := &stubMeter{}
	a := newAPI(t, stub, apiOpts{maxBody: 256})
	r := grantRoute
	r.body = `{"scope":` + scopeJSON + `,"amount":1,"reason":"` + strings.Repeat("x", 1000) + `"}`
	rec := a.do("all", r)
	if rec.Code != http.StatusRequestEntityTooLarge || decodeErr(t, rec).Error.Code != "invalid_request" || stub.calls.Load() != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// Money crosses as an exact integer in both directions: 2^53+1 is the first integer a float64 cannot hold.
func TestMoneyIsAnExactIntegerOnTheWire(t *testing.T) {
	t.Parallel()
	const hi = int64(1)<<53 + 1
	stub := &stubMeter{grant: func(_ context.Context, g domain.Grant) (domain.Receipt, error) {
		return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Delta: g.Amount, Balance: g.Amount, Seq: hi}, nil
	}}
	r := grantRoute
	r.body = `{"scope":` + scopeJSON + `,"amount":9007199254740993,"reason":"purchase"}`
	rec := newAPI(t, stub, apiOpts{}).do("all", r)
	if rec.Code != 200 || int64(stub.gotGrant.Amount) != hi {
		t.Fatalf("%d %s; the engine saw %d", rec.Code, rec.Body.String(), stub.gotGrant.Amount)
	}
	for _, field := range []string{`"delta":9007199254740993`, `"balance":9007199254740993`, `"seq":9007199254740993`} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Fatalf("%s is not exact in %s", field, rec.Body.String())
		}
	}
}

func TestBalanceTotalsThePoolsAndRefusesToWrap(t *testing.T) {
	t.Parallel()
	t.Run("the pools and their total", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{balance: func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) {
			return map[domain.Pool]domain.Credits{domain.GeneralPool: 700, "promo": 30, "debt": -50}, nil
		}}
		rec := newAPI(t, stub, apiOpts{}).do("record", balanceRoute)
		var got struct {
			Pools map[string]int64 `json:"pools"`
			Total int64            `json:"total"`
		}
		mustDecode(t, rec.Body.Bytes(), &got)
		if rec.Code != 200 || got.Total != 680 || got.Pools["general"] != 700 || got.Pools["promo"] != 30 || got.Pools["debt"] != -50 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if stub.scope != (domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}) {
			t.Fatalf("scope %+v", stub.scope)
		}
	})
	t.Run("a total that would wrap is the server's error", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{balance: func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) {
			return map[domain.Pool]domain.Credits{domain.GeneralPool: domain.Credits(1<<63 - 1), "promo": 1}, nil
		}}
		if rec := newAPI(t, stub, apiOpts{}).do("record", balanceRoute); rec.Code != 500 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
}
