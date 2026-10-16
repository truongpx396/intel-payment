package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ---- admin ----
//
// The admin API is how a person changes what is sold and who it is sold through (FR-049). The writer underneath keeps the data
// honest; these are the rules above it: every change names a person and a reason, a provider account is only accepted if this
// deployment can speak to it and read its secrets, and nothing is linked to an account that does not serve the realm.

var ctx = context.Background()

type adminWorld struct {
	admin  ports.Admin
	writer *contracts.MemConfigWriter
}

func newAdmin(t testing.TB) adminWorld {
	t.Helper()
	w := contracts.NewMemConfigWriter(contracts.WorldAccounts()...)
	a, err := app.NewAdmin(app.AdminDeps{Writer: w, Providers: []domain.Provider{contracts.ProviderName}, SecretSchemes: []string{"env", "file"}})
	if err != nil {
		t.Fatal(err)
	}
	return adminWorld{a, w}
}

var alice = ports.ConfigChange{Actor: "alice", Reason: "launch", Via: "ci"}

func pack(realm md.Realm, code string) domain.Plan {
	c := domain.Credits(500)
	return domain.Plan{Realm: realm, Code: code, Name: code, Kind: domain.OneTime, CreditAllotment: &c, Pool: md.GeneralPool, Active: true,
		Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}}}
}

func TestEveryAdminWriteNamesAPersonAndAReasonAndWritesNothingWithoutBoth(t *testing.T) {
	t.Parallel()
	for label, ch := range map[string]ports.ConfigChange{
		"no actor": {Reason: "launch"}, "a blank actor": {Actor: "  ", Reason: "launch"}, "no reason": {Actor: "alice"}, "a blank reason": {Actor: "alice", Reason: "\t"},
	} {
		w := newAdmin(t)
		renamed := "x"
		if _, err := w.admin.CreatePlan(ctx, ch, pack(md.DefaultRealm, "pack")); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: CreatePlan = %v, want ErrInvalid", label, err)
		}
		if _, err := w.admin.PatchPlan(ctx, ch, md.DefaultRealm, "pack", ports.PlanPatch{Name: &renamed}); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: PatchPlan = %v, want ErrInvalid", label, err)
		}
		if _, err := w.admin.PutProviderPrice(ctx, ch, md.DefaultRealm, "pack", domain.ProviderPrice{Account: contracts.AccountID, Currency: "USD", ProviderPriceID: "p"}); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: PutProviderPrice = %v, want ErrInvalid", label, err)
		}
		acct := domain.ProviderAccount{ID: "a", Provider: contracts.ProviderName, Realms: []md.Realm{md.DefaultRealm}, Mode: "test", SecretRef: "env:K", WebhookSecretRef: "env:W", Active: true}
		if _, err := w.admin.PutAccount(ctx, ch, acct); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: PutAccount = %v, want ErrInvalid", label, err)
		}
		cust := domain.Customer{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w"}, Account: contracts.AccountID, ProviderCustomerID: "cus"}
		if _, err := w.admin.PutCustomer(ctx, ch, cust); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: PutCustomer = %v, want ErrInvalid", label, err)
		}
		if n := len(w.writer.Changes()); n != 0 {
			t.Errorf("%s: %d changes were written without a person and a reason", label, n)
		}
	}
}

func TestAnAdminChangeIsPassedOnWithItsAttribution(t *testing.T) {
	t.Parallel()
	w := newAdmin(t)
	if _, err := w.admin.CreatePlan(ctx, alice, pack(md.DefaultRealm, "pack")); err != nil {
		t.Fatal(err)
	}
	if got := w.writer.Changes(); len(got) != 1 || got[0] != alice {
		t.Fatalf("the writer was told %+v, want %+v: the database writes the audit row from this", got, alice)
	}
}

func TestAPlanIsValidatedBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	w := newAdmin(t)
	bad := pack(md.DefaultRealm, "Not A Slug")
	if _, err := w.admin.CreatePlan(ctx, alice, bad); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("CreatePlan = %v, want ErrInvalid", err)
	}
	if len(w.writer.Changes()) != 0 {
		t.Fatal("an invalid plan reached the writer")
	}
}

func TestAPatchThatChangesNothingIsARequestThatSaysNothing(t *testing.T) {
	t.Parallel()
	w := newAdmin(t)
	if _, err := w.admin.CreatePlan(ctx, alice, pack(md.DefaultRealm, "pack")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.admin.PatchPlan(ctx, alice, md.DefaultRealm, "pack", ports.PlanPatch{}); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("an empty patch = %v, want ErrInvalid: it is a typo, and a typo that silently does nothing is the worst kind", err)
	}
	blank := "  "
	if _, err := w.admin.PatchPlan(ctx, alice, md.DefaultRealm, "pack", ports.PlanPatch{Name: &blank}); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("a blank name = %v, want ErrInvalid", err)
	}
	if _, err := w.admin.PatchPlan(ctx, alice, md.DefaultRealm, "pack", ports.PlanPatch{Prices: map[string]*int64{"usd": nil}}); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("a currency that is not one = %v, want ErrInvalid", err)
	}
}

// A mapping is how a verified payment finds its plan, so it is only made through an account that serves the plan's realm.
func TestAMappingIsOnlyMadeThroughAnAccountThatNamesThePlansRealm(t *testing.T) {
	t.Parallel()
	w := newAdmin(t)
	if _, err := w.admin.CreatePlan(ctx, alice, pack("other", "pack")); err != nil {
		t.Fatal(err)
	}
	pp := domain.ProviderPrice{Account: contracts.AccountID, Currency: "USD", ProviderPriceID: "price_x"} // the account serves only "default"
	if _, err := w.admin.PutProviderPrice(ctx, alice, "other", "pack", pp); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("a mapping through an account of another realm = %v, want ErrInvalid", err)
	}
	if _, err := w.admin.CreatePlan(ctx, alice, pack(md.DefaultRealm, "pack")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.admin.PutProviderPrice(ctx, alice, md.DefaultRealm, "pack", pp); err != nil {
		t.Fatalf("a mapping through an account of the realm: %v", err)
	}
	// An account that is switched off may be wired before it is switched on.
	inactive := domain.ProviderPrice{Account: contracts.InactiveID, Currency: "USD", ProviderPriceID: "price_y"}
	if _, err := w.admin.PutProviderPrice(ctx, alice, md.DefaultRealm, "pack", inactive); err != nil {
		t.Fatalf("a mapping through an inactive account: %v", err)
	}
}

func TestAnAccountIsOnlyAcceptedIfThisDeploymentCanSpeakToItAndReadItsSecrets(t *testing.T) {
	t.Parallel()
	good := domain.ProviderAccount{ID: "fake-new", Provider: contracts.ProviderName, Realms: []md.Realm{md.DefaultRealm}, Mode: "test",
		SecretRef: "env:FAKE_KEY", WebhookSecretRef: "file:/run/secrets/fake_whsec", Active: true}
	w := newAdmin(t)
	if created, err := w.admin.PutAccount(ctx, alice, good); err != nil || !created {
		t.Fatalf("a good account = %v, %v", created, err)
	}
	for name, mutate := range map[string]func(*domain.ProviderAccount){
		"a provider no adapter serves":           func(a *domain.ProviderAccount) { a.Provider = "paypal" },
		"a scheme nothing resolves":              func(a *domain.ProviderAccount) { a.SecretRef = "vault:secret/fake" },
		"a raw secret where a reference belongs": func(a *domain.ProviderAccount) { a.SecretRef = "sk_live_" + "51Habcdef" },
		"an invalid id":                          func(a *domain.ProviderAccount) { a.ID = "Fake New" },
	} {
		w := newAdmin(t)
		bad := good
		mutate(&bad)
		if _, err := w.admin.PutAccount(ctx, alice, bad); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: PutAccount = %v, want ErrInvalid", name, err)
		}
		if len(w.writer.Changes()) != 0 {
			t.Errorf("%s: the account reached the writer", name)
		}
	}
}

func TestACustomerIsOnlyLinkedThroughAnAccountThatNamesItsRealm(t *testing.T) {
	t.Parallel()
	w := newAdmin(t)
	elsewhere := domain.Customer{Scope: md.Scope{Realm: "other", Kind: "workspace", ID: "w"}, Account: contracts.AccountID, ProviderCustomerID: "cus_1"}
	if _, err := w.admin.PutCustomer(ctx, alice, elsewhere); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("a customer of a realm the account does not serve = %v, want ErrInvalid: a payment of that account would be refused as a foreign realm", err)
	}
	here := elsewhere
	here.Scope.Realm = md.DefaultRealm
	if created, err := w.admin.PutCustomer(ctx, alice, here); err != nil || !created {
		t.Fatalf("a customer of the account's realm = %v, %v", created, err)
	}
	got, err := w.admin.Customer(ctx, here.Scope, contracts.AccountID)
	if err != nil || got.ProviderCustomerID != "cus_1" {
		t.Fatalf("Customer = %+v, %v", got, err)
	}
	if _, err := w.admin.Customer(ctx, md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "nobody"}, contracts.AccountID); !errors.Is(err, md.ErrNotFound) {
		t.Fatalf("an unlinked scope = %v, want ErrNotFound", err)
	}
}

func TestAnAccountThatDoesNotExistIsNotFound(t *testing.T) {
	t.Parallel()
	w := newAdmin(t)
	if _, err := w.admin.Account(ctx, "nobody"); !errors.Is(err, md.ErrNotFound) {
		t.Fatalf("Account = %v, want ErrNotFound", err)
	}
	list, err := w.admin.Accounts(ctx)
	if err != nil || len(list) != len(contracts.WorldAccounts()) {
		t.Fatalf("Accounts = %d, %v; want the world's %d, inactive ones included", len(list), err, len(contracts.WorldAccounts()))
	}
}

func TestNewAdminRefusesWhatWouldLetAnyoneChangeWhatIsSold(t *testing.T) {
	t.Parallel()
	w := contracts.NewMemConfigWriter()
	if _, err := app.NewAdmin(app.AdminDeps{Providers: []domain.Provider{"fake"}, SecretSchemes: []string{"env"}}); err == nil {
		t.Error("an admin with no writer was built")
	}
	if _, err := app.NewAdmin(app.AdminDeps{Writer: w, SecretSchemes: []string{"env"}}); err == nil {
		t.Error("an admin with no provider to speak to was built: every account would be refused")
	}
	if _, err := app.NewAdmin(app.AdminDeps{Writer: w, Providers: []domain.Provider{"fake"}}); err == nil {
		t.Error("an admin that resolves no secret scheme was built: every account would be refused")
	}
}
