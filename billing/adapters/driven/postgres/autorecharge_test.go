//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/billing/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

func instruction(id string) domain.AutoRecharge {
	return domain.AutoRecharge{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: id}, Enabled: true, Pool: md.GeneralPool, Threshold: 1000,
		PlanCode: contracts.PackCode, Currency: "USD", Account: contracts.AccountID, PaymentMethodRef: "pm_1", MaxPerDay: 3}
}

// watchesOf is the balance watches of a scope, as "pool|key" → threshold.
func watchesOf(t testing.TB, pool *pgxpool.Pool, s domain.Scope) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT pool, key, threshold FROM balance_watches WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`,
		string(s.Realm.Or()), s.Kind, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var p, k string
		var th int64
		if err := rows.Scan(&p, &k, &th); err != nil {
			t.Fatal(err)
		}
		out[p+"|"+k] = th
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameWatches(got, want map[string]int64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return false
		}
	}
	return true
}

// Metering owns the mechanism — the hot function flags a crossing of a balance watch and the writer publishes
// billing.balance.low — and billing owns what it triggers. So an instruction to top up IS a balance watch, kept in the same
// transaction as the instruction, and the host's own watches beside it are never touched.
func TestAnInstructionKeepsABalanceWatchSoTheWriterPublishesBalanceLow(t *testing.T) {
	t.Parallel()
	pool := world(t)
	store := pgadapter.NewAutoRecharges(pool)
	a := instruction("r1")
	exec(t, pool, `INSERT INTO balance_watches (realm, scope_kind, scope_id, pool, key, threshold) VALUES ('default','workspace','r1','general','low_credits',5000)`)
	hosts := map[string]int64{"general|low_credits": 5000}
	with := func(extra map[string]int64) map[string]int64 {
		out := map[string]int64{}
		for k, v := range hosts {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	check := func(step string, want map[string]int64) {
		t.Helper()
		if got := watchesOf(t, pool, a.Scope); !sameWatches(got, want) {
			t.Fatalf("%s: watches = %v, want %v", step, got, want)
		}
	}
	ctx := context.Background()

	if err := store.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	check("an enabled instruction", with(map[string]int64{"general|auto_recharge": 1000}))

	a.Pool, a.Threshold = "promo", 300
	if err := store.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	check("moved to another pool and threshold: the old watch goes, the new one comes", with(map[string]int64{"promo|auto_recharge": 300}))

	a.Enabled, a.DisabledReason = false, "paused by the customer"
	if err := store.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	check("switched off by its owner", with(nil))

	a.Enabled, a.DisabledReason = true, ""
	if err := store.Put(ctx, a); err != nil {
		t.Fatal(err)
	}
	check("switched on again", with(map[string]int64{"promo|auto_recharge": 300}))

	// Three failed charges in a row switch it off, and with it the watch: nothing is left to trigger a charge that cannot be made.
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for i := range 3 {
		if _, refusal, err := store.Claim(ctx, a.Scope, now.Add(time.Duration(i)*time.Minute), 10*time.Minute); err != nil || refusal != "" {
			t.Fatalf("claim %d: %q %v", i, refusal, err)
		}
		if _, err := store.Settle(ctx, a.Scope, false, "card_declined", now); err != nil {
			t.Fatal(err)
		}
	}
	check("switched off by three failures", with(nil))
}

func TestAnInstructionForAnAccountThatDoesNotExistIsRefusedAndLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	pool := world(t)
	store := pgadapter.NewAutoRecharges(pool)
	a := instruction("r1")
	a.Account = "no-such-account"
	if err := store.Put(context.Background(), a); err == nil {
		t.Fatal("an instruction to charge an account that does not exist was stored")
	}
	if _, ok, err := store.Get(context.Background(), a.Scope); ok || err != nil {
		t.Fatalf("Get = %v, %v: the refused instruction was stored", ok, err)
	}
	if got := watchesOf(t, pool, a.Scope); len(got) != 0 {
		t.Fatalf("watches = %v: a watch was left for an instruction that was refused", got)
	}
}

// The ledger columns are the database's own guard: whatever the application does, a day cannot hold more slots than were
// handed out, and a slot cannot be in flight without a time.
func TestTheLedgerColumnsRefuseWhatTheRulesNeverWrite(t *testing.T) {
	t.Parallel()
	pool := world(t)
	store := pgadapter.NewAutoRecharges(pool)
	a := instruction("r1")
	if err := store.Put(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"a slot in flight with no time":             `UPDATE auto_recharge SET inflight_n = 1, charges_today = 1 WHERE scope_id = 'r1'`,
		"a slot in flight beyond the day's charges": `UPDATE auto_recharge SET inflight_n = 2, charges_today = 1, inflight_since = now() WHERE scope_id = 'r1'`,
		"more charges a day than the schema allows": `UPDATE auto_recharge SET max_per_day = 51 WHERE scope_id = 'r1'`,
	} {
		if _, err := pool.Exec(context.Background(), sql); err == nil {
			t.Errorf("%s was accepted by the database", name)
		}
	}
}
