//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/entitlement/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/internal/pgtest"
	md "github.com/truongpx396/intel-payment/metering/domain"
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

// seed writes the rows an operator's admin API would, straight into the schema. It is what a suite uses to arrange a store it
// is not testing the writes of.
type seed struct{ pool *pgxpool.Pool }

var _ contracts.Seeder = seed{}

func (s seed) DeclareKey(t testing.TB, realm md.Realm, d domain.KeyDecl) {
	t.Helper()
	var order []string
	if d.Kind == domain.Enum {
		order = d.EnumOrder
	}
	exec(t, s.pool, `INSERT INTO entitlement_keys (realm, key, kind, enum_order) VALUES ($1,$2,$3,$4)
	                 ON CONFLICT (realm, key) DO UPDATE SET kind = $3, enum_order = $4`, string(realm.Or()), string(d.Key), string(d.Kind), order)
}

func insertValue(t testing.TB, pool *pgxpool.Pool, sql string, args []any, v domain.Value) {
	t.Helper()
	var b *bool
	var i *int64
	var x *string
	switch v.Kind {
	case domain.Flag:
		b = &v.Bool
	case domain.Quota:
		i = &v.Int
	case domain.Enum:
		x = &v.Text
	}
	exec(t, pool, sql, append(args, string(v.Kind), b, i, x)...)
}

func (s seed) PutPlan(t testing.TB, realm md.Realm, code string, order int, ents ...domain.Entitlement) {
	t.Helper()
	var id string
	err := s.pool.QueryRow(context.Background(), `INSERT INTO plans (realm, code, name, kind, billing_interval, sort_order, active)
	        VALUES ($1,$2,$2,'subscription','month',$3,true)
	        ON CONFLICT (realm, code) DO UPDATE SET sort_order = $3, active = true RETURNING id`, string(realm.Or()), code, order).Scan(&id)
	if err != nil {
		t.Fatalf("PutPlan(%s): %v", code, err)
	}
	exec(t, s.pool, `DELETE FROM plan_entitlements WHERE plan_id = $1`, id)
	for _, e := range ents {
		insertValue(t, s.pool, `INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_bool, value_int, value_text)
		                        VALUES ($1,$2,$3,$4,$5,$6)`, []any{id, string(e.Key)}, e.Value)
	}
}

func (s seed) RetirePlan(t testing.TB, realm md.Realm, code string) {
	t.Helper()
	exec(t, s.pool, `UPDATE plans SET active = false WHERE realm = $1 AND code = $2`, string(realm.Or()), code)
}

func (s seed) PutDefaults(t testing.TB, realm md.Realm, ents ...domain.Entitlement) {
	t.Helper()
	exec(t, s.pool, `DELETE FROM realm_defaults WHERE realm = $1`, string(realm.Or()))
	for _, e := range ents {
		insertValue(t, s.pool, `INSERT INTO realm_defaults (realm, feature_key, value_kind, value_bool, value_int, value_text)
		                        VALUES ($1,$2,$3,$4,$5,$6)`, []any{string(realm.Or()), string(e.Key)}, e.Value)
	}
}

func (s seed) Subscribe(t testing.TB, sc md.Scope, plan string, status domain.SubStatus, grace *time.Time) {
	t.Helper()
	sc = sc.Normalized()
	exec(t, s.pool, `INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref)
	                 VALUES ('acct','stripe',ARRAY['t1','other','default'],'test','env:K','env:W') ON CONFLICT (id) DO NOTHING`)
	s.Unsubscribe(t, sc)
	tag, err := s.pool.Exec(context.Background(), `INSERT INTO subscriptions
	        (realm, scope_kind, scope_id, plan_id, provider_account, provider_subscription_id, status, grace_until)
	        SELECT $1, $2, $3, p.id, 'acct', gen_random_uuid()::text, $5, $6 FROM plans p WHERE p.realm = $1 AND p.code = $4`,
		string(sc.Realm), sc.Kind, sc.ID, plan, string(status), grace)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("Subscribe(%s): %v (rows %d): the plan must exist in the realm", plan, err, tag.RowsAffected())
	}
}

func (s seed) Unsubscribe(t testing.TB, sc md.Scope) {
	t.Helper()
	sc = sc.Normalized()
	exec(t, s.pool, `UPDATE subscriptions SET status = 'canceled', canceled_at = now()
	                 WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND status IN ('trialing','active','past_due','paused')`,
		string(sc.Realm), sc.Kind, sc.ID)
}

func TestPostgresStoreHonoursTheStoreContract(t *testing.T) {
	t.Parallel()
	contracts.StoreContract(t, func(t testing.TB) *contracts.Backend {
		pool := db(t)
		return &contracts.Backend{Store: postgres.New(pool), Seed: seed{pool}}
	})
}

func TestEntitlerOverPostgresHonoursTheEntitlerContract(t *testing.T) {
	t.Parallel()
	contracts.EntitlerContract(t, func(t testing.TB) *contracts.World {
		pool := db(t)
		store := postgres.New(pool)
		faults := contracts.NewBreakable(store)
		clock := contracts.NewFakeClock(contracts.T0)
		e, err := app.New(app.Deps{Store: faults, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		return &contracts.World{
			Entitler: e, Store: store, Faults: faults, Seed: seed{pool}, Clock: clock,
			Scope: md.Scope{Realm: "t1", Kind: "org", ID: "o1"},
		}
	})
}
