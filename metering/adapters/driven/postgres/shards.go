package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Acquire tries to take ownership of a shard's writer role. The lock belongs to the SESSION, so a
// worker that dies releases it with its connection and another takes over (hot-path-consistency.md §3).
func (b *Books) Acquire(ctx context.Context, shard domain.Shard) (ports.Lease, bool, error) {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: acquire connection: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, swClass, int32(shard)).Scan(&got); err != nil { //nolint:gosec // shard < 16384
		conn.Release()
		return nil, false, fmt.Errorf("postgres: try shard lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return &shardLease{conn: conn, shard: shard}, true, nil
}

func (b *Books) HotConfig(ctx context.Context) (int, bool, error) {
	var n int
	err := b.pool.QueryRow(ctx, `SELECT shards FROM hot_config WHERE id`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("postgres: read hot_config: %w", err)
	}
	return n, true, nil
}

func (b *Books) EnsureHotConfig(ctx context.Context, shards int) error {
	if _, err := b.pool.Exec(ctx, `INSERT INTO hot_config (id, shards) VALUES (true, $1) ON CONFLICT (id) DO NOTHING`, shards); err != nil {
		return fmt.Errorf("postgres: record hot_config: %w", err)
	}
	got, _, err := b.HotConfig(ctx)
	if err != nil {
		return err
	}
	if got != shards {
		return fmt.Errorf("%w: configured %d, recorded %d — changing it is a planned reshard", domain.ErrShardCountMismatch, shards, got)
	}
	return nil
}

func (b *Books) EnsureShard(ctx context.Context, sh domain.Shard) (ports.ShardRecord, error) {
	if _, err := b.pool.Exec(ctx, `INSERT INTO hot_shards (shard) VALUES ($1) ON CONFLICT (shard) DO NOTHING`, int(sh)); err != nil {
		return ports.ShardRecord{}, fmt.Errorf("postgres: ensure shard: %w", err)
	}
	return b.shardRecord(ctx, sh)
}

func (b *Books) shardRecord(ctx context.Context, sh domain.Shard) (ports.ShardRecord, error) {
	r := ports.ShardRecord{Shard: sh}
	var reason, node *string
	err := b.pool.QueryRow(ctx, `SELECT gen, frozen, frozen_reason, node_replid FROM hot_shards WHERE shard = $1`, int(sh)).
		Scan(&r.Gen, &r.Frozen, &reason, &node)
	if err != nil {
		return ports.ShardRecord{}, fmt.Errorf("postgres: read shard: %w", err)
	}
	r.Reason, r.NodeReplID = deref(reason), deref(node)
	return r, nil
}

func (b *Books) SetFrozen(ctx context.Context, sh domain.Shard, frozen bool, reason string) error {
	var why any
	if frozen && reason != "" {
		why = reason
	}
	tag, err := b.pool.Exec(ctx, `UPDATE hot_shards SET frozen = $2, frozen_reason = $3, updated_at = now() WHERE shard = $1`, int(sh), frozen, why)
	if err != nil {
		return fmt.Errorf("postgres: set frozen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: shard %d has no hot_shards row (EnsureShard first)", sh)
	}
	return nil
}

func (b *Books) BumpGen(ctx context.Context, sh domain.Shard) (int64, error) {
	var gen int64
	err := b.pool.QueryRow(ctx, `UPDATE hot_shards SET gen = gen + 1, updated_at = now() WHERE shard = $1 RETURNING gen`, int(sh)).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("postgres: shard %d has no hot_shards row (EnsureShard first)", sh)
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: bump gen: %w", err)
	}
	return gen, nil
}

func (b *Books) SetNodeReplID(ctx context.Context, sh domain.Shard, id string) error {
	if _, err := b.pool.Exec(ctx, `UPDATE hot_shards SET node_replid = $2, updated_at = now() WHERE shard = $1`, int(sh), id); err != nil {
		return fmt.Errorf("postgres: set node_replid: %w", err)
	}
	return nil
}

// Booked reads a scope's watermark, booked balances and open suspense in ONE snapshot, so a
// rehydrate (or a reconcile) never combines a balance from before a booking with a watermark from after.
func (b *Books) Booked(ctx context.Context, scope domain.Scope) (ports.BookedState, error) {
	scope = scope.Normalized()
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ports.BookedState{}, fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	r, k, i := string(scope.Realm), scope.Kind, scope.ID

	st := ports.BookedState{Booked: map[domain.Pool]domain.Credits{}, Suspense: map[domain.Pool]domain.Credits{}}
	err = tx.QueryRow(ctx, `SELECT gen, applied_seq, blocked FROM account_watermarks WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`, r, k, i).
		Scan(&st.Watermark.Gen, &st.Watermark.AppliedSeq, &st.Watermark.Blocked)
	switch {
	case err == nil:
		st.Watermark.Exists = true
	case !errors.Is(err, pgx.ErrNoRows):
		return ports.BookedState{}, fmt.Errorf("postgres: read watermark: %w", err)
	}
	if err := scanPools(ctx, tx, `SELECT pool, balance FROM account_credits WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`, st.Booked, r, k, i); err != nil {
		return ports.BookedState{}, err
	}
	if err := scanPools(ctx, tx, `SELECT pool, sum(delta)::bigint FROM credit_suspense WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND status = 'open' GROUP BY pool`, st.Suspense, r, k, i); err != nil {
		return ports.BookedState{}, err
	}
	return st, nil
}

func scanPools(ctx context.Context, tx pgx.Tx, sql string, into map[domain.Pool]domain.Credits, args ...any) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("postgres: read pools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			return fmt.Errorf("postgres: scan pool: %w", err)
		}
		into[domain.Pool(p)] = domain.Credits(n)
	}
	return rows.Err()
}

// ParkDead records an intent that exhausted its retry budget. It is never dropped (FR-043).
func (b *Books) ParkDead(ctx context.Context, d ports.DeadIntent) error {
	s := d.Scope.Normalized()
	raw, err := jsonMarshal(d.Fields)
	if err != nil {
		return fmt.Errorf("postgres: park payload: %w", err)
	}
	_, err = b.pool.Exec(ctx, `
		INSERT INTO credit_outbox_dead (realm, scope_kind, scope_id, op, gen, seq, payload, idem_key, attempts, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		string(s.Realm), s.Kind, s.ID, string(d.Op), d.Gen, d.Seq, raw, d.IdemKey, d.Attempts, d.LastErr)
	if err != nil {
		return fmt.Errorf("postgres: park dead intent: %w", err)
	}
	return nil
}

// shardLease is a session-level advisory lock on one shard, held on a dedicated pooled connection.
type shardLease struct {
	conn  *pgxpool.Conn
	shard domain.Shard
}

func (l *shardLease) Alive(ctx context.Context) bool { return l.conn.Conn().Ping(ctx) == nil }

// Release unlocks and returns the connection. If the unlock fails the session is closed instead, so
// a lock can never leak into the pool and quietly keep a shard from being taken over.
func (l *shardLease) Release(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	var ok bool
	err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1, $2)`, swClass, int32(l.shard)).Scan(&ok) //nolint:gosec // shard < 16384
	if err != nil || !ok {
		_ = l.conn.Conn().Close(ctx) // the pool discards a closed connection; the server drops the lock with the session
	}
	l.conn.Release()
	if err != nil {
		return fmt.Errorf("postgres: release shard lock: %w", err)
	}
	return nil
}
