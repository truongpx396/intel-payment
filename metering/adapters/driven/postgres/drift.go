package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Drift reads the reconcile runs the writer left behind. It never writes.
type Drift struct{ pool *pgxpool.Pool }

var _ ports.DriftReader = (*Drift)(nil)

// NewDrift returns a reader over the pool.
func NewDrift(pool *pgxpool.Pool) *Drift { return &Drift{pool: pool} }

// RecentRuns implements ports.DriftReader: the newest runs first, each with the asking realm's findings — the worst first (a regression, which
// froze the shard and carries no drift to rank by, before any drift), capped, and with how many there were. A run's counters are the run's own; a finding of another realm is never read, never listed and never counted: the
// findings are selected by realm in the same statement that counts and caps them.
func (d *Drift) RecentRuns(ctx context.Context, q ports.DriftQuery) ([]ports.DriftRun, error) {
	if q.Realm == "" {
		return nil, fmt.Errorf("%w: realm is required: the findings are a realm's own", domain.ErrInvalid)
	}
	if err := domain.Realm(q.Realm).Validate(); err != nil {
		return nil, err
	}
	if q.Limit < 0 || q.Limit > ports.MaxDriftRuns {
		return nil, fmt.Errorf("%w: limit is from 1 to %d", domain.ErrInvalid, ports.MaxDriftRuns)
	}
	if q.Shard != nil && *q.Shard < 0 {
		return nil, fmt.Errorf("%w: shard is not negative", domain.ErrInvalid)
	}
	limit := q.Limit
	if limit == 0 {
		limit = ports.DefaultDriftRuns
	}
	var shard *int32
	if q.Shard != nil {
		s := int32(min(*q.Shard, 1<<30)) //nolint:gosec // bounded above, and not negative: checked at the top
		shard = &s
	}

	rows, err := d.pool.Query(ctx, `
		SELECT r.id::text, r.shard, r.kind, r.started_at, r.finished_at, r.scopes_checked, r.in_sync, r.deferred, r.cold, r.regressions, r.drifted, r.abs_drift
		FROM reconcile_runs r
		WHERE ($1::int IS NULL OR r.shard = $1)
		  AND (NOT $2::bool OR EXISTS (SELECT 1 FROM reconcile_findings f WHERE f.run_id = r.id AND f.realm = $3))
		ORDER BY r.started_at DESC, r.id DESC
		LIMIT $4`, shard, q.WithFindings, q.Realm, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: read reconcile runs: %w", err)
	}
	defer rows.Close()
	runs := make([]ports.DriftRun, 0, limit)
	index := map[string]int{}
	for rows.Next() {
		var r ports.DriftRun
		var kind string
		var abs int64
		if err := rows.Scan(&r.ID, &r.Shard, &kind, &r.StartedAt, &r.FinishedAt, &r.Checked, &r.InSync, &r.Deferred, &r.Cold, &r.Regressions, &r.Drifted, &abs); err != nil {
			return nil, fmt.Errorf("postgres: read a reconcile run: %w", err)
		}
		r.Kind, r.AbsDrift, r.Findings = domain.ReconcileKind(kind), domain.Credits(abs), []domain.ReconcileFinding{}
		index[r.ID] = len(runs)
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read reconcile runs: %w", err)
	}
	if len(runs) == 0 {
		return runs, nil
	}

	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	frows, err := d.pool.Query(ctx, `
		SELECT run_id, scope_kind, scope_id, pool, outcome, hot_seq, applied_seq, hot_balance, expected_balance, drift, healed, alarmed, total
		FROM (
			SELECT f.run_id::text AS run_id, f.scope_kind, f.scope_id, f.pool, f.outcome, f.hot_seq, f.applied_seq, f.hot_balance, f.expected_balance, f.drift, f.healed, f.alarmed,
			       row_number() OVER (PARTITION BY f.run_id ORDER BY (f.outcome = 'regression') DESC, abs(f.drift) DESC NULLS LAST, f.id) AS rn,
			       count(*) OVER (PARTITION BY f.run_id) AS total
			FROM reconcile_findings f
			WHERE f.run_id::text = ANY($1::text[]) AND f.realm = $2
		) t
		WHERE rn <= $3
		ORDER BY run_id, rn`, ids, q.Realm, ports.MaxDriftFindings)
	if err != nil {
		return nil, fmt.Errorf("postgres: read reconcile findings: %w", err)
	}
	defer frows.Close()
	for frows.Next() {
		var runID, kind, id, pool, outcome string
		var hotSeq, appliedSeq, total int64
		var hot, expected, drift *int64
		var f domain.ReconcileFinding
		if err := frows.Scan(&runID, &kind, &id, &pool, &outcome, &hotSeq, &appliedSeq, &hot, &expected, &drift, &f.Healed, &f.Alarmed, &total); err != nil {
			return nil, fmt.Errorf("postgres: read a reconcile finding: %w", err)
		}
		f.Scope = domain.Scope{Realm: domain.Realm(q.Realm), Kind: kind, ID: id}
		f.Pool, f.Outcome, f.HotSeq, f.AppliedSeq = domain.Pool(pool), outcome, hotSeq, appliedSeq
		f.Hot, f.Expected, f.Drift = credits(hot), credits(expected), credits(drift)
		i := index[runID]
		runs[i].Findings = append(runs[i].Findings, f)
		runs[i].FindingsTotal = int(total)
	}
	if err := frows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read reconcile findings: %w", err)
	}
	return runs, nil
}

// credits reads a nullable BIGINT: a balance the run did not record is zero.
func credits(v *int64) domain.Credits {
	if v == nil {
		return 0
	}
	return domain.Credits(*v)
}
