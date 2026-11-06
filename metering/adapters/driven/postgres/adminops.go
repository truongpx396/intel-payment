package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// AdminOps is the queue of operator requests (admin_operations), as a ports.AdminOperationStore. The table does the guarding that must not
// depend on this code: it refuses to let a request change or be deleted and an outcome be altered, and it writes the audit row of every
// request and every outcome in the same transaction. This store adds the queue: one request per (realm, key), claims that no two workers
// share, and a lease that lapses when a worker dies.
type AdminOps struct {
	pool  *pgxpool.Pool
	clock ports.Clock
}

var _ ports.AdminOperationStore = (*AdminOps)(nil)

// NewAdminOps builds the store over a pool. Time is the clock's, so a lease is as testable here as in memory.
func NewAdminOps(pool *pgxpool.Pool, clock ports.Clock) *AdminOps {
	return &AdminOps{pool: pool, clock: clock}
}

const adminOpColumns = `id::text, realm, kind, params, actor, reason, key, via, status, result, COALESCE(error, ''), attempts, created_at, started_at, finished_at`

func scanAdminOp(row pgx.Row) (ports.AdminOperation, error) {
	var o ports.AdminOperation
	var kind, status string
	var params, result []byte
	if err := row.Scan(&o.ID, &o.Realm, &kind, &params, &o.Actor, &o.Reason, &o.Key, &o.Via, &status, &result, &o.Error, &o.Attempts,
		&o.CreatedAt, &o.StartedAt, &o.FinishedAt); err != nil {
		return ports.AdminOperation{}, err
	}
	o.Kind, o.Status = ports.AdminOperationKind(kind), ports.AdminOperationStatus(status)
	o.Params, o.Result = json.RawMessage(params), json.RawMessage(result)
	return o, nil
}

// Submit implements ports.AdminOperationStore. The insert and its audit row are one statement; a retry inserts nothing and reads what is
// there, and is a conflict when what is there was asked for differently.
func (s *AdminOps) Submit(ctx context.Context, op ports.AdminOperation) (ports.AdminOperation, bool, error) {
	if !json.Valid(op.Params) {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: an operation's arguments are JSON", domain.ErrInvalid)
	}
	made, err := scanAdminOp(s.pool.QueryRow(ctx, `INSERT INTO admin_operations (realm, kind, params, actor, reason, key, via, created_at)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8) ON CONFLICT (realm, key) DO NOTHING RETURNING `+adminOpColumns,
		op.Realm, string(op.Kind), string(op.Params), op.Actor, op.Reason, op.Key, op.Via, s.clock.Now()))
	switch {
	case err == nil:
		return made, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return ports.AdminOperation{}, false, fmt.Errorf("postgres: record operator request: %w", err)
	}
	var same bool
	var have ports.AdminOperation
	row := s.pool.QueryRow(ctx, `SELECT `+adminOpColumns+`, (kind = $3 AND actor = $4 AND reason = $5 AND params = $6::jsonb) FROM admin_operations
		WHERE realm = $1 AND key = $2`, op.Realm, op.Key, string(op.Kind), op.Actor, op.Reason, string(op.Params))
	var kind, status string
	var params, result []byte
	if err := row.Scan(&have.ID, &have.Realm, &kind, &params, &have.Actor, &have.Reason, &have.Key, &have.Via, &status, &result, &have.Error, &have.Attempts,
		&have.CreatedAt, &have.StartedAt, &have.FinishedAt, &same); err != nil {
		return ports.AdminOperation{}, false, fmt.Errorf("postgres: read operator request %q: %w", op.Key, err)
	}
	have.Kind, have.Status = ports.AdminOperationKind(kind), ports.AdminOperationStatus(status)
	have.Params, have.Result = json.RawMessage(params), json.RawMessage(result)
	if !same {
		return have, false, fmt.Errorf("%w: key %q was used for a different request", domain.ErrIdemConflict, op.Key)
	}
	return have, false, nil
}

// Get implements ports.AdminOperationStore. The id is compared as text, so one that is not a UUID is not found rather than an error of
// the database's.
func (s *AdminOps) Get(ctx context.Context, realm, id string) (ports.AdminOperation, error) {
	o, err := scanAdminOp(s.pool.QueryRow(ctx, `SELECT `+adminOpColumns+` FROM admin_operations WHERE realm = $1 AND id::text = $2`, realm, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ports.AdminOperation{}, fmt.Errorf("%w: operation %q", domain.ErrNotFound, id)
	case err != nil:
		return ports.AdminOperation{}, fmt.Errorf("postgres: operation %q: %w", id, err)
	}
	return o, nil
}

// Claim implements ports.AdminOperationStore: the oldest request that is pending, or running under a lease that has lapsed, locked
// SKIP LOCKED so that workers claiming at once each get a different one.
func (s *AdminOps) Claim(ctx context.Context, lease time.Duration) (ports.AdminOperation, bool, error) {
	if lease <= 0 {
		return ports.AdminOperation{}, false, fmt.Errorf("%w: a lease is a positive duration, got %v", domain.ErrInvalid, lease)
	}
	now := s.clock.Now()
	o, err := scanAdminOp(s.pool.QueryRow(ctx, `UPDATE admin_operations SET status = 'running', attempts = attempts + 1, started_at = $1, lease_until = $2
		WHERE id = (SELECT id FROM admin_operations WHERE status = 'pending' OR (status = 'running' AND lease_until <= $1)
		            ORDER BY created_at, seq LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+adminOpColumns, now, now.Add(lease)))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ports.AdminOperation{}, false, nil
	case err != nil:
		return ports.AdminOperation{}, false, fmt.Errorf("postgres: claim an operator request: %w", err)
	}
	return o, true, nil
}

// Finish implements ports.AdminOperationStore: the outcome is recorded only by the attempt that holds the claim, and only once.
func (s *AdminOps) Finish(ctx context.Context, id string, attempt int, status ports.AdminOperationStatus, result json.RawMessage, errText string) (bool, error) {
	if status != ports.AdminSucceeded && status != ports.AdminFailed {
		return false, fmt.Errorf("%w: an outcome is succeeded or failed, got %q", domain.ErrInvalid, status)
	}
	var res any
	if len(result) > 0 {
		res = string(result)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE admin_operations SET status = $3, result = $4::jsonb, error = NULLIF($5, ''), finished_at = $6, lease_until = NULL
		WHERE id::text = $1 AND status = 'running' AND attempts = $2`, id, attempt, string(status), res, errText, s.clock.Now())
	if err != nil {
		return false, fmt.Errorf("postgres: record the outcome of operation %q: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Backlog implements ports.AdminOperationStore: what is pending or running under a lapsed lease, and the age of the oldest of them.
func (s *AdminOps) Backlog(ctx context.Context) (ports.AdminBacklog, error) {
	now := s.clock.Now()
	var n int64
	var oldest *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), min(created_at) FROM admin_operations
		WHERE status = 'pending' OR (status = 'running' AND lease_until <= $1)`, now).Scan(&n, &oldest); err != nil {
		return ports.AdminBacklog{}, fmt.Errorf("postgres: operator request backlog: %w", err)
	}
	b := ports.AdminBacklog{Waiting: n}
	if oldest != nil {
		b.Oldest = max(0, now.Sub(*oldest))
	}
	return b, nil
}
