//go:build integration

package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"
)

// One PostgreSQL 16 for the package; every test creates its own database in it, so tests run in
// parallel without sharing state.
var (
	sharedOnce sync.Once
	sharedCtr  *postgres.PostgresContainer
	sharedDSN  string // admin DSN, database "postgres"
	sharedErr  error
	dbSeq      atomic.Int64
)

func leakOptions() []goleak.Option {
	return []goleak.Option{
		// testcontainers' Ryuk reaper connection outlives the last test by design.
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
	}
}

func stopShared() {
	if sharedCtr != nil {
		_ = testcontainers.TerminateContainer(sharedCtr)
	}
	http.DefaultClient.CloseIdleConnections()
}

func adminDSN(t *testing.T) string {
	t.Helper()
	sharedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sharedCtr, sharedErr = postgres.Run(ctx, "postgres:16-alpine",
			postgres.WithDatabase("postgres"), postgres.WithUsername("payment"), postgres.WithPassword("payment"),
			postgres.BasicWaitStrategies(),
			testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")))
		if sharedErr == nil {
			sharedDSN, sharedErr = sharedCtr.ConnectionString(ctx, "sslmode=disable")
		}
	})
	if sharedErr != nil {
		t.Fatalf("start postgres: %v", sharedErr)
	}
	return sharedDSN
}

// freshDB creates an empty database and returns a pool on it.
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	admin, err := sql.Open("pgx", adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("t%d", dbSeq.Add(1))
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	dsn := sharedDSN[:len(sharedDSN)-len("postgres?sslmode=disable")] + name + "?sslmode=disable"
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUpAppliesEmbeddedSchemaAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := freshDB(t)
	ms, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(ctx, db, ms); !errors.Is(err, ErrBehind) {
		t.Fatalf("an empty database must be behind, got %v", err)
	}
	done, err := Up(ctx, db, ms)
	if err != nil || len(done) != len(ms) {
		t.Fatalf("Up applied %d of %d: %v", len(done), len(ms), err)
	}
	st, err := Check(ctx, db, ms)
	if err != nil || st.Current != st.Head {
		t.Fatalf("after Up: %+v, %v", st, err)
	}
	again, err := Up(ctx, db, ms)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Up must be a no-op, applied %d: %v", len(again), err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'credit_ledger'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("the baseline schema is not there: n=%d err=%v", n, err)
	}
}

func TestConcurrentUpAppliesEachMigrationOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := freshDB(t)
	ms, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	const racers = 4
	var total atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := Up(ctx, db, ms)
			total.Add(int64(len(done)))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := total.Load(); got != int64(len(ms)) {
		t.Fatalf("%d racers applied %d migrations in total, want exactly %d", racers, got, len(ms))
	}
}

func TestEditedMigrationIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := freshDB(t)
	orig := fstest.MapFS{"0001_a.sql": {Data: []byte("BEGIN;\nCREATE TABLE a (id int);\nCOMMIT;\n")}}
	edited := fstest.MapFS{"0001_a.sql": {Data: []byte("BEGIN;\nCREATE TABLE a (id bigint);\nCOMMIT;\n")}}
	o, _ := Load(orig)
	e, _ := Load(edited)
	if _, err := Up(ctx, db, o); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(ctx, db, e); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Check: got %v, want ErrChecksum", err)
	}
	if _, err := Up(ctx, db, e); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Up: got %v, want ErrChecksum", err)
	}
}

func TestFailedMigrationRollsBackAndStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := freshDB(t)
	ms, err := Load(fstest.MapFS{
		"0001_a.sql": {Data: []byte("BEGIN;\nCREATE TABLE a (id int);\nCOMMIT;\n")},
		"0002_b.sql": {Data: []byte("BEGIN;\nCREATE TABLE b (id int);\nSELECT 1/0;\nCOMMIT;\n")},
		"0003_c.sql": {Data: []byte("BEGIN;\nCREATE TABLE c (id int);\nCOMMIT;\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := Up(ctx, db, ms)
	if err == nil || len(done) != 1 {
		t.Fatalf("want 1 applied then an error, got %d, %v", len(done), err)
	}
	st, err := Check(ctx, db, ms)
	if !errors.Is(err, ErrBehind) || st.Current != 1 {
		t.Fatalf("after failure: %+v, %v", st, err)
	}
	var b, c sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('b')::text, to_regclass('c')::text`).Scan(&b, &c); err != nil {
		t.Fatal(err)
	}
	if b.Valid || c.Valid {
		t.Fatalf("the failed migration left tables behind (b=%v c=%v) — it must roll back completely", b, c)
	}
	// The connection returned to the pool must be usable, and the advisory lock free.
	if _, err := Check(ctx, db, ms); !errors.Is(err, ErrBehind) {
		t.Fatalf("pool poisoned after failure: %v", err)
	}
}
