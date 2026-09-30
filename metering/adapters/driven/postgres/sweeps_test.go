//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func bookOne(t *testing.T, b *pgadapter.Books, s domain.Scope, shard domain.Shard, seqn int64, delta domain.Credits) {
	t.Helper()
	ctx := context.Background()
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		if _, err := tx.Book(ctx, []ports.LedgerRow{{Scope: s, Delta: delta, OperationType: "purchase", IdemKey: fmt.Sprint(seqn), Gen: 1, SeqFrom: seqn, SeqTo: seqn}}); err != nil {
			return err
		}
		return tx.SetWatermark(ctx, ports.Watermark{Gen: 1, AppliedSeq: seqn}, shard)
	}))
}

func TestScopesSinceIsAPerShardRangeScanWithPagination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	var mine []domain.Scope
	for i := 0; i < 5; i++ {
		s := domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w%d", i)}
		mine = append(mine, s)
		bookOne(t, b, s, 3, 1, 10)
	}
	bookOne(t, b, domain.Scope{Realm: "t1", Kind: "workspace", ID: "elsewhere"}, 4, 1, 10)

	all, err := b.ScopesSince(ctx, 3, time.Time{}, nil, 100)
	must(t, err)
	if len(all) != 5 || all[0].Scope.ID != "w0" || !all[0].Watermark.Exists || all[0].Watermark.AppliedSeq != 1 {
		t.Fatalf("only THIS shard's scopes, in key order: %+v", all)
	}
	page1, _ := b.ScopesSince(ctx, 3, time.Time{}, nil, 2)
	page2, _ := b.ScopesSince(ctx, 3, time.Time{}, &page1[1].Scope, 2)
	page3, _ := b.ScopesSince(ctx, 3, time.Time{}, &page2[1].Scope, 2)
	if len(page1) != 2 || len(page2) != 2 || len(page3) != 1 || page3[0].Scope != mine[4] || page2[0].Scope != mine[2] {
		t.Fatalf("keyset pagination must cover every scope exactly once: %v %v %v", page1, page2, page3)
	}

	// Incremental: only what was booked since the last run.
	mid := time.Now()
	time.Sleep(20 * time.Millisecond)
	bookOne(t, b, mine[1], 3, 2, 5)
	recent, _ := b.ScopesSince(ctx, 3, mid, nil, 100)
	if len(recent) != 1 || recent[0].Scope != mine[1] || recent[0].Watermark.AppliedSeq != 2 {
		t.Fatalf("an incremental reconcile costs the shard's activity, not its history: %+v", recent)
	}
}

func TestReconcileRunsAreRecordedWithOneRowPerScopeAndPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	if last, err := b.LastReconcile(ctx, 3, domain.ReconcileIncremental); err != nil || !last.IsZero() {
		t.Fatalf("never reconciled: %v %v", last, err)
	}
	started := time.Now().Add(-time.Second).UTC().Truncate(time.Microsecond)
	s1, s2 := domain.Scope{Realm: "t1", Kind: "workspace", ID: "a"}, domain.Scope{Realm: "t1", Kind: "workspace", ID: "b"}
	run := domain.ReconcileRun{Shard: 3, Kind: domain.ReconcileIncremental, Checked: 2, Drifted: 2, AbsDrift: 200, Findings: []domain.ReconcileFinding{
		{Scope: s1, Pool: "general", Outcome: domain.OutcomeDrift, HotSeq: 5, AppliedSeq: 5, Hot: 110, Expected: 10, Drift: 100, Healed: true, Alarmed: true},
		{Scope: s2, Pool: "general", Outcome: domain.OutcomeDrift, HotSeq: 9, AppliedSeq: 9, Hot: 0, Expected: 100, Drift: -100, Healed: true, Alarmed: true},
	}}
	must(t, b.SaveReconcile(ctx, ports.ReconcileRecord{Run: run, StartedAt: started}))
	if last, _ := b.LastReconcile(ctx, 3, domain.ReconcileIncremental); !last.Equal(started) {
		t.Fatalf("%v vs %v", last, started)
	}
	if last, _ := b.LastReconcile(ctx, 3, domain.ReconcileFull); !last.IsZero() {
		t.Fatal("kinds are tracked separately")
	}
	if last, _ := b.LastReconcile(ctx, 4, domain.ReconcileIncremental); !last.IsZero() {
		t.Fatal("shards are tracked separately")
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM reconcile_findings`); n != 2 {
		t.Fatalf("a +100 and a -100 on two scopes are two findings, never netted: %d", n)
	}
	if abs := scalar[int64](t, p, `SELECT abs_drift FROM reconcile_runs`); abs != 200 {
		t.Fatalf("Σ|drift|, not the net: %d", abs)
	}
}

func TestInTransitAndSuspenseAndLotsAreListedForTheSweeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	from, to := domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}, domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	must(t, b.InTx(ctx, from, func(ctx context.Context, tx ports.BookTx) error {
		return tx.OpenTransfer(ctx, ports.TransferRecord{Realm: "t1", IdemKey: "old", From: from, To: to, Amount: 5})
	}))
	exec(t, p, `UPDATE credit_transfers SET created_at = now() - interval '10 minutes' WHERE idem_key = 'old'`)
	must(t, b.InTx(ctx, from, func(ctx context.Context, tx ports.BookTx) error {
		return tx.OpenTransfer(ctx, ports.TransferRecord{Realm: "t1", IdemKey: "new", From: from, To: to, Amount: 5})
	}))
	stuck, err := b.InTransit(ctx, time.Now().Add(-5*time.Minute), 10)
	must(t, err)
	if len(stuck) != 1 || stuck[0].IdemKey != "old" || stuck[0].To != to || stuck[0].Status != domain.TransferInTransit {
		t.Fatalf("only the transfer older than the redrive threshold: %+v", stuck)
	}
	oldest, _ := b.OldestInTransit(ctx, "t1")
	if time.Since(oldest) < 9*time.Minute {
		t.Fatalf("the age reference is the oldest: %v", oldest)
	}
	if none, _ := b.OldestInTransit(ctx, "other"); !none.IsZero() {
		t.Fatal("realms are separate")
	}

	must(t, b.InTx(ctx, from, func(ctx context.Context, tx ports.BookTx) error {
		for _, r := range []string{ports.SuspenseDuplicate, ports.SuspenseIdemConflict, ports.SuspensePoison, ports.SuspenseExpiryTrueup} {
			if _, err := tx.OpenSuspense(ctx, ports.Suspense{Pool: "general", Delta: -1, Reason: r, Gen: 1, Seq: 1, IdemKey: r}); err != nil {
				return err
			}
		}
		exp := time.Now().Add(-time.Minute)
		future := time.Now().Add(time.Hour)
		if err := tx.OpenLot(ctx, ports.Lot{Pool: "general", Granted: 10, ExpiresAt: &exp, SourceKey: "past"}); err != nil {
			return err
		}
		return tx.OpenLot(ctx, ports.Lot{Pool: "general", Granted: 10, ExpiresAt: &future, SourceKey: "future"})
	}))
	need, err := b.OpenSuspenseOlderThan(ctx, time.Now().Add(time.Minute), 10)
	must(t, err)
	if len(need) != 3 {
		t.Fatalf("a poison entry is resolved by an operator's replay, not a correction: %d", len(need))
	}
	counts, _ := b.SuspenseCounts(ctx)
	var total int64
	for _, c := range counts {
		total += c.Count
	}
	if total != 4 {
		t.Fatalf("%+v", counts)
	}
	due, err := b.DueLots(ctx, time.Now(), 10)
	must(t, err)
	if len(due) != 1 || due[0].SourceKey != "past" || due[0].Remaining != 10 {
		t.Fatalf("%+v", due)
	}
}

func TestDeepAuditProvesBalancesAndOnlyAdvancesWhatItProved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: "a"}
	for i := int64(1); i <= 4; i++ {
		bookOne(t, b, s, 3, i, 10)
	}
	rows, err := b.AuditScope(ctx, s, true)
	must(t, err)
	if len(rows) != 1 || rows[0].Booked != 40 || rows[0].CheckpointBalance != 0 || rows[0].SumSince != 40 || !rows[0].Advanced {
		t.Fatalf("booked 40 = no checkpoint + 40 in the ledger, so it is proved and advances: %+v", rows)
	}
	// After the checkpoint, more bookings: booked == checkpoint + the rows since.
	bookOne(t, b, s, 3, 5, 7)
	rows, _ = b.AuditScope(ctx, s, false)
	if rows[0].Booked != 47 || rows[0].CheckpointBalance != 40 || rows[0].SumSince != 7 || rows[0].Advanced {
		t.Fatalf("%+v", rows[0])
	}
	// Tamper with the running balance (a direct SQL write nobody should make). The audit sees it, and
	// REFUSES to advance a checkpoint over a history it could not prove.
	exec(t, p, `UPDATE account_credits SET balance = balance + 1 WHERE scope_id = 'a'`)
	rows, _ = b.AuditScope(ctx, s, true)
	if rows[0].Booked == rows[0].CheckpointBalance+rows[0].SumSince || rows[0].Advanced {
		t.Fatalf("a disagreement must be visible and must not become a checkpoint: %+v", rows[0])
	}
	if cp := scalar[int64](t, p, `SELECT balance FROM ledger_checkpoints WHERE scope_id = 'a'`); cp != 40 {
		t.Fatalf("the checkpoint stayed where it was proved: %d", cp)
	}
}

func TestOnlyFullyCheckpointedPastPartitionsAreDetachable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	// A partition for a past month, with a row in it.
	exec(t, p, `CREATE TABLE credit_ledger_old PARTITION OF credit_ledger FOR VALUES FROM ('2020-01-01') TO ('2020-02-01')`)
	exec(t, p, `INSERT INTO credit_ledger (realm, scope_kind, scope_id, pool, delta, operation_type, idem_key, gen, seq_from, seq_to, created_at)
		VALUES ('t1','workspace','a','general',10,'purchase','k',1,1,1,'2020-01-15')`)
	got, err := b.DetachableLedgerPartitions(ctx)
	must(t, err)
	for _, n := range got {
		if n == "credit_ledger_old" {
			t.Fatal("a partition whose rows no checkpoint covers must never be detached: the balances behind it would be unprovable")
		}
	}
	exec(t, p, `INSERT INTO ledger_checkpoints (realm, scope_kind, scope_id, pool, through_created_at, balance, rows_counted)
		VALUES ('t1','workspace','a','general','2020-06-01',10,1)`)
	got, _ = b.DetachableLedgerPartitions(ctx)
	found := false
	for _, n := range got {
		if n == "credit_ledger_old" {
			found = true
		}
		if n == "credit_ledger_default" {
			t.Fatal("the DEFAULT partition is never detachable")
		}
	}
	if !found {
		t.Fatalf("covered and in the past: %v", got)
	}
	// The current month is not in the past yet, whatever the checkpoints say.
	for _, n := range got {
		if n == fmt.Sprintf("credit_ledger_y%sm%s", time.Now().UTC().Format("2006"), time.Now().UTC().Format("01")) {
			t.Fatal("the current month's partition is still being written")
		}
	}
}

func TestDefaultPartitionRowsAreCounted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	got, err := b.DefaultPartitionRows(ctx)
	must(t, err)
	if got["credit_ledger"] != 0 || got["usage_idem"] != 0 || got["usage_events"] != 0 {
		t.Fatalf("%v", got)
	}
	exec(t, p, `INSERT INTO credit_ledger (realm, scope_kind, scope_id, pool, delta, operation_type, idem_key, gen, seq_from, seq_to, created_at)
		VALUES ('t1','workspace','a','general',10,'purchase','k',1,1,1,'2001-01-15')`)
	if got, _ = b.DefaultPartitionRows(ctx); got["credit_ledger"] != 1 {
		t.Fatalf("a row in a DEFAULT partition means the partition tick is behind: %v", got)
	}
}
