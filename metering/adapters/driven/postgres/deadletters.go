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

// DeadLetters is the durable parking lot for messages that used their delivery budget (FR-043). (subject, source_id) is unique, so
// parking is idempotent whoever sees the envelope and however often; the database refuses to delete a letter or to rewrite what it
// says about the failure, so "parked, never dropped" does not rest on this code. Time comes from the injected Clock, as everywhere
// else, so a lease can be tested without waiting for it.
type DeadLetters struct {
	pool  *pgxpool.Pool
	clock ports.Clock
}

var _ ports.DeadLetterStore = (*DeadLetters)(nil)

// NewDeadLetters builds the store over a pool.
func NewDeadLetters(pool *pgxpool.Pool, clock ports.Clock) *DeadLetters {
	return &DeadLetters{pool: pool, clock: clock}
}

const deadLetterColumns = `id::text, realm, subject, source_id, payload, attempts, COALESCE(last_error, ''), parked_at, replayed_at`

func scanDeadLetter(row pgx.Row) (ports.DeadLetter, error) {
	var l ports.DeadLetter
	err := row.Scan(&l.ID, &l.Realm, &l.Subject, &l.SourceID, &l.Payload, &l.Attempts, &l.LastError, &l.ParkedAt, &l.ReplayedAt)
	if l.Payload == nil {
		l.Payload = []byte{}
	}
	return l, err
}

// invalidID is a path segment that is not a UUID: a letter that cannot exist.
func invalidID(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "22P02"
}

func notFound(id string) error { return fmt.Errorf("%w: dead letter %s", domain.ErrNotFound, id) }

func (d *DeadLetters) Park(ctx context.Context, l ports.DeadLetter) (ports.DeadLetter, bool, error) {
	row := d.pool.QueryRow(ctx, `
		INSERT INTO dead_letters (realm, subject, source_id, payload, attempts, last_error, parked_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (subject, source_id) DO NOTHING
		RETURNING `+deadLetterColumns,
		l.Realm, l.Subject, l.SourceID, nonNilBytes(l.Payload), l.Attempts, l.LastError, d.clock.Now())
	stored, err := scanDeadLetter(row)
	switch {
	case err == nil:
		return stored, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return ports.DeadLetter{}, false, fmt.Errorf("postgres: park dead letter: %w", err)
	}
	// Parked already — by an earlier delivery, or by a sweeper whose transaction has just committed.
	stored, err = scanDeadLetter(d.pool.QueryRow(ctx,
		`SELECT `+deadLetterColumns+` FROM dead_letters WHERE subject = $1 AND source_id = $2`, l.Subject, l.SourceID))
	if err != nil {
		return ports.DeadLetter{}, false, fmt.Errorf("postgres: read the dead letter already parked: %w", err)
	}
	return stored, false, nil
}

func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func (d *DeadLetters) Get(ctx context.Context, id string) (ports.DeadLetter, error) {
	l, err := scanDeadLetter(d.pool.QueryRow(ctx, `SELECT `+deadLetterColumns+` FROM dead_letters WHERE id::text = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || invalidID(err):
		return ports.DeadLetter{}, notFound(id)
	case err != nil:
		return ports.DeadLetter{}, fmt.Errorf("postgres: read dead letter: %w", err)
	}
	return l, nil
}

func (d *DeadLetters) ClaimReplay(ctx context.Context, id string, lease time.Duration) (ports.DeadLetter, bool, error) {
	now := d.clock.Now()
	l, err := scanDeadLetter(d.pool.QueryRow(ctx, `
		UPDATE dead_letters SET replay_claimed_until = $2
		WHERE id::text = $1 AND replayed_at IS NULL AND (replay_claimed_until IS NULL OR replay_claimed_until <= $3)
		RETURNING `+deadLetterColumns, id, now.Add(lease), now))
	switch {
	case err == nil:
		return l, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return ports.DeadLetter{}, false, fmt.Errorf("postgres: claim dead letter: %w", err)
	}
	held, err := d.Get(ctx, id) // already replayed, or another replay holds it — or there is no such letter
	return held, false, err
}

func (d *DeadLetters) FinishReplay(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `
		UPDATE dead_letters SET replayed_at = COALESCE(replayed_at, $2), replay_claimed_until = NULL WHERE id::text = $1`,
		id, d.clock.Now())
	if err != nil {
		return fmt.Errorf("postgres: finish dead letter replay: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound(id)
	}
	return nil
}

func (d *DeadLetters) ReleaseReplay(ctx context.Context, id string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE dead_letters SET replay_claimed_until = NULL WHERE id::text = $1 AND replayed_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("postgres: release dead letter claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		_, err := d.Get(ctx, id) // a letter that is already replayed has no claim to give back
		return err
	}
	return nil
}

func (d *DeadLetters) Open(ctx context.Context) (map[string]int64, error) {
	rows, err := d.pool.Query(ctx, `SELECT realm, count(*) FILTER (WHERE replayed_at IS NULL) FROM dead_letters GROUP BY realm`)
	if err != nil {
		return nil, fmt.Errorf("postgres: count dead letters: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var realm string
		var n int64
		if err := rows.Scan(&realm, &n); err != nil {
			return nil, fmt.Errorf("postgres: scan dead letter count: %w", err)
		}
		out[realm] = n
	}
	return out, rows.Err()
}
