package postgres

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var _ ports.WriterBooks = (*Books)(nil)

// ScopesSince lists a shard's scopes whose watermark changed at or after `since`, in key order,
// resuming after `after`. The (shard, updated_at) index makes the filter a range scan.
func (b *Books) ScopesSince(ctx context.Context, shard domain.Shard, since time.Time, after *domain.Scope, limit int) ([]ports.ScopeState, error) {
	var sinceArg any
	if !since.IsZero() {
		sinceArg = since
	}
	ar, ak, ai := "", "", ""
	if after != nil {
		a := after.Normalized()
		ar, ak, ai = string(a.Realm), a.Kind, a.ID
	}
	rows, err := b.pool.Query(ctx, `
		SELECT realm, scope_kind, scope_id, gen, applied_seq, blocked, updated_at
		FROM account_watermarks
		WHERE shard = $1 AND ($2::timestamptz IS NULL OR updated_at >= $2) AND (realm, scope_kind, scope_id) > ($3, $4, $5)
		ORDER BY realm, scope_kind, scope_id LIMIT $6`, int(shard), sinceArg, ar, ak, ai, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: scopes since: %w", err)
	}
	defer rows.Close()
	var out []ports.ScopeState
	for rows.Next() {
		var s ports.ScopeState
		var realm string
		s.Watermark.Exists = true
		if err := rows.Scan(&realm, &s.Scope.Kind, &s.Scope.ID, &s.Watermark.Gen, &s.Watermark.AppliedSeq, &s.Watermark.Blocked, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan scope: %w", err)
		}
		s.Scope.Realm = domain.Realm(realm)
		out = append(out, s)
	}
	return out, rows.Err()
}

// LastReconcile returns when the shard's last finished reconcile of this kind started.
func (b *Books) LastReconcile(ctx context.Context, shard domain.Shard, kind domain.ReconcileKind) (time.Time, error) {
	var t *time.Time
	err := b.pool.QueryRow(ctx, `SELECT max(started_at) FROM reconcile_runs WHERE shard = $1 AND kind = $2 AND finished_at IS NOT NULL`, int(shard), string(kind)).Scan(&t)
	if err != nil {
		return time.Time{}, fmt.Errorf("postgres: last reconcile: %w", err)
	}
	if t == nil {
		return time.Time{}, nil
	}
	return *t, nil
}

// SaveReconcile records one run and one row per finding, in a single transaction.
func (b *Books) SaveReconcile(ctx context.Context, r ports.ReconcileRecord) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	run := r.Run
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO reconcile_runs (shard, kind, started_at, finished_at, scopes_checked, in_sync, deferred, cold, regressions, drifted, abs_drift)
		VALUES ($1, $2, $3, now(), $4, $5, $6, $7, $8, $9, $10) RETURNING id::text`,
		int(run.Shard), string(run.Kind), r.StartedAt, run.Checked, run.InSync, run.Deferred, run.Cold, run.Regressions, run.Drifted, int64(run.AbsDrift)).Scan(&id)
	if err != nil {
		return fmt.Errorf("postgres: save reconcile run: %w", err)
	}
	for _, f := range run.Findings {
		s := f.Scope.Normalized()
		if _, err := tx.Exec(ctx, `
			INSERT INTO reconcile_findings (run_id, realm, scope_kind, scope_id, pool, outcome, hot_seq, applied_seq, hot_balance, expected_balance, drift, healed, alarmed)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			id, string(s.Realm), s.Kind, s.ID, string(f.Pool.Or()), f.Outcome, f.HotSeq, f.AppliedSeq, int64(f.Hot), int64(f.Expected), int64(f.Drift), f.Healed, f.Alarmed); err != nil {
			return fmt.Errorf("postgres: save reconcile finding: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func scanTransfer(rows pgx.Rows) (ports.TransferRecord, error) {
	var t ports.TransferRecord
	var realm, fk, fi, fp, tk, ti, tp string
	var amt int64
	if err := rows.Scan(&realm, &t.IdemKey, &fk, &fi, &fp, &tk, &ti, &tp, &amt, &t.Status, &t.CreatedAt, &t.SettledAt); err != nil {
		return t, err
	}
	t.Realm, t.Amount = domain.Realm(realm), domain.Credits(amt)
	t.From, t.To = domain.Scope{Realm: t.Realm, Kind: fk, ID: fi}, domain.Scope{Realm: t.Realm, Kind: tk, ID: ti}
	t.FromPool, t.ToPool = domain.Pool(fp), domain.Pool(tp)
	return t, nil
}

// InTransit lists transfers in transit since before olderThan, oldest first.
func (b *Books) InTransit(ctx context.Context, olderThan time.Time, limit int) ([]ports.TransferRecord, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT realm, idem_key, from_kind, from_id, from_pool, to_kind, to_id, to_pool, amount, status, created_at, settled_at
		FROM credit_transfers WHERE status = 'in_transit' AND created_at < $1 ORDER BY created_at LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: in transit: %w", err)
	}
	defer rows.Close()
	var out []ports.TransferRecord
	for rows.Next() {
		t, err := scanTransfer(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan transfer: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OldestInTransit is when the realm's oldest unsettled transfer began; zero if there is none.
func (b *Books) OldestInTransit(ctx context.Context, realm domain.Realm) (time.Time, error) {
	var t *time.Time
	if err := b.pool.QueryRow(ctx, `SELECT min(created_at) FROM credit_transfers WHERE realm = $1 AND status = 'in_transit'`, string(realm.Or())).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("postgres: oldest in transit: %w", err)
	}
	if t == nil {
		return time.Time{}, nil
	}
	return *t, nil
}

// OpenSuspenseOlderThan lists open entries that need a correction and are older than `before`.
func (b *Books) OpenSuspenseOlderThan(ctx context.Context, before time.Time, limit int) ([]ports.Suspense, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, realm, scope_kind, scope_id, pool, delta, reason, gen, seq, idem_key, status, created_at
		FROM credit_suspense
		WHERE status = 'open' AND reason IN ('duplicate','idem_conflict','expiry_trueup') AND created_at < $1
		ORDER BY created_at LIMIT $2`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: open suspense: %w", err)
	}
	defer rows.Close()
	var out []ports.Suspense
	for rows.Next() {
		var s ports.Suspense
		var realm, pool string
		var delta int64
		if err := rows.Scan(&s.ID, &realm, &s.Scope.Kind, &s.Scope.ID, &pool, &delta, &s.Reason, &s.Gen, &s.Seq, &s.IdemKey, &s.Status, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan suspense: %w", err)
		}
		s.Scope.Realm, s.Pool, s.Delta = domain.Realm(realm), domain.Pool(pool), domain.Credits(delta)
		out = append(out, s)
	}
	return out, rows.Err()
}

// SuspenseCounts counts open entries by realm and reason.
func (b *Books) SuspenseCounts(ctx context.Context) ([]ports.SuspenseCount, error) {
	rows, err := b.pool.Query(ctx, `SELECT realm, reason, count(*) FROM credit_suspense WHERE status = 'open' GROUP BY realm, reason`)
	if err != nil {
		return nil, fmt.Errorf("postgres: suspense counts: %w", err)
	}
	defer rows.Close()
	var out []ports.SuspenseCount
	for rows.Next() {
		var c ports.SuspenseCount
		var realm string
		if err := rows.Scan(&realm, &c.Reason, &c.Count); err != nil {
			return nil, fmt.Errorf("postgres: scan suspense count: %w", err)
		}
		c.Realm = domain.Realm(realm)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DueLots lists lots that have expired by now and still hold a remainder, oldest first.
func (b *Books) DueLots(ctx context.Context, now time.Time, limit int) ([]ports.Lot, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, realm, scope_kind, scope_id, pool, granted, remaining, expires_at, source_idem_key, created_at
		FROM credit_lots WHERE expires_at <= $1 AND remaining > 0 AND expired_at IS NULL
		ORDER BY expires_at, id LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: due lots: %w", err)
	}
	defer rows.Close()
	var out []ports.Lot
	for rows.Next() {
		var l ports.Lot
		var realm, pool string
		var granted, remaining int64
		if err := rows.Scan(&l.ID, &realm, &l.Scope.Kind, &l.Scope.ID, &pool, &granted, &remaining, &l.ExpiresAt, &l.SourceKey, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan lot: %w", err)
		}
		l.Scope.Realm, l.Pool, l.Granted, l.Remaining = domain.Realm(realm), domain.Pool(pool), domain.Credits(granted), domain.Credits(remaining)
		out = append(out, l)
	}
	return out, rows.Err()
}

// AuditScope compares, per pool, the booked balance with the checkpoint plus every ledger row after
// it — in one transaction under the scope's lock, so no booking can slip between the reads. With
// advance it moves a pool's checkpoint to its newest row, but only for a pool it just verified.
func (b *Books) AuditScope(ctx context.Context, scope domain.Scope, advance bool) ([]ports.AuditRow, error) {
	scope = scope.Normalized()
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, scope.Tag()); err != nil {
		return nil, fmt.Errorf("postgres: scope lock: %w", err)
	}
	r, k, i := string(scope.Realm), scope.Kind, scope.ID
	rows, err := tx.Query(ctx, `
		SELECT ac.pool, ac.balance, coalesce(cp.balance, 0), cp.through_created_at,
		       coalesce((SELECT sum(l.delta) FROM credit_ledger l
		                 WHERE l.realm = ac.realm AND l.scope_kind = ac.scope_kind AND l.scope_id = ac.scope_id AND l.pool = ac.pool
		                   AND (cp.through_created_at IS NULL OR l.created_at > cp.through_created_at)), 0)::bigint,
		       (SELECT max(l2.created_at) FROM credit_ledger l2
		         WHERE l2.realm = ac.realm AND l2.scope_kind = ac.scope_kind AND l2.scope_id = ac.scope_id AND l2.pool = ac.pool),
		       (SELECT count(*) FROM credit_ledger l3
		         WHERE l3.realm = ac.realm AND l3.scope_kind = ac.scope_kind AND l3.scope_id = ac.scope_id AND l3.pool = ac.pool)
		FROM account_credits ac
		LEFT JOIN ledger_checkpoints cp USING (realm, scope_kind, scope_id, pool)
		WHERE ac.realm = $1 AND ac.scope_kind = $2 AND ac.scope_id = $3 ORDER BY ac.pool`, r, k, i)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit scope: %w", err)
	}
	var out []ports.AuditRow
	var counts []int64
	for rows.Next() {
		var a ports.AuditRow
		var pool string
		var booked, cp, since, n int64
		var through, latest *time.Time
		if err := rows.Scan(&pool, &booked, &cp, &through, &since, &latest, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: scan audit: %w", err)
		}
		a.Pool, a.Booked, a.CheckpointBalance, a.SumSince = domain.Pool(pool), domain.Credits(booked), domain.Credits(cp), domain.Credits(since)
		if through != nil {
			a.CheckpointThrough = *through
		}
		if latest != nil {
			a.LatestRow = *latest
		}
		out = append(out, a)
		counts = append(counts, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if advance {
		for idx := range out {
			a := &out[idx]
			// Only a pool that just proved itself, and only if there is a newer row to prove it to.
			if a.Booked != a.CheckpointBalance+a.SumSince || a.LatestRow.IsZero() || !a.LatestRow.After(a.CheckpointThrough) {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO ledger_checkpoints (realm, scope_kind, scope_id, pool, through_created_at, balance, rows_counted)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (realm, scope_kind, scope_id, pool) DO UPDATE SET
					through_created_at = EXCLUDED.through_created_at, balance = EXCLUDED.balance,
					rows_counted = EXCLUDED.rows_counted, checked_at = now()`,
				r, k, i, string(a.Pool), a.LatestRow, int64(a.Booked), counts[idx]); err != nil {
				return nil, fmt.Errorf("postgres: advance checkpoint: %w", err)
			}
			a.Advanced = true
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: commit: %w", err)
	}
	return out, nil
}

var partitionUpper = regexp.MustCompile(`TO \('([^']+)'\)`)

// DetachableLedgerPartitions names the credit_ledger partitions that lie wholly in the past and
// every row of which is covered by a checkpoint: the only ones that may be detached and archived.
func (b *Books) DetachableLedgerPartitions(ctx context.Context) ([]string, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'credit_ledger' ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list partitions: %w", err)
	}
	type part struct{ name, bound string }
	var parts []part
	for rows.Next() {
		var p part
		if err := rows.Scan(&p.name, &p.bound); err != nil {
			rows.Close()
			return nil, err
		}
		parts = append(parts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range parts {
		m := partitionUpper.FindStringSubmatch(p.bound)
		if m == nil { // the DEFAULT partition is never detachable
			continue
		}
		var upper time.Time
		if err := b.pool.QueryRow(ctx, `SELECT $1::timestamptz`, m[1]).Scan(&upper); err != nil {
			return nil, fmt.Errorf("postgres: partition bound %q: %w", m[1], err)
		}
		var past, covered bool
		if err := b.pool.QueryRow(ctx, `SELECT $1::timestamptz <= now()`, upper).Scan(&past); err != nil {
			return nil, err
		}
		if !past {
			continue
		}
		q := fmt.Sprintf(`SELECT NOT EXISTS (
			SELECT 1 FROM %s l WHERE NOT EXISTS (
				SELECT 1 FROM ledger_checkpoints c
				WHERE c.realm = l.realm AND c.scope_kind = l.scope_kind AND c.scope_id = l.scope_id AND c.pool = l.pool
				  AND c.through_created_at >= $1))`, pgx.Identifier{p.name}.Sanitize())
		if err := b.pool.QueryRow(ctx, q, upper).Scan(&covered); err != nil {
			return nil, fmt.Errorf("postgres: cover check %s: %w", p.name, err)
		}
		if covered {
			out = append(out, p.name)
		}
	}
	return out, nil
}

// DefaultPartitionRows counts rows that landed in a DEFAULT partition: the partition tick is behind.
func (b *Books) DefaultPartitionRows(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []string{"credit_ledger", "usage_idem", "usage_events"} {
		var n int64
		q := fmt.Sprintf(`SELECT count(*) FROM %s`, pgx.Identifier{t + "_default"}.Sanitize())
		if err := b.pool.QueryRow(ctx, q).Scan(&n); err != nil {
			return nil, fmt.Errorf("postgres: default partition %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}
