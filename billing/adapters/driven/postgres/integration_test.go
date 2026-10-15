//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	pgadapter "github.com/truongpx396/intel-payment/billing/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	"github.com/truongpx396/intel-payment/internal/pgtest"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
)

var pg *pgtest.Postgres

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if pg, err = pgtest.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = pg.Terminate(ctx)
	if code == 0 {
		if err := goleak.Find(
			goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// db gives a test its own migrated database and a pool on it.
func db(t testing.TB) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), pg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func exec(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seedAccount stores a provider account.
func seedAccount(t testing.TB, pool *pgxpool.Pool, a domain.ProviderAccount) {
	t.Helper()
	realms := make([]string, len(a.Realms))
	for i, r := range a.Realms {
		realms[i] = string(r.Or())
	}
	exec(t, pool, `INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref, active) VALUES ($1,$2,$3,$4,$5,$6,$7)
	               ON CONFLICT (id) DO UPDATE SET provider=$2, realms=$3, mode=$4, secret_ref=$5, webhook_secret_ref=$6, active=$7`,
		a.ID, string(a.Provider), realms, a.Mode, a.SecretRef, a.WebhookSecretRef, a.Active)
}

// seedPlan puts a plan on sale: the plan, its prices, its entitlements and the provider price that sells it.
func seedPlan(t testing.TB, pool *pgxpool.Pool, w contracts.WorldPlan) {
	t.Helper()
	p := w.Plan
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
	if err := pool.QueryRow(context.Background(), `INSERT INTO plans (realm, code, name, kind, credit_allotment, pool, billing_interval, active, sort_order)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (realm, code) DO UPDATE SET name=$3, kind=$4, credit_allotment=$5, pool=$6, billing_interval=$7, active=$8, sort_order=$9
		RETURNING id::text`, string(p.Realm.Or()), p.Code, p.Name, string(p.Kind), allot, string(p.Pool.Or()), interval, p.Active, p.SortOrder).Scan(&id); err != nil {
		t.Fatalf("seed plan %s: %v", p.Code, err)
	}
	for cur, m := range p.Prices {
		exec(t, pool, `INSERT INTO plan_prices (plan_id, currency, minor_units) VALUES ($1::uuid,$2,$3) ON CONFLICT (plan_id, currency) DO UPDATE SET minor_units=$3`, id, cur, m.MinorUnits)
	}
	exec(t, pool, `DELETE FROM plan_entitlements WHERE plan_id = $1::uuid`, id)
	for _, e := range p.Entitlements {
		exec(t, pool, `INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_bool, value_int, value_text) VALUES ($1::uuid,$2,$3,$4,$5,$6)`, id, e.Key, e.Kind, e.Bool, e.Int, e.Text)
	}
	if w.ProviderPriceID != "" {
		exec(t, pool, `INSERT INTO plan_provider_prices (plan_id, provider_account, currency, provider_price_id) VALUES ($1::uuid,$2,$3,$4)
		               ON CONFLICT (plan_id, provider_account, currency) DO UPDATE SET provider_price_id=$4`, id, w.Account, w.Currency, w.ProviderPriceID)
	}
}

// world is a database with the harness's provider accounts and plans in place.
func world(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := db(t)
	for _, a := range contracts.WorldAccounts() {
		seedAccount(t, pool, a)
	}
	for _, w := range contracts.WorldPlans() {
		seedPlan(t, pool, w)
	}
	return pool
}

func TestPostgresInboxHonoursTheInboxContract(t *testing.T) {
	t.Parallel()
	contracts.InboxContract(t, func(t testing.TB, clock *mcontracts.FakeClock, maxAttempts int) ports.Inbox {
		pool := world(t)
		return pgadapter.NewInbox(pool, clock, pgadapter.InboxOptions{MaxAttempts: maxAttempts})
	})
}

func TestPostgresPaymentsHonourThePaymentsContract(t *testing.T) {
	t.Parallel()
	contracts.PaymentsContract(t, func(t testing.TB) ports.Payments { return pgadapter.NewPayments(world(t)) })
}

func TestPostgresSubscriptionsHonourTheSubsContract(t *testing.T) {
	t.Parallel()
	contracts.SubsContract(t, func(t testing.TB) ports.Subs { return pgadapter.NewSubs(world(t)) })
}

func TestPostgresCustomersHonourTheCustomersContract(t *testing.T) {
	t.Parallel()
	contracts.CustomersContract(t, func(t testing.TB) ports.Customers { return pgadapter.NewCustomers(world(t)) })
}

func TestPostgresCatalogHonoursTheCatalogContract(t *testing.T) {
	t.Parallel()
	contracts.CatalogContract(t, func(t testing.TB) (ports.Catalog, contracts.CatalogSeeder) {
		pool := db(t)
		for _, a := range contracts.WorldAccounts() {
			seedAccount(t, pool, a)
		}
		return pgadapter.NewCatalog(pool), func(w contracts.WorldPlan) { seedPlan(t, pool, w) }
	})
}

func TestPostgresAccountsHonourTheAccountsContract(t *testing.T) {
	t.Parallel()
	contracts.AccountsContract(t, func(t testing.TB) (ports.Accounts, contracts.AccountSeeder) {
		pool := db(t)
		return pgadapter.NewAccounts(pool), func(a domain.ProviderAccount) { seedAccount(t, pool, a) }
	})
}
