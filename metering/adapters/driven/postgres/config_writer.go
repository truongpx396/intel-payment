package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// ConfigWriter writes configuration: rate cards, limits and pools. Configuration is data, written by anything
// that holds a connection, so the audit row and the cache-invalidation signal are written BY THE DATABASE, in the
// same transaction as the change (a trigger on each table). This adapter's part is to tell the trigger who and
// why — `intelpay.actor` and `intelpay.reason`, transaction-local so they cannot leak onto the next statement
// on a pooled connection — and to make each change atomic.
type ConfigWriter struct{ pool *pgxpool.Pool }

var _ ports.ConfigWriter = (*ConfigWriter)(nil)

// NewConfigWriter returns a writer on pool.
func NewConfigWriter(pool *pgxpool.Pool) *ConfigWriter { return &ConfigWriter{pool: pool} }

// tx runs fn in a transaction with the actor and reason set for the trigger.
func (w *ConfigWriter) tx(ctx context.Context, ch ports.ConfigChange, fn func(pgx.Tx) error) error {
	actor := ch.Actor
	if ch.Via != "" {
		actor += " (via " + ch.Via + ")"
	}
	t, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = t.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := t.Exec(ctx, `SELECT set_config('intelpay.actor', $1, true), set_config('intelpay.reason', $2, true)`, actor, ch.Reason); err != nil {
		return fmt.Errorf("postgres: set audit context: %w", err)
	}
	if err := fn(t); err != nil {
		return err
	}
	if err := t.Commit(ctx); err != nil {
		return mapWriteError(err)
	}
	return nil
}

// mapWriteError turns what the database refuses into the domain's words: a constraint it enforces is a request
// that is not valid, not a server fault.
func mapWriteError(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23505": // unique_violation
		return fmt.Errorf("%w: %s", domain.ErrAlreadyExists, pg.ConstraintName)
	case "23514", "23502", "22P02", "23503", "22001", "22003": // check, not null, bad text, foreign key, too long, out of range
		return fmt.Errorf("%w: the database refuses it (%s): %s", domain.ErrInvalid, pg.ConstraintName, pg.Message)
	}
	return err
}

// ---------------------------------------------------------------- rate cards --

// PublishRateCard inserts a card and its entries, retiring the realm's active card in the same transaction
// when asked. A realm has exactly one active card (the database enforces it), so publishing without retiring
// one that exists, and publishing a version that exists, are ErrAlreadyExists — and either leaves the old card
// active.
func (w *ConfigWriter) PublishRateCard(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, card domain.RateCard, retirePrevious bool) (ports.RateCardInfo, error) {
	realm = realm.Or()
	var info ports.RateCardInfo
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		if retirePrevious {
			if _, err := t.Exec(ctx, `UPDATE rate_cards SET retired_at = now() WHERE realm = $1 AND retired_at IS NULL`, string(realm)); err != nil {
				return mapWriteError(err)
			}
		}
		params, err := json.Marshal(card.Params)
		if err != nil || card.Params == nil {
			params = []byte("{}")
		}
		err = t.QueryRow(ctx, `INSERT INTO rate_cards (realm, version, pricer, params) VALUES ($1, $2, $3, $4::jsonb) RETURNING published_at`,
			string(realm), card.Version, card.Pricer, string(params)).Scan(&info.PublishedAt)
		if err != nil {
			err = mapWriteError(err)
			switch {
			case errors.Is(err, domain.ErrAlreadyExists) && strings.Contains(err.Error(), "one_active"):
				return fmt.Errorf("%w: realm %q already has an active rate card: publish with retire_previous to replace it", domain.ErrAlreadyExists, realm)
			case errors.Is(err, domain.ErrAlreadyExists):
				return fmt.Errorf("%w: rate card version %q is already published", domain.ErrAlreadyExists, card.Version)
			}
			return err
		}
		for _, e := range card.Entries {
			var cost any
			if e.CostMicrosPerBlock != 0 {
				cost = e.CostMicrosPerBlock
			}
			if _, err := t.Exec(ctx, `INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size, cost_micros_per_block) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				string(realm), card.Version, e.RateKey, string(e.Unit), e.CreditsPerBlock, e.BlockSize, cost); err != nil {
				return mapWriteError(err)
			}
		}
		return nil
	})
	if err != nil {
		return ports.RateCardInfo{}, err
	}
	card.Realm = realm
	info.Card = card
	return info, nil
}

// RateCards lists the realm's cards oldest first, retired ones included, each with its entries.
func (w *ConfigWriter) RateCards(ctx context.Context, realm domain.Realm) ([]ports.RateCardInfo, error) {
	realm = realm.Or()
	rows, err := w.pool.Query(ctx, `SELECT version, pricer, params, published_at, retired_at FROM rate_cards WHERE realm = $1 ORDER BY published_at, version`, string(realm))
	if err != nil {
		return nil, fmt.Errorf("postgres: list cards: %w", err)
	}
	type head struct {
		version, pricer string
		params          []byte
		published       time.Time
		retired         *time.Time
	}
	var heads []head
	for rows.Next() {
		var h head
		if err := rows.Scan(&h.version, &h.pricer, &h.params, &h.published, &h.retired); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: scan card: %w", err)
		}
		heads = append(heads, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list cards: %w", err)
	}

	erows, err := w.pool.Query(ctx, `SELECT version, rate_key, unit, credits_per_block, block_size, coalesce(cost_micros_per_block, 0)
		FROM rate_card_entries WHERE realm = $1 ORDER BY version, rate_key, unit`, string(realm))
	if err != nil {
		return nil, fmt.Errorf("postgres: list card entries: %w", err)
	}
	defer erows.Close()
	entries := map[string][]domain.RateEntry{}
	for erows.Next() {
		var version string
		var e domain.RateEntry
		var unit string
		if err := erows.Scan(&version, &e.RateKey, &unit, &e.CreditsPerBlock, &e.BlockSize, &e.CostMicrosPerBlock); err != nil {
			return nil, fmt.Errorf("postgres: scan card entry: %w", err)
		}
		e.Unit = domain.Unit(unit)
		entries[version] = append(entries[version], e)
	}
	if err := erows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: list card entries: %w", err)
	}

	out := make([]ports.RateCardInfo, 0, len(heads))
	for _, h := range heads {
		var params map[string]string
		if len(h.params) > 0 && strings.TrimSpace(string(h.params)) != "{}" {
			if err := json.Unmarshal(h.params, &params); err != nil {
				return nil, fmt.Errorf("postgres: card %q params: %w", h.version, err)
			}
		}
		card, err := domain.NewRateCard(realm, h.version, h.pricer, entries[h.version], params)
		if err != nil {
			return nil, fmt.Errorf("postgres: card %q: %w", h.version, err)
		}
		out = append(out, ports.RateCardInfo{Card: card, PublishedAt: h.published, RetiredAt: h.retired})
	}
	return out, nil
}

// --------------------------------------------------------------------- limits --

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PutLimit creates or replaces a limit. A new one goes last (its place is the order limits are evaluated in), a
// replaced one keeps its place, and an explicit SortOrder overrides both.
func (w *ConfigWriter) PutLimit(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, l ports.LimitConfig) (bool, error) {
	realm = realm.Or()
	var dur any
	if l.Limit.Dur != 0 {
		dur = int(l.Limit.Dur / time.Second)
	}
	tz := l.Limit.TZ
	if tz == "" {
		tz = "UTC"
	}
	var order any
	if l.SortOrder != nil {
		order = *l.SortOrder
	}
	var created bool
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		return t.QueryRow(ctx, `
			INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, dur_seconds, tz, warn_at, deny_code, resource, sort_order, active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
			        COALESCE($13::int, (SELECT COALESCE(MAX(sort_order), 0) + 10 FROM limits WHERE realm = $1)), true)
			ON CONFLICT (realm, name) DO UPDATE SET
			    subject_kind = EXCLUDED.subject_kind, unit = EXCLUDED.unit, max = EXCLUDED.max, max_entitlement = EXCLUDED.max_entitlement,
			    window_kind = EXCLUDED.window_kind, dur_seconds = EXCLUDED.dur_seconds, tz = EXCLUDED.tz, warn_at = EXCLUDED.warn_at,
			    deny_code = EXCLUDED.deny_code, resource = EXCLUDED.resource, active = true,
			    sort_order = COALESCE($13::int, limits.sort_order)
			RETURNING (xmax = 0)`,
			string(realm), l.Limit.Name, nilIfEmpty(l.Limit.Subject), string(l.Limit.Unit), l.Limit.Max, nilIfEmpty(l.Limit.MaxEntitlement),
			l.Limit.Window.String(), dur, tz, l.Limit.WarnAt, l.Limit.DenyCode, nilIfEmpty(l.Limit.Resource), order).Scan(&created)
	})
	return created, mapWriteError(err)
}

// Limit reads one limit by name.
func (w *ConfigWriter) Limit(ctx context.Context, realm domain.Realm, name string) (ports.LimitConfig, error) {
	realm = realm.Or()
	var (
		l                    domain.Limit
		subject, entitlement *string
		resource             *string
		window, unit         string
		dur                  *int
		warn                 float64
		order                int
	)
	err := w.pool.QueryRow(ctx, `SELECT subject_kind, unit, max, max_entitlement, window_kind, dur_seconds, tz, warn_at::float8, deny_code, resource, sort_order
		FROM limits WHERE realm = $1 AND name = $2`, string(realm), name).
		Scan(&subject, &unit, &l.Max, &entitlement, &window, &dur, &l.TZ, &warn, &l.DenyCode, &resource, &order)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.LimitConfig{}, fmt.Errorf("%w: limit %q in realm %q", domain.ErrNotFound, name, realm)
	}
	if err != nil {
		return ports.LimitConfig{}, fmt.Errorf("postgres: read limit: %w", err)
	}
	if l.Window, err = domain.ParseWindow(window); err != nil {
		return ports.LimitConfig{}, fmt.Errorf("postgres: limit %q: %w", name, err)
	}
	l.Name, l.Unit, l.WarnAt = name, domain.Unit(unit), warn
	l.Subject, l.MaxEntitlement, l.Resource = deref(subject), deref(entitlement), deref(resource)
	if dur != nil {
		l.Dur = time.Duration(*dur) * time.Second
	}
	return ports.LimitConfig{Limit: l, SortOrder: &order}, nil
}

// DeleteLimit removes a limit; ErrNotFound when there is none.
func (w *ConfigWriter) DeleteLimit(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, name string) error {
	realm = realm.Or()
	return w.tx(ctx, ch, func(t pgx.Tx) error {
		tag, err := t.Exec(ctx, `DELETE FROM limits WHERE realm = $1 AND name = $2`, string(realm), name)
		if err != nil {
			return mapWriteError(err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: limit %q in realm %q", domain.ErrNotFound, name, realm)
		}
		return nil
	})
}

// ---------------------------------------------------------------------- pools --

// PutPool creates or replaces a pool. A pool that applies to no particular resource applies to every one.
func (w *ConfigWriter) PutPool(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, p ports.PoolConfig) (bool, error) {
	realm = realm.Or()
	var applies any
	if len(p.Pool.AppliesTo) > 0 {
		applies = p.Pool.AppliesTo
	}
	var created bool
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		return t.QueryRow(ctx, `
			INSERT INTO credit_pools (realm, name, priority, applies_to, description) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (realm, name) DO UPDATE SET priority = EXCLUDED.priority, applies_to = EXCLUDED.applies_to, description = EXCLUDED.description
			RETURNING (xmax = 0)`,
			string(realm), string(p.Pool.Name), p.Pool.Priority, applies, nilIfEmpty(p.Description)).Scan(&created)
	})
	return created, mapWriteError(err)
}

// Pool reads one pool by name.
func (w *ConfigWriter) Pool(ctx context.Context, realm domain.Realm, name string) (ports.PoolConfig, error) {
	realm = realm.Or()
	var (
		p           ports.PoolConfig
		applies     []string
		description *string
	)
	err := w.pool.QueryRow(ctx, `SELECT priority, applies_to, description FROM credit_pools WHERE realm = $1 AND name = $2`, string(realm), name).
		Scan(&p.Pool.Priority, &applies, &description)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.PoolConfig{}, fmt.Errorf("%w: pool %q in realm %q", domain.ErrNotFound, name, realm)
	}
	if err != nil {
		return ports.PoolConfig{}, fmt.Errorf("postgres: read pool: %w", err)
	}
	p.Pool.Name, p.Pool.AppliesTo, p.Description = domain.Pool(name), applies, deref(description)
	return p, nil
}
