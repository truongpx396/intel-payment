package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Store is a ports.EntitlementStore over a pool.
type Store struct{ pool *pgxpool.Pool }

var _ ports.EntitlementStore = (*Store)(nil)

// New is the store over pool. The pool belongs to the caller.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Postgres error classes the store translates into the domain's errors.
const codeCheckViolation = "23514"

// mapErr translates what the database refuses into the domain's words, keeping the cause for the operator's log.
func mapErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == codeCheckViolation {
		return fmt.Errorf("%w: %s (%s)", domain.ErrInvalid, pe.Message, pe.ConstraintName)
	}
	return err
}

// valueCols is how a Value sits in a row: one nullable column per kind.
type valueCols struct {
	kind string
	b    *bool
	i    *int64
	s    *string
}

func (c valueCols) value() domain.Value {
	switch domain.Kind(c.kind) {
	case domain.Flag:
		return domain.Value{Kind: domain.Flag, Bool: c.b != nil && *c.b}
	case domain.Quota:
		v := domain.Value{Kind: domain.Quota}
		if c.i != nil {
			v.Int = *c.i
		}
		return v
	case domain.Enum:
		v := domain.Value{Kind: domain.Enum}
		if c.s != nil {
			v.Text = *c.s
		}
		return v
	}
	return domain.Value{Kind: domain.Kind(c.kind)} // the schema's CHECK keeps this from happening; Validate refuses it if it does
}

func colsOf(v domain.Value) valueCols {
	c := valueCols{kind: string(v.Kind)}
	switch v.Kind {
	case domain.Flag:
		c.b = &v.Bool
	case domain.Quota:
		c.i = &v.Int
	case domain.Enum:
		c.s = &v.Text
	}
	return c
}

// Keys implements ports.EntitlementStore.
func (s *Store) Keys(ctx context.Context, realm md.Realm) ([]domain.KeyDecl, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, kind, enum_order FROM entitlement_keys WHERE realm = $1 ORDER BY key`, string(realm.Or()))
	if err != nil {
		return nil, fmt.Errorf("postgres: keys: %w", err)
	}
	defer rows.Close()
	var out []domain.KeyDecl
	for rows.Next() {
		var d domain.KeyDecl
		var key, kind string
		if err := rows.Scan(&key, &kind, &d.EnumOrder); err != nil {
			return nil, fmt.Errorf("postgres: keys: %w", err)
		}
		d.Key, d.Kind = domain.Key(key), domain.Kind(kind)
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanEntitlements reads rows of (feature_key, value_kind, value_bool, value_int, value_text).
func scanEntitlements(rows pgx.Rows) ([]domain.Entitlement, error) {
	defer rows.Close()
	var out []domain.Entitlement
	for rows.Next() {
		var key string
		var c valueCols
		if err := rows.Scan(&key, &c.kind, &c.b, &c.i, &c.s); err != nil {
			return nil, err
		}
		out = append(out, domain.Entitlement{Key: domain.Key(key), Value: c.value()})
	}
	return out, rows.Err()
}

// Defaults implements ports.EntitlementStore.
func (s *Store) Defaults(ctx context.Context, realm md.Realm) ([]domain.Entitlement, error) {
	rows, err := s.pool.Query(ctx, `SELECT feature_key, value_kind, value_bool, value_int, value_text FROM realm_defaults
	                                WHERE realm = $1 ORDER BY feature_key`, string(realm.Or()))
	if err != nil {
		return nil, fmt.Errorf("postgres: defaults: %w", err)
	}
	out, err := scanEntitlements(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: defaults: %w", err)
	}
	return out, nil
}

// PlanEntitlements implements ports.EntitlementStore. A plan off sale still answers: it is not revoked.
func (s *Store) PlanEntitlements(ctx context.Context, realm md.Realm, planCode string) ([]domain.Entitlement, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.feature_key, e.value_kind, e.value_bool, e.value_int, e.value_text
	                                FROM plan_entitlements e JOIN plans p ON p.id = e.plan_id
	                                WHERE p.realm = $1 AND p.code = $2 ORDER BY e.feature_key`, string(realm.Or()), planCode)
	if err != nil {
		return nil, fmt.Errorf("postgres: plan entitlements: %w", err)
	}
	out, err := scanEntitlements(rows)
	if err != nil {
		return nil, fmt.Errorf("postgres: plan entitlements: %w", err)
	}
	return out, nil
}

// Overrides implements ports.EntitlementStore.
func (s *Store) Overrides(ctx context.Context, sc md.Scope) ([]domain.Grant, error) {
	sc = sc.Normalized()
	rows, err := s.pool.Query(ctx, `SELECT feature_key, value_kind, value_bool, value_int, value_text, expires_at, reason
	                                FROM entitlement_overrides WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3
	                                ORDER BY feature_key`, string(sc.Realm), sc.Kind, sc.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: overrides: %w", err)
	}
	defer rows.Close()
	var out []domain.Grant
	for rows.Next() {
		var key string
		var c valueCols
		var exp *time.Time
		g := domain.Grant{Scope: sc, Source: domain.FromOverride}
		if err := rows.Scan(&key, &c.kind, &c.b, &c.i, &c.s, &exp, &g.Reason); err != nil {
			return nil, fmt.Errorf("postgres: overrides: %w", err)
		}
		g.Key, g.Value = domain.Key(key), c.value()
		if exp != nil {
			u := exp.UTC()
			g.ExpiresAt = &u
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Subscription implements ports.EntitlementStore. The partial unique index on subscriptions allows one live subscription per
// scope, so at most one row can match.
func (s *Store) Subscription(ctx context.Context, sc md.Scope) (domain.Subscription, bool, error) {
	sc = sc.Normalized()
	var sub domain.Subscription
	var status string
	var grace *time.Time
	err := s.pool.QueryRow(ctx, `SELECT p.code, s.status, s.grace_until
	                             FROM subscriptions s JOIN plans p ON p.id = s.plan_id AND p.realm = s.realm
	                             WHERE s.realm = $1 AND s.scope_kind = $2 AND s.scope_id = $3
	                               AND s.status IN ('trialing','active','past_due','paused')`,
		string(sc.Realm), sc.Kind, sc.ID).Scan(&sub.PlanCode, &status, &grace)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Subscription{}, false, nil
	}
	if err != nil {
		return domain.Subscription{}, false, fmt.Errorf("postgres: subscription: %w", err)
	}
	sub.Status = domain.SubStatus(status)
	if grace != nil {
		u := grace.UTC()
		sub.GraceUntil = &u
	}
	return sub, true, nil
}

// Plans implements ports.EntitlementStore: the subscription plans on sale, in catalogue order.
func (s *Store) Plans(ctx context.Context, realm md.Realm) ([]domain.Offer, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.code, e.feature_key, e.value_kind, e.value_bool, e.value_int, e.value_text
	                                FROM plans p LEFT JOIN plan_entitlements e ON e.plan_id = p.id
	                                WHERE p.realm = $1 AND p.active AND p.kind = 'subscription'
	                                ORDER BY p.sort_order, p.code, e.feature_key`, string(realm.Or()))
	if err != nil {
		return nil, fmt.Errorf("postgres: plans: %w", err)
	}
	defer rows.Close()
	var out []domain.Offer
	for rows.Next() {
		var code string
		var key *string
		var kind *string
		var c valueCols
		if err := rows.Scan(&code, &key, &kind, &c.b, &c.i, &c.s); err != nil {
			return nil, fmt.Errorf("postgres: plans: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].PlanCode != code {
			out = append(out, domain.Offer{PlanCode: code})
		}
		if key != nil && kind != nil {
			c.kind = *kind
			last := &out[len(out)-1]
			last.Entitlements = append(last.Entitlements, domain.Entitlement{Key: domain.Key(*key), Value: c.value()})
		}
	}
	return out, rows.Err()
}

// PutOverride implements ports.EntitlementStore. One override per scope and key: writing it again replaces it. The actor and
// the reason reach the audit trigger through the transaction's settings, so the audit row says who and why.
func (s *Store) PutOverride(ctx context.Context, g domain.Grant, actor string) error {
	sc := g.Scope.Normalized()
	c := colsOf(g.Value)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('intelpay.actor', $1, true), set_config('intelpay.reason', $2, true)`, actor, g.Reason); err != nil {
		return fmt.Errorf("postgres: set audit context: %w", err)
	}
	var exp *time.Time
	if g.ExpiresAt != nil {
		u := g.ExpiresAt.UTC().Truncate(time.Microsecond)
		exp = &u
	}
	if _, err := tx.Exec(ctx, `INSERT INTO entitlement_overrides
	        (realm, scope_kind, scope_id, feature_key, value_kind, value_bool, value_int, value_text, expires_at, reason, actor)
	        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	        ON CONFLICT (realm, scope_kind, scope_id, feature_key) DO UPDATE SET
	            value_kind = EXCLUDED.value_kind, value_bool = EXCLUDED.value_bool, value_int = EXCLUDED.value_int,
	            value_text = EXCLUDED.value_text, expires_at = EXCLUDED.expires_at, reason = EXCLUDED.reason,
	            actor = EXCLUDED.actor, created_at = now()`,
		string(sc.Realm), sc.Kind, sc.ID, string(g.Key), c.kind, c.b, c.i, c.s, exp, g.Reason, actor); err != nil {
		return fmt.Errorf("postgres: put override: %w", mapErr(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: put override: %w", mapErr(err))
	}
	return nil
}
