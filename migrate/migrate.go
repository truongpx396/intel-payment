// Package migrate applies the embedded, forward-only migrations and answers one question for
// /readyz: is this database at least at the head this binary was built for (FR-042)?
//
// It is driver-agnostic (database/sql) and knows nothing about the metering or billing tables.
// It records what it applied in schema_migrations together with a checksum, so an edited
// migration is an error rather than a silent divergence between two databases.
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/truongpx396/intel-payment/migrations"
)

// lockKey serialises concurrent migrators (a rolling deploy starts several). It is the ASCII of
// "intelmig"; nothing else in the system takes this advisory lock.
const lockKey int64 = 0x696e74656c6d6967

var (
	// ErrBehind means the database has migrations this binary expects and has not applied.
	ErrBehind = errors.New("migrate: schema is behind head")
	// ErrChecksum means an applied migration no longer matches the SQL in this binary.
	ErrChecksum = errors.New("migrate: applied migration differs from the embedded one")
)

// Migration is one NNNN_name.sql file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string // hex sha256 of SQL exactly as shipped
}

var (
	fileName  = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)
	beginLine = regexp.MustCompile(`(?m)^BEGIN;[ \t]*$`)
)

// Embedded loads the migrations this binary was built with.
func Embedded() ([]Migration, error) { return Load(migrations.FS) }

// Load reads NNNN_name.sql files from fsys. Versions must run 1..N with no gap, and every file
// must be one transaction — BEGIN; … COMMIT; — so that recording the version can join it
// atomically (a crash can never leave a migration applied but unrecorded, or the reverse).
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrate: %q is not NNNN_name.sql", e.Name())
		}
		raw, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
		}
		v, _ := strconv.Atoi(m[1])
		sum := sha256.Sum256(raw)
		mig := Migration{Version: v, Name: m[2], SQL: string(raw), Checksum: hex.EncodeToString(sum[:])}
		if _, err := mig.script(); err != nil {
			return nil, fmt.Errorf("migrate: %s: %w", e.Name(), err)
		}
		out = append(out, mig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migrate: versions must run 1..N without a gap or duplicate; found %04d at position %d", m.Version, i+1)
		}
	}
	return out, nil
}

// script is the migration's SQL with the version record inserted before its COMMIT.
func (m Migration) script() (string, error) {
	body := strings.TrimRight(m.SQL, " \t\r\n")
	if !strings.HasSuffix(body, "COMMIT;") {
		return "", errors.New("must end with COMMIT;")
	}
	body = strings.TrimSuffix(body, "COMMIT;")
	if !beginLine.MatchString(body) {
		return "", errors.New("must contain BEGIN; on its own line")
	}
	// Name and checksum are constrained by fileName / hex, so inlining them is safe.
	return fmt.Sprintf("%s\nINSERT INTO schema_migrations (version, name, checksum) VALUES (%d, '%s', '%s');\nCOMMIT;\n",
		body, m.Version, m.Name, m.Checksum), nil
}

// Status is where a database stands against a set of migrations.
type Status struct {
	Head    int   // highest version this binary knows
	Current int   // highest version applied contiguously
	Pending []int // versions in (Current, Head]
	Ahead   int   // applied versions this binary does not know (a newer release ran first)
}

// Check is read-only and cheap enough for /readyz. It returns ErrBehind (wrapped) when versions are
// pending, ErrChecksum (wrapped) when an applied migration was edited, and a database that is
// AHEAD of this binary is ready: migrations are additive, so the previous binary still runs.
func Check(ctx context.Context, db *sql.DB, ms []Migration) (Status, error) {
	applied, err := readApplied(ctx, db)
	if err != nil {
		return Status{}, err
	}
	return status(ms, applied)
}

func status(ms []Migration, applied map[int]string) (Status, error) {
	st := Status{Head: len(ms)}
	for _, m := range ms {
		sum, ok := applied[m.Version]
		switch {
		case !ok:
			st.Pending = append(st.Pending, m.Version)
		case sum != m.Checksum:
			return st, fmt.Errorf("%w: %04d_%s", ErrChecksum, m.Version, m.Name)
		case len(st.Pending) == 0:
			st.Current = m.Version
		}
	}
	for v := range applied {
		if v > st.Head {
			st.Ahead++
		}
	}
	if len(st.Pending) > 0 {
		return st, fmt.Errorf("%w: at %04d, head is %04d", ErrBehind, st.Current, st.Head)
	}
	return st, nil
}

// readApplied returns version → checksum, or an empty map when the table does not exist yet.
func readApplied(ctx context.Context, db *sql.DB) (map[int]string, error) {
	var reg sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations')::text`).Scan(&reg); err != nil {
		return nil, fmt.Errorf("migrate: probe schema_migrations: %w", err)
	}
	applied := map[int]string{}
	if !reg.Valid {
		return applied, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		applied[v] = sum
	}
	return applied, rows.Err()
}

// Up applies every pending migration in order, each in its own transaction, and returns the ones
// it applied. Concurrent callers queue on an advisory lock; the loser finds nothing pending.
// A failed migration rolls back completely and stops the run — the path forward is a new
// migration, never a partial edit (migrations/README.md).
func Up(ctx context.Context, db *sql.DB, ms []Migration) ([]Migration, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return nil, fmt.Errorf("migrate: take lock: %w", err)
	}
	// The lock is session-scoped: release it even when ctx is already cancelled.
	defer func() {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer     PRIMARY KEY CHECK (version > 0),
		name       text        NOT NULL,
		checksum   text        NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied, err := readAppliedConn(ctx, conn)
	if err != nil {
		return nil, err
	}
	if _, err := status(ms, applied); err != nil && !errors.Is(err, ErrBehind) {
		return nil, err
	}

	var done []Migration
	for _, m := range ms {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		script, err := m.script()
		if err != nil {
			return done, fmt.Errorf("migrate: %04d_%s: %w", m.Version, m.Name, err)
		}
		// No arguments: the driver sends this as one simple-protocol query, so the file's own
		// BEGIN/COMMIT frames the transaction. A failure leaves the session in an aborted
		// transaction, which ROLLBACK clears before the connection returns to the pool.
		if _, err := conn.ExecContext(ctx, script); err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
			return done, fmt.Errorf("migrate: apply %04d_%s: %w", m.Version, m.Name, err)
		}
		done = append(done, m)
	}
	return done, nil
}

func readAppliedConn(ctx context.Context, conn *sql.Conn) (map[int]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("migrate: scan schema_migrations: %w", err)
		}
		applied[v] = sum
	}
	return applied, rows.Err()
}

// Ready adapts Check to the func(context.Context) error a readiness probe wants.
func Ready(db *sql.DB, ms []Migration) func(context.Context) error {
	return func(ctx context.Context) error {
		_, err := Check(ctx, db, ms)
		return err
	}
}
