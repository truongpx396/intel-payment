package resthandler_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// What the caller sent is what the engine receives, field by field, and what the engine answers is what
// the caller reads. The JSON names are the contract's.

func TestEveryRequestFieldReachesTheEngine(t *testing.T) {
	t.Parallel()
	t.Run("admit, with every kind of limit", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		body := `{"scope":{"realm":"acme","kind":"workspace","id":"w1"},"resource":"llm.chat","subjects":{"user":"u1","job":"r1"},"max_cost":50000,"limits":[
			{"name":"user_cap","subject":"user","resource":"llm.chat","unit":"credit","max":1000,"window":"rolling","dur_seconds":3600,"tz":"Asia/Ho_Chi_Minh","warn_at":0.75,"deny_code":"limit_reached","max_entitlement":"daily_user_credits"},
			{"name":"run_cap","subject":"job","unit":"credit","max":500,"window":"job","dur_seconds":60,"deny_code":"limit_reached"},
			{"name":"daily","unit":"api_call","max":3,"window":"daily","deny_code":"limit_reached"},
			{"name":"hourly","unit":"api_call","max":3,"window":"hourly","deny_code":"limit_reached"},
			{"name":"scope_balance","unit":"credit","window":"balance","deny_code":"payment_required"}]}`
		if rec := newAPI(t, stub, apiOpts{}).call("all", "POST", "/v1/admit", body); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		want := domain.AdmitRequest{
			Resource: "llm.chat", Subjects: domain.Subjects{"user": "u1", "job": "r1"}, MaxCost: 50_000,
			Limits: []domain.Limit{
				{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: time.Hour, TZ: "Asia/Ho_Chi_Minh", WarnAt: 0.75, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"},
				{Name: "run_cap", Subject: "job", Unit: "credit", Max: 500, Window: domain.Job, Dur: time.Minute, DenyCode: "limit_reached"},
				{Name: "daily", Unit: "api_call", Max: 3, Window: domain.Daily, DenyCode: "limit_reached"},
				{Name: "hourly", Unit: "api_call", Max: 3, Window: domain.Hourly, DenyCode: "limit_reached"},
				{Name: "scope_balance", Unit: "credit", Window: domain.Balance, DenyCode: "payment_required"},
			},
		}
		if !reflect.DeepEqual(stub.admitReq, want) {
			t.Fatalf("got  %+v\nwant %+v", stub.admitReq, want)
		}
	})

	t.Run("record, to the nanosecond, in any zone", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		body := `{"scope":` + scopeJSON + `,"resource":"llm.chat","rate_key":"gpt-4o","subjects":{"user":"u1"},
			"occurred_at":"2026-09-29T19:00:00.123456789+07:00","attributes":{"trace_id":"t-1","operation_type":"query"},
			"quantities":[{"unit":"llm_input_token","amount":1200},{"unit":"llm_output_token","amount":800}]}`
		if rec := newAPI(t, stub, apiOpts{}).call("all", "POST", "/v1/record", body, "Idempotency-Key", "call-abc"); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		want := domain.Event{
			Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Resource: "llm.chat", RateKey: "gpt-4o", IdemKey: "call-abc",
			OccurredAt: mustTime("2026-09-29T12:00:00.123456789Z"),
			Quantities: []domain.Quantity{{Unit: "llm_input_token", Amount: 1200}, {Unit: "llm_output_token", Amount: 800}},
			Attributes: map[string]string{"trace_id": "t-1", "operation_type": "query"}, Subjects: domain.Subjects{"user": "u1"},
		}
		if !reflect.DeepEqual(stub.event.Attributes, want.Attributes) || !stub.event.OccurredAt.Equal(want.OccurredAt) ||
			!reflect.DeepEqual(stub.event.Quantities, want.Quantities) || stub.event.Scope != want.Scope || stub.event.IdemKey != "call-abc" ||
			stub.event.RateKey != "gpt-4o" || !reflect.DeepEqual(stub.event.Subjects, want.Subjects) || stub.event.Resource != "llm.chat" {
			t.Fatalf("got  %+v\nwant %+v", stub.event, want)
		}
		if stub.event.OccurredAt.Location() != time.UTC {
			t.Fatalf("timestamps are UTC: %v", stub.event.OccurredAt.Location())
		}
	})

	t.Run("grant: a clawback names its source, a positive grant its expiry", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		a := newAPI(t, stub, apiOpts{})
		rec := a.call("all", "POST", "/v1/grant", `{"scope":`+scopeJSON+`,"pool":"promo","amount":-250,"reason":"refund","source_key":"pay-1","ref":{"payment":"pi_1"}}`, "Idempotency-Key", "refund-1")
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		want := domain.Grant{Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Pool: "promo", Amount: -250, IdemKey: "refund-1", Reason: "refund", SourceKey: "pay-1", Ref: map[string]string{"payment": "pi_1"}}
		if !reflect.DeepEqual(stub.gotGrant, want) {
			t.Fatalf("got  %+v\nwant %+v", stub.gotGrant, want)
		}
		if rec := a.call("all", "POST", "/v1/grant", `{"scope":`+scopeJSON+`,"amount":100,"reason":"purchase","expires_at":"2027-01-01T00:00:00Z"}`, "Idempotency-Key", "pay-2"); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if g := stub.gotGrant; g.ExpiresAt == nil || !g.ExpiresAt.Equal(mustTime("2027-01-01T00:00:00Z")) || g.Pool != "" {
			t.Fatalf("the expiry arrives, and the default pool stays unnamed: %+v", g)
		}
	})

	t.Run("transfer", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		body := `{"from":` + scopeJSON + `,"to":` + scope2 + `,"from_pool":"promo","to_pool":"general","amount":2000,"reason":"allocation","max_dest_balance":9000,"ref":{"by":"ops"}}`
		if rec := newAPI(t, stub, apiOpts{}).call("all", "POST", "/v1/transfer", body, "Idempotency-Key", "alloc-1"); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		want := domain.Transfer{
			From: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, To: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w2"},
			FromPool: "promo", ToPool: "general", Amount: 2000, IdemKey: "alloc-1", Reason: "allocation", MaxDestBalance: 9000, Ref: map[string]string{"by": "ops"},
		}
		if !reflect.DeepEqual(stub.gotXfer, want) {
			t.Fatalf("got  %+v\nwant %+v", stub.gotXfer, want)
		}
	})
}

func TestEveryResponseFieldReachesTheCaller(t *testing.T) {
	t.Parallel()
	asMap := func(t *testing.T, raw []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("a receipt, writeoff included", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{grant: func(context.Context, domain.Grant) (domain.Receipt, error) {
			return domain.Receipt{IdemKey: "k-1", Applied: true, Delta: -10, Balance: 0, Seq: 7, RateCardVersion: "2026-01-01", Writeoff: 90}, nil
		}}
		rec := newAPI(t, stub, apiOpts{}).do("all", grantRoute)
		m := asMap(t, rec.Body.Bytes())
		for k, want := range map[string]any{"idem_key": "k-1", "applied": true, "deferred": false, "delta": float64(-10), "balance": float64(0), "seq": float64(7), "rate_card_version": "2026-01-01", "writeoff": float64(90)} {
			if m[k] != want {
				t.Fatalf("%s = %v, want %v in %s", k, m[k], want, rec.Body.String())
			}
		}
	})

	t.Run("a transfer receipt", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{transfer: func(context.Context, domain.Transfer) (domain.TransferReceipt, error) {
			return domain.TransferReceipt{IdemKey: "k-1", Applied: true, Amount: 2000, FromBalance: 3000, Status: domain.TransferInTransit}, nil
		}}
		m := asMap(t, newAPI(t, stub, apiOpts{}).do("all", transferRoute).Body.Bytes())
		for k, want := range map[string]any{"idem_key": "k-1", "applied": true, "amount": float64(2000), "from_balance": float64(3000), "status": "in_transit"} {
			if m[k] != want {
				t.Fatalf("%s = %v, want %v", k, m[k], want)
			}
		}
	})

	t.Run("an admission's limit, in full", func(t *testing.T) {
		t.Parallel()
		warn := domain.Limit{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: 90 * time.Minute, TZ: "UTC", WarnAt: 0.8, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"}
		stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{Allowed: true, Warning: &warn, Headroom: 340}, nil
		}}
		m := asMap(t, newAPI(t, stub, apiOpts{}).do("all", admitRoute).Body.Bytes())
		w, _ := m["warning"].(map[string]any)
		want := map[string]any{"name": "user_cap", "subject": "user", "resource": "llm.chat", "unit": "credit", "max": float64(1000), "window": "rolling", "dur_seconds": float64(5400), "tz": "UTC", "warn_at": 0.8, "deny_code": "limit_reached", "max_entitlement": "daily_user_credits"}
		for k, v := range want {
			if w[k] != v {
				t.Fatalf("warning.%s = %v, want %v in %v", k, w[k], v, w)
			}
		}
		if m["allowed"] != true || m["headroom"] != float64(340) || m["blocked"] != false {
			t.Fatalf("%v", m)
		}
	})
}
