//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// R112a, end to end: an operator corrects the books. The adjustment is a signed grant with the reserved reason, booked by the writer like
// any other, led back to the operation and the person by its ref, done once however often it is asked for, and audited by the database.

func TestAnOperatorAdjustsTheBooksOnceHoweverOftenTheyAskAndItIsLedBackToThem(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL)))
	awaitOwning(t, pool, w, 8)
	tokens := "integration:" + auth.HashToken(serviceToken) + ":" + realm + ":record+grant+admin,reader:" + auth.HashToken(readerToken) + ":" + realm + ":record+grant"
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8", "PAYMENT_SERVICE_TOKENS", tokens)))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)

	id := fmt.Sprintf("w-%d", seq.Add(1))
	scope := fmt.Sprintf(`{"kind":"workspace","id":%q}`, id)
	grantWhenReady(t, c, base, scope) // 100000 credits in the general pool
	who := `"actor":"alice@example.com","reason":"INC-4412: the chargeback was booked twice"`
	post := func(token, key, body string) (int, string) {
		t.Helper()
		return httpDo(t, c, "POST", base+"/v1/admin/adjustments?realm="+realm, token, body, "Idempotency-Key", key)
	}
	accepted := func(key, body string) (opID string, created bool) {
		t.Helper()
		code, out := post(serviceToken, key, body)
		if code != http.StatusAccepted {
			t.Fatalf("POST /v1/admin/adjustments = %d %s", code, out)
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
	adjustments := func() (n, sum int) {
		n = countRows(t, pool, `SELECT count(*) FROM credit_ledger WHERE realm = $1 AND scope_kind = 'workspace' AND scope_id = $2 AND operation_type = 'admin_adjustment'`, realm, id)
		sum = countRows(t, pool, `SELECT coalesce(sum(delta), 0)::bigint FROM credit_ledger WHERE realm = $1 AND scope_kind = 'workspace' AND scope_id = $2 AND operation_type = 'admin_adjustment'`, realm, id)
		return
	}
	audit := func(action string) int {
		return countRows(t, pool, `SELECT count(*) FROM audit_log WHERE subject_kind = 'admin_operations' AND action = $1 AND actor = 'alice@example.com'`, action)
	}
	hot := func() int64 {
		code, body := httpDo(t, c, "GET", base+"/v1/balance?scope_kind=workspace&scope_id="+id, serviceToken, "")
		if code != 200 {
			t.Fatalf("balance = %d %s", code, body)
		}
		var b struct{ Total int64 }
		must(t, json.Unmarshal([]byte(body), &b))
		return b.Total
	}

	// A credential that can spend and mint cannot operate, and nothing is recorded for it.
	if code, _ := post(readerToken, "k-denied", `{"scope":`+scope+`,"amount":5,`+who+`}`); code != http.StatusForbidden {
		t.Fatalf("a credential without the admin privilege: %d, want 403", code)
	}
	// An amount that is not a whole number is refused where it is read.
	if code, out := post(serviceToken, "k-float", `{"scope":`+scope+`,"amount":1.5,`+who+`}`); code != http.StatusBadRequest {
		t.Fatalf("an amount of 1.5: %d %s, want 400", code, out)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM admin_operations`); n != 0 {
		t.Fatalf("%d operations were recorded for requests that were refused", n)
	}

	// A correction in the books: 500 credits more.
	opID, created := accepted("adjust-4412-a", `{"scope":`+scope+`,"amount":500,`+who+`}`)
	if !created {
		t.Fatal("a new request was not created")
	}
	v := finished(opID)
	if v.Status != "succeeded" || v.Kind != "adjust" || v.Attempts != 1 {
		t.Fatalf("the adjustment came to %+v", v)
	}
	var res struct {
		Applied bool
		Delta   int64
		IdemKey string `json:"idem_key"`
	}
	must(t, json.Unmarshal(v.Result, &res))
	if !res.Applied || res.Delta != 500 || res.IdemKey != "admin-adjustment:"+opID {
		t.Fatalf("the result is %s", v.Result)
	}
	eventually(t, 30*time.Second, "the writer to book the adjustment", func() bool { n, _ := adjustments(); return n == 1 })
	if n, sum := adjustments(); n != 1 || sum != 500 {
		t.Fatalf("%d adjustment rows summing to %d, want 1 summing to 500", n, sum)
	}
	// The ledger row leads back to the operation and to the person.
	if got := scalarString(t, pool, `SELECT ref->>'operation' || ' ' || (ref->>'actor') FROM credit_ledger WHERE realm = $1 AND scope_id = $2 AND operation_type = 'admin_adjustment'`, realm, id); got != opID+" alice@example.com" {
		t.Fatalf("the ledger row's ref is %q, want the operation and the person", got)
	}
	if got := hot(); got != 100500 {
		t.Fatalf("the hot balance is %d, want 100500", got)
	}
	if audit("admin.adjust.requested") != 1 || audit("admin.adjust.succeeded") != 1 {
		t.Fatalf("audit rows: %d requested, %d succeeded, want 1 and 1", audit("admin.adjust.requested"), audit("admin.adjust.succeeded"))
	}

	// The same request again — a client that did not hear the first answer — is the same operation, booked once.
	again, created := accepted("adjust-4412-a", `{`+who+`,"amount":500,"pool":"general","scope":`+scope+`}`)
	if created || again != opID {
		t.Fatalf("a retry made operation %s (created %v) after %s", again, created, opID)
	}
	time.Sleep(3 * time.Second) // time enough for a worker to do it a second time, were it going to
	if n, sum := adjustments(); n != 1 || sum != 500 || hot() != 100500 {
		t.Fatalf("a retry booked again: %d rows summing to %d, hot balance %d", n, sum, hot())
	}
	// A key is not a licence to adjust by another amount.
	if code, out := post(serviceToken, "adjust-4412-a", `{"scope":`+scope+`,"amount":501,`+who+`}`); code != http.StatusConflict {
		t.Fatalf("a key reused for another amount: %d %s, want 409", code, out)
	}

	// A correction the other way, under a new key: a second adjustment, because the operator asked for a second.
	opB, _ := accepted("adjust-4412-b", `{"scope":`+scope+`,"amount":-120,`+who+`}`)
	if bv := finished(opB); bv.Status != "succeeded" {
		t.Fatalf("the second adjustment came to %+v", bv)
	}
	eventually(t, 30*time.Second, "the writer to book the second adjustment", func() bool { n, _ := adjustments(); return n == 2 })
	if n, sum := adjustments(); n != 2 || sum != 380 {
		t.Fatalf("%d adjustment rows summing to %d, want 2 summing to 380", n, sum)
	}
	if got := hot(); got != 100380 {
		t.Fatalf("the hot balance is %d, want 100380", got)
	}
}
