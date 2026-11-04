//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R104: a limit whose size comes from an entitlement is sized by a NUMBER. A flag or an enum named as the size is refused when the
// limit is configured and not when the first customer's Admit meets it as a 503 — or, under fail_open, as a scope with no
// ceiling. The database refuses it, so it holds for the admin API and for a write that bypasses it.

const insertLimit = `INSERT INTO limits (realm, name, unit, max, max_entitlement, window_kind, deny_code)
                     VALUES ('t1', $1, 'credit', 10, $2, 'daily', 'limit_reached')`

func declare(t *testing.T, pool *pgxpool.Pool, key, kind string) {
	t.Helper()
	var order any
	if kind == "enum" {
		order = []string{"a", "b"}
	}
	exec(t, pool, `INSERT INTO entitlement_keys (realm, key, kind, enum_order) VALUES ('t1', $1, $2, $3)
	               ON CONFLICT (realm, key) DO UPDATE SET kind = $2, enum_order = $3`, key, kind, order)
}

func refused(t *testing.T, err error, constraint string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != constraint {
		t.Fatalf("want SQLSTATE 23514 (check_violation) %s, got %v", constraint, err)
	}
}

func TestALimitCannotBeSizedByAKeyThatIsNotAQuota(t *testing.T) {
	t.Parallel()
	pool := db(t)
	ctx := context.Background()
	declare(t, pool, "daily_user_credits", "quota")
	declare(t, pool, "sso", "flag")
	declare(t, pool, "tier", "enum")

	exec(t, pool, insertLimit, "sized", "daily_user_credits")
	for name, key := range map[string]string{"a flag": "sso", "an enum": "tier"} {
		_, err := pool.Exec(ctx, insertLimit, "bad-"+key, key)
		if err == nil {
			t.Errorf("%s was accepted as the size of a limit", name)
			continue
		}
		refused(t, err, "limits_max_entitlement_is_quota")
	}
	// Re-pointing a limit that exists at one is refused too.
	_, err := pool.Exec(ctx, `UPDATE limits SET max_entitlement = 'sso' WHERE name = 'sized'`)
	refused(t, err, "limits_max_entitlement_is_quota")

	// A key not declared yet is accepted: keys can be declared in any order, and until then nothing grants it and the limit's own
	// max applies.
	exec(t, pool, insertLimit, "early", "not_declared_yet")
	// And a limit that names no entitlement is not the trigger's business.
	exec(t, pool, `INSERT INTO limits (realm, name, unit, max, window_kind, deny_code) VALUES ('t1','plain','credit',10,'daily','limit_reached')`)

	// Another realm's declaration is another realm's: t1's limit is judged by t1's key.
	exec(t, pool, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('other', 'seats', 'flag')`)
	exec(t, pool, insertLimit, "seats-is-a-flag-only-elsewhere", "seats")
}

// The other direction: a key a limit sizes itself by cannot be turned into something that is not a number underneath it.
func TestAQuotaKeyALimitNamesCannotBeRedeclaredAsAFlagOrAnEnum(t *testing.T) {
	t.Parallel()
	pool := db(t)
	ctx := context.Background()
	declare(t, pool, "daily_user_credits", "quota")
	declare(t, pool, "unused", "quota")
	exec(t, pool, insertLimit, "sized", "daily_user_credits")

	_, err := pool.Exec(ctx, `UPDATE entitlement_keys SET kind = 'flag' WHERE realm = 't1' AND key = 'daily_user_credits'`)
	refused(t, err, "entitlement_keys_quota_in_use")
	_, err = pool.Exec(ctx, `UPDATE entitlement_keys SET kind = 'enum', enum_order = ARRAY['a'] WHERE realm = 't1' AND key = 'daily_user_credits'`)
	refused(t, err, "entitlement_keys_quota_in_use")

	// A key nothing sizes by is free to change, and the key is still a quota where it was.
	exec(t, pool, `UPDATE entitlement_keys SET kind = 'flag' WHERE realm = 't1' AND key = 'unused'`)
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM entitlement_keys WHERE realm = 't1' AND key = 'daily_user_credits'`).Scan(&kind); err != nil || kind != "quota" {
		t.Errorf("the refused change left kind = %q, %v", kind, err)
	}
	// Once the limit is gone the key may change.
	exec(t, pool, `DELETE FROM limits WHERE name = 'sized'`)
	exec(t, pool, `UPDATE entitlement_keys SET kind = 'flag' WHERE realm = 't1' AND key = 'daily_user_credits'`)
}
