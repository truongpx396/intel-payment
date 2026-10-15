package contracts

import (
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ----------------------------------------------------------------- customers --

// CustomersFactory builds the customers store under test, on a world that has the harness's provider accounts.
type CustomersFactory func(t testing.TB) ports.Customers

// CustomersCases are the subtests of CustomersContract (payments invariants 4, 19).
func CustomersCases() []StoreCase[CustomersFactory] {
	a := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}
	b := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w2"}
	return []StoreCase[CustomersFactory]{
		{"a customer maps its provider id to its scope, and only that", func(t testing.TB, mk CustomersFactory) {
			c := mk(t)
			if err := c.Upsert(Ctx(), domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_x", BillingEmail: "x@example.test"}); err != nil {
				t.Fatal(err)
			}
			got, ok, err := c.Get(Ctx(), a, AccountID)
			if err != nil || !ok || got.ProviderCustomerID != "cus_x" || got.BillingEmail != "x@example.test" || got.Scope != a {
				t.Fatalf("Get = %+v, %v, %v", got, ok, err)
			}
			if s, ok, err := c.ResolveScope(Ctx(), AccountID, "cus_x"); err != nil || !ok || s != a {
				t.Fatalf("ResolveScope = %+v, %v, %v", s, ok, err)
			}
			if _, ok, _ := c.ResolveScope(Ctx(), AccountID, "cus_unknown"); ok {
				t.Fatal("an unknown provider customer resolved to a scope")
			}
			if _, ok, _ := c.Get(Ctx(), b, AccountID); ok {
				t.Fatal("another scope has this customer")
			}
		}},
		{"a provider customer cannot be re-pointed at another scope", func(t testing.TB, mk CustomersFactory) {
			c := mk(t)
			if err := c.Upsert(Ctx(), domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_x"}); err != nil {
				t.Fatal(err)
			}
			if err := c.Upsert(Ctx(), domain.Customer{Scope: b, Account: AccountID, ProviderCustomerID: "cus_x"}); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("Upsert = %v, want ErrAlreadyExists: the payments of cus_x would have landed in the wrong scope", err)
			}
			if s, _, _ := c.ResolveScope(Ctx(), AccountID, "cus_x"); s != a {
				t.Fatalf("cus_x now resolves to %+v", s)
			}
		}},
		{"a scope's provider customer is immutable once mapped; the email moves", func(t testing.TB, mk CustomersFactory) {
			c := mk(t)
			_ = c.Upsert(Ctx(), domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_x", BillingEmail: "old@example.test"})
			if err := c.Upsert(Ctx(), domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_y", BillingEmail: "new@example.test"}); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			got, _, _ := c.Get(Ctx(), a, AccountID)
			if got.ProviderCustomerID != "cus_x" || got.BillingEmail != "new@example.test" {
				t.Fatalf("customer = %+v: replacing the mapping would orphan the old customer's webhooks", got)
			}
			if _, ok, _ := c.ResolveScope(Ctx(), AccountID, "cus_y"); ok {
				t.Fatal("the discarded customer id resolves")
			}
		}},
		{"the same provider customer id under another account is another customer", func(t testing.TB, mk CustomersFactory) {
			c := mk(t)
			_ = c.Upsert(Ctx(), domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_x"})
			if err := c.Upsert(Ctx(), domain.Customer{Scope: b, Account: OtherAccountID, ProviderCustomerID: "cus_x"}); err != nil {
				t.Fatal(err)
			}
			if s, _, _ := c.ResolveScope(Ctx(), AccountID, "cus_x"); s != a {
				t.Fatalf("account A: %+v", s)
			}
			if s, _, _ := c.ResolveScope(Ctx(), OtherAccountID, "cus_x"); s != b {
				t.Fatalf("account B: %+v", s)
			}
		}},
		{"realms are separate", func(t testing.TB, mk CustomersFactory) {
			c := mk(t)
			elsewhere := md.Scope{Realm: "other", Kind: "workspace", ID: "w1"}
			_ = c.Upsert(Ctx(), domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_x"})
			_ = c.Upsert(Ctx(), domain.Customer{Scope: elsewhere, Account: AccountID, ProviderCustomerID: "cus_y"})
			if g, _, _ := c.Get(Ctx(), a, AccountID); g.ProviderCustomerID != "cus_x" {
				t.Fatalf("realm default: %+v", g)
			}
			if g, _, _ := c.Get(Ctx(), elsewhere, AccountID); g.ProviderCustomerID != "cus_y" || g.Scope.Realm != "other" {
				t.Fatalf("realm other: %+v", g)
			}
			if s, _, _ := c.ResolveScope(Ctx(), AccountID, "cus_y"); s.Realm != "other" {
				t.Fatalf("cus_y resolved to %+v: the realm is how the processor decides whether the account may touch it", s)
			}
		}},
	}
}

// CustomersContract is the specification of the customers store (payments invariants 4, 19): the only sanctioned way
// a webhook learns whom it is about, so a provider customer maps to exactly one scope, never moves, and is read with
// its realm.
func CustomersContract(t *testing.T, mk CustomersFactory) {
	t.Helper()
	runStoreCases(t, CustomersCases(), mk)
}

// ------------------------------------------------------------------ catalogue --

// CatalogSeeder puts a plan on sale: it writes the plan, its prices and the provider price that sells it.
type CatalogSeeder func(WorldPlan)

// CatalogFactory builds the catalogue under test and a way to seed it. The world's provider accounts are in place; the
// catalogue is empty.
type CatalogFactory func(t testing.TB) (ports.Catalog, CatalogSeeder)

// CatalogCases are the subtests of CatalogContract (R091: the catalogue is per account and currency; FR-021).
func CatalogCases() []StoreCase[CatalogFactory] {
	return []StoreCase[CatalogFactory]{
		{"List is the realm's active plans priced in the currency, in order, with every field", func(t testing.TB, mk CatalogFactory) {
			cat, seed := mk(t)
			var proWant domain.Plan
			for _, w := range WorldPlans() {
				if w.Plan.Code == ProCode {
					flag, quota, text := true, int64(-1), "gold"
					w.Plan.Entitlements = []domain.Entitlement{{Key: "sso", Kind: "flag", Bool: &flag}, {Key: "seats", Kind: "quota", Int: &quota}, {Key: "support", Kind: "enum", Text: &text}}
					w.Plan.Prices["EUR"] = domain.Money{MinorUnits: 1800, Currency: "EUR"}
					proWant = w.Plan
				}
				seed(w)
			}
			seed(WorldPlan{Plan: domain.Plan{Realm: md.DefaultRealm, Code: "old", Name: "Retired", Kind: domain.OneTime, Pool: md.GeneralPool, Active: false,
				Prices: map[string]domain.Money{"USD": {MinorUnits: 100, Currency: "USD"}}}, Account: AccountID, Currency: "USD", ProviderPriceID: "price_old"})
			seed(WorldPlan{Plan: domain.Plan{Realm: "other", Code: "elsewhere", Name: "Another product", Kind: domain.OneTime, Pool: md.GeneralPool, Active: true,
				Prices: map[string]domain.Money{"USD": {MinorUnits: 100, Currency: "USD"}}}, Account: AccountID, Currency: "USD", ProviderPriceID: "price_elsewhere"})

			got, err := cat.List(Ctx(), md.DefaultRealm, "USD")
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, p := range got {
				codes = append(codes, p.Code)
			}
			if want := []string{PackCode, ProCode, TeamCode, LiteCode}; len(codes) != len(want) || codes[0] != want[0] || codes[1] != want[1] || codes[2] != want[2] || codes[3] != want[3] {
				t.Fatalf("List = %v, want %v (by sort order)", codes, want)
			}
			for _, p := range got {
				if p.Code == "old" || p.Code == "elsewhere" {
					t.Fatalf("List returned %s: a retired plan is not on sale, and another realm's is not this realm's", p.Code)
				}
				if p.Code == ProCode && jsonOf(t, p) != jsonOf(t, proWant) {
					t.Fatalf("pro did not round-trip:\n got  %s\n want %s", jsonOf(t, p), jsonOf(t, proWant))
				}
			}
			if eur, _ := cat.List(Ctx(), md.DefaultRealm, "EUR"); len(eur) != 1 || eur[0].Code != ProCode {
				t.Fatalf("List(EUR) = %+v: only pro is priced in euros", eur)
			}
			if jpy, _ := cat.List(Ctx(), md.DefaultRealm, "JPY"); len(jpy) != 0 {
				t.Fatalf("List(JPY) = %+v", jpy)
			}
		}},
		{"Resolve is the plan and the provider price for one account and currency, never a default", func(t testing.TB, mk CatalogFactory) {
			cat, seed := mk(t)
			for _, w := range WorldPlans() {
				if w.Plan.Code == ProCode {
					w.Plan.Prices["EUR"] = domain.Money{MinorUnits: 1800, Currency: "EUR"}
					seed(w)
					seed(WorldPlan{Plan: w.Plan, Account: AccountID, Currency: "EUR", ProviderPriceID: "price_pro_eur"})
				}
			}
			seed(WorldPlan{Plan: domain.Plan{Realm: md.DefaultRealm, Code: "old", Name: "Retired", Kind: domain.OneTime, Pool: md.GeneralPool, Active: false,
				Prices: map[string]domain.Money{"USD": {MinorUnits: 100, Currency: "USD"}}}, Account: AccountID, Currency: "USD", ProviderPriceID: "price_old"})

			plan, price, err := cat.Resolve(Ctx(), md.DefaultRealm, ProCode, "USD", AccountID)
			if err != nil || plan.Code != ProCode || price != ProPriceID || *plan.CreditAllotment != ProCredits || plan.Pool != "plan" {
				t.Fatalf("Resolve = %+v, %q, %v", plan, price, err)
			}
			if _, price, _ := cat.Resolve(Ctx(), md.DefaultRealm, ProCode, "EUR", AccountID); price != "price_pro_eur" {
				t.Fatalf("the euro price is its own mapping: %q", price)
			}
			for name, tc := range map[string][4]string{
				"a code nobody sells":                       {"default", "ghost", "USD", AccountID},
				"a currency the plan has no price in":       {"default", ProCode, "JPY", AccountID},
				"an account that does not sell it":          {"default", ProCode, "USD", OtherAccountID},
				"an account that does not exist":            {"default", ProCode, "USD", "nope"},
				"a retired plan":                            {"default", "old", "USD", AccountID},
				"the plan in a realm that does not sell it": {"other", ProCode, "USD", AccountID},
			} {
				if p, id, err := cat.Resolve(Ctx(), md.Realm(tc[0]), tc[1], tc[2], tc[3]); !errors.Is(err, domain.ErrUnknownPrice) || id != "" || p.Code != "" {
					t.Errorf("%s: Resolve = %+v, %q, %v; want domain.ErrUnknownPrice and nothing", name, p, id, err)
				}
			}
		}},
		{"PlanForPrice maps a verified provider price back to the plan, and recognizes a plan since retired", func(t testing.TB, mk CatalogFactory) {
			cat, seed := mk(t)
			for _, w := range WorldPlans() {
				seed(w)
			}
			seed(WorldPlan{Plan: domain.Plan{Realm: md.DefaultRealm, Code: "old", Name: "Retired", Kind: domain.OneTime, Pool: md.GeneralPool, Active: false,
				CreditAllotment: ptr(md.Credits(777)), Prices: map[string]domain.Money{"USD": {MinorUnits: 100, Currency: "USD"}}}, Account: AccountID, Currency: "USD", ProviderPriceID: "price_old"})

			plan, ok, err := cat.PlanForPrice(Ctx(), AccountID, PackPriceID)
			if err != nil || !ok || plan.Code != PackCode || *plan.CreditAllotment != PackCredits || plan.Realm.Or() != md.DefaultRealm {
				t.Fatalf("PlanForPrice = %+v, %v, %v", plan, ok, err)
			}
			if plan, ok, _ := cat.PlanForPrice(Ctx(), AccountID, "price_old"); !ok || plan.Code != "old" || plan.Active || *plan.CreditAllotment != 777 {
				t.Fatalf("a customer who paid for a plan retired a minute later must still get their credits: %+v, %v", plan, ok)
			}
			if p, ok, err := cat.PlanForPrice(Ctx(), AccountID, "price_mystery"); ok || err != nil || p.Code != "" {
				t.Fatalf("an unknown price: %+v, %v, %v — never a default plan", p, ok, err)
			}
			if _, ok, _ := cat.PlanForPrice(Ctx(), OtherAccountID, PackPriceID); ok {
				t.Fatal("another account's price id resolved: a price id means something only within its account")
			}
		}},
		{"changes to the catalogue are seen at once", func(t testing.TB, mk CatalogFactory) {
			cat, seed := mk(t)
			if _, _, err := cat.Resolve(Ctx(), md.DefaultRealm, PackCode, "USD", AccountID); !errors.Is(err, domain.ErrUnknownPrice) {
				t.Fatalf("before seeding: %v", err)
			}
			for _, w := range WorldPlans() {
				seed(w)
			}
			if _, _, err := cat.Resolve(Ctx(), md.DefaultRealm, PackCode, "USD", AccountID); err != nil {
				t.Fatalf("a price written a moment ago must be sold now (plans are data, not a deploy): %v", err)
			}
		}},
	}
}

func ptr[T any](v T) *T { return &v }

// CatalogContract is the specification of the catalogue (R091, FR-021): it is per account and currency, an unknown
// price is not ok and never a default plan, a retired plan is no longer sold but a payment for it is still
// recognized, and a change is visible at once.
func CatalogContract(t *testing.T, mk CatalogFactory) {
	t.Helper()
	runStoreCases(t, CatalogCases(), mk)
}

// ------------------------------------------------------------------- accounts --

// AccountSeeder stores a provider account.
type AccountSeeder func(domain.ProviderAccount)

// AccountsFactory builds the accounts under test and a way to seed them (empty to begin with).
type AccountsFactory func(t testing.TB) (ports.Accounts, AccountSeeder)

// AccountsCases are the subtests of AccountsContract.
func AccountsCases() []StoreCase[AccountsFactory] {
	return []StoreCase[AccountsFactory]{
		{"an account round-trips every field, and an unknown one is not found", func(t testing.TB, mk AccountsFactory) {
			acc, seed := mk(t)
			want := domain.ProviderAccount{ID: "stripe-live", Provider: "stripe", Realms: []domain.Realm{"acme", "default"}, Mode: "live",
				SecretRef: "env:STRIPE_SECRET_KEY", WebhookSecretRef: "env:STRIPE_WEBHOOK_SECRET", Active: true}
			seed(want)
			got, ok, err := acc.Account(Ctx(), "stripe-live")
			if err != nil || !ok || jsonOf(t, got) != jsonOf(t, want) {
				t.Fatalf("Account = %+v, %v, %v\nwant %+v", got, ok, err, want)
			}
			if _, ok, err := acc.Account(Ctx(), "nope"); ok || err != nil {
				t.Fatalf("an unknown account: %v, %v", ok, err)
			}
		}},
		{"ForRealm is the active accounts that serve the realm, by id", func(t testing.TB, mk AccountsFactory) {
			acc, seed := mk(t)
			mkAcct := func(id string, active bool, realms ...domain.Realm) domain.ProviderAccount {
				return domain.ProviderAccount{ID: id, Provider: "stripe", Realms: realms, Mode: "live", SecretRef: "env:S", WebhookSecretRef: "env:W", Active: active}
			}
			seed(mkAcct("b-both", true, "default", "acme"))
			seed(mkAcct("a-default", true, "default"))
			seed(mkAcct("c-acme-only", true, "acme"))
			seed(mkAcct("d-off", false, "default"))
			ids := func(realm domain.Realm) []string {
				got, err := acc.ForRealm(Ctx(), realm)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, a := range got {
					out = append(out, a.ID)
				}
				return out
			}
			for realm, want := range map[domain.Realm][]string{"default": {"a-default", "b-both"}, "": {"a-default", "b-both"}, "acme": {"b-both", "c-acme-only"}, "nobody": nil} {
				got := ids(realm)
				if len(got) != len(want) {
					t.Fatalf("ForRealm(%q) = %v, want %v", realm, got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("ForRealm(%q) = %v, want %v", realm, got, want)
					}
				}
			}
		}},
	}
}

// AccountsContract is the specification of provider accounts as data: they round-trip whole, and a realm is served by
// exactly the active accounts that name it.
func AccountsContract(t *testing.T, mk AccountsFactory) {
	t.Helper()
	runStoreCases(t, AccountsCases(), mk)
}
