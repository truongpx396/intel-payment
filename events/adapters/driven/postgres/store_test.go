//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/events/contracts"
	"github.com/truongpx396/intel-payment/internal/pgtest"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
)

var pg *pgtest.Postgres

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if pg, err = pgtest.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = pg.Terminate(ctx)
	if code == 0 {
		if err := goleak.Find(
			goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func db(t testing.TB) *pgxpool.Pool {
	t.Helper()
	p, err := pgadapter.Connect(context.Background(), pg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// seeder writes endpoints as an operator does: rows, audited by the database.
type seeder struct{ pool *pgxpool.Pool }

func (s seeder) AddEndpoint(ctx context.Context, ep events.Endpoint) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO event_endpoints (realm, url, secret_ref, event_types, active) VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
		ep.Realm, ep.URL, ep.SecretRef, ep.Types, ep.Active).Scan(&id)
	return id, err
}

func (s seeder) SetActive(ctx context.Context, id string, active bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE event_endpoints SET active = $2 WHERE id::text = $1`, id, active)
	return err
}

// R115 against PostgreSQL 16: the contract the in-memory store honours, with a database of its own per case.
func TestPostgresStoreHonoursTheContract(t *testing.T) {
	t.Parallel()
	contracts.StoreContract(t, func(tb testing.TB, clock *contracts.Clock) *contracts.Backend {
		pool := db(tb)
		return &contracts.Backend{Store: postgres.NewStore(pool, clock), Seed: seeder{pool}}
	})
}

func integrity(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23000"
}

// An event never changes, whoever asks; it can be purged, and its deliveries go with it.
func TestTheDatabaseKeepsAnEventAsRecordedAndRecordsASourceOnce(t *testing.T) {
	t.Parallel()
	pool := db(t)
	clock := contracts.NewClock(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	st := postgres.NewStore(pool, clock)
	ctx := context.Background()
	e, created, err := st.Append(ctx, events.Event{Realm: "acme", Type: events.CreditsLow, Data: []byte(`{"a":1}`)}, "src")
	if err != nil || !created {
		t.Fatal(created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbound_events SET type = 'payment.succeeded' WHERE id::text = $1`, e.ID); !integrity(err) {
		t.Fatalf("editing an event: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbound_events SET payload = '{}' WHERE id::text = $1`, e.ID); !integrity(err) {
		t.Fatalf("rewriting an event's data: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO outbound_events (id, realm, type, payload, source) VALUES ('0192f3c4-7b1e-7a2c-9d4e-5f6a7b8c9d0e', 'acme', 'x.y', '{}', 'src')`); err == nil {
		t.Fatal("a source was recorded twice")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO event_endpoints (realm, url, secret_ref, event_types) VALUES ('acme', 'http://hooks.example.com/x', 'env:X', ARRAY['credits.low'])`); err == nil {
		t.Fatal("a plain-http endpoint was stored: only https (and http on localhost for development) is allowed")
	}
}

// Two worker replicas whose clocks disagree by an hour still issue ids in the order events became visible: the cursor of a feed never skips.
func TestTwoReplicasWhoseClocksDisagreeIssueIdsInRecordedOrder(t *testing.T) {
	t.Parallel()
	pool := db(t)
	t0 := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	ahead, behind := contracts.NewClock(t0), contracts.NewClock(t0.Add(-time.Hour))
	a, b := postgres.NewStore(pool, ahead), postgres.NewStore(pool, behind)
	var order []string
	for i := range 40 {
		st := a
		if i%2 == 1 {
			st = b
		}
		e, _, err := st.Append(context.Background(), events.Event{Realm: "acme", Type: events.CreditsLow, Data: []byte(`{}`)}, fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, e.ID)
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("event %d (%s) does not sort after event %d (%s): a replica with a slow clock put an event behind a cursor", i, order[i], i-1, order[i-1])
		}
	}
}
