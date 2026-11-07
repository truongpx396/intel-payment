//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// R112, end to end: an operator asks paymentd, which has no writer, and the worker that holds the sole durable writer does it — recorded
// first with the person and the reason, audited by the database, done once however often it is asked for, and answered from the record.

const readerToken = "integration-reader-token"

type opView struct {
	ID, Kind, Status, Error string
	Result                  json.RawMessage
	Attempts                int
}

func TestAnOperatorAsksAndTheWorkerDoesItRecordedAuditedAndOnceHoweverOftenItIsAsked(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_RECONCILE_INTERVAL", "1h", "PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", "24h")))
	awaitOwning(t, pool, w, 8)
	tokens := "integration:" + auth.HashToken(serviceToken) + ":" + realm + ":record+grant+admin,reader:" + auth.HashToken(readerToken) + ":" + realm + ":record+grant"
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8", "PAYMENT_SERVICE_TOKENS", tokens)))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)

	who := `"actor":"alice@example.com","reason":"INC-4412: the full sweep after the failover"`
	post := func(token, path, key, body string) (int, string) {
		t.Helper()
		hdr := []string{}
		if key != "" {
			hdr = []string{"Idempotency-Key", key}
		}
		return httpDo(t, c, "POST", base+path+"?realm="+realm, token, body, hdr...)
	}
	accepted := func(path, key, body string) (id string, created bool) {
		t.Helper()
		code, out := post(serviceToken, path, key, body)
		if code != http.StatusAccepted {
			t.Fatalf("POST %s = %d %s", path, code, out)
		}
		var r struct {
			Created   bool
			Operation opView
		}
		must(t, json.Unmarshal([]byte(out), &r))
		return r.Operation.ID, r.Created
	}
	finished := func(id string) opView {
		t.Helper()
		var v opView
		eventually(t, 30*time.Second, "operation "+id+" to finish", func() bool {
			code, out := httpDo(t, c, "GET", base+"/v1/admin/operations/"+id+"?realm="+realm, serviceToken, "")
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

	// A credential that can spend and mint cannot operate; nothing is recorded for it.
	if code, _ := post(readerToken, "/v1/admin/reconcile", "k-denied", `{"shard":2,`+who+`}`); code != http.StatusForbidden {
		t.Fatalf("a credential without the admin privilege: %d, want 403", code)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM admin_operations`); n != 0 {
		t.Fatalf("%d operations were recorded for a caller who may not ask", n)
	}

	// A reconcile: the full sweep of a shard where nothing was booked. The scheduled tick would find nothing due and write nothing; an
	// operator who asked for the sweep gets it, and gets the run it left behind.
	body := `{"shard":2,"kind":"full",` + who + `}`
	id, created := accepted("/v1/admin/reconcile", "ticket-4412-a", body)
	if !created {
		t.Fatal("a new request was not created")
	}
	v := finished(id)
	if v.Status != "succeeded" || v.Kind != "reconcile" || v.Attempts != 1 {
		t.Fatalf("the reconcile came to %+v", v)
	}
	runs := func() int {
		return countRows(t, pool, `SELECT count(*) FROM reconcile_runs WHERE shard = 2 AND kind = 'full'`)
	}
	if runs() != 1 {
		t.Fatalf("%d full reconcile runs of shard 2 after the operator's request, want 1", runs())
	}
	if audit("admin.reconcile.requested") != 1 || audit("admin.reconcile.succeeded") != 1 {
		t.Fatalf("audit rows: %d requested, %d succeeded, want 1 and 1", audit("admin.reconcile.requested"), audit("admin.reconcile.succeeded"))
	}

	// The same request again — a client that did not hear the first answer — is the same operation, done once.
	again, created := accepted("/v1/admin/reconcile", "ticket-4412-a", `{"kind":"full","shard":2,`+who+`}`)
	if created || again != id {
		t.Fatalf("a retry made operation %s (created %v) after %s", again, created, id)
	}
	time.Sleep(3 * time.Second) // time enough for a worker to do it a second time, were it going to
	if runs() != 1 || audit("admin.reconcile.requested") != 1 {
		t.Fatalf("a retry was done again: %d runs, %d requests audited", runs(), audit("admin.reconcile.requested"))
	}
	// A key is not a licence to ask for something else.
	if code, out := post(serviceToken, "/v1/admin/reconcile", "ticket-4412-a", `{"shard":3,"kind":"full",`+who+`}`); code != http.StatusConflict {
		t.Fatalf("a key reused for another shard: %d %s, want 409", code, out)
	}

	// A recovery: freeze, drain, bump the generation, unfreeze. The generation is what the hot tier's history is judged by.
	gen := func() int64 {
		return int64(countRows(t, pool, `SELECT gen FROM hot_shards WHERE shard = 1`))
	}
	before := gen()
	rid, _ := accepted("/v1/admin/shards/1/recover", "ticket-4412-b", `{`+who+`}`)
	if rv := finished(rid); rv.Status != "succeeded" {
		t.Fatalf("the recovery came to %+v", rv)
	}
	if after := gen(); after != before+1 {
		t.Fatalf("shard 1 is at generation %d after a recovery, was %d: it bumps once", after, before)
	}
	if frozen := countRows(t, pool, `SELECT count(*) FROM hot_shards WHERE shard = 1 AND frozen`); frozen != 0 {
		t.Fatal("the shard was left frozen")
	}

	// A rehydrate of a scope that has an account: done, and says so.
	sc := fmt.Sprintf(`{"kind":"workspace","id":"w-%d"}`, seq.Add(1))
	grantWhenReady(t, c, base, sc)
	hid, _ := accepted("/v1/admin/rehydrate", "ticket-4412-c", `{"scope":`+sc+`,`+who+`}`)
	if hv := finished(hid); hv.Status != "succeeded" {
		t.Fatalf("the rehydrate came to %+v", hv)
	}

	// A dead letter is replayed once — and only by the realm it belongs to.
	mine := scalarString(t, pool, `INSERT INTO dead_letters (realm, subject, source_id, payload, attempts, last_error)
		VALUES ($1, 'billing.payment.t1', '1-0', '\x7b7d', 5, 'boom') RETURNING id::text`, realm)
	theirs := scalarString(t, pool, `INSERT INTO dead_letters (realm, subject, source_id, payload, attempts, last_error)
		VALUES ('someone-else', 'billing.payment.t2', '2-0', '\x7b7d', 5, 'boom') RETURNING id::text`)
	pid, _ := accepted("/v1/admin/dead-letters/"+mine+"/replay", "ticket-4412-d", `{`+who+`}`)
	if pv := finished(pid); pv.Status != "succeeded" {
		t.Fatalf("the replay came to %+v", pv)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM dead_letters WHERE id::text = $1 AND replayed_at IS NOT NULL`, mine); n != 1 {
		t.Fatal("the letter was not marked replayed")
	}
	xid, _ := accepted("/v1/admin/dead-letters/"+theirs+"/replay", "ticket-4412-e", `{`+who+`}`)
	if xv := finished(xid); xv.Status != "failed" {
		t.Fatalf("another realm's letter was replayed by this realm's operator: %+v", xv)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM dead_letters WHERE id::text = $1 AND replayed_at IS NOT NULL`, theirs); n != 0 {
		t.Fatal("another realm's letter was marked replayed")
	}

	// A drift sweep of an account that does not exist fails where the operator can read why, and is not tried again.
	sid, _ := accepted("/v1/admin/subdrift", "ticket-4412-f", `{"account":"no-such-account",`+who+`}`)
	sv := finished(sid)
	if sv.Status != "failed" || sv.Error == "" {
		t.Fatalf("the sweep of an account that does not exist came to %+v", sv)
	}
	if audit("admin.subdrift.requested") != 1 || audit("admin.subdrift.failed") != 1 {
		t.Fatalf("the failed sweep was audited %d and %d times, want 1 and 1", audit("admin.subdrift.requested"), audit("admin.subdrift.failed"))
	}

	// Another realm's operation is not readable here.
	other := scalarString(t, pool, `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('someone-else', 'recover', '{"shard":1}', 'bob', 'x', 'k-x') RETURNING id::text`)
	if code, _ := httpDo(t, c, "GET", base+"/v1/admin/operations/"+other+"?realm="+realm, serviceToken, ""); code != http.StatusNotFound {
		t.Fatalf("another realm's operation: %d, want 404", code)
	}
}

// scalarString runs a query that returns one text column in one row, such as an INSERT ... RETURNING id.
func scalarString(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	var out string
	must(t, pool.QueryRow(t.Context(), q, args...).Scan(&out))
	return out
}
