package postgres

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// Claims is the tick claim (ports.Claimer) as a session advisory lock held on a pooled connection. Taking one writes nothing, so a tick
// with nothing due leaves no trace; and the server drops the lock with the session, so a holder that dies — a killed process, a lost
// connection — loses it, and the next run takes the work over. The lock is on a 64-bit hash of the key in the one-argument key space,
// which is not the space shard ownership lives in (two int4 keys), so the two can never collide.
type Claims struct{ pool *pgxpool.Pool }

var _ ports.Claimer = (*Claims)(nil)

// NewClaims builds the claimer over a pool. Each held claim holds one connection until it is released.
func NewClaims(pool *pgxpool.Pool) *Claims { return &Claims{pool: pool} }

func claimKey(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("intelpay.tick|" + key))
	return int64(h.Sum64()) //nolint:gosec // a bit pattern is what an advisory lock key is
}

func (c *Claims) TryClaim(ctx context.Context, key string) (func(), bool, error) {
	conn, err := c.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: claim %q: %w", key, err)
	}
	k := claimKey(key)
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, k).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("postgres: claim %q: %w", key, err)
	}
	if !ok {
		conn.Release()
		return func() {}, false, nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			rctx := context.WithoutCancel(ctx)
			var unlocked bool
			// If the unlock fails the session is closed instead, so a lock can never leak into the pool and quietly keep a tick from running.
			if err := conn.QueryRow(rctx, `SELECT pg_advisory_unlock($1)`, k).Scan(&unlocked); err != nil || !unlocked {
				_ = conn.Conn().Close(rctx)
			}
			conn.Release()
		})
	}, true, nil
}
