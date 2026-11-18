//go:build integration && hardening

package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
)

// How long it takes to claim an entire backlog, 200 at a time, with nothing else going on (no Redis, no worker, no replay):
// the claim in isolation. "Before" is the statement as Journal.Due ran it until Phase 7 found it reading the whole backlog
// per claim — copied here as a reference, deliberately without the planner setting; "after" is Journal.Due itself. No
// ANALYZE in either, which is the window the claim has to be cheap in. A measurement, not an assertion: run with
// `go test -tags=integration,hardening -run JournalClaimCost -v ./metering/adapters/driven/postgres/`.
func TestJournalClaimCostAgainstTheBacklog(t *testing.T) { //nolint:paralleltest // a measurement: a parallel test would measure itself
	ctx := context.Background()
	const batch = 200
	drain := func(n int, after bool) (claims int, took time.Duration) {
		dsn := pg.NewDB(t)
		p, err := pgadapter.Connect(ctx, dsn)
		must(t, err)
		defer p.Close()
		exec(t, p, `INSERT INTO spend_journal (scope_kind, scope_id, idem_key, fingerprint, amount, rate_card_version, payload, next_attempt_at)
			SELECT 'workspace', 'w-' || g, 'k-' || g, 'fp', 50, 'v1', '{}'::jsonb, now() - interval '1 hour' + g * interval '1 millisecond'
			FROM generate_series(1, $1) g`, n)
		j := pgadapter.NewJournal(p)
		start := time.Now()
		for {
			var got int
			if after {
				due, err := j.Due(ctx, time.Now(), batch)
				must(t, err)
				got = len(due)
			} else {
				rows, err := p.Query(ctx, `
					WITH due AS (
						SELECT id FROM spend_journal
						WHERE status = 'pending' AND next_attempt_at <= $1
						ORDER BY next_attempt_at, created_at
						LIMIT $2
						FOR UPDATE SKIP LOCKED)
					UPDATE spend_journal j SET next_attempt_at = $1 + $3::interval
					FROM due WHERE j.id = due.id
					RETURNING j.id::text`, time.Now(), batch, "30 seconds")
				must(t, err)
				for rows.Next() {
					got++
				}
				must(t, rows.Err())
				rows.Close()
			}
			if got == 0 {
				return claims, time.Since(start)
			}
			claims++
		}
	}
	for _, n := range []int{5_000, 20_000, 50_000} {
		bc, before := drain(n, false)
		ac, after := drain(n, true)
		t.Logf("MEASURED T130 journal-claim backlog=%d claims=%d/%d before=%v after=%v per_claim_before=%v per_claim_after=%v",
			n, bc, ac, before.Round(time.Millisecond), after.Round(time.Millisecond), (before / time.Duration(bc)).Round(10*time.Microsecond), (after / time.Duration(ac)).Round(10*time.Microsecond))
	}
}
