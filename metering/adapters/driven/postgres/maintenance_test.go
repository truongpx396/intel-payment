//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
)

// R110: partitions are created ahead of need and the usage guard is dropped a day at a time once it is past its window (FR-050). Both are
// idempotent, and both write nothing when there is nothing to do.

func idemPartitions(t *testing.T, p *pgxpool.Pool) []string {
	t.Helper()
	rows, err := p.Query(context.Background(), `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class q ON q.oid = i.inhparent WHERE q.relname = 'usage_idem' ORDER BY 1`)
	must(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		out = append(out, n)
	}
	return out
}

func addIdemPartition(t *testing.T, p *pgxpool.Pool, name, from, to string) {
	t.Helper()
	exec(t, p, fmt.Sprintf(`CREATE TABLE %s PARTITION OF usage_idem FOR VALUES FROM ('%s') TO ('%s')`, name, from, to))
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestPartitionsAreCreatedAheadOfNeedOnceAndIdempotently(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	m := pgadapter.NewMaintenance(p)
	if n, err := m.EnsurePartitions(ctx, 3, 4); err != nil || n != 0 {
		t.Fatalf("a fresh schema already has them: created %d, %v", n, err)
	}
	// One goes missing — a tick that fell behind — and the next tick makes it again, and only it.
	today := time.Now().UTC().AddDate(0, 0, 2).Format("20060102")
	exec(t, p, `DROP TABLE usage_idem_d`+today)
	exec(t, p, `DROP TABLE usage_events_d`+today)
	if n, err := m.EnsurePartitions(ctx, 3, 4); err != nil || n != 2 {
		t.Fatalf("created %d, %v: want exactly the two that were missing", n, err)
	}
	if n, err := m.EnsurePartitions(ctx, 3, 4); err != nil || n != 0 {
		t.Fatalf("a second tick created %d, %v", n, err)
	}
	// Further ahead is made too.
	if n, err := m.EnsurePartitions(ctx, 5, 9); err != nil || n < 2+5 {
		t.Fatalf("asking for more made %d, %v", n, err)
	}
}

func TestOnlyDaysWhollyBeforeTheWindowAreDroppedAndTheDefaultAndStrangersAreNeverTouched(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	m := pgadapter.NewMaintenance(p)
	for _, d := range [][3]string{
		{"usage_idem_d20200101", "2020-01-01", "2020-01-02"}, {"usage_idem_d20200102", "2020-01-02", "2020-01-03"},
		{"usage_idem_d20200103", "2020-01-03", "2020-01-04"}, {"usage_idem_d20200104", "2020-01-04", "2020-01-05"},
	} {
		addIdemPartition(t, p, d[0], d[1], d[2])
	}
	addIdemPartition(t, p, "usage_idem_archive_2019", "2019-01-01", "2019-12-31") // someone's own partition: not named like ours
	exec(t, p, `INSERT INTO usage_idem (realm, scope_kind, scope_id, idem_key, fingerprint, gen, seq, created_at) VALUES ('r','k','i','keep-me','f',1,1,'2020-01-04 12:00:00+00')`)

	// The window ends at noon on the 4th: the 4th still holds instants inside it, so it stays; the 1st to the 3rd go.
	before := time.Date(2020, 1, 4, 12, 0, 0, 0, time.UTC)
	n, err := m.PurgeIdemPartitions(ctx, before)
	if err != nil || n != 3 {
		t.Fatalf("dropped %d, %v: want the three days wholly before the 4th", n, err)
	}
	got := idemPartitions(t, p)
	for _, want := range []string{"usage_idem_d20200104", "usage_idem_default", "usage_idem_archive_2019"} {
		if !contains(got, want) {
			t.Errorf("%s was dropped: %v", want, got)
		}
	}
	for _, gone := range []string{"usage_idem_d20200101", "usage_idem_d20200102", "usage_idem_d20200103"} {
		if contains(got, gone) {
			t.Errorf("%s survives past the window", gone)
		}
	}
	if k := scalar[int](t, p, `SELECT count(*) FROM usage_idem WHERE idem_key = 'keep-me'`); k != 1 {
		t.Fatalf("a guard inside the window was lost")
	}
	// The recent days the migration made are untouched, and a second tick has nothing to do.
	if !contains(got, "usage_idem_d"+time.Now().UTC().Format("20060102")) {
		t.Errorf("today's partition was dropped: %v", got)
	}
	if n, err := m.PurgeIdemPartitions(ctx, before); err != nil || n != 0 {
		t.Fatalf("a second tick dropped %d, %v", n, err)
	}
}

func TestPurgingWithNothingOldWritesNothing(t *testing.T) {
	t.Parallel()
	p := db(t)
	m := pgadapter.NewMaintenance(p)
	ctx := context.Background()
	// Partition DDL is not row writes, so the catalogue is compared too: nothing was created and nothing dropped.
	state := func() string {
		return scalar[string](t, p, `SELECT string_agg(c.relname || ':' || c.oid::text, ',' ORDER BY c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public'`)
	}
	before := state()
	if n, err := m.PurgeIdemPartitions(ctx, time.Now().Add(-72*time.Hour)); err != nil || n != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if n, err := m.EnsurePartitions(ctx, 3, 4); err != nil || n != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if after := state(); after != before {
		t.Fatal("an idle maintenance created or dropped a relation")
	}
}
