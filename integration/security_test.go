//go:build integration && hardening

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// T132: the security review, executed. docs/security.md ends in a pre-production checklist and
// contracts/payment-provider-ports.md in a security checklist; most of their items are claims about a deployment
// (a secret manager, TLS on Postgres, backups tested by a restore) that nothing in this repository can verify, and
// docs/hardening.md says which. What CAN be tried is tried here, against the real binaries and a real Redis, the way
// an attacker or a careless host would try it — and each attempt must be refused, not merely absent.

// ------------------------------------------------------------ Redis, from the outside --

// The ACL (deploy/redis/users.acl) turns the default user off and gives each principal the least it needs. A host has
// NO Redis credential at all, so what a host can do is exactly what an anonymous or guessing client can do over the
// network. verify-deploy.sh tries the same from `docker exec redis-cli` inside the container; this tries it from a
// client on the other side of the network, with go-redis, which is what a host's process would be.
func TestSecurityRedisRefusesWhatEveryPrincipalButTheServiceIsNotMeantToDo(t *testing.T) { //nolint:paralleltest // security: one Redis, attempted in order
	ctx := context.Background()
	srv, err := redistest.RunWith(ctx, redistest.Options{Mode: redistest.Standalone, ACLFile: "../deploy/redis/users.acl"})
	must(t, err)
	t.Cleanup(func() { _ = srv.Terminate(context.Background()) })

	as := func(user, pass string) *goredis.Client {
		c := goredis.NewClient(&goredis.Options{Addr: srv.Addr, Username: user, Password: pass, MaxRetries: -1, DialTimeout: 3 * time.Second})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	// The deploy role installs the hot-path functions, as `payment-migrate functions` does: the only principal that may.
	_, err = hotredis.EnsureInstalled(ctx, as("deploy", "dev-deploy"))
	must(t, err)

	const (
		admitKey = "meta:{s1}"
		acctKey  = "acct:{s1}:default/w:x"
	)
	// denied: every one of these must come back as a refusal NAMED in want — not a timeout, not a different error, and
	// above all not success.
	type attempt struct {
		who  string
		c    *goredis.Client
		want string
		args []any
	}
	nobody, guess, wrong := as("", ""), as("default", ""), as("payment", "not-the-password")
	hc, deploy, svc := as("healthcheck", "dev-health"), as("deploy", "dev-deploy"), as("payment", "dev-payment")
	destructive := [][]any{
		{"FLUSHALL"}, {"FLUSHDB"}, {"CONFIG", "GET", "*"}, {"CONFIG", "SET", "maxmemory-policy", "allkeys-lru"}, {"ACL", "LIST"},
		{"KEYS", "*"}, {"SCAN", 0}, {"EVAL", "return 1", 0}, {"EVALSHA", strings.Repeat("0", 40), 0}, {"SCRIPT", "FLUSH"},
		{"FUNCTION", "LOAD", "#!lua name=evil\nredis.register_function('f', function() return 1 end)"}, {"FUNCTION", "DELETE", "intelpay"},
		{"FUNCTION", "FLUSH"}, {"REPLICAOF", "NO", "ONE"}, {"BGREWRITEAOF"}, {"SAVE"}, {"MONITOR"}, {"SHUTDOWN", "NOSAVE"},
	}
	var cases []attempt
	for _, args := range [][]any{
		{"GET", "x"}, {"SET", "x", "1"}, {"FCALL", "ip_admit", 2, admitKey, acctKey}, {"FCALL_RO", "ip_admit", 2, admitKey, acctKey},
		{"XADD", "billing:outbox:{s1}", "*", "junk", "x"}, {"HSET", acctKey, "b:general", 999999999}, {"FLUSHALL"}, {"KEYS", "*"}, {"CONFIG", "GET", "*"},
	} {
		cases = append(cases, attempt{"a host with no credential", nobody, "NOAUTH", args}) // the default user is off
	}
	cases = append(cases,
		attempt{"a host guessing the default user", guess, "", []any{"GET", "x"}},
		attempt{"a host guessing the service user's password", wrong, "", []any{"FCALL_RO", "ip_admit", 2, admitKey, acctKey}},
	)
	// The health probe's credential is the one a host sharing the compose environment could learn. It may PING and nothing else.
	for _, args := range [][]any{{"GET", "x"}, {"SET", "x", "1"}, {"FCALL_RO", "ip_admit", 2, admitKey, acctKey}, {"XADD", "billing:outbox:{s1}", "*", "a", "b"}, {"KEYS", "*"}, {"FLUSHALL"}} {
		cases = append(cases, attempt{"the health probe", hc, "NOPERM", args})
	}
	// The deploy role loads functions and nothing else: it can neither move a balance nor read one.
	for _, args := range [][]any{{"GET", "x"}, {"SET", "x", "1"}, {"FCALL", "ip_admit", 2, admitKey, acctKey}, {"XADD", "billing:outbox:{s1}", "*", "a", "b"}, {"FLUSHALL"}, {"CONFIG", "GET", "*"}} {
		cases = append(cases, attempt{"the deploy role", deploy, "NOPERM", args})
	}
	// Even the service role is closed to everything but the hot path: no arbitrary Lua, no flush, no config, no function
	// surgery. (DEBUG is not in the list: Redis 7 disables it outright, so it is refused before the ACL is consulted, as
	// "DEBUG command not allowed" and not as NOPERM.)
	for _, args := range destructive {
		cases = append(cases, attempt{"the service role", svc, "NOPERM", args})
	}
	for _, c := range cases {
		err := c.c.Do(ctx, c.args...).Err()
		switch {
		case err == nil:
			t.Errorf("%s: %v SUCCEEDED; it must be refused", c.who, c.args)
		case c.want == "" && !strings.Contains(err.Error(), "NOAUTH") && !strings.Contains(err.Error(), "WRONGPASS") && !strings.Contains(err.Error(), "NOPERM"):
			t.Errorf("%s: %v failed, but not as an access refusal: %v", c.who, c.args, err)
		case c.want != "" && !strings.Contains(err.Error(), c.want):
			t.Errorf("%s: %v was refused as %q, want %s", c.who, c.args, err, c.want)
		}
	}

	// And the control: the ACL is not simply closed to everyone. The service role can do its job.
	if err := svc.Ping(ctx).Err(); err != nil {
		t.Errorf("the service role cannot PING: %v", err)
	}
	if v, err := svc.Do(ctx, "FCALL_RO", "ip_admit", 2, admitKey, acctKey).Result(); err != nil {
		t.Errorf("the service role cannot call the hot path: %v", err)
	} else if r, ok := v.([]any); !ok || len(r) == 0 || (r[0] != "COLD_SHARD" && r[0] != "OK") {
		t.Errorf("the service role's admit call returned %#v", v)
	}
	if err := hc.Ping(ctx).Err(); err != nil {
		t.Errorf("the health probe cannot PING: %v", err)
	}
	measured(t, "T132", "redis-acl", "refusals_checked", len(cases))
}

// ------------------------------------------------------- the checklists, on the binaries --

// securityDoor is a raw request: httpDo hides response headers, and CORS is a header.
func securityDoor(t *testing.T, method, url string, body []byte, hdr ...string) (int, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	must(t, err)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header
}

// The last line of docs/security.md's checklist: "A tampered webhook, a webhook for an unknown account, a cross-realm
// scope, a record-only caller attempting Grant, Transfer and an admin write, and a direct SQL config change (which
// must appear in audit_log) have each been tried against the deployment." Here each is tried, with a positive control
// beside it so that a refusal means the guard worked and not that the route was missing.
func TestSecurityTheChecklistsAttacksAreEachTriedAndEachRefused(t *testing.T) { //nolint:paralleltest // security: the attacks share one deployment and one inbox count
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	pool := ledgerPool(t, dsn)
	ctx := context.Background()
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	seedPayments(t, pool, realm, sc)
	openScope(t, rd, realm, dsn, sc)

	const recOnly = "record-only-token"
	tokens := "full:" + auth.HashToken(serviceToken) + ":" + realm + ":record+grant+admin," + "rec:" + auth.HashToken(recOnly) + ":" + realm + ":record"
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0", "PAYMENT_SERVICE_TOKENS", tokens)))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 20 * time.Second}
	awaitReady(t, d, c, base)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	q := fmt.Sprintf("?scope_kind=%s&scope_id=%s", sc.Kind, sc.ID)

	// 1. A record-only caller. It can read and spend; it cannot mint, move, or administer.
	grantBody := `{"scope":` + scopeJSON + `,"amount":1000,"reason":"purchase"}`
	if code, body := httpDo(t, c, "POST", base+"/v1/grant", serviceToken, grantBody, "Idempotency-Key", "sec-g1"); code != 200 {
		t.Fatalf("control: a grant by the full credential = %d %s", code, body)
	}
	if code, body := httpDo(t, c, "GET", base+"/v1/balance"+q, recOnly, ""); code != 200 {
		t.Fatalf("control: a record-only caller's balance read = %d %s", code, body)
	}
	for _, a := range []struct{ what, method, path, body string }{
		{"Grant", "POST", "/v1/grant", grantBody},
		{"Transfer", "POST", "/v1/transfer", `{"from":` + scopeJSON + `,"to":` + scopeJSON + `,"amount":1}`},
		{"an admin write", "PUT", "/v1/admin/limits/anything", `{"actor":"x","reason":"y"}`},
		{"an admin operation", "POST", "/v1/admin/reconcile", `{"shard":0,"kind":"full","actor":"x","reason":"y"}`},
		{"an admin read", "GET", "/v1/admin/drift", ``},
	} {
		code, body := httpDo(t, c, a.method, base+a.path+"?realm="+realm, recOnly, a.body, "Idempotency-Key", "sec-"+a.what)
		if code != http.StatusForbidden {
			t.Errorf("a record-only caller attempting %s got %d %s, want 403", a.what, code, body)
		}
		// And no credential at all is 401, never a fall-through.
		if code, _ := httpDo(t, c, a.method, base+a.path+"?realm="+realm, "", a.body, "Idempotency-Key", "sec-anon"); code != http.StatusUnauthorized {
			t.Errorf("an anonymous %s got %d, want 401", a.what, code)
		}
	}
	if hot := hotBalance(t, rd, realm, dsn, sc); hot != 1000 {
		t.Errorf("the hot balance is %d after the refused calls, want only the one legitimate grant, 1000", hot)
	}

	// 2. A cross-realm scope: the realm is the credential's, never the body's.
	other := `{"realm":"someone-else","kind":"workspace","id":"w-x"}`
	for _, a := range []struct{ what, method, path, body string }{
		{"admit", "POST", "/v1/admit", `{"scope":` + other + `,"resource":"llm.chat"}`},
		{"record", "POST", "/v1/record", `{"scope":` + other + `,"resource":"llm.chat","rate_key":"gpt","occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","quantities":[{"unit":"in","amount":1}]}`},
		{"grant", "POST", "/v1/grant", `{"scope":` + other + `,"amount":1000,"reason":"purchase"}`},
		{"balance", "GET", "/v1/balance?scope_kind=workspace&scope_id=w-x&scope_realm=someone-else", ``},
	} {
		code, body := httpDo(t, c, a.method, base+a.path, serviceToken, a.body, "Idempotency-Key", "sec-x-"+a.what)
		if code != http.StatusForbidden {
			t.Errorf("a cross-realm %s got %d %s, want 403 realm_mismatch", a.what, code, body)
		} else if !strings.Contains(body, "realm_mismatch") && !strings.Contains(body, "cross_realm") {
			t.Errorf("a cross-realm %s was refused without saying why: %s", a.what, body)
		}
	}

	// 3. The webhook door: every attack is refused AND leaves nothing in the inbox (nothing before verification).
	hook := base + "/webhooks/stripe/" + itAccount
	inbox := func() int { return countRows(t, pool, `SELECT count(*) FROM payment_events`) }
	good, goodSig := delivery(t, "evt_sec_good", "pi_sec_good")
	if code, body := httpDo(t, c, "POST", hook, "", good, "Stripe-Signature", goodSig, "Content-Type", "application/json"); code != 200 {
		t.Fatalf("control: a genuine delivery = %d %s", code, body)
	}
	if inbox() != 1 {
		t.Fatalf("control: the genuine delivery left %d inbox rows, want 1", inbox())
	}
	staleBody, staleSig := deliveryAt(t, "evt_sec_stale", "pi_sec_stale", time.Now().Add(-time.Hour))
	wrongBody, _ := delivery(t, "evt_sec_wrong", "pi_sec_wrong")
	for _, a := range []struct {
		what, url, body string
		hdr             []string
	}{
		{"a tampered body", hook, good + " ", []string{"Stripe-Signature", goodSig}},
		{"an unsigned delivery", hook, good, nil},
		{"a garbage signature", hook, good, []string{"Stripe-Signature", "t=1,v1=00"}},
		{"a stale (replayed an hour later) delivery", hook, staleBody, []string{"Stripe-Signature", staleSig}},
		{"a body signed with the wrong secret", hook, wrongBody, []string{"Stripe-Signature", "t=" + fmt.Sprint(time.Now().Unix()) + ",v1=" + strings.Repeat("ab", 32)}},
		{"a delivery for an unknown account", base + "/webhooks/stripe/no-such-account", good, []string{"Stripe-Signature", goodSig}},
		{"a delivery for an unknown provider", base + "/webhooks/not-a-provider/" + itAccount, good, []string{"Stripe-Signature", goodSig}},
		{"a body far over the size limit", hook, strings.Repeat("x", 8<<20), []string{"Stripe-Signature", goodSig}},
	} {
		code, _ := securityDoor(t, "POST", a.url, []byte(a.body), append(a.hdr, "Content-Type", "application/json")...)
		if code < 400 || code >= 500 {
			t.Errorf("%s was answered %d, want a 4xx refusal", a.what, code)
		}
		if a.what == "a delivery for an unknown account" && code != http.StatusNotFound {
			t.Errorf("an unknown account was answered %d, want 404", code)
		}
	}
	if inbox() != 1 {
		t.Errorf("the inbox holds %d rows after the attacks, want only the genuine delivery: something was persisted before verification", inbox())
	}
	// No CORS on the door: a browser at another origin cannot read or preflight it.
	for _, m := range []string{"OPTIONS", "POST"} {
		_, h := securityDoor(t, m, hook, []byte(`{}`), "Origin", "https://evil.example", "Access-Control-Request-Method", "POST")
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("the webhook door answered a cross-origin %s with Access-Control-Allow-Origin: %q", m, h.Get("Access-Control-Allow-Origin"))
		}
	}

	// 4. A direct SQL change to configuration is audited by the database, attributed to the role that made it.
	var before int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE realm = $1 AND action = 'config.update' AND subject_kind = 'limits'`, realm).Scan(&before))
	_, err := pool.Exec(ctx, `UPDATE limits SET max = 123 WHERE realm = $1 AND name = 'scope_balance'`, realm)
	must(t, err)
	var after int
	var actor string
	must(t, pool.QueryRow(ctx, `SELECT count(*), coalesce(max(actor), '') FROM audit_log WHERE realm = $1 AND action = 'config.update' AND subject_kind = 'limits'`, realm).Scan(&after, &actor))
	if after != before+1 || !strings.HasPrefix(actor, "db:") {
		t.Errorf("a direct SQL UPDATE of limits left %d new audit rows (actor %q); the database must record it, attributed to its role", after-before, actor)
	}

	// Recorded, not asserted: what the service exposes without a credential. The checklist asks that /metrics and the
	// admin endpoints not be publicly reachable; admin is refused above, and /metrics is the operator's to keep off the
	// public network.
	metricsCode, _ := httpDo(t, c, "GET", base+"/metrics", "", "")
	measured(t, "T132", "checklist-attacks", "metrics_without_a_credential_status", metricsCode, "inbox_rows_after_attacks", inbox())
}

// The checklist's first line: "PAYMENT_SERVICE_AUTH is mtls or bearer; none only on loopback/UDS." The service
// credential is what keeps `/v1/*` closed, so a deployment that turns it off on an address anything else can reach
// must not start at all — not start and log a warning.
func TestSecurityPaymentdWillNotServeWithoutAuthenticationOnAnAddressOtherThanLoopback(t *testing.T) { //nolint:paralleltest // security: the processes share this test's ports
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	env := func(addr string) map[string]string {
		return paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0", "PAYMENT_SERVICE_AUTH", "none", "PAYMENT_SERVICE_TOKENS", "-", "PAYMENT_HTTP_ADDR", addr)
	}

	// Control: with no authentication on loopback it serves, so a refusal below is about the address and not about "none".
	ok := startPaymentd(t, env("127.0.0.1:0"))
	awaitReady(t, ok, &http.Client{Timeout: 10 * time.Second}, ok.httpURL(t))

	bad := startPaymentd(t, env("0.0.0.0:0"))
	if code := bad.waitExit(t, 30*time.Second); code == 0 {
		t.Errorf("paymentd with PAYMENT_SERVICE_AUTH=none on 0.0.0.0 exited 0; it must refuse to start")
	}
	if !strings.Contains(bad.logs.String(), "PAYMENT_SERVICE_AUTH") {
		t.Errorf("it refused without naming the setting at fault; logs:\n%s", bad.logs.String())
	}
}
