//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R111 against PostgreSQL 16: the same contract the in-memory store honours, with a database of its own per case.
func TestPostgresDeadLettersHonourTheContract(t *testing.T) {
	t.Parallel()
	contracts.DeadLetterContract(t, func(tb testing.TB, clock *contracts.FakeClock) ports.DeadLetterStore {
		t := tb.(*testing.T)
		return pgadapter.NewDeadLetters(db(t), clock)
	})
}

func integrityViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23000"
}

// "Parked, never dropped" does not rest on the adapter: the database refuses, whoever asks.
func TestTheDatabaseKeepsEveryDeadLetterWhoeverAsks(t *testing.T) {
	t.Parallel()
	p := db(t)
	ctx := context.Background()
	id := scalar[string](t, p, `INSERT INTO dead_letters (realm, subject, source_id, payload, attempts, last_error)
		VALUES ('acme', 'billing.payment.t1', '1-0', '\x00ff7b', 5, 'boom') RETURNING id::text`)

	if _, err := p.Exec(ctx, `DELETE FROM dead_letters WHERE id::text = $1`, id); !integrityViolation(err) {
		t.Fatalf("deleting a dead letter: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE dead_letters SET payload = '\x00' WHERE id::text = $1`, id); !integrityViolation(err) {
		t.Fatalf("rewriting a parked payload: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE dead_letters SET attempts = 1, last_error = 'fine' WHERE id::text = $1`, id); !integrityViolation(err) {
		t.Fatalf("rewriting what a letter says about its failure: %v", err)
	}
	exec(t, p, `UPDATE dead_letters SET replayed_at = now() WHERE id::text = $1`, id)
	if _, err := p.Exec(ctx, `UPDATE dead_letters SET replayed_at = NULL WHERE id::text = $1`, id); !integrityViolation(err) {
		t.Fatalf("un-replaying a replayed letter: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO dead_letters (realm, subject, source_id, payload) VALUES ('acme', 'billing.payment.t1', '1-0', '\x00')`); err == nil {
		t.Fatal("the same envelope was parked twice")
	}
	if n := scalar[int](t, p, `SELECT count(*) FROM dead_letters`); n != 1 {
		t.Fatalf("%d letters, want 1", n)
	}
}

func TestAMalformedLetterIdIsNotFoundNotAnError(t *testing.T) {
	t.Parallel()
	s := pgadapter.NewDeadLetters(db(t), contracts.NewFakeClock(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)))
	if _, err := s.Get(context.Background(), "not-a-uuid"); err == nil || !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
