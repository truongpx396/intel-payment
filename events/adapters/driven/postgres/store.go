// Package postgres is the outbound log's durable home: event_endpoints, outbound_events and event_deliveries.
package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// appendLock serialises Append across every process. Ids are issued under it, strictly after the newest id in the log, and the
// transaction commits before the next holder reads that id, so ids sort in the order events became visible and a feed's cursor — an id —
// never skips one that commits behind it. Recording is a few events a second per realm, not the hot path.
const (
	lockClass = 0x494D4F45 // "IMOE"
	lockKey   = 1
)

// Store implements events.Store over PostgreSQL. Time comes from the injected Clock, so a lease can be tested without waiting for it.
type Store struct {
	pool    *pgxpool.Pool
	clock   ports.Clock
	entropy io.Reader
}

var _ events.Store = (*Store)(nil)

// NewStore builds the store over a pool.
func NewStore(pool *pgxpool.Pool, clock ports.Clock) *Store {
	return &Store{pool: pool, clock: clock, entropy: rand.Reader}
}

const eventColumns = `e.id::text, e.realm, e.type, COALESCE(e.scope_kind, ''), COALESCE(e.scope_id, ''), e.payload, e.created_at`

func scanEvent(row pgx.Row) (events.Event, error) {
	var e events.Event
	err := row.Scan(&e.ID, &e.Realm, &e.Type, &e.ScopeKind, &e.ScopeID, &e.Data, &e.CreatedAt)
	return e, err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Store) Append(ctx context.Context, e events.Event, source string) (events.Event, bool, error) {
	if source == "" {
		return events.Event{}, false, fmt.Errorf("%w: an event needs a source", events.ErrInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return events.Event{}, false, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, int32(lockClass), int32(lockKey)); err != nil { //nolint:gosec // constants
		return events.Event{}, false, fmt.Errorf("postgres: lock the log: %w", err)
	}
	if prior, err := scanEvent(tx.QueryRow(ctx, `SELECT `+eventColumns+` FROM outbound_events e WHERE e.source = $1`, source)); err == nil {
		return prior, false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return events.Event{}, false, fmt.Errorf("postgres: look up the source: %w", err)
	}
	var last string
	switch err := tx.QueryRow(ctx, `SELECT id::text FROM outbound_events ORDER BY id DESC LIMIT 1`).Scan(&last); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return events.Event{}, false, fmt.Errorf("postgres: read the newest id: %w", err)
	}
	now := s.clock.Now()
	id, err := events.NextID(last, now, s.entropy)
	if err != nil {
		return events.Event{}, false, err
	}
	payload := e.Data
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	stored, err := scanEvent(tx.QueryRow(ctx, `
		INSERT INTO outbound_events AS e (id, realm, type, scope_kind, scope_id, payload, source, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+eventColumns, id, e.Realm, e.Type, nullable(e.ScopeKind), nullable(e.ScopeID), payload, source, now))
	if err != nil {
		return events.Event{}, false, fmt.Errorf("postgres: append event: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO event_deliveries (event_id, endpoint_id, next_attempt_at, created_at)
		SELECT $1::uuid, ep.id, $3, $3 FROM event_endpoints ep
		WHERE ep.active AND ep.realm = $2 AND $4 = ANY (ep.event_types)`, id, e.Realm, now, e.Type); err != nil {
		return events.Event{}, false, fmt.Errorf("postgres: owe the deliveries: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return events.Event{}, false, fmt.Errorf("postgres: commit the event: %w", err)
	}
	return stored, true, nil
}

func (s *Store) Feed(ctx context.Context, q events.FeedQuery) ([]events.Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+eventColumns+` FROM outbound_events e
		WHERE e.realm = ANY ($1) AND ($2 = '' OR e.id > $2::uuid) AND (cardinality($3::text[]) = 0 OR e.type = ANY ($3))
		ORDER BY e.id LIMIT $4`, q.Realms, q.After, nonNilStrings(q.Types), max(q.Limit, 1))
	if err != nil {
		return nil, fmt.Errorf("postgres: read the feed: %w", err)
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func (s *Store) Claim(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]events.Due, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT d.id FROM event_deliveries d JOIN event_endpoints ep ON ep.id = d.endpoint_id
			WHERE d.status = 'pending' AND d.next_attempt_at <= $1 AND ep.active
			ORDER BY d.next_attempt_at, d.id LIMIT $3
			FOR UPDATE OF d SKIP LOCKED)
		UPDATE event_deliveries d SET next_attempt_at = $2
		FROM due, outbound_events e, event_endpoints ep
		WHERE d.id = due.id AND e.id = d.event_id AND ep.id = d.endpoint_id
		RETURNING d.id::text, d.attempts, d.created_at,
		          `+eventColumns+`, ep.id::text, ep.realm, ep.url, ep.secret_ref, ep.event_types, ep.active`,
		now, now.Add(lease), max(limit, 1))
	if err != nil {
		return nil, fmt.Errorf("postgres: claim deliveries: %w", err)
	}
	defer rows.Close()
	var out []events.Due
	for rows.Next() {
		var d events.Due
		if err := rows.Scan(&d.DeliveryID, &d.Attempts, &d.CreatedAt,
			&d.Event.ID, &d.Event.Realm, &d.Event.Type, &d.Event.ScopeKind, &d.Event.ScopeID, &d.Event.Data, &d.Event.CreatedAt,
			&d.Endpoint.ID, &d.Endpoint.Realm, &d.Endpoint.URL, &d.Endpoint.SecretRef, &d.Endpoint.Types, &d.Endpoint.Active); err != nil {
			return nil, fmt.Errorf("postgres: scan delivery: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// invalidID is a text that is not a UUID: a delivery that cannot exist.
func invalidID(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "22P02"
}

func notFound(id string) error { return fmt.Errorf("%w: delivery %s", events.ErrNotFound, id) }

func (s *Store) settle(ctx context.Context, id, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, append([]any{id}, args...)...)
	switch {
	case invalidID(err):
		return notFound(id)
	case err != nil:
		return fmt.Errorf("postgres: settle delivery: %w", err)
	case tag.RowsAffected() == 0:
		return notFound(id)
	}
	return nil
}

func (s *Store) Delivered(ctx context.Context, id string, status int, at time.Time) error {
	return s.settle(ctx, id, `
		UPDATE event_deliveries SET status = 'delivered', attempts = attempts + 1, last_status = $2, last_error = NULL, delivered_at = $3
		WHERE id::text = $1`, status, at)
}

func (s *Store) Failed(ctx context.Context, id string, f events.Failure) error {
	return s.settle(ctx, id, `
		UPDATE event_deliveries SET attempts = attempts + 1, last_status = NULLIF($2, 0), last_error = $3,
		       status = CASE WHEN $5 THEN 'dead' ELSE 'pending' END,
		       next_attempt_at = CASE WHEN $5 THEN next_attempt_at ELSE $4 END
		WHERE id::text = $1`, f.Status, f.Err, f.Next, f.Dead)
}

func (s *Store) ExpireStale(ctx context.Context, now time.Time) ([]events.Ended, error) {
	rows, err := s.pool.Query(ctx, `
		WITH ended AS (
			UPDATE event_deliveries d SET status = 'dead', last_error = 'the retry window passed'
			FROM outbound_events e
			WHERE e.id = d.event_id AND d.status = 'pending' AND d.created_at <= $1
			RETURNING e.realm, d.endpoint_id::text AS endpoint)
		SELECT realm, endpoint, count(*) FROM ended GROUP BY realm, endpoint`, now.Add(-events.Window))
	if err != nil {
		return nil, fmt.Errorf("postgres: end stale deliveries: %w", err)
	}
	defer rows.Close()
	var out []events.Ended
	for rows.Next() {
		var e events.Ended
		if err := rows.Scan(&e.Realm, &e.Endpoint, &e.N); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Redeliver(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE event_deliveries SET status = 'pending', next_attempt_at = $2 WHERE id::text = $1 AND status = 'dead'`, id, at)
	switch {
	case invalidID(err):
		return notFound(id)
	case err != nil:
		return fmt.Errorf("postgres: re-drive delivery: %w", err)
	case tag.RowsAffected() == 1:
		return nil
	}
	var status string
	switch err := s.pool.QueryRow(ctx, `SELECT status FROM event_deliveries WHERE id::text = $1`, id).Scan(&status); {
	case errors.Is(err, pgx.ErrNoRows):
		return notFound(id)
	case err != nil:
		return fmt.Errorf("postgres: read delivery: %w", err)
	}
	return fmt.Errorf("%w: delivery %s is %s, and only a dead one is re-driven", events.ErrInvalid, id, status)
}

func (s *Store) DeadCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.realm, count(d.id)
		FROM (SELECT realm FROM event_endpoints UNION SELECT realm FROM outbound_events) r
		LEFT JOIN outbound_events e ON e.realm = r.realm
		LEFT JOIN event_deliveries d ON d.event_id = e.id AND d.status = 'dead'
		GROUP BY r.realm`)
	if err != nil {
		return nil, fmt.Errorf("postgres: count dead deliveries: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var realm string
		var n int64
		if err := rows.Scan(&realm, &n); err != nil {
			return nil, err
		}
		out[realm] = n
	}
	return out, rows.Err()
}

func (s *Store) Purge(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM outbound_events e WHERE e.created_at < $1
		AND NOT EXISTS (SELECT 1 FROM event_deliveries d WHERE d.event_id = e.id AND d.status = 'pending')`, before)
	if err != nil {
		return 0, fmt.Errorf("postgres: purge events: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
