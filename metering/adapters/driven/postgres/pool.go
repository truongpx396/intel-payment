package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pgx pool on the ledger DSN and verifies it can reach the server. The pool is the
// caller's to Close.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return p, nil
}
