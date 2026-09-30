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

// Books is the writer's Postgres: the account of record and everything that keeps it consistent
// with the hot tier. Only the LedgerWriter uses InTx; the request tier reads Booked for a rehydrate.
type Books struct{ pool *pgxpool.Pool }

var _ ports.BookStore = (*Books)(nil)

// NewBooks builds the store over a pool.
func NewBooks(pool *pgxpool.Pool) *Books { return &Books{pool: pool} }

// swClass is the advisory-lock class of shard writer ownership (the ASCII of "IMSW"). Per-scope
// transaction locks use a different, single-key form, so the two can never collide.
const swClass int32 = 0x494D5357

// InTx runs fn in one transaction holding the scope's advisory lock. The lock is what makes a
// booking safe when two writers briefly overlap during a shard takeover: whichever takes the lock
// second sees the first's committed watermark and idempotency rows.
func (b *Books) InTx(ctx context.Context, scope domain.Scope, fn func(ctx context.Context, tx ports.BookTx) error) error {
	scope = scope.Normalized()
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, scope.Tag()); err != nil {
		return fmt.Errorf("postgres: scope lock: %w", err)
	}
	if err := fn(ctx, &bookTx{tx: tx, scope: scope}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

type bookTx struct {
	tx    pgx.Tx
	scope domain.Scope
}

var _ ports.BookTx = (*bookTx)(nil)

func (t *bookTx) key() (string, string, string) {
	return string(t.scope.Realm), t.scope.Kind, t.scope.ID
}

func (t *bookTx) Watermark(ctx context.Context) (ports.Watermark, error) {
	r, k, i := t.key()
	w := ports.Watermark{Exists: true}
	err := t.tx.QueryRow(ctx, `
		SELECT gen, applied_seq, blocked FROM account_watermarks
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`, r, k, i).Scan(&w.Gen, &w.AppliedSeq, &w.Blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.Watermark{}, nil
	}
	if err != nil {
		return ports.Watermark{}, fmt.Errorf("postgres: read watermark: %w", err)
	}
	return w, nil
}

func (t *bookTx) SetWatermark(ctx context.Context, w ports.Watermark, shard domain.Shard) error {
	r, k, i := t.key()
	_, err := t.tx.Exec(ctx, `
		INSERT INTO account_watermarks (realm, scope_kind, scope_id, gen, applied_seq, blocked, shard)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (realm, scope_kind, scope_id) DO UPDATE SET
			gen         = GREATEST(account_watermarks.gen, EXCLUDED.gen),
			applied_seq = GREATEST(account_watermarks.applied_seq, EXCLUDED.applied_seq),
			blocked     = EXCLUDED.blocked,
			shard       = EXCLUDED.shard,
			updated_at  = now()`, r, k, i, w.Gen, w.AppliedSeq, w.Blocked, int(shard))
	if err != nil {
		return fmt.Errorf("postgres: set watermark: %w", err)
	}
	return nil
}

func (t *bookTx) UsageIdem(ctx context.Context, key string, since time.Time) (ports.IdemRecord, bool, error) {
	r, k, i := t.key()
	var rec ports.IdemRecord
	// created_at >= since prunes to the window's daily partitions (plus DEFAULT); each is one index
	// lookup on the partition's primary key.
	err := t.tx.QueryRow(ctx, `
		SELECT fingerprint, gen, seq FROM usage_idem
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND idem_key = $4 AND created_at >= $5
		ORDER BY created_at LIMIT 1`, r, k, i, key, since).Scan(&rec.Fingerprint, &rec.Gen, &rec.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.IdemRecord{}, false, nil
	}
	if err != nil {
		return ports.IdemRecord{}, false, fmt.Errorf("postgres: probe usage_idem: %w", err)
	}
	return rec, true, nil
}

func (t *bookTx) PutUsageIdem(ctx context.Context, key, fp string, gen, seq int64) error {
	r, k, i := t.key()
	_, err := t.tx.Exec(ctx, `
		INSERT INTO usage_idem (realm, scope_kind, scope_id, idem_key, fingerprint, gen, seq) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r, k, i, key, fp, gen, seq)
	if err != nil {
		return fmt.Errorf("postgres: put usage_idem: %w", err)
	}
	return nil
}

func (t *bookTx) CreditIdem(ctx context.Context, op, key string) (ports.IdemRecord, bool, error) {
	var rec ports.IdemRecord
	err := t.tx.QueryRow(ctx, `SELECT fingerprint, gen, seq FROM credit_idem WHERE realm = $1 AND op = $2 AND idem_key = $3`,
		string(t.scope.Realm), op, key).Scan(&rec.Fingerprint, &rec.Gen, &rec.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.IdemRecord{}, false, nil
	}
	if err != nil {
		return ports.IdemRecord{}, false, fmt.Errorf("postgres: read credit_idem: %w", err)
	}
	return rec, true, nil
}

func (t *bookTx) PutCreditIdem(ctx context.Context, op, key, fp string, gen, seq int64, ledgerID string) error {
	r, k, i := t.key()
	var lid any
	if ledgerID != "" {
		lid = ledgerID
	}
	_, err := t.tx.Exec(ctx, `
		INSERT INTO credit_idem (realm, op, idem_key, fingerprint, scope_kind, scope_id, gen, seq, ledger_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid)`, r, op, key, fp, k, i, gen, seq, lid)
	if err != nil {
		return fmt.Errorf("postgres: put credit_idem: %w", err)
	}
	return nil
}

func jsonOrNil(v any, empty bool) any {
	if empty {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

func (t *bookTx) Book(ctx context.Context, rows []ports.LedgerRow) ([]string, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	batch := &pgx.Batch{}
	balance := map[domain.Pool]domain.Credits{}
	var order []domain.Pool
	for _, r := range rows {
		s := r.Scope.Normalized()
		if s != t.scope {
			return nil, fmt.Errorf("postgres: row for %s booked in the transaction of %s", s.Tag(), t.scope.Tag())
		}
		var ck, ci any
		if r.Counterparty != nil {
			c := r.Counterparty.Normalized()
			ck, ci = c.Kind, c.ID
		}
		qs := make([]domain.PayloadQuantity, 0, len(r.Quantities))
		for _, q := range r.Quantities {
			qs = append(qs, domain.PayloadQuantity{Unit: string(q.Unit), Amount: q.Amount})
		}
		var occurred any
		if !r.OccurredAt.IsZero() {
			occurred = r.OccurredAt.UTC()
		}
		var rc, res, rk, actor any
		if r.RateCardVersion != "" {
			rc = r.RateCardVersion
		}
		if r.Resource != "" {
			res = r.Resource
		}
		if r.RateKey != "" {
			rk = r.RateKey
		}
		if r.ActorID != "" {
			actor = r.ActorID
		}
		var cost any
		if r.CostMicros != 0 {
			cost = r.CostMicros
		}
		batch.Queue(`
			INSERT INTO credit_ledger (realm, scope_kind, scope_id, pool, delta, operation_type, counter_scope_kind, counter_scope_id,
			                           idem_key, gen, seq_from, seq_to, event_count, rate_card_version, cost_micros, resource, rate_key,
			                           quantities, subjects, actor_id, ref, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
			RETURNING id::text`,
			string(s.Realm), s.Kind, s.ID, string(r.Pool.Or()), int64(r.Delta), r.OperationType, ck, ci,
			r.IdemKey, r.Gen, r.SeqFrom, r.SeqTo, max(r.EventCount, 1), rc, cost, res, rk,
			jsonOrNil(qs, len(qs) == 0), jsonOrNil(r.Subjects, len(r.Subjects) == 0), actor, jsonOrNil(r.Ref, len(r.Ref) == 0), occurred)
		p := r.Pool.Or()
		if _, seen := balance[p]; !seen {
			order = append(order, p)
		}
		var err error
		if balance[p], err = balance[p].Add(r.Delta); err != nil {
			return nil, err
		}
	}
	// The pool balances are updated in the SAME batch as the rows: `balance == SUM(ledger)` is a
	// property of this one call, not of two that must agree.
	for _, p := range order {
		batch.Queue(`
			INSERT INTO account_credits (realm, scope_kind, scope_id, pool, balance) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (realm, scope_kind, scope_id, pool) DO UPDATE SET
				balance = account_credits.balance + EXCLUDED.balance, updated_at = now()`,
			string(t.scope.Realm), t.scope.Kind, t.scope.ID, string(p), int64(balance[p]))
	}
	res := t.tx.SendBatch(ctx, batch)
	ids := make([]string, 0, len(rows))
	for range rows {
		var id string
		if err := res.QueryRow().Scan(&id); err != nil {
			_ = res.Close()
			return nil, fmt.Errorf("postgres: insert ledger row: %w", err)
		}
		ids = append(ids, id)
	}
	for range order {
		if _, err := res.Exec(); err != nil {
			_ = res.Close()
			return nil, fmt.Errorf("postgres: update balance: %w", err)
		}
	}
	if err := res.Close(); err != nil {
		return nil, fmt.Errorf("postgres: book batch: %w", err)
	}
	return ids, nil
}

func (t *bookTx) HasSuspenseAt(ctx context.Context, gen, seq int64) (bool, error) {
	r, k, i := t.key()
	var ok bool
	err := t.tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM credit_suspense WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND gen = $4 AND seq = $5)`,
		r, k, i, gen, seq).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("postgres: probe suspense: %w", err)
	}
	return ok, nil
}

func (t *bookTx) OpenSuspense(ctx context.Context, s ports.Suspense) (string, error) {
	r, k, i := t.key()
	var id string
	err := t.tx.QueryRow(ctx, `
		INSERT INTO credit_suspense (realm, scope_kind, scope_id, pool, delta, reason, gen, seq, idem_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text`,
		r, k, i, string(s.Pool.Or()), int64(s.Delta), s.Reason, s.Gen, s.Seq, s.IdemKey).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("postgres: open suspense: %w", err)
	}
	return id, nil
}

func (t *bookTx) CloseSuspense(ctx context.Context, id string) (bool, error) {
	tag, err := t.tx.Exec(ctx, `
		UPDATE credit_suspense SET status = 'closed', closed_at = now()
		WHERE id = $1::uuid AND status = 'open' AND realm = $2 AND scope_kind = $3 AND scope_id = $4`,
		id, string(t.scope.Realm), t.scope.Kind, t.scope.ID)
	if err != nil {
		return false, fmt.Errorf("postgres: close suspense: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// OpenLot stamps created_at with clock_timestamp(), not the transaction's now(): several grants
// booked in one transaction must still consume in the order they were granted (FIFO), and now() is
// the same instant for all of them.
func (t *bookTx) OpenLot(ctx context.Context, l ports.Lot) error {
	r, k, i := t.key()
	_, err := t.tx.Exec(ctx, `
		INSERT INTO credit_lots (realm, scope_kind, scope_id, pool, granted, remaining, expires_at, source_idem_key, created_at)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, clock_timestamp())`, r, k, i, string(l.Pool.Or()), int64(l.Granted), l.ExpiresAt, l.SourceKey)
	if err != nil {
		return fmt.Errorf("postgres: open lot: %w", err)
	}
	return nil
}

func (t *bookTx) ConsumeLots(ctx context.Context, pool domain.Pool, n domain.Credits) (domain.Credits, error) {
	if n <= 0 {
		return 0, nil
	}
	r, k, i := t.key()
	rows, err := t.tx.Query(ctx, `
		SELECT id::text, remaining FROM credit_lots
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND pool = $4 AND remaining > 0 AND expired_at IS NULL
		ORDER BY created_at, id FOR UPDATE`, r, k, i, string(pool.Or()))
	if err != nil {
		return 0, fmt.Errorf("postgres: read lots: %w", err)
	}
	type lot struct {
		id  string
		rem int64
	}
	var lots []lot
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.id, &l.rem); err != nil {
			rows.Close()
			return 0, fmt.Errorf("postgres: scan lot: %w", err)
		}
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var consumed domain.Credits
	for _, l := range lots {
		if consumed >= n {
			break
		}
		take := min(int64(n-consumed), l.rem)
		if _, err := t.tx.Exec(ctx, `UPDATE credit_lots SET remaining = remaining - $2 WHERE id = $1::uuid`, l.id, take); err != nil {
			return 0, fmt.Errorf("postgres: consume lot: %w", err)
		}
		consumed += domain.Credits(take)
	}
	return consumed, nil
}

func (t *bookTx) ExpireLot(ctx context.Context, lotID string) (domain.Credits, bool, error) {
	var rem int64
	err := t.tx.QueryRow(ctx, `
		SELECT remaining FROM credit_lots WHERE id = $1::uuid AND expired_at IS NULL
		AND realm = $2 AND scope_kind = $3 AND scope_id = $4 FOR UPDATE`, lotID, string(t.scope.Realm), t.scope.Kind, t.scope.ID).Scan(&rem)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("postgres: read lot: %w", err)
	}
	if _, err := t.tx.Exec(ctx, `UPDATE credit_lots SET remaining = 0, expired_at = now() WHERE id = $1::uuid`, lotID); err != nil {
		return 0, false, fmt.Errorf("postgres: expire lot: %w", err)
	}
	return domain.Credits(rem), true, nil
}

func (t *bookTx) OpenTransfer(ctx context.Context, tr ports.TransferRecord) error {
	from, to := tr.From.Normalized(), tr.To.Normalized()
	_, err := t.tx.Exec(ctx, `
		INSERT INTO credit_transfers (realm, idem_key, from_kind, from_id, from_pool, to_kind, to_id, to_pool, amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (realm, idem_key) DO NOTHING`,
		string(from.Realm), tr.IdemKey, from.Kind, from.ID, string(tr.FromPool.Or()), to.Kind, to.ID, string(tr.ToPool.Or()), int64(tr.Amount))
	if err != nil {
		return fmt.Errorf("postgres: open transfer: %w", err)
	}
	return nil
}

func (t *bookTx) SettleTransfer(ctx context.Context, realm domain.Realm, idemKey string) (ports.TransferRecord, bool, error) {
	var tr ports.TransferRecord
	var fk, fi, fp, tk, ti, tp string
	var amt int64
	err := t.tx.QueryRow(ctx, `
		UPDATE credit_transfers SET status = 'settled', settled_at = coalesce(settled_at, now())
		WHERE realm = $1 AND idem_key = $2
		RETURNING from_kind, from_id, from_pool, to_kind, to_id, to_pool, amount, status, created_at, settled_at`,
		string(realm.Or()), idemKey).Scan(&fk, &fi, &fp, &tk, &ti, &tp, &amt, &tr.Status, &tr.CreatedAt, &tr.SettledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.TransferRecord{}, false, nil
	}
	if err != nil {
		return ports.TransferRecord{}, false, fmt.Errorf("postgres: settle transfer: %w", err)
	}
	tr.Realm, tr.IdemKey, tr.Amount = realm.Or(), idemKey, domain.Credits(amt)
	tr.From = domain.Scope{Realm: realm.Or(), Kind: fk, ID: fi}
	tr.To = domain.Scope{Realm: realm.Or(), Kind: tk, ID: ti}
	tr.FromPool, tr.ToPool = domain.Pool(fp), domain.Pool(tp)
	return tr, true, nil
}

func (t *bookTx) SettleJournal(ctx context.Context, key string) error {
	r, k, i := t.key()
	_, err := t.tx.Exec(ctx, `
		UPDATE spend_journal SET status = 'settled', settled_at = now()
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND idem_key = $4 AND status = 'pending'`, r, k, i, key)
	if err != nil {
		return fmt.Errorf("postgres: settle journal: %w", err)
	}
	return nil
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
