// Package pgtest starts a real PostgreSQL 16 for tests and hands each test its OWN database with
// the shipped schema applied. Every test therefore runs in parallel against the real tables, guards
// and triggers — a schema is verified by applying it (constitution XI), and a store is verified by
// running it against that schema.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver, for the migrator and admin
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/truongpx396/intel-payment/migrate"
)

// Image is the PostgreSQL under test.
const Image = "postgres:16-alpine"

// Postgres is one running server with a migrated template database.
type Postgres struct {
	ctr  *postgres.PostgresContainer
	base string // DSN without a database name, e.g. postgres://u:p@host:port
	seq  atomic.Int64
}

// Run starts the server and prepares a template database with every shipped migration applied.
// Use it from a package's TestMain; Terminate when done.
func Run(ctx context.Context) (*Postgres, error) {
	ctr, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("postgres"), postgres.WithUsername("payment"), postgres.WithPassword("payment"),
		postgres.BasicWaitStrategies())
	if err != nil {
		return nil, fmt.Errorf("pgtest: start postgres: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(ctr)
		return nil, err
	}
	p := &Postgres{ctr: ctr, base: strings.TrimSuffix(strings.SplitN(dsn, "?", 2)[0], "/postgres")}

	admin, err := p.open("postgres")
	if err != nil {
		_ = p.Terminate(ctx)
		return nil, err
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE tpl`); err != nil {
		_ = p.Terminate(ctx)
		return nil, err
	}
	tpl, err := p.open("tpl")
	if err != nil {
		_ = p.Terminate(ctx)
		return nil, err
	}
	defer func() { _ = tpl.Close() }()
	ms, err := migrate.Embedded()
	if err != nil {
		_ = p.Terminate(ctx)
		return nil, err
	}
	if _, err := migrate.Up(ctx, tpl, ms); err != nil {
		_ = p.Terminate(ctx)
		return nil, fmt.Errorf("pgtest: migrate template: %w", err)
	}
	return p, nil
}

// Terminate stops the server.
func (p *Postgres) Terminate(_ context.Context) error {
	return testcontainers.TerminateContainer(p.ctr)
}

func (p *Postgres) open(db string) (*sql.DB, error) {
	return sql.Open("pgx", p.DSN(db))
}

// DSN returns a connection string for a database on this server.
func (p *Postgres) DSN(db string) string { return p.base + "/" + db + "?sslmode=disable" }

// NewDB creates a fresh database from the migrated template (fast: a file copy) and returns its
// DSN. It is dropped with the test.
func (p *Postgres) NewDB(tb testing.TB) string {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := p.open("postgres")
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("t%d_%d", time.Now().UnixNano(), p.seq.Add(1))
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE tpl`); err != nil {
		tb.Fatalf("pgtest: create database: %v", err)
	}
	tb.Cleanup(func() {
		a, err := p.open("postgres")
		if err != nil {
			return
		}
		defer func() { _ = a.Close() }()
		_, _ = a.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})
	return p.DSN(name)
}
