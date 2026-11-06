//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112 against PostgreSQL 16: the same contract the in-memory queue honours, with a database of its own per case.
func TestPostgresAdminOpsHonourTheContract(t *testing.T) {
	t.Parallel()
	contracts.AdminOpContract(t, func(tb testing.TB, clock *contracts.FakeClock) ports.AdminOperationStore {
		t := tb.(*testing.T)
		return pgadapter.NewAdminOps(db(t), clock)
	})
}

func adminReq(key string) ports.AdminOperation {
	return ports.AdminOperation{
		Realm: "acme", Kind: ports.AdminRecover, Params: json.RawMessage(`{"shard":2}`),
		Actor: "alice@example.com", Reason: "INC-4412: the failover took the shard", Key: key, Via: "ops-console",
	}
}

// What was asked, by whom and why is the record of a privileged action, so the database refuses to let it change — whoever holds the
// connection. An outcome is final.
func TestTheDatabaseKeepsWhatWasAskedAndWhoAskedAndAnOutcomeIsFinal(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	s := pgadapter.NewAdminOps(p, clock)
	op, _, err := s.Submit(ctx, adminReq("k-1"))
	must(t, err)

	for _, change := range []string{
		`actor = 'mallory'`, `reason = 'nothing to see'`, `params = '{"shard":3}'::jsonb`, `kind = 'reconcile'`, `key = 'k-2'`,
		`realm = 'other'`, `via = 'someone else'`, `created_at = now()`,
	} {
		if _, err := p.Exec(ctx, `UPDATE admin_operations SET `+change+` WHERE id::text = $1`, op.ID); !integrityViolation(err) {
			t.Errorf("rewriting the request with %s: %v", change, err)
		}
	}
	if _, err := p.Exec(ctx, `DELETE FROM admin_operations WHERE id::text = $1`, op.ID); !integrityViolation(err) {
		t.Fatalf("deleting an operation: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE admin_operations SET status = 'succeeded' WHERE id::text = $1`, op.ID); err != nil {
		t.Fatalf("the worker's own transition was refused: %v", err)
	}
	for _, change := range []string{`status = 'pending'`, `status = 'running'`, `status = 'failed'`, `result = '{"x":1}'::jsonb`, `error = 'after the fact'`} {
		if _, err := p.Exec(ctx, `UPDATE admin_operations SET `+change+` WHERE id::text = $1`, op.ID); !integrityViolation(err) {
			t.Errorf("changing a finished operation with %s: %v", change, err)
		}
	}
	for name, ddl := range map[string]string{
		"no person":         `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'recover', '{}', '', 'why', 'k-9')`,
		"a blank person":    `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'recover', '{}', '  ', 'why', 'k-9')`,
		"no reason":         `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'recover', '{}', 'bob', '', 'k-9')`,
		"a kind nobody has": `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'burn', '{}', 'bob', 'why', 'k-9')`,
		"a key reused":      `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'recover', '{}', 'bob', 'why', 'k-1')`,
	} {
		if _, err := p.Exec(ctx, ddl); err == nil {
			t.Errorf("%s: the database accepted the row", name)
		}
	}
}

// Every request and every outcome is written to audit_log by the database in the transaction that made it, so a write that never went
// through the API is audited too, and a transaction that does not commit leaves no audit row.
func TestEveryRequestAndEveryOutcomeIsAuditedByTheDatabaseInTheSameTransaction(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	s := pgadapter.NewAdminOps(p, clock)

	op, created, err := s.Submit(ctx, adminReq("k-1"))
	must(t, err)
	if _, again, err := s.Submit(ctx, adminReq("k-1")); err != nil || again {
		t.Fatalf("a retry: %v, %v", again, err)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM audit_log WHERE subject_kind = 'admin_operations'`); !created || n != 1 {
		t.Fatalf("%d audit rows after a request and its retry, want one: a retry is not a second action", n)
	}
	var actor, action, reason, subject, realm string
	must(t, p.QueryRow(ctx, `SELECT actor, action, reason, subject_id, realm FROM audit_log WHERE subject_kind = 'admin_operations'`).Scan(&actor, &action, &reason, &subject, &realm))
	if actor != "alice@example.com" || action != "admin.recover.requested" || reason != "INC-4412: the failover took the shard" || subject != op.ID || realm != "acme" {
		t.Fatalf("audited as actor %q action %q reason %q subject %q realm %q", actor, action, reason, subject, realm)
	}

	claimed, ok, err := s.Claim(ctx, time.Minute)
	must(t, err)
	if !ok {
		t.Fatal("nothing to claim")
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM audit_log WHERE subject_kind = 'admin_operations'`); n != 1 {
		t.Fatalf("a claim is bookkeeping and was audited: %d rows", n)
	}
	done, err := s.Finish(ctx, claimed.ID, claimed.Attempts, ports.AdminFailed, nil, "the shard is not ours")
	must(t, err)
	if !done {
		t.Fatal("Finish refused the claimant")
	}
	var after []byte
	must(t, p.QueryRow(ctx, `SELECT actor, action, after FROM audit_log WHERE subject_kind = 'admin_operations' AND action = 'admin.recover.failed'`).Scan(&actor, &action, &after))
	if actor != "alice@example.com" || !json.Valid(after) {
		t.Fatalf("the outcome was audited as actor %q, after %s", actor, after)
	}
	var got struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	must(t, json.Unmarshal(after, &got))
	if got.Status != "failed" || got.Error != "the shard is not ours" {
		t.Fatalf("the audited outcome says %+v", got)
	}

	// A write that does not go through the adapter is audited all the same, attributed to the person it names.
	exec(t, p, `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'reconcile', '{"shard":1}', 'dba@example.com', 'by hand', 'k-sql')`)
	if n := scalar[int](t, p, `SELECT count(*) FROM audit_log WHERE actor = 'dba@example.com' AND action = 'admin.reconcile.requested'`); n != 1 {
		t.Fatalf("a request written in SQL was audited %d times", n)
	}

	// ...and the audit row is part of the same transaction: roll it back and neither exists.
	tx, err := p.Begin(ctx)
	must(t, err)
	if _, err := tx.Exec(ctx, `INSERT INTO admin_operations (realm, kind, params, actor, reason, key) VALUES ('acme', 'reconcile', '{"shard":2}', 'eve@example.com', 'x', 'k-rb')`); err != nil {
		t.Fatal(err)
	}
	must(t, tx.Rollback(ctx))
	if n := scalar[int](t, p, `SELECT count(*) FROM audit_log WHERE actor = 'eve@example.com'`); n != 0 {
		t.Fatalf("a transaction that did not commit left %d audit rows", n)
	}
}

// A key reused for a different request is a conflict the caller can tell from a database failure.
func TestAReusedKeyIsAConflictAndNotADatabaseError(t *testing.T) {
	t.Parallel()
	p := db(t)
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	s := pgadapter.NewAdminOps(p, clock)
	_, _, err := s.Submit(context.Background(), adminReq("k-1"))
	must(t, err)
	other := adminReq("k-1")
	other.Params = json.RawMessage(`{"shard":5}`)
	if _, _, err := s.Submit(context.Background(), other); !errors.Is(err, domain.ErrIdemConflict) {
		t.Fatalf("err = %v, want %v", err, domain.ErrIdemConflict)
	}
}

// The queue is read in the order it was made even when two requests arrive in the same instant: the tie is broken by the order of creation
// (an identity column; a random id has no order) and not left to the planner.
func TestRequestsMadeInTheSameInstantAreClaimedInTheOrderTheyWereMade(t *testing.T) {
	t.Parallel()
	p := db(t)
	clock := contracts.NewFakeClock(time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC))
	s := pgadapter.NewAdminOps(p, clock)
	var made []string
	for _, k := range []string{"a", "b", "c", "d"} {
		op, _, err := s.Submit(context.Background(), adminReq(k))
		must(t, err)
		made = append(made, op.ID)
	}
	for i, want := range made {
		got, ok, err := s.Claim(context.Background(), time.Minute)
		must(t, err)
		if !ok || got.ID != want {
			t.Fatalf("claim %d: %s (ok %v), want %s", i+1, got.ID, ok, want)
		}
	}
}
