//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/contracts"
)

// R110 against PostgreSQL 16: the claim is a session advisory lock. Taking one writes nothing; a holder that dies — here, its backend is
// terminated — loses it, because the server drops a session's locks with the session.
func TestPostgresClaimsHonourTheContract(t *testing.T) {
	t.Parallel()
	contracts.ClaimerContract(t, func(tb testing.TB) contracts.ClaimerBackend {
		t := tb.(*testing.T)
		p := db(t)
		return contracts.ClaimerBackend{
			Claims: pgadapter.NewClaims(p),
			Kill: func(string) {
				// The claims' locks are the one-key form (objsubid 1); a shard lease's is the two-key form. pg_locks spans the whole server, and
				// the tests share one: only this test's own database is touched, or its neighbours' holders would die with it.
				_, err := p.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
					WHERE locktype = 'advisory' AND objsubid = 1 AND granted AND pid <> pg_backend_pid()
					  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
				must(t, err)
			},
		}
	})
}

// A claim is a lock and not a record: taking and releasing it leaves nothing in any table, so a tick with nothing due writes nothing.
// (Measured by each table's row count and newest writer, which change the instant a row is written; the statistics views report an idle
// backend's counters ten seconds late.)
func TestTakingAndReleasingAClaimWritesNothing(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	fingerprint := func() string {
		tables, err := p.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition ORDER BY 1`)
		must(t, err)
		var names []string
		for tables.Next() {
			var n string
			must(t, tables.Scan(&n))
			names = append(names, n)
		}
		tables.Close()
		var out string
		for _, n := range names {
			out += n + ":" + scalar[string](t, p, fmt.Sprintf(`SELECT count(*)::text || '/' || coalesce(max(xmin::text::bigint), 0)::text FROM %q`, n)) + " "
		}
		return out
	}
	before := fingerprint()
	c := pgadapter.NewClaims(p)
	for range 20 {
		release, ok, err := c.TryClaim(ctx, "reconcile|3|incremental")
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		release()
	}
	if after := fingerprint(); after != before {
		t.Fatalf("20 claims changed a table:\n%s\n%s", before, after)
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid() AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`); n != 0 {
		t.Fatalf("%d advisory locks are still held after every claim was released", n)
	}
}

// Shard ownership and a tick's claim are different key spaces: a worker holding a shard does not hold up its reconcile tick's claim, and
// the reverse.
func TestAShardLeaseAndATickClaimNeverCollide(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	books := pgadapter.NewBooks(p)
	lease, ok, err := books.Acquire(ctx, 3)
	if err != nil || !ok {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	defer func() { _ = lease.Release(ctx) }()
	release, ok, err := pgadapter.NewClaims(p).TryClaim(ctx, "reconcile|3|incremental")
	if err != nil || !ok {
		t.Fatalf("a tick's claim while the shard is leased: %v %v", ok, err)
	}
	release()
}
