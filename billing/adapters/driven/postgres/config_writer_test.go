//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/billing/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// What only the database can say about the writer: who changed configuration, and why, is written by the DATABASE in the same
// transaction as the change (D37) — so it is there for a change made through the admin API, with the person's name and the
// credential it came through; a customer link, which decides where a payment lands, is attributed too; and a change the
// database refused leaves no trace of having happened.

type auditRow struct{ actor, action, subject, reason string }

func auditRows(t *testing.T, pool *pgxpool.Pool, kind string) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT actor, action, coalesce(subject_id,''), coalesce(reason,'') FROM audit_log WHERE subject_kind = $1 ORDER BY created_at, id`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.action, &r.subject, &r.reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writerWorld(t *testing.T) (*pgadapter.ConfigWriter, *pgxpool.Pool) {
	t.Helper()
	pool := db(t)
	for _, a := range contracts.WorldAccounts() {
		seedAccount(t, pool, a)
	}
	return pgadapter.NewConfigWriter(pool), pool
}

func TestAConfigurationChangeNamesThePersonTheReasonAndTheCredential(t *testing.T) {
	t.Parallel()
	w, pool := writerWorld(t)
	ctx := context.Background()
	ch := ports.ConfigChange{Actor: "alice", Reason: "launch the pro plan", Via: "billing-admin"}
	month := domain.Monthly
	credits := domain.Credits(2000)
	plan := domain.Plan{Realm: md.DefaultRealm, Code: "pro", Name: "Pro", Kind: domain.Subscription, CreditAllotment: &credits, Pool: "plan", Interval: &month, Active: true,
		Prices: map[string]domain.Money{"USD": {MinorUnits: 2000, Currency: "USD"}}}
	if _, err := w.CreatePlan(ctx, ch, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PutProviderPrice(ctx, ch, md.DefaultRealm, "pro", domain.ProviderPrice{Account: contracts.AccountID, Currency: "USD", ProviderPriceID: "price_pro"}); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := w.PatchPlan(ctx, ports.ConfigChange{Actor: "bob", Reason: "retire it", Via: "billing-admin"}, md.DefaultRealm, "pro", ports.PlanPatch{Active: &off}); err != nil {
		t.Fatal(err)
	}
	acct := domain.ProviderAccount{ID: "stripe-new", Provider: "fake", Realms: []md.Realm{md.DefaultRealm}, Mode: "test", SecretRef: "env:K", WebhookSecretRef: "env:W", Active: true}
	if _, err := w.PutAccount(ctx, ch, acct); err != nil {
		t.Fatal(err)
	}

	for kind, want := range map[string][]auditRow{
		"plans":                {{"alice (via billing-admin)", "config.insert", "", "launch the pro plan"}, {"bob (via billing-admin)", "config.update", "", "retire it"}},
		"plan_prices":          {{"alice (via billing-admin)", "config.insert", "", "launch the pro plan"}},
		"plan_provider_prices": {{"alice (via billing-admin)", "config.insert", "", "launch the pro plan"}},
		"provider_accounts":    {{"alice (via billing-admin)", "config.insert", "stripe-new", "launch the pro plan"}},
	} {
		got := auditRows(t, pool, kind)
		if kind == "provider_accounts" {
			// The world's own accounts were seeded straight into SQL, and are attributed to the role that made them.
			mine := got[:0:0]
			for _, r := range got {
				if r.subject == "stripe-new" {
					mine = append(mine, r)
				}
			}
			got = mine
		} else {
			for i := range got {
				got[i].subject = "" // a plan, and a row of one, is named by the plan's uuid
			}
		}
		if len(got) < len(want) {
			t.Errorf("%s: %d audit rows, want at least %d: %+v", kind, len(got), len(want), got)
			continue
		}
		for i, wr := range want {
			if got[i] != wr {
				t.Errorf("%s audit row %d = %+v, want %+v", kind, i, got[i], wr)
			}
		}
	}
}

func TestACustomerLinkIsAttributedAndAnIdenticalRepeatIsNot(t *testing.T) {
	t.Parallel()
	w, pool := writerWorld(t)
	ctx := context.Background()
	ch := ports.ConfigChange{Actor: "carol", Reason: "migrate the customer from the old billing", Via: "billing-admin"}
	c := domain.Customer{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}, Account: contracts.AccountID, ProviderCustomerID: "cus_old"}
	if _, err := w.PutCustomer(ctx, ch, c); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PutCustomer(ctx, ch, c); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, pool, "billing_customers")
	if len(rows) != 1 {
		t.Fatalf("%d audit rows for one link and an identical repeat, want 1: %+v", len(rows), rows)
	}
	if r := rows[0]; r.actor != "carol (via billing-admin)" || r.action != "customer.link" || r.reason != "migrate the customer from the old billing" || r.subject != "default/workspace:w1/"+contracts.AccountID {
		t.Errorf("audit row = %+v", r)
	}
}

func TestAChangeTheDatabaseRefusedLeavesNoTrace(t *testing.T) {
	t.Parallel()
	w, pool := writerWorld(t)
	ctx := context.Background()
	ch := ports.ConfigChange{Actor: "alice", Reason: "twice", Via: "ci"}
	credits := domain.Credits(1)
	plan := domain.Plan{Realm: md.DefaultRealm, Code: "pack", Name: "Pack", Kind: domain.OneTime, CreditAllotment: &credits, Pool: md.GeneralPool, Active: true,
		Prices: map[string]domain.Money{"USD": {MinorUnits: 100, Currency: "USD"}}}
	if _, err := w.CreatePlan(ctx, ch, plan); err != nil {
		t.Fatal(err)
	}
	before := len(auditRows(t, pool, "plans")) + len(auditRows(t, pool, "plan_prices"))
	if _, err := w.CreatePlan(ctx, ch, plan); err == nil {
		t.Fatal("a plan was created twice")
	}
	if after := len(auditRows(t, pool, "plans")) + len(auditRows(t, pool, "plan_prices")); after != before {
		t.Errorf("a refused creation left audit rows (%d before, %d after): the audit says a change happened that did not", before, after)
	}
	// The database's own check, not the writer's: a plan that renews, sold once, cannot be a pack with an interval.
	if _, err := pool.Exec(ctx, `UPDATE plans SET kind = 'subscription', billing_interval = NULL WHERE code = 'pack'`); err == nil {
		t.Error("the database let a subscription with no interval in")
	}
}
