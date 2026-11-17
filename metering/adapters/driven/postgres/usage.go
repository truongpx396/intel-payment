package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Usage is the usage_daily rollup over PostgreSQL. A day is the calendar date in the zone given, computed by the server from the zone
// database, so a zone that skipped a date has no row for it and loses no event.
type Usage struct{ pool *pgxpool.Pool }

var _ ports.UsageStore = (*Usage)(nil)

// NewUsage builds the rollup over a pool.
func NewUsage(pool *pgxpool.Pool) *Usage { return &Usage{pool: pool} }

// freshRows is the rollup of the window, as the ledger says it now. Spend is a priced usage row — the only rows that carry a rate key. (Not
// operation_type: the writer books metered usage under the resource's name, and grants, expiries and transfers under theirs.) created_at is a safe lower bound for partition pruning: a row is booked
// no earlier than it happened, and the day's start less one day covers a zone's offset.
const freshRows = `
	SELECT l.realm, l.scope_kind, l.scope_id, ((COALESCE(l.occurred_at, l.created_at)) AT TIME ZONE $3)::date AS day,
	       COALESCE(l.resource, '') AS dimension, COALESCE(l.actor_id, '') AS actor_id,
	       (-sum(l.delta))::bigint AS credits, COALESCE(sum(l.cost_micros), 0)::bigint AS cost_micros, sum(l.event_count)::bigint AS events
	FROM credit_ledger l
	WHERE l.rate_key IS NOT NULL
	  AND l.created_at >= ($1::date - 1)::timestamp AT TIME ZONE $3
	  AND ((COALESCE(l.occurred_at, l.created_at)) AT TIME ZONE $3)::date BETWEEN $1::date AND $2::date
	GROUP BY 1, 2, 3, 4, 5, 6`

func (u *Usage) Rollup(ctx context.Context, from, to time.Time, loc *time.Location) (int, error) {
	if loc == nil {
		loc = time.UTC
	}
	f, t := domain.DayOf(from, loc), domain.DayOf(to, loc)
	if t < f {
		return 0, fmt.Errorf("%w: the window ends (%s) before it starts (%s)", domain.ErrInvalid, t, f)
	}
	// One snapshot for both statements, so the rows written and the rows deleted are judged against the same ledger.
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Rows already right are not written: ON CONFLICT DO UPDATE ... WHERE writes only where a total differs.
	up, err := tx.Exec(ctx, `
		INSERT INTO usage_daily AS d (realm, scope_kind, scope_id, day, dimension, actor_id, credits, cost_micros, events)
		SELECT * FROM (`+freshRows+`) fresh
		ON CONFLICT (realm, scope_kind, scope_id, day, dimension, actor_id) DO UPDATE
		   SET credits = EXCLUDED.credits, cost_micros = EXCLUDED.cost_micros, events = EXCLUDED.events
		 WHERE (d.credits, d.cost_micros, d.events) IS DISTINCT FROM (EXCLUDED.credits, EXCLUDED.cost_micros, EXCLUDED.events)`, f, t, loc.String())
	if err != nil {
		return 0, fmt.Errorf("postgres: roll up usage: %w", err)
	}
	// A row the ledger no longer supports (a day that held rows once and holds none now) goes.
	del, err := tx.Exec(ctx, `
		DELETE FROM usage_daily d
		WHERE d.day BETWEEN $1::date AND $2::date
		  AND NOT EXISTS (SELECT 1 FROM (`+freshRows+`) f
		                  WHERE f.realm = d.realm AND f.scope_kind = d.scope_kind AND f.scope_id = d.scope_id AND f.day = d.day
		                    AND f.dimension = d.dimension AND f.actor_id = d.actor_id)`, f, t, loc.String())
	if err != nil {
		return 0, fmt.Errorf("postgres: remove unsupported usage rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("postgres: commit the rollup: %w", err)
	}
	return int(up.RowsAffected() + del.RowsAffected()), nil
}

func (u *Usage) Breakdown(ctx context.Context, q domain.BreakdownQuery) ([]domain.BreakdownRow, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	s := q.Scope.Normalized()
	rows, err := u.pool.Query(ctx, `
		SELECT dimension, sum(credits)::bigint, sum(cost_micros)::bigint, sum(events)::bigint
		FROM usage_daily
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND day BETWEEN $4::date AND $5::date AND ($6 OR actor_id = $7)
		GROUP BY dimension ORDER BY sum(credits) DESC, dimension`,
		string(s.Realm), s.Kind, s.ID, q.From, q.To, q.AllMembers, q.Member)
	if err != nil {
		return nil, fmt.Errorf("postgres: breakdown: %w", err)
	}
	defer rows.Close()
	var out []domain.BreakdownRow
	for rows.Next() {
		var r domain.BreakdownRow
		var c int64
		if err := rows.Scan(&r.Dimension, &c, &r.CostMicros, &r.Events); err != nil {
			return nil, fmt.Errorf("postgres: scan breakdown: %w", err)
		}
		r.Credits = domain.Credits(c)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Series is the spend series: the days of one scope between two dates, of one member or of everyone, oldest first. The query is the
// breakdown's and so is the filter, which is the point — the days and the dimensions are the same rows, summed two ways.
func (u *Usage) Series(ctx context.Context, q domain.BreakdownQuery) ([]domain.DailyRow, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	s := q.Scope.Normalized()
	rows, err := u.pool.Query(ctx, `
		SELECT to_char(day, 'YYYY-MM-DD'), sum(credits)::bigint
		FROM usage_daily
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND day BETWEEN $4::date AND $5::date AND ($6 OR actor_id = $7)
		GROUP BY day ORDER BY day`,
		string(s.Realm), s.Kind, s.ID, q.From, q.To, q.AllMembers, q.Member)
	if err != nil {
		return nil, seriesErr(err)
	}
	defer rows.Close()
	var out []domain.DailyRow
	for rows.Next() {
		var r domain.DailyRow
		var c int64
		if err := rows.Scan(&r.Day, &c); err != nil {
			return nil, seriesErr(err)
		}
		r.Credits = domain.Credits(c)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, seriesErr(err)
	}
	return out, nil
}

// seriesErr says what a failed read was. A day whose dimensions add up past a bigint makes sum(...)::bigint raise numeric_value_out_of_range
// (22003); the model calls that ErrUsageOverflow and so does the store, so a caller tells an overflow from an outage by the error, not the text.
func seriesErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "22003" {
		return fmt.Errorf("%w: a day's credits in usage_daily", domain.ErrUsageOverflow)
	}
	return fmt.Errorf("postgres: series: %w", err)
}
