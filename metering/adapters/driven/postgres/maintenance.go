package postgres

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Maintenance keeps the partitioned tables bounded (FR-050): partitions are created ahead of need, so a late tick can never reject a
// charge, and the usage dedup guard is dropped a day at a time once it is past its window. Both are DDL, and both write nothing when
// there is nothing to do.
type Maintenance struct{ pool *pgxpool.Pool }

// NewMaintenance builds the maintenance over a pool.
func NewMaintenance(pool *pgxpool.Pool) *Maintenance { return &Maintenance{pool: pool} }

// EnsurePartitions creates the ledger's monthly partitions for this month and monthsAhead more, and the usage guard's and the usage
// events' daily ones for today and daysAhead more. It returns how many it created; zero when every one existed, which is the common
// case and changes nothing.
func (m *Maintenance) EnsurePartitions(ctx context.Context, monthsAhead, daysAhead int) (int, error) {
	var created int
	err := m.pool.QueryRow(ctx, `
		SELECT ensure_monthly_partitions('credit_ledger', $1) + ensure_daily_partitions('usage_idem', $2) + ensure_daily_partitions('usage_events', $2)`,
		monthsAhead, daysAhead).Scan(&created)
	if err != nil {
		return 0, fmt.Errorf("postgres: ensure partitions: %w", err)
	}
	return created, nil
}

var idemPartition = regexp.MustCompile(`^usage_idem_d(\d{4})(\d{2})(\d{2})$`)

// PurgeIdemPartitions drops the daily partitions of the usage dedup guard that lie wholly before `before` (now minus UsageIdemWindow). A
// day that holds any instant inside the window is kept: Record refuses an event older than the window (`422 stale_event`), so a guard
// older than that can never be asked, and dropping it a day at a time is how the table stays bounded. The DEFAULT partition is never
// touched. Each drop waits for its lock for two seconds at most and is tried again by the next tick, so it can never stall the writer.
func (m *Maintenance) PurgeIdemPartitions(ctx context.Context, before time.Time) (int, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'usage_idem' ORDER BY c.relname`)
	if err != nil {
		return 0, fmt.Errorf("postgres: list usage guard partitions: %w", err)
	}
	var old []string
	cutoff := before.UTC().Truncate(24 * time.Hour)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		g := idemPartition.FindStringSubmatch(name)
		if g == nil {
			continue // the default partition, or one nobody made by ensure_daily_partitions: not ours to drop
		}
		day, err := time.Parse("20060102", g[1]+g[2]+g[3])
		if err == nil && day.Before(cutoff) {
			old = append(old, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	dropped := 0
	for _, name := range old {
		if err := m.drop(ctx, name); err != nil {
			return dropped, err
		}
		dropped++
	}
	return dropped, nil
}

func (m *Maintenance) drop(ctx context.Context, name string) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return fmt.Errorf("postgres: lock timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `DROP TABLE IF EXISTS `+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("postgres: drop %s: %w", name, err)
	}
	return tx.Commit(ctx)
}
