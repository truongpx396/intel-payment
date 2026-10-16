//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/billing/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// What only the database can say: the schema the adapter depends on, and what its constraints do to a bad write.

func TestTheSchemaCarriesWhatTheStoresNeed(t *testing.T) {
	t.Parallel()
	pool := db(t)
	for _, col := range [][2]string{{"payments", "dispute_clawed"}, {"subscriptions", "grace_lapsed_at"},
		{"auto_recharge", "day"}, {"auto_recharge", "charges_today"}, {"auto_recharge", "inflight_n"}, {"auto_recharge", "inflight_since"}} {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`, col[0], col[1]).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s.%s is missing (%v): a chargeback could not be reversed exactly / a grace announced once / a top-up bounded", col[0], col[1], err)
		}
	}
	// A rejected parked delivery has a status of its own, and the body of anything not parked must be gone.
	exec(t, pool, `INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref) VALUES ('a','fake','{default}','test','env:X','env:Y')`)
	exec(t, pool, `INSERT INTO payment_events (provider_account, provider, provider_event_id, event_type, payload_hash, status) VALUES ('a','fake','e1','parked','h','rejected')`)
	if _, err := pool.Exec(context.Background(), `INSERT INTO payment_events (provider_account, provider, provider_event_id, event_type, payload_hash, status, parked_body) VALUES ('a','fake','e2','x','h','received','\x00')`); err == nil {
		t.Error("a verified event kept a raw body: the body is deleted the moment it verifies")
	}
}

func TestAPlanThatDoesNotExistIsRefusedNotInvented(t *testing.T) {
	t.Parallel()
	pool := world(t)
	scope := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}
	sub := domain.Sub{Scope: scope, PlanCode: "ghost", Account: contracts.AccountID, ProviderSubscriptionID: "sub-1", Status: domain.Active}
	if _, err := pgadapter.NewSubs(pool).ApplyIfNewer(context.Background(), sub, contracts.Start); !errors.Is(err, domain.ErrUnknownPrice) {
		t.Errorf("a subscription on a plan nobody sells: %v, want ErrUnknownPrice", err)
	}
	pay := domain.Payment{Scope: scope, Account: contracts.AccountID, ProviderPaymentID: "pay-1", PlanCode: "ghost", Kind: "one_time",
		Amount: domain.Money{MinorUnits: 1, Currency: "USD"}, Pool: md.GeneralPool, Status: domain.Succeeded, IdemKey: "pay-1", CreatedAt: contracts.Start}
	if err := pgadapter.NewPayments(pool).Upsert(context.Background(), pay); !errors.Is(err, domain.ErrUnknownPrice) {
		t.Errorf("a payment for a plan nobody sells: %v, want ErrUnknownPrice", err)
	}
}

func TestAnAccountOfAnotherRealmCannotBeUsedForAForeignPayment(t *testing.T) {
	t.Parallel()
	pool := world(t)
	pay := domain.Payment{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}, Account: "no-such-account", ProviderPaymentID: "pay-1", Kind: "one_time",
		Amount: domain.Money{MinorUnits: 1, Currency: "USD"}, Pool: md.GeneralPool, Status: domain.Succeeded, IdemKey: "pay-1", CreatedAt: contracts.Start}
	if err := pgadapter.NewPayments(pool).Upsert(context.Background(), pay); err == nil {
		t.Error("a payment was stored against a provider account that does not exist")
	}
}

func TestTimesComeBackInUTCToTheMicrosecond(t *testing.T) {
	t.Parallel()
	pool := world(t)
	p := pgadapter.NewPayments(pool)
	at := time.Date(2026, 3, 1, 12, 0, 0, 123456000, time.FixedZone("ICT", 7*3600))
	pay := domain.Payment{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}, Account: contracts.AccountID, ProviderPaymentID: "pay-1", Kind: "one_time",
		Amount: domain.Money{MinorUnits: 1, Currency: "USD"}, Pool: md.GeneralPool, Status: domain.Succeeded, IdemKey: "pay-1", CreatedAt: at}
	if err := p.Upsert(context.Background(), pay); err != nil {
		t.Fatal(err)
	}
	got, _, _ := p.Get(context.Background(), contracts.AccountID, "pay-1")
	if got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(at) {
		t.Errorf("CreatedAt = %v (%v), want %v in UTC", got.CreatedAt, got.CreatedAt.Location(), at)
	}
}
