package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// Catalog is what is sold, as data (constitution VIII): plans, their prices per currency, their entitlements and the
// provider price that sells each through an account. It reads the database on every call — no cache — because a price
// written a moment ago must be sold now.
type Catalog struct{ pool *pgxpool.Pool }

var _ ports.Catalog = (*Catalog)(nil)

// NewCatalog returns the catalogue of a database.
func NewCatalog(pool *pgxpool.Pool) *Catalog { return &Catalog{pool: pool} }

const planColumns = `p.id::text, p.realm, p.code, p.name, p.kind, p.credit_allotment, p.pool, p.billing_interval, p.active, p.sort_order`

// loadPlans reads plans with their prices and entitlements. rows are the plan columns, in order, with optional
// extra columns scanned by extra.
func loadPlans(ctx context.Context, q db, sql string, args []any, extra func() []any, after func(i int)) ([]domain.Plan, []string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	defer rows.Close()
	var plans []domain.Plan
	var ids []string
	for rows.Next() {
		var id, realm, kind, pool string
		var allot *int64
		var interval *string
		var p domain.Plan
		dest := []any{&id, &realm, &p.Code, &p.Name, &kind, &allot, &pool, &interval, &p.Active, &p.SortOrder}
		if extra != nil {
			dest = append(dest, extra()...)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		p.Realm, p.Kind, p.Pool = domain.Realm(realm), domain.PlanKind(kind), domain.Pool(pool)
		if allot != nil {
			v := domain.Credits(*allot)
			p.CreditAllotment = &v
		}
		if interval != nil {
			v := domain.Interval(*interval)
			p.Interval = &v
		}
		p.Prices = map[string]domain.Money{}
		plans = append(plans, p)
		ids = append(ids, id)
		if after != nil {
			after(len(plans) - 1)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil, nil
	}
	index := map[string]int{}
	for i, id := range ids {
		index[id] = i
	}
	pr, err := q.Query(ctx, `SELECT plan_id::text, currency, minor_units FROM plan_prices WHERE plan_id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	defer pr.Close()
	for pr.Next() {
		var id, cur string
		var minor int64
		if err := pr.Scan(&id, &cur, &minor); err != nil {
			return nil, nil, err
		}
		plans[index[id]].Prices[cur] = domain.Money{MinorUnits: minor, Currency: cur}
	}
	if err := pr.Err(); err != nil {
		return nil, nil, err
	}
	pr.Close()
	er, err := q.Query(ctx, `SELECT plan_id::text, feature_key, value_kind, value_bool, value_int, value_text FROM plan_entitlements
	                         WHERE plan_id = ANY($1::uuid[]) ORDER BY feature_key`, ids)
	if err != nil {
		return nil, nil, mapErr(err)
	}
	defer er.Close()
	for er.Next() {
		var id string
		var e domain.Entitlement
		if err := er.Scan(&id, &e.Key, &e.Kind, &e.Bool, &e.Int, &e.Text); err != nil {
			return nil, nil, err
		}
		plans[index[id]].Entitlements = append(plans[index[id]].Entitlements, e)
	}
	return plans, ids, er.Err()
}

// List implements ports.Catalog: the realm's plans that are on sale and priced in the currency, in their order.
func (c *Catalog) List(ctx context.Context, realm domain.Realm, currency string) ([]domain.Plan, error) {
	plans, _, err := loadPlans(ctx, c.pool, `SELECT `+planColumns+` FROM plans p
		WHERE p.realm = $1 AND p.active AND EXISTS (SELECT 1 FROM plan_prices pp WHERE pp.plan_id = p.id AND pp.currency = $2)
		ORDER BY p.sort_order, p.code`, []any{string(realm.Or()), currency}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: list plans of %s in %s: %w", realm.Or(), currency, err)
	}
	return plans, nil
}

// Resolve implements ports.Catalog: a plan that is on sale, priced in the currency, and sold through the account — or
// domain.ErrUnknownPrice. There is no default.
func (c *Catalog) Resolve(ctx context.Context, realm domain.Realm, code, currency, account string) (domain.Plan, string, error) {
	var priceID string
	plans, _, err := loadPlans(ctx, c.pool, `SELECT `+planColumns+`, ppp.provider_price_id FROM plans p
		JOIN plan_prices pp ON pp.plan_id = p.id AND pp.currency = $3
		JOIN plan_provider_prices ppp ON ppp.plan_id = p.id AND ppp.provider_account = $4 AND ppp.currency = $3
		WHERE p.realm = $1 AND p.code = $2 AND p.active`,
		[]any{string(realm.Or()), code, currency, account}, func() []any { return []any{&priceID} }, nil)
	if err != nil {
		return domain.Plan{}, "", fmt.Errorf("postgres: resolve %s in %s for %s: %w", code, currency, account, err)
	}
	if len(plans) == 0 {
		return domain.Plan{}, "", fmt.Errorf("%w: %s in %s is not sold through %s", domain.ErrUnknownPrice, code, currency, account)
	}
	return plans[0], priceID, nil
}

// PlanForPrice implements ports.Catalog. A plan since retired is still recognized: a customer who paid a minute before
// it was withdrawn must get what they bought.
func (c *Catalog) PlanForPrice(ctx context.Context, account, providerPriceID string) (domain.Plan, bool, error) {
	plans, _, err := loadPlans(ctx, c.pool, `SELECT `+planColumns+` FROM plans p
		JOIN plan_provider_prices ppp ON ppp.plan_id = p.id
		WHERE ppp.provider_account = $1 AND ppp.provider_price_id = $2`, []any{account, providerPriceID}, nil, nil)
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return domain.Plan{}, false, fmt.Errorf("postgres: plan for price %s: %w", providerPriceID, err)
	case len(plans) == 0:
		return domain.Plan{}, false, nil
	}
	return plans[0], true, nil
}
