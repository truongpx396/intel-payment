package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ConfigWriter writes the payment configuration: plans, their prices, the provider prices that sell them, provider accounts and
// the customers a payment may land on. Configuration is data, written by anything that holds a connection, so the audit row
// and the cache-invalidation signal are written BY THE DATABASE, in the same transaction as the change (a trigger on each
// table). This adapter's part is to tell the trigger who and why — `intelpay.actor` and `intelpay.reason`, transaction-local
// so they cannot leak onto the next statement on a pooled connection — and to make each change atomic.
//
// A customer link is the exception that proves it: billing_customers is written by checkout as well as by an operator, so it
// has no trigger, and a link an operator makes writes its own audit row in the same transaction.
type ConfigWriter struct{ pool *pgxpool.Pool }

var _ ports.ConfigWriter = (*ConfigWriter)(nil)

// NewConfigWriter returns a writer on pool.
func NewConfigWriter(pool *pgxpool.Pool) *ConfigWriter { return &ConfigWriter{pool: pool} }

func actorOf(ch ports.ConfigChange) string {
	if ch.Via != "" {
		return ch.Actor + " (via " + ch.Via + ")"
	}
	return ch.Actor
}

// tx runs fn in a transaction with the actor and reason set for the trigger.
func (w *ConfigWriter) tx(ctx context.Context, ch ports.ConfigChange, fn func(pgx.Tx) error) error {
	t, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = t.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := t.Exec(ctx, `SELECT set_config('intelpay.actor', $1, true), set_config('intelpay.reason', $2, true)`, actorOf(ch), ch.Reason); err != nil {
		return fmt.Errorf("postgres: set audit context: %w", err)
	}
	if err := fn(t); err != nil {
		return err
	}
	if err := t.Commit(ctx); err != nil {
		return mapWrite(err)
	}
	return nil
}

// mapWrite turns what the database refuses into the domain's words: a constraint it enforces is a request that is not valid,
// not a server fault.
func mapWrite(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case codeUniqueViolation:
		return fmt.Errorf("%w: %s", errAlreadyExists, pg.ConstraintName)
	case codeCheckViolation, codeForeignKey, codeBadText, "23502", "22001", "22003":
		return fmt.Errorf("%w: the database refuses it (%s): %s", errInvalid, pg.ConstraintName, pg.Message)
	}
	return err
}

func notFound(what string, a ...any) error {
	return fmt.Errorf("%w: %s", errNotFound, fmt.Sprintf(what, a...))
}

// ----------------------------------------------------------------------- plans --

// planConfigs reads plans (by the query) with their prices and the provider prices that sell them.
func planConfigs(ctx context.Context, q db, sql string, args ...any) ([]ports.PlanConfig, error) {
	plans, ids, err := loadPlans(ctx, q, sql, args, nil, nil)
	if err != nil {
		return nil, err
	}
	out := make([]ports.PlanConfig, len(plans))
	index := map[string]int{}
	for i, p := range plans {
		out[i].Plan = p
		index[ids[i]] = i
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT plan_id::text, provider_account, currency, provider_price_id FROM plan_provider_prices
	                           WHERE plan_id = ANY($1::uuid[]) ORDER BY provider_account, currency`, ids)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var pp domain.ProviderPrice
		if err := rows.Scan(&id, &pp.Account, &pp.Currency, &pp.ProviderPriceID); err != nil {
			return nil, err
		}
		out[index[id]].ProviderPrices = append(out[index[id]].ProviderPrices, pp)
	}
	return out, rows.Err()
}

// Plans implements ports.ConfigStore.
func (w *ConfigWriter) Plans(ctx context.Context, realm md.Realm) ([]ports.PlanConfig, error) {
	out, err := planConfigs(ctx, w.pool, `SELECT `+planColumns+` FROM plans p WHERE p.realm = $1 ORDER BY p.sort_order, p.code`, string(realm.Or()))
	if err != nil {
		return nil, fmt.Errorf("postgres: plans of %s: %w", realm.Or(), err)
	}
	return out, nil
}

func planOf(ctx context.Context, q db, realm md.Realm, code string) (ports.PlanConfig, error) {
	out, err := planConfigs(ctx, q, `SELECT `+planColumns+` FROM plans p WHERE p.realm = $1 AND p.code = $2`, string(realm.Or()), code)
	switch {
	case err != nil:
		return ports.PlanConfig{}, fmt.Errorf("postgres: plan %s: %w", code, err)
	case len(out) == 0:
		return ports.PlanConfig{}, notFound("plan %s in realm %s", code, realm.Or())
	}
	return out[0], nil
}

// Plan implements ports.ConfigStore.
func (w *ConfigWriter) Plan(ctx context.Context, realm md.Realm, code string) (ports.PlanConfig, error) {
	return planOf(ctx, w.pool, realm, code)
}

// CreatePlan implements ports.ConfigStore.
func (w *ConfigWriter) CreatePlan(ctx context.Context, ch ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error) {
	if err := p.Validate(); err != nil {
		return ports.PlanConfig{}, err
	}
	var out ports.PlanConfig
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		var allot *int64
		if p.CreditAllotment != nil {
			v := int64(*p.CreditAllotment)
			allot = &v
		}
		var interval *string
		if p.Interval != nil {
			v := string(*p.Interval)
			interval = &v
		}
		var id string
		if err := t.QueryRow(ctx, `INSERT INTO plans (realm, code, name, kind, credit_allotment, pool, billing_interval, active, sort_order)
		                           VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
			string(p.Realm.Or()), p.Code, p.Name, string(p.Kind), allot, string(p.Pool.Or()), interval, p.Active, p.SortOrder).Scan(&id); err != nil {
			return mapWrite(err)
		}
		for _, cur := range sortedKeys(p.Prices) {
			if _, err := t.Exec(ctx, `INSERT INTO plan_prices (plan_id, currency, minor_units) VALUES ($1::uuid,$2,$3)`, id, cur, p.Prices[cur].MinorUnits); err != nil {
				return mapWrite(err)
			}
		}
		var err error
		out, err = planOf(ctx, t, p.Realm, p.Code)
		return err
	})
	return out, err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lockPlan takes the plan's row for the transaction: patches and mappings of one plan run one after the other.
func lockPlan(ctx context.Context, t pgx.Tx, realm md.Realm, code string) (id string, err error) {
	err = t.QueryRow(ctx, `SELECT id::text FROM plans WHERE realm = $1 AND code = $2 FOR UPDATE`, string(realm.Or()), code).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", notFound("plan %s in realm %s", code, realm.Or())
	}
	return id, err
}

// PatchPlan implements ports.ConfigStore.
func (w *ConfigWriter) PatchPlan(ctx context.Context, ch ports.ConfigChange, realm md.Realm, code string, patch ports.PlanPatch) (ports.PlanConfig, error) {
	var out ports.PlanConfig
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		id, err := lockPlan(ctx, t, realm, code)
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `UPDATE plans SET name = coalesce($2, name), active = coalesce($3, active), sort_order = coalesce($4, sort_order), updated_at = now()
		                          WHERE id = $1::uuid`, id, patch.Name, patch.Active, patch.SortOrder); err != nil {
			return mapWrite(err)
		}
		for _, cur := range sortedKeys(patch.Prices) {
			minor := patch.Prices[cur]
			if minor == nil {
				_, err = t.Exec(ctx, `DELETE FROM plan_prices WHERE plan_id = $1::uuid AND currency = $2`, id, cur)
			} else {
				_, err = t.Exec(ctx, `INSERT INTO plan_prices (plan_id, currency, minor_units) VALUES ($1::uuid,$2,$3)
				                      ON CONFLICT (plan_id, currency) DO UPDATE SET minor_units = EXCLUDED.minor_units`, id, cur, *minor)
			}
			if err != nil {
				return mapWrite(err)
			}
		}
		if out, err = planOf(ctx, t, realm, code); err != nil {
			return err
		}
		return out.Plan.Validate() // a patch cannot leave a plan that could not have been created
	})
	return out, err
}

// PutProviderPrice implements ports.ConfigStore.
func (w *ConfigWriter) PutProviderPrice(ctx context.Context, ch ports.ConfigChange, realm md.Realm, code string, pp domain.ProviderPrice) (bool, error) {
	if err := pp.Validate(); err != nil {
		return false, err
	}
	created := false
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		id, err := lockPlan(ctx, t, realm, code)
		if err != nil {
			return err
		}
		var one int
		if err := t.QueryRow(ctx, `SELECT 1 FROM provider_accounts WHERE id = $1`, pp.Account).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: there is no provider account %s", errInvalid, pp.Account)
		} else if err != nil {
			return err
		}
		if err := t.QueryRow(ctx, `SELECT 1 FROM plan_prices WHERE plan_id = $1::uuid AND currency = $2`, id, pp.Currency).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: plan %s has no price in %s, so a mapping for it would never sell", errInvalid, code, pp.Currency)
		} else if err != nil {
			return err
		}
		var have string
		err = t.QueryRow(ctx, `SELECT provider_price_id FROM plan_provider_prices WHERE plan_id = $1::uuid AND provider_account = $2 AND currency = $3`,
			id, pp.Account, pp.Currency).Scan(&have)
		switch {
		case err == nil && have == pp.ProviderPriceID:
			return nil // already so: nothing to write, nothing to audit
		case err == nil:
			return fmt.Errorf("%w: plan %s is already sold through %s in %s as %s: a subscriber still renews at that price, so it is not replaced — sell a new plan instead",
				errAlreadyExists, code, pp.Account, pp.Currency, have)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if _, err := t.Exec(ctx, `INSERT INTO plan_provider_prices (plan_id, provider_account, currency, provider_price_id) VALUES ($1::uuid,$2,$3,$4)`,
			id, pp.Account, pp.Currency, pp.ProviderPriceID); err != nil {
			return mapWrite(err)
		}
		created = true
		return nil
	})
	return created, err
}

// -------------------------------------------------------------------- accounts --

// Accounts implements ports.ConfigStore.
func (w *ConfigWriter) Accounts(ctx context.Context) ([]domain.ProviderAccount, error) {
	rows, err := w.pool.Query(ctx, `SELECT `+accountColumns+` FROM provider_accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("postgres: provider accounts: %w", err)
	}
	defer rows.Close()
	var out []domain.ProviderAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: provider accounts: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Account implements ports.ConfigStore.
func (w *ConfigWriter) Account(ctx context.Context, id string) (domain.ProviderAccount, bool, error) {
	return NewAccounts(w.pool).Account(ctx, id)
}

// PutAccount implements ports.ConfigStore.
func (w *ConfigWriter) PutAccount(ctx context.Context, ch ports.ConfigChange, a domain.ProviderAccount) (bool, error) {
	if err := a.Validate(); err != nil {
		return false, err
	}
	realms := make([]string, len(a.Realms))
	for i, r := range a.Realms {
		realms[i] = string(r.Or())
	}
	created := false
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		if err := lockKey(ctx, t, "account:"+a.ID); err != nil {
			return err
		}
		var provider string
		err := t.QueryRow(ctx, `SELECT provider FROM provider_accounts WHERE id = $1 FOR UPDATE`, a.ID).Scan(&provider)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			created = true
			_, err = t.Exec(ctx, `INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref, active) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				a.ID, string(a.Provider), realms, a.Mode, a.SecretRef, a.WebhookSecretRef, a.Active)
		case err != nil:
			return err
		case provider != string(a.Provider):
			return fmt.Errorf("%w: account %s belongs to provider %s, whose webhooks arrive at a route that names it", errInvalid, a.ID, provider)
		default:
			_, err = t.Exec(ctx, `UPDATE provider_accounts SET realms = $2, mode = $3, secret_ref = $4, webhook_secret_ref = $5, active = $6 WHERE id = $1`,
				a.ID, realms, a.Mode, a.SecretRef, a.WebhookSecretRef, a.Active)
		}
		return mapWrite(err)
	})
	return created, err
}

// ------------------------------------------------------------------- customers --

// Customer implements ports.ConfigStore.
func (w *ConfigWriter) Customer(ctx context.Context, s md.Scope, account string) (domain.Customer, bool, error) {
	return NewCustomers(w.pool).Get(ctx, s, account)
}

// PutCustomer implements ports.ConfigStore.
func (w *ConfigWriter) PutCustomer(ctx context.Context, ch ports.ConfigChange, c domain.Customer) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	c.Scope = c.Scope.Normalized()
	created := false
	err := w.tx(ctx, ch, func(t pgx.Tx) error {
		var one int
		if err := t.QueryRow(ctx, `SELECT 1 FROM provider_accounts WHERE id = $1`, c.Account).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: there is no provider account %s", errInvalid, c.Account)
		} else if err != nil {
			return err
		}
		if err := lockKey(ctx, t, "customer:"+c.Account+"/"+c.Scope.Tag()); err != nil {
			return err
		}
		var haveID string
		var haveEmail *string
		err := t.QueryRow(ctx, `SELECT provider_customer_id, billing_email FROM billing_customers
		                        WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND provider_account = $4 FOR UPDATE`,
			realmOf(c.Scope), c.Scope.Kind, c.Scope.ID, c.Account).Scan(&haveID, &haveEmail)
		action := "customer.link"
		before := map[string]string(nil)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			created = true
			_, err = t.Exec(ctx, `INSERT INTO billing_customers (realm, scope_kind, scope_id, provider_account, provider_customer_id, billing_email) VALUES ($1,$2,$3,$4,$5,$6)`,
				realmOf(c.Scope), c.Scope.Kind, c.Scope.ID, c.Account, c.ProviderCustomerID, nilIfEmpty(c.BillingEmail))
			if err != nil {
				return mapWrite(err)
			}
		case err != nil:
			return err
		case haveID != c.ProviderCustomerID:
			return fmt.Errorf("%w: %s is already linked to provider customer %s on %s, and re-pointing it would orphan that customer's webhooks", errAlreadyExists, c.Scope.Tag(), haveID, c.Account)
		case orEmpty(haveEmail) == c.BillingEmail:
			return nil // already so: nothing to write, nothing to audit
		default:
			action = "customer.update"
			before = map[string]string{"provider_customer_id": haveID, "billing_email": orEmpty(haveEmail)}
			if _, err := t.Exec(ctx, `UPDATE billing_customers SET billing_email = $5, updated_at = now()
			                          WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND provider_account = $4`,
				realmOf(c.Scope), c.Scope.Kind, c.Scope.ID, c.Account, nilIfEmpty(c.BillingEmail)); err != nil {
				return mapWrite(err)
			}
		}
		return auditCustomer(ctx, t, ch, c, action, before)
	})
	return created, err
}

// auditCustomer records a customer change in the same transaction. billing_customers has no trigger, because checkout writes it
// too; this is the operator's half.
func auditCustomer(ctx context.Context, t pgx.Tx, ch ports.ConfigChange, c domain.Customer, action string, before map[string]string) error {
	after := map[string]string{"provider_customer_id": c.ProviderCustomerID, "billing_email": c.BillingEmail}
	_, err := t.Exec(ctx, `INSERT INTO audit_log (realm, actor, action, subject_kind, subject_id, before, after, reason)
	                       VALUES ($1,$2,$3,'billing_customers',$4,$5::jsonb,$6::jsonb,$7)`,
		realmOf(c.Scope), actorOf(ch), action, c.Scope.Tag()+"/"+c.Account, jsonOrNull(before), jsonOrNull(after), ch.Reason)
	if err != nil {
		return fmt.Errorf("postgres: audit the customer link: %w", err)
	}
	return nil
}

// jsonOrNull is a value for a jsonb column: its JSON, or NULL for nothing.
func jsonOrNull(m map[string]string) any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m) // a map of strings always marshals
	return string(b)
}
