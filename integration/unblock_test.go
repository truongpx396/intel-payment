//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// R112a, end to end: an account held by block_and_flag stays held however much is paid into it, until an operator clears it. The operator
// asks paymentd, the worker clears the books and then the hot account, and the account spends again — recorded with the person and the
// reason, audited by the database, and done once however often it is asked for.

func TestAnOperatorUnblocksAnAccountInTheBooksAndTheHotTierAndItSpendsAgain(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	const policy = "block_and_flag"
	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_NEGATIVE_BALANCE_POLICY", policy)))
	awaitOwning(t, pool, w, 8)
	tokens := "integration:" + auth.HashToken(serviceToken) + ":" + realm + ":record+grant+admin,reader:" + auth.HashToken(readerToken) + ":" + realm + ":record+grant"
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8", "PAYMENT_NEGATIVE_BALANCE_POLICY", policy, "PAYMENT_SERVICE_TOKENS", tokens)))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)

	id := fmt.Sprintf("w-%d", seq.Add(1))
	scope := fmt.Sprintf(`{"kind":"workspace","id":%q}`, id)
	grant := func(key string, amount int64, reason string) {
		t.Helper()
		eventually(t, 30*time.Second, "the grant "+key+" to be accepted", func() bool {
			code, _ := httpDo(t, c, "POST", base+"/v1/grant", serviceToken, fmt.Sprintf(`{"scope":%s,"amount":%d,"reason":%q}`, scope, amount, reason), "Idempotency-Key", key)
			return code == 200
		})
	}
	admit := func() (int, string) {
		t.Helper()
		return httpDo(t, c, "POST", base+"/v1/admit", serviceToken, `{"scope":`+scope+`,"resource":"llm.chat","max_cost":1}`)
	}
	booksBlocked := func() bool {
		return countRows(t, pool, `SELECT count(*) FROM account_watermarks WHERE realm = $1 AND scope_kind = 'workspace' AND scope_id = $2 AND blocked`, realm, id) == 1
	}

	// A chargeback larger than the balance: the shortfall is a write-off, and under block_and_flag the account is held.
	grant("g-1", 10, "purchase")
	grant("cb-1", -100, "chargeback")
	eventually(t, 30*time.Second, "the writer to book the write-off and the block", booksBlocked)
	// A later top-up does not release it: only an operator does.
	grant("g-2", 1000, "purchase")
	if code, body := admit(); code != http.StatusForbidden || !strings.Contains(body, "account_blocked") {
		t.Fatalf("a blocked account was admitted after a top-up: %d %s", code, body)
	}

	who := `"actor":"alice@example.com","reason":"INC-4412: the chargeback was reversed by the bank"`
	post := func(token, key, body string) (int, string) {
		t.Helper()
		return httpDo(t, c, "POST", base+"/v1/admin/unblock?realm="+realm, token, body, "Idempotency-Key", key)
	}
	accepted := func(key, body string) (opID string, created bool) {
		t.Helper()
		code, out := post(serviceToken, key, body)
		if code != http.StatusAccepted {
			t.Fatalf("POST /v1/admin/unblock = %d %s", code, out)
		}
		var r struct {
			Created   bool
			Operation opView
		}
		must(t, json.Unmarshal([]byte(out), &r))
		return r.Operation.ID, r.Created
	}
	finished := func(opID string) opView {
		t.Helper()
		var v opView
		eventually(t, 30*time.Second, "operation "+opID+" to finish", func() bool {
			code, out := httpDo(t, c, "GET", base+"/v1/admin/operations/"+opID+"?realm="+realm, serviceToken, "")
			if code != 200 {
				return false
			}
			must(t, json.Unmarshal([]byte(out), &v))
			return v.Status == "succeeded" || v.Status == "failed"
		})
		return v
	}
	audit := func(action string) int {
		return countRows(t, pool, `SELECT count(*) FROM audit_log WHERE subject_kind = 'admin_operations' AND action = $1 AND actor = 'alice@example.com'`, action)
	}
	held := func(v opView) (books, hot bool) {
		t.Helper()
		var r struct{ Books, Hot bool }
		must(t, json.Unmarshal(v.Result, &r))
		return r.Books, r.Hot
	}

	// A credential that can spend and mint cannot operate, and nothing is recorded for it.
	if code, _ := post(readerToken, "k-denied", `{"scope":`+scope+`,`+who+`}`); code != http.StatusForbidden {
		t.Fatalf("a credential without the admin privilege: %d, want 403", code)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM admin_operations`); n != 0 {
		t.Fatalf("%d operations were recorded for a caller who may not ask", n)
	}

	opID, created := accepted("unblock-4412-a", `{"scope":`+scope+`,`+who+`}`)
	if !created {
		t.Fatal("a new request was not created")
	}
	v := finished(opID)
	if v.Status != "succeeded" || v.Kind != "unblock" || v.Attempts != 1 {
		t.Fatalf("the unblock came to %+v", v)
	}
	if books, hot := held(v); !books || !hot {
		t.Fatalf("the operation says the books held the block: %v and the hot account: %v; both did", books, hot)
	}
	if booksBlocked() {
		t.Fatal("the books still hold the block after the unblock")
	}
	if code, body := admit(); code != http.StatusOK || !strings.Contains(body, `"allowed":true`) {
		t.Fatalf("the account was not admitted after the unblock: %d %s", code, body)
	}
	if audit("admin.unblock.requested") != 1 || audit("admin.unblock.succeeded") != 1 {
		t.Fatalf("audit rows: %d requested, %d succeeded, want 1 and 1", audit("admin.unblock.requested"), audit("admin.unblock.succeeded"))
	}

	// The same request again — a client that did not hear the first answer — is the same operation, done once.
	again, created := accepted("unblock-4412-a", `{`+who+`,"scope":`+scope+`}`)
	if created || again != opID {
		t.Fatalf("a retry made operation %s (created %v) after %s", again, created, opID)
	}
	time.Sleep(3 * time.Second) // time enough for a worker to do it a second time, were it going to
	if v := finished(opID); v.Attempts != 1 || audit("admin.unblock.requested") != 1 {
		t.Fatalf("a retry was done again: %+v, %d requests audited", v, audit("admin.unblock.requested"))
	}
	// A key is not a licence to clear another scope.
	other := fmt.Sprintf(`{"kind":"workspace","id":"w-%d"}`, seq.Add(1))
	if code, out := post(serviceToken, "unblock-4412-a", `{"scope":`+other+`,`+who+`}`); code != http.StatusConflict {
		t.Fatalf("a key reused for another scope: %d %s, want 409", code, out)
	}

	// Asking for an account that is not blocked is not an error, and says that nothing was held.
	second, _ := accepted("unblock-4412-b", `{"scope":`+scope+`,`+who+`}`)
	sv := finished(second)
	if books, hot := held(sv); sv.Status != "succeeded" || books || hot {
		t.Fatalf("an unblock of an account that was not blocked came to %+v (books %v, hot %v)", sv, books, hot)
	}
}
