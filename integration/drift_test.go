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

// R112a, end to end: what reconcile found is read through paymentd, as the realm the credential covers and no other. The run is one the
// worker made at an operator's request; the findings are rows in the table the writer's reconcile writes, for this realm and for another.

func TestAnOperatorReadsWhatReconcileFoundAndOnlyTheirRealmsAccounts(t *testing.T) {
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

	// A full reconcile of shard 2, asked for by an operator, leaves a run behind.
	who := `"actor":"alice@example.com","reason":"INC-4412: the full sweep after the failover"`
	code, out := httpDo(t, c, "POST", base+"/v1/admin/reconcile?realm="+realm, serviceToken, `{"shard":2,"kind":"full",`+who+`}`, "Idempotency-Key", "drift-4412")
	if code != http.StatusAccepted {
		t.Fatalf("POST /v1/admin/reconcile = %d %s", code, out)
	}
	var acc struct{ Operation opView }
	must(t, json.Unmarshal([]byte(out), &acc))
	eventually(t, 30*time.Second, "the reconcile to finish", func() bool {
		code, out := httpDo(t, c, "GET", base+"/v1/admin/operations/"+acc.Operation.ID+"?realm="+realm, serviceToken, "")
		var v opView
		return code == 200 && json.Unmarshal([]byte(out), &v) == nil && v.Status == "succeeded"
	})

	type finding struct {
		Scope  map[string]string
		Pool   string
		Drift  int64
		Healed bool
	}
	type run struct {
		ID            string
		Shard         int
		Kind          string
		Findings      []finding
		FindingsTotal int `json:"findings_total"`
	}
	read := func(token, query string) (int, []run, string) {
		t.Helper()
		code, body := httpDo(t, c, "GET", base+"/v1/admin/drift?realm="+realm+query, token, "")
		var r struct{ Runs []run }
		if code == 200 {
			must(t, json.Unmarshal([]byte(body), &r))
		}
		return code, r.Runs, body
	}

	code, runs, body := read(serviceToken, "&shard=2")
	if code != 200 || len(runs) != 1 || runs[0].Shard != 2 || runs[0].Kind != "full" || runs[0].FindingsTotal != 0 || len(runs[0].Findings) != 0 {
		t.Fatalf("the run the operator asked for: %d %s", code, body)
	}

	// Findings are rows the writer's reconcile writes; here, one for this realm and one for another, in the same run. The reader gives the
	// first and never the second.
	runID := runs[0].ID
	mine := fmt.Sprintf("w-%d", seq.Add(1))
	theirs := fmt.Sprintf("secret-account-%d", seq.Add(1))
	for _, f := range []struct{ realm, id string }{{realm, mine}, {"someone-else", theirs}} {
		_, err := pool.Exec(t.Context(), `INSERT INTO reconcile_findings (run_id, realm, scope_kind, scope_id, pool, outcome, hot_seq, applied_seq, hot_balance, expected_balance, drift, healed, alarmed)
			VALUES ($1::uuid, $2, 'workspace', $3, 'general', 'drift', 9, 9, 900, 1000, -100, true, true)`, runID, f.realm, f.id)
		must(t, err)
	}
	code, runs, body = read(serviceToken, "&shard=2&with_findings=true")
	if code != 200 || len(runs) != 1 || runs[0].FindingsTotal != 1 || len(runs[0].Findings) != 1 {
		t.Fatalf("the run with this realm's finding: %d %s", code, body)
	}
	f := runs[0].Findings[0]
	if f.Scope["id"] != mine || f.Scope["kind"] != "workspace" || f.Pool != "general" || f.Drift != -100 || !f.Healed {
		t.Fatalf("the finding came back as %+v", f)
	}
	if strings.Contains(body, theirs) || strings.Contains(body, "someone-else") {
		t.Fatalf("another realm's account is in the answer: %s", body)
	}

	// A run whose only findings belong to another realm is not this realm's to see when asked for runs with findings.
	var other string
	must(t, pool.QueryRow(t.Context(), `INSERT INTO reconcile_runs (shard, kind, drifted, abs_drift) VALUES (3, 'incremental', 1, 50) RETURNING id::text`).Scan(&other))
	_, err := pool.Exec(t.Context(), `INSERT INTO reconcile_findings (run_id, realm, scope_kind, scope_id, pool, outcome, hot_seq, applied_seq, drift) VALUES ($1::uuid, 'someone-else', 'workspace', $2, 'general', 'drift', 1, 1, 50)`, other, theirs)
	must(t, err)
	if code, runs, body := read(serviceToken, "&shard=3&with_findings=true"); code != 200 || len(runs) != 0 {
		t.Fatalf("a run with only another realm's findings: %d %s, want none", code, body)
	}
	if code, runs, body := read(serviceToken, "&shard=3"); code != 200 || len(runs) != 1 || len(runs[0].Findings) != 0 || strings.Contains(body, theirs) {
		t.Fatalf("shard 3's run without the filter: %d %s, want the run and none of its findings", code, body)
	}

	// Newest first, and the limit is honoured.
	if code, runs, body := read(serviceToken, "&limit=1"); code != 200 || len(runs) != 1 {
		t.Fatalf("limit=1: %d %s", code, body)
	}
	// A credential that can spend and mint cannot read it; a limit nobody allows is refused.
	if code, _, _ := read(readerToken, ""); code != http.StatusForbidden {
		t.Fatalf("a credential without the admin privilege: %d, want 403", code)
	}
	if code, _, body := read(serviceToken, "&limit=0"); code != http.StatusBadRequest {
		t.Fatalf("limit=0: %d %s, want 400", code, body)
	}
}
