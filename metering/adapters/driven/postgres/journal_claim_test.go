//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
)

// The journal's claim (Journal.Due) runs on every replay tick, up to twenty times a tick, for as long as a backlog of
// deferred usage takes to drain after an outage. Its cost therefore has to be the BATCH it claims, never the backlog
// behind it: a claim that reads and sorts every due row to hand back 200 makes the drain slower the longer the outage was.
// (Found by the Phase 7 recovery rehearsal: at a 12 581-entry backlog the plan was a Bitmap Heap Scan of 12 581 rows,
// a Sort, then a Limit of 200.)
//
// The test runs the real Due, on a pool of its own, and asks Postgres how many entries of the pending index that one
// claim read. The pool is then closed because an idle backend reports its statistics only every ten seconds, while one
// that exits reports at once.
func TestClaimingABatchOfTheJournalReadsTheBatchAndNotTheBacklog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pg.NewDB(t)
	seed, err := pgadapter.Connect(ctx, dsn)
	must(t, err)
	t.Cleanup(seed.Close)

	const backlog, batch = 20_000, 200
	// Every entry is due, each a millisecond after the one before: entry k-1 is the oldest.
	exec(t, seed, `INSERT INTO spend_journal (scope_kind, scope_id, idem_key, fingerprint, amount, rate_card_version, payload, next_attempt_at)
		SELECT 'workspace', 'w-' || g, 'k-' || g, 'fp', 50, 'v1', '{}'::jsonb, now() - interval '1 hour' + g * interval '1 millisecond'
		FROM generate_series(1, $1) g`, backlog)
	// Deliberately NO ANALYZE. A backlog forms in seconds, and autovacuum's statistics lag it by up to a minute, so the
	// planner is working from a table it believes is nearly empty exactly when the claim has to be cheap. (With fresh
	// statistics the existing index already gets an ordered scan; the trouble is the window in which they are stale.)

	claim, err := pgadapter.Connect(ctx, dsn)
	must(t, err)
	due, err := pgadapter.NewJournal(claim).Due(ctx, time.Now(), batch)
	must(t, err)
	claim.Close()

	if len(due) != batch {
		t.Fatalf("a claim of %d entries from a backlog of %d returned %d", batch, backlog, len(due))
	}
	got := map[string]bool{}
	for _, e := range due {
		got[e.Charge.IdemKey] = true
	}
	for k := 1; k <= batch; k++ {
		if !got[fmt.Sprintf("k-%d", k)] {
			t.Fatalf("the claim is not the %d OLDEST due entries: k-%d is missing", batch, k)
		}
	}

	var read, last int64 = 0, -1
	eventually(t, 15*time.Second, "the claim's reads of the pending index to be reported", func() bool {
		read = scalar[int64](t, seed, `SELECT coalesce(sum(idx_tup_read), 0)::bigint FROM pg_stat_user_indexes WHERE indexrelname = 'spend_journal_pending_idx'`)
		stable := read > 0 && read == last
		last = read
		return stable
	})
	if read > 5*batch {
		t.Fatalf("claiming %d entries read %d entries of the pending index (the backlog is %d): the claim costs the backlog, not the batch", batch, read, backlog)
	}
}
