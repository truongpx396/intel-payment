// Package postgres is the default UsageArchive: per-event detail in usage_events, kept when the
// ledger books one row per drain batch (LedgerGranularity=rollup, FR-050). Every event stays
// re-priceable (SC-010) though the ledger holds only the rollup. A columnar store is the scale-out
// adapter behind the same port.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Archive implements ports.UsageArchive over Postgres.
type Archive struct{ pool *pgxpool.Pool }

var _ ports.UsageArchive = (*Archive)(nil)

// New builds an Archive over a pool.
func New(pool *pgxpool.Pool) *Archive { return &Archive{pool: pool} }

type drawDoc struct {
	Pool  string `json:"pool"`
	Delta int64  `json:"delta"`
}

// Append stores events idempotently on (scope, gen, seq): a retried batch adds nothing. The row's
// created_at is the event's OccurredAt, not the wall clock — so a retry lands on the SAME primary
// key (which includes the partition column) and conflicts, instead of duplicating in a later day.
func (a *Archive) Append(ctx context.Context, events []ports.ArchivedEvent) error {
	if len(events) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range events {
		if e.OccurredAt.IsZero() {
			return fmt.Errorf("%w: an archived event needs occurred_at (it is the row's idempotency time)", domain.ErrInvalid)
		}
		s := e.Scope.Normalized()
		draws := make([]drawDoc, len(e.PoolDraws))
		for i, d := range e.PoolDraws {
			draws[i] = drawDoc{Pool: string(d.Pool), Delta: int64(d.Delta)}
		}
		qs := make([]domain.PayloadQuantity, len(e.Quantities))
		for i, q := range e.Quantities {
			qs[i] = domain.PayloadQuantity{Unit: string(q.Unit), Amount: q.Amount}
		}
		dj, err := json.Marshal(draws)
		if err != nil {
			return fmt.Errorf("postgres archive: draws: %w", err)
		}
		qj, err := json.Marshal(qs)
		if err != nil {
			return fmt.Errorf("postgres archive: quantities: %w", err)
		}
		var sj any
		if len(e.Subjects) > 0 {
			if sj, err = json.Marshal(e.Subjects); err != nil {
				return fmt.Errorf("postgres archive: subjects: %w", err)
			}
		}
		batch.Queue(`
			INSERT INTO usage_events (realm, scope_kind, scope_id, gen, seq, idem_key, pool_draws, credits, resource, rate_key,
			                          rate_card_version, quantities, subjects, cost_micros, occurred_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,nullif($9,''),nullif($10,''),nullif($11,''),$12,$13,$14,$15,$15)
			ON CONFLICT DO NOTHING`,
			string(s.Realm), s.Kind, s.ID, e.Gen, e.Seq, e.IdemKey, dj, int64(e.Credits), e.Resource, e.RateKey,
			e.RateCardVersion, qj, sj, e.CostMicros, e.OccurredAt.UTC())
	}
	res := a.pool.SendBatch(ctx, batch)
	for range events {
		if _, err := res.Exec(); err != nil {
			_ = res.Close()
			return fmt.Errorf("postgres archive: append: %w", err)
		}
	}
	return res.Close()
}

// Events reads a scope's events in [from, to), oldest first.
func (a *Archive) Events(ctx context.Context, scope domain.Scope, from, to time.Time, limit int) ([]ports.ArchivedEvent, error) {
	scope = scope.Normalized()
	rows, err := a.pool.Query(ctx, `
		SELECT gen, seq, idem_key, pool_draws, credits, coalesce(resource,''), coalesce(rate_key,''), coalesce(rate_card_version,''),
		       quantities, subjects, coalesce(cost_micros,0), occurred_at
		FROM usage_events
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND created_at >= $4 AND created_at < $5
		ORDER BY created_at, gen, seq LIMIT $6`, string(scope.Realm), scope.Kind, scope.ID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres archive: events: %w", err)
	}
	defer rows.Close()
	var out []ports.ArchivedEvent
	for rows.Next() {
		e := ports.ArchivedEvent{Scope: scope}
		var draws, qs, subj []byte
		var credits int64
		if err := rows.Scan(&e.Gen, &e.Seq, &e.IdemKey, &draws, &credits, &e.Resource, &e.RateKey, &e.RateCardVersion, &qs, &subj, &e.CostMicros, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("postgres archive: scan: %w", err)
		}
		e.Credits = domain.Credits(credits)
		var dd []drawDoc
		if err := json.Unmarshal(draws, &dd); err != nil {
			return nil, fmt.Errorf("postgres archive: draws: %w", err)
		}
		for _, d := range dd {
			e.PoolDraws = append(e.PoolDraws, domain.PoolDelta{Pool: domain.Pool(d.Pool), Delta: domain.Credits(d.Delta)})
		}
		var qq []domain.PayloadQuantity
		if len(qs) > 0 {
			if err := json.Unmarshal(qs, &qq); err != nil {
				return nil, fmt.Errorf("postgres archive: quantities: %w", err)
			}
		}
		for _, q := range qq {
			e.Quantities = append(e.Quantities, domain.Quantity{Unit: domain.Unit(q.Unit), Amount: q.Amount})
		}
		if len(subj) > 0 {
			if err := json.Unmarshal(subj, &e.Subjects); err != nil {
				return nil, fmt.Errorf("postgres archive: subjects: %w", err)
			}
		}
		e.OccurredAt = e.OccurredAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
