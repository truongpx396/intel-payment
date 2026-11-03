//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/entitlement/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R102 (entitlement invariant 5): every override write records who, what, why and when, in the same transaction as the override
// itself, written by the DATABASE's audit trigger — so it holds for a write that never passed through the API. Configuration
// is data, and data can be written by anything that holds a connection.

type auditRow struct {
	Action, Actor, Realm, SubjectKind string
	Reason                            *string
	HadBefore, HadAfter               bool
	AfterReason, AfterActor, AfterKey string
	At                                time.Time
}

func auditOf(t *testing.T, pool *pgxpool.Pool) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT action, actor, realm, coalesce(subject_kind,''), reason, before IS NOT NULL, after IS NOT NULL,
	        coalesce(after->>'reason',''), coalesce(after->>'actor',''), coalesce(coalesce(after, before)->>'feature_key',''), created_at
	        FROM audit_log WHERE subject_kind = 'entitlement_overrides' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Action, &r.Actor, &r.Realm, &r.SubjectKind, &r.Reason, &r.HadBefore, &r.HadAfter, &r.AfterReason, &r.AfterActor, &r.AfterKey, &r.At); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func override(reason string) domain.Grant {
	return domain.Grant{
		Scope: md.Scope{Realm: "t1", Kind: "org", ID: "o1"}, Key: "export", Value: domain.FlagValue(true),
		Source: domain.FromOverride, Reason: reason,
	}
}

func TestAnOverrideWrittenThroughTheStoreIsAuditedWithWhoAndWhy(t *testing.T) {
	t.Parallel()
	pool := db(t)
	store := postgres.New(pool)
	if err := store.PutOverride(context.Background(), override("2026-Q3 enterprise pilot, approved by sales"), "alice"); err != nil {
		t.Fatal(err)
	}
	g := override("renewed for the second quarter")
	g.Value = domain.FlagValue(false)
	if err := store.PutOverride(context.Background(), g, "bob"); err != nil {
		t.Fatal(err)
	}
	rows := auditOf(t, pool)
	if len(rows) != 2 {
		t.Fatalf("two writes, %d audit rows: %+v", len(rows), rows)
	}
	first, second := rows[0], rows[1]
	if first.Action != "config.insert" || first.Actor != "alice" || first.Realm != "t1" || first.AfterKey != "export" ||
		first.Reason == nil || *first.Reason != "2026-Q3 enterprise pilot, approved by sales" || first.HadBefore || !first.HadAfter {
		t.Errorf("the first write's audit row = %+v; want an insert by alice, with her reason and what was written", first)
	}
	if second.Action != "config.update" || second.Actor != "bob" || second.Reason == nil || *second.Reason != "renewed for the second quarter" || !second.HadBefore || !second.HadAfter {
		t.Errorf("the replacement's audit row = %+v; want an update by bob, with before and after", second)
	}
	if !second.At.After(first.At) {
		t.Errorf("the audit rows are not in the order of the writes: %v then %v", first.At, second.At)
	}
}

func TestAnOverrideWrittenAroundTheStoreIsStillAuditedByTheDatabase(t *testing.T) {
	t.Parallel()
	pool := db(t)
	ctx := context.Background()
	start := time.Now().Add(-time.Minute)
	// Straight SQL: no API, no store, no audit context. The row still has to say who and why, and the database still records it.
	exec(t, pool, `INSERT INTO entitlement_overrides (realm, scope_kind, scope_id, feature_key, value_kind, value_bool, reason, actor)
	               VALUES ('t1','org','o1','export','flag',true,'sales promised it on the call','sales-eng')`)
	exec(t, pool, `UPDATE entitlement_overrides SET value_bool = false, reason = 'revoked, contract dispute' WHERE feature_key = 'export'`)
	exec(t, pool, `DELETE FROM entitlement_overrides WHERE feature_key = 'export'`)

	rows := auditOf(t, pool)
	if len(rows) != 3 {
		t.Fatalf("insert, update, delete: %d audit rows: %+v", len(rows), rows)
	}
	for i, want := range []string{"config.insert", "config.update", "config.delete"} {
		r := rows[i]
		if r.Action != want || r.Realm != "t1" || r.AfterKey != "export" {
			t.Errorf("row %d = %+v; want %s on export in t1", i, r, want)
		}
		// With no actor told to the database, the row is attributed to the role that made the write.
		if !strings.HasPrefix(r.Actor, "db:") {
			t.Errorf("row %d actor = %q; want the database role that made the write (db:<role>)", i, r.Actor)
		}
		if r.At.Before(start) {
			t.Errorf("row %d has no sensible time: %v", i, r.At)
		}
	}
	if rows[0].AfterReason != "sales promised it on the call" || rows[0].AfterActor != "sales-eng" {
		t.Errorf("the audit row of the insert lost why and who: %+v", rows[0])
	}
	if rows[1].AfterReason != "revoked, contract dispute" || !rows[1].HadBefore {
		t.Errorf("the audit row of the update lost the new reason or the before image: %+v", rows[1])
	}
	if !rows[2].HadBefore || rows[2].HadAfter {
		t.Errorf("a delete audits what was removed: %+v", rows[2])
	}
	// And the store sees what is left: nothing.
	if os, err := postgres.New(pool).Overrides(ctx, md.Scope{Realm: "t1", Kind: "org", ID: "o1"}); err != nil || len(os) != 0 {
		t.Errorf("Overrides after the delete = %+v, %v", os, err)
	}
}

// The API refuses an override that does not say who and why; the database does too, so that a write that bypasses the API
// cannot be unexplained either. The refusal is the whole statement: nothing is stored and nothing is audited.
func TestTheDatabaseRefusesAnOverrideThatDoesNotSayWhoAndWhy(t *testing.T) {
	t.Parallel()
	pool := db(t)
	ctx := context.Background()
	const insert = `INSERT INTO entitlement_overrides (realm, scope_kind, scope_id, feature_key, value_kind, value_bool, reason, actor)
	                VALUES ('t1','org','o1','export','flag',true,$1,$2)`
	for name, c := range map[string]struct {
		reason, actor any
		code          string
		constraint    string
	}{
		"an empty reason":    {"", "sales-eng", "23514", "entitlement_overrides_reason_not_blank"},
		"a reason of spaces": {"  \t ", "sales-eng", "23514", "entitlement_overrides_reason_not_blank"},
		"an empty actor":     {"pilot", "", "23514", "entitlement_overrides_actor_not_blank"},
		"an actor of spaces": {"pilot", "   ", "23514", "entitlement_overrides_actor_not_blank"},
		"no reason (NULL)":   {nil, "sales-eng", "23502", ""},
		"no actor (NULL)":    {"pilot", nil, "23502", ""},
	} {
		_, err := pool.Exec(ctx, insert, c.reason, c.actor)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != c.code || (c.constraint != "" && pe.ConstraintName != c.constraint) {
			t.Errorf("%s: want SQLSTATE %s %s, got %v", name, c.code, c.constraint, err)
		}
	}
	// Nor can an explained override be made unexplained afterwards.
	exec(t, pool, insert, "pilot", "sales-eng")
	if _, err := pool.Exec(ctx, `UPDATE entitlement_overrides SET reason = ' '`); err == nil {
		t.Error("an override was blanked after the fact")
	}
	if _, err := pool.Exec(ctx, `UPDATE entitlement_overrides SET actor = ''`); err == nil {
		t.Error("an override's actor was blanked after the fact")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlement_overrides`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d overrides stored, %v; want only the explained one", n, err)
	}
	if rows := auditOf(t, pool); len(rows) != 1 {
		t.Errorf("%d audit rows for one stored override: a refused statement left a trace: %+v", len(rows), rows)
	}
}

// The store refuses first, with the domain's words, and writes nothing.
func TestTheStoreRefusesAnUnexplainedOverrideBeforeTheDatabaseHasToAndWritesNothing(t *testing.T) {
	t.Parallel()
	pool := db(t)
	store := postgres.New(pool)
	for name, c := range map[string]struct {
		g     domain.Grant
		actor string
	}{
		"no reason": {override(""), "alice"},
		"no actor":  {override("pilot"), "  "},
	} {
		if err := store.PutOverride(context.Background(), c.g, c.actor); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if rows := auditOf(t, pool); len(rows) != 0 {
		t.Errorf("a refusal left audit rows: %+v", rows)
	}
}
