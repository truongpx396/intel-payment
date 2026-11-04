//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/truongpx396/intel-payment/api/entitlementv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R105: entitlements run in the processes. paymentd serves them over REST and gRPC behind the credential; a limit sized by a plan
// is sized (and no longer a 503); and a configuration change made by plain SQL — no API, no processor, no announcement from anyone
// — reaches paymentd's cache through the audit trigger, payment-worker's config relay, the bus and the replica's Watch, with a
// cache TTL so long that nothing else could have refreshed it. Real PostgreSQL 16 and Redis 7, the binaries built and run.

const meterOnlyToken = "integration-meter-only-token"

func ptrI(v int64) *int64 { return &v }

func TestPaymentdServesEntitlementsBehindTheCredentialAndAChangeBySQLReachesItsCache(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// A small product: two plans, a scope on the first, and a daily ceiling sized by the plan.
	exec(`INSERT INTO entitlement_keys (realm, key, kind) VALUES ($1,'sso','flag'), ($1,'export','flag'), ($1,'seats','quota'), ($1,'daily_user_credits','quota')`, realm)
	exec(`INSERT INTO plans (realm, code, name, kind, billing_interval, sort_order) VALUES ($1,'team','Team','subscription','month',1), ($1,'pro','Pro','subscription','month',2)`, realm)
	exec(`INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_int) SELECT id, 'seats', 'quota', 25 FROM plans WHERE realm = $1 AND code = 'team'`, realm)
	exec(`INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_int) SELECT id, 'daily_user_credits', 'quota', 500 FROM plans WHERE realm = $1 AND code = 'team'`, realm)
	exec(`INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_bool) SELECT id, 'sso', 'flag', true FROM plans WHERE realm = $1 AND code = 'pro'`, realm)
	exec(`INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_int) SELECT id, 'seats', 'quota', 100 FROM plans WHERE realm = $1 AND code = 'pro'`, realm)
	exec(`INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref) VALUES ('acct-ent','stripe',ARRAY[$1]::text[],'test','env:K','env:W')`, realm)
	exec(`INSERT INTO subscriptions (realm, scope_kind, scope_id, plan_id, provider_account, provider_subscription_id, status)
	      SELECT $1, $2, $3, id, 'acct-ent', 'sub_it', 'active' FROM plans WHERE realm = $1 AND code = 'team'`, realm, sc.Kind, sc.ID)

	w := startWorker(t, workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL))
	awaitOwning(t, pool, w, 8)
	env := paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8",
		"PAYMENT_ENTITLEMENT_CACHE_TTL", "10m", "PAYMENT_INSTANCE_ID", "it-replica-1")
	env["PAYMENT_SERVICE_TOKENS"] += ",meter:" + auth.HashToken(meterOnlyToken) + ":" + realm + ":grant"
	d := startPaymentd(t, env)
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)

	check := `{"scope":{"kind":"` + sc.Kind + `","id":"` + sc.ID + `"},"checks":[{"key":"seats","current":25},{"key":"sso"},{"key":"export"},{"key":"nope"}]}`
	type decision struct {
		Key         string
		Allowed     bool
		Limit       *int64
		Reason      string
		UpgradePath string `json:"upgrade_path"`
	}
	ask := func() []decision {
		t.Helper()
		code, body := httpDo(t, c, "POST", base+"/v1/entitlements/check", serviceToken, check)
		if code != 200 {
			t.Fatalf("check = %d %s", code, body)
		}
		var out struct{ Data []decision }
		must(t, json.Unmarshal([]byte(body), &out))
		return out.Data
	}

	// Behind the credential: a stranger, a wrong token, and a credential that can only meter cannot read a customer's entitlements.
	for name, tc := range map[string]struct {
		token string
		want  int
	}{"no credential": {"", 401}, "a wrong token": {"not-the-token", 401}, "a credential that only meters": {meterOnlyToken, 403}} {
		if code, body := httpDo(t, c, "POST", base+"/v1/entitlements/check", tc.token, check); code != tc.want {
			t.Errorf("check with %s = %d %s, want %d", name, code, body, tc.want)
		}
		if code, _ := httpDo(t, c, "GET", fmt.Sprintf("%s/v1/entitlements?scope_kind=%s&scope_id=%s", base, sc.Kind, sc.ID), tc.token, ""); code != tc.want {
			t.Errorf("grants with %s = %d, want %d", name, code, tc.want)
		}
	}
	// A realm outside the credential is a 403, not an empty answer.
	other := `{"scope":{"kind":"workspace","id":"x","realm":"someone-else"},"checks":[{"key":"sso"}]}`
	if code, body := httpDo(t, c, "POST", base+"/v1/entitlements/check", serviceToken, other); code != 403 || !strings.Contains(body, "realm_mismatch") {
		t.Errorf("a realm outside the credential = %d %s, want 403 realm_mismatch", code, body)
	}

	// The plan's answers, with a reason and a way out.
	d0 := ask()
	if len(d0) != 4 || d0[0].Allowed || d0[0].Limit == nil || *d0[0].Limit != 25 || d0[0].UpgradePath != "pro" || d0[0].Reason == "" ||
		d0[1].Allowed || d0[1].UpgradePath != "pro" || d0[2].Allowed || d0[3].Allowed || d0[3].Key != "nope" {
		t.Fatalf("decisions on the team plan: %+v", d0)
	}

	// The same service through gRPC, behind the same credential.
	g, err := grpc.NewClient(d.listening(t, "grpc"), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcclient.WithBearer(serviceToken, grpcclient.OverInsecureTransport()))
	must(t, err)
	t.Cleanup(func() { _ = g.Close() })
	res, err := pb.NewEntitlementClient(g).Check(t.Context(), &pb.CheckRequest{Scope: &pb.Scope{Kind: sc.Kind, Id: sc.ID}, Checks: []*pb.Query{{Key: "seats", Current: ptrI(25)}, {Key: "sso"}}})
	must(t, err)
	if r := res.GetDecisions(); len(r) != 2 || r[0].GetAllowed() || r[0].GetLimit() != 25 || r[0].GetUpgradePath() != "pro" || r[1].GetAllowed() {
		t.Errorf("gRPC decisions: %v", r)
	}
	for name, dial := range map[string][]grpc.DialOption{
		"no credential": nil, "a credential that only meters": {grpcclient.WithBearer(meterOnlyToken, grpcclient.OverInsecureTransport())},
	} {
		conn, err := grpc.NewClient(d.listening(t, "grpc"), append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dial...)...)
		must(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		want := codes.Unauthenticated
		if dial != nil {
			want = codes.PermissionDenied
		}
		if _, err := pb.NewEntitlementClient(conn).Resolve(t.Context(), &pb.ResolveRequest{Scope: &pb.Scope{Kind: sc.Kind, Id: sc.ID}}); status.Code(err) != want {
			t.Errorf("gRPC Resolve with %s = %v, want %s", name, err, want)
		}
	}

	// The names an operator alerts on are declared before any traffic matters.
	_, scraped := httpDo(t, c, "GET", base+"/metrics", "", "")
	for _, name := range []string{"entitlement_decisions_total counter", "entitlement_invalid_total counter", "entitlement_cache_total counter", "entitlement_cache_invalidations_total counter"} {
		if !strings.Contains(scraped, "# TYPE "+name) {
			t.Errorf("paymentd does not declare %s", name)
		}
	}

	// A limit sized by the plan is sized: paymentd was given an entitlement source, so the answer is not a 503.
	exec(`INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, deny_code, sort_order) VALUES ($1,'user_daily','user','credit',10,'daily_user_credits','daily','limit_reached',10)`, realm)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	if code, body := httpDo(t, c, "POST", base+"/v1/grant", serviceToken, `{"scope":`+scopeJSON+`,"amount":100000,"reason":"purchase"}`, "Idempotency-Key", "g-ent"); code != 200 {
		t.Fatalf("grant = %d %s", code, body)
	}
	eventually(t, 15*time.Second, "paymentd to learn of the limit sized by a plan and to size it", func() bool {
		code, body := httpDo(t, c, "POST", base+"/v1/admit", serviceToken, `{"scope":`+scopeJSON+`,"resource":"llm.chat","subjects":{"user":"u1"},"max_cost":1}`)
		if code == 503 && strings.Contains(body, "quota_unavailable") {
			t.Fatalf("a limit sized by a plan is a 503 quota_unavailable: paymentd was not given an entitlement source: %s", body)
		}
		return code == 200 && strings.Contains(body, `"allowed":true`)
	})

	// The change is made behind everyone's back: plain SQL. The cache TTL is ten minutes, so only the relay can have refreshed it.
	exec(`UPDATE plan_entitlements SET value_int = 40 WHERE feature_key = 'seats' AND plan_id = (SELECT id FROM plans WHERE realm = $1 AND code = 'team')`, realm)
	eventually(t, 20*time.Second, "the plan's new size to reach paymentd through the audit trigger, the worker's relay, the bus and its Watch", func() bool {
		r := ask()
		return len(r) == 4 && r[0].Limit != nil && *r[0].Limit == 40
	})
	exec(`INSERT INTO entitlement_overrides (realm, scope_kind, scope_id, feature_key, value_kind, value_bool, reason, actor) VALUES ($1,$2,$3,'export','flag',true,'launch partner','sales-eng')`, realm, sc.Kind, sc.ID)
	eventually(t, 20*time.Second, "an override written by SQL to reach paymentd", func() bool {
		r := ask()
		return len(r) == 4 && r[2].Allowed
	})
	// And it says where it came from, and why.
	_, grants := httpDo(t, c, "GET", fmt.Sprintf("%s/v1/entitlements?scope_kind=%s&scope_id=%s", base, sc.Kind, sc.ID), serviceToken, "")
	if !strings.Contains(grants, `"key":"export","kind":"flag","value":true,"source":"override"`) || !strings.Contains(grants, `"reason":"launch partner"`) {
		t.Errorf("the override's provenance: %s", grants)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM audit_log WHERE subject_kind = 'entitlement_overrides' AND realm = $1`, realm); got != 1 {
		t.Errorf("%d audit rows for one override written by SQL, want 1", got)
	}
}
