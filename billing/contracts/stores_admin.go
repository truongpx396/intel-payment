package contracts

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ConfigWriterFactory builds the configuration writer under test on a world that has the harness's provider accounts and no
// plans, customers or mappings.
type ConfigWriterFactory func(t testing.TB) ports.ConfigWriter

const otherRealm md.Realm = "other"

var change = ports.ConfigChange{Actor: "alice", Reason: "contract test", Via: "ci"}

func adminPack(realm md.Realm, code string, order int) domain.Plan {
	return domain.Plan{
		Realm: realm, Code: code, Name: "Pack " + code, Kind: domain.OneTime, CreditAllotment: ptr(domain.Credits(500)), Pool: md.GeneralPool,
		Active: true, SortOrder: order, Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}, "EUR": {MinorUnits: 900, Currency: "EUR"}},
	}
}

func adminSub(realm md.Realm, code string) domain.Plan {
	month := domain.Monthly
	p := adminPack(realm, code, 0)
	p.Kind, p.Interval, p.Pool, p.CreditAllotment = domain.Subscription, &month, "plan", ptr(domain.Credits(2000))
	return p
}

func createPlan(t testing.TB, w ports.ConfigWriter, p domain.Plan) ports.PlanConfig {
	t.Helper()
	got, err := w.CreatePlan(Ctx(), change, p)
	if err != nil {
		t.Fatalf("CreatePlan(%s): %v", p.Code, err)
	}
	return got
}

func planOf(t testing.TB, w ports.ConfigWriter, realm md.Realm, code string) ports.PlanConfig {
	t.Helper()
	got, err := w.Plan(Ctx(), realm, code)
	if err != nil {
		t.Fatalf("Plan(%s): %v", code, err)
	}
	return got
}

func accountOf(a string, provider domain.Provider, realms ...md.Realm) domain.ProviderAccount {
	return domain.ProviderAccount{ID: a, Provider: provider, Realms: realms, Mode: "test", SecretRef: "env:K_" + a, WebhookSecretRef: "env:W_" + a, Active: true}
}

// ConfigWriterCases are the subtests of ConfigWriterContract (FR-049, constitution VIII: configuration is data, and the admin API
// is its supported interface). The rules that matter most are the ones that protect money already in flight: a plan's
// mapping to a provider's price is never replaced (a subscriber still renews at the old price, and a payment for a price
// nobody maps is ignored), a provider's price belongs to one plan, and a customer is never re-pointed at another scope.
func ConfigWriterCases() []StoreCase[ConfigWriterFactory] {
	return []StoreCase[ConfigWriterFactory]{
		{"a plan is created with its prices, read back whole, and listed in its realm in order", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			p := adminPack(md.DefaultRealm, "pack-b", 2)
			got := createPlan(t, w, p)
			if jsonOf(t, got.Plan) != jsonOf(t, p) || len(got.ProviderPrices) != 0 {
				t.Fatalf("CreatePlan returned %s with %d mappings, want %s and none", jsonOf(t, got.Plan), len(got.ProviderPrices), jsonOf(t, p))
			}
			if again := planOf(t, w, md.DefaultRealm, "pack-b"); jsonOf(t, again.Plan) != jsonOf(t, p) {
				t.Fatalf("Plan = %s, want %s", jsonOf(t, again.Plan), jsonOf(t, p))
			}
			createPlan(t, w, adminPack(md.DefaultRealm, "pack-a", 2))
			createPlan(t, w, adminPack(md.DefaultRealm, "pack-0", 1))
			createPlan(t, w, adminSub(otherRealm, "elsewhere"))
			list, err := w.Plans(Ctx(), md.DefaultRealm)
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, pc := range list {
				codes = append(codes, pc.Plan.Code)
			}
			if fmt.Sprint(codes) != "[pack-0 pack-a pack-b]" {
				t.Fatalf("Plans = %v, want [pack-0 pack-a pack-b]: the realm's, by sort order and then code", codes)
			}
		}},
		{"a plan code is unique within a realm, and creating it again changes nothing", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminPack(md.DefaultRealm, "pack", 1))
			second := adminPack(md.DefaultRealm, "pack", 9)
			second.Name, second.CreditAllotment = "Renamed", ptr(domain.Credits(99999))
			if _, err := w.CreatePlan(Ctx(), change, second); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("CreatePlan twice = %v, want ErrAlreadyExists", err)
			}
			if got := planOf(t, w, md.DefaultRealm, "pack"); got.Plan.Name != "Pack pack" || *got.Plan.CreditAllotment != 500 {
				t.Fatalf("the second creation changed the plan: %+v — a plan that was sold at 500 credits must not become 99999", got.Plan)
			}
			if _, err := w.CreatePlan(Ctx(), change, adminPack(otherRealm, "pack", 1)); err != nil {
				t.Fatalf("the same code in another realm was refused: %v", err)
			}
		}},
		{"a plan that is not valid is refused and stored nowhere", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			bad := adminSub(md.DefaultRealm, "no-interval")
			bad.Interval = nil
			if _, err := w.CreatePlan(Ctx(), change, bad); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("CreatePlan = %v, want ErrInvalid", err)
			}
			if _, err := w.Plan(Ctx(), md.DefaultRealm, "no-interval"); !errors.Is(err, md.ErrNotFound) {
				t.Fatalf("the refused plan was stored: %v", err)
			}
		}},
		{"what the realm does not have is not found", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminPack(otherRealm, "theirs", 1))
			if _, err := w.Plan(Ctx(), md.DefaultRealm, "theirs"); !errors.Is(err, md.ErrNotFound) {
				t.Errorf("Plan of another realm's code = %v, want ErrNotFound", err)
			}
			name := "x"
			if _, err := w.PatchPlan(Ctx(), change, md.DefaultRealm, "theirs", ports.PlanPatch{Name: &name}); !errors.Is(err, md.ErrNotFound) {
				t.Errorf("PatchPlan = %v, want ErrNotFound", err)
			}
			if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "theirs", domain.ProviderPrice{Account: AccountID, Currency: "USD", ProviderPriceID: "price_x"}); !errors.Is(err, md.ErrNotFound) {
				t.Errorf("PutProviderPrice = %v, want ErrNotFound", err)
			}
		}},
		{"a patch moves the name, the sale flag, the order and the prices, and nothing that changes what is granted", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminSub(md.DefaultRealm, "pro"))
			name, off, order := "Pro (legacy)", false, 40
			eur := int64(1700)
			got, err := w.PatchPlan(Ctx(), change, md.DefaultRealm, "pro", ports.PlanPatch{
				Name: &name, Active: &off, SortOrder: &order, Prices: map[string]*int64{"USD": ptr(int64(1500)), "EUR": nil, "GBP": ptr(eur)},
			})
			if err != nil {
				t.Fatal(err)
			}
			p := got.Plan
			if p.Name != name || p.Active || p.SortOrder != 40 {
				t.Errorf("name/active/order = %q %v %d", p.Name, p.Active, p.SortOrder)
			}
			if len(p.Prices) != 2 || p.Prices["USD"].MinorUnits != 1500 || p.Prices["GBP"].MinorUnits != 1700 || p.Prices["GBP"].Currency != "GBP" {
				t.Errorf("prices = %+v, want USD 1500 (changed), GBP 1700 (added) and no EUR (removed)", p.Prices)
			}
			if p.Kind != domain.Subscription || *p.CreditAllotment != 2000 || p.Pool != "plan" || p.Interval == nil || *p.Interval != domain.Monthly {
				t.Errorf("a patch changed what the plan is or grants: %+v", p)
			}
			list, _ := w.Plans(Ctx(), md.DefaultRealm)
			if len(list) != 1 {
				t.Errorf("a plan taken off sale is still the admin's to see: Plans = %d, want 1", len(list))
			}
			same, err := w.PatchPlan(Ctx(), change, md.DefaultRealm, "pro", ports.PlanPatch{Name: &name})
			if err != nil || jsonOf(t, same.Plan) != jsonOf(t, got.Plan) {
				t.Errorf("the same patch twice: %v, changed %s into %s", err, jsonOf(t, got.Plan), jsonOf(t, same.Plan))
			}
		}},
		{"a provider price maps a plan's price to an account once, and repeating it is a no-op", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminPack(md.DefaultRealm, "pack", 1))
			pp := domain.ProviderPrice{Account: AccountID, Currency: "USD", ProviderPriceID: "price_a"}
			created, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "pack", pp)
			if err != nil || !created {
				t.Fatalf("PutProviderPrice = %v, %v; want created", created, err)
			}
			created, err = w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "pack", pp)
			if err != nil || created {
				t.Fatalf("PutProviderPrice again = %v, %v; want a no-op, not created", created, err)
			}
			got := planOf(t, w, md.DefaultRealm, "pack")
			if len(got.ProviderPrices) != 1 || got.ProviderPrices[0] != pp {
				t.Fatalf("mappings = %+v, want exactly %+v", got.ProviderPrices, pp)
			}
		}},
		{"a mapping is never replaced: a subscriber renews at the old price and a payment for an unmapped one is ignored", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminPack(md.DefaultRealm, "pack", 1))
			first := domain.ProviderPrice{Account: AccountID, Currency: "USD", ProviderPriceID: "price_a"}
			if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "pack", first); err != nil {
				t.Fatal(err)
			}
			second := first
			second.ProviderPriceID = "price_b"
			if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "pack", second); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("a replacement = %v, want ErrAlreadyExists", err)
			}
			if got := planOf(t, w, md.DefaultRealm, "pack"); len(got.ProviderPrices) != 1 || got.ProviderPrices[0] != first {
				t.Fatalf("mappings = %+v, want the first, unchanged", got.ProviderPrices)
			}
		}},
		{"a provider's price belongs to one plan", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminPack(md.DefaultRealm, "one", 1))
			createPlan(t, w, adminPack(md.DefaultRealm, "two", 2))
			pp := domain.ProviderPrice{Account: AccountID, Currency: "USD", ProviderPriceID: "price_shared"}
			if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "one", pp); err != nil {
				t.Fatal(err)
			}
			if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "two", pp); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("one price on two plans = %v, want ErrAlreadyExists: a payment would be fulfilled as the wrong plan", err)
			}
			if got := planOf(t, w, md.DefaultRealm, "two"); len(got.ProviderPrices) != 0 {
				t.Fatalf("the second plan got a mapping: %+v", got.ProviderPrices)
			}
			// The same id under another account is another provider price.
			other := pp
			other.Account = OtherAccountID
			if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "two", other); err != nil {
				t.Fatalf("the same id under another account was refused: %v", err)
			}
		}},
		{"a mapping needs a price in its currency and an account that exists", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			createPlan(t, w, adminPack(md.DefaultRealm, "pack", 1))
			for name, pp := range map[string]domain.ProviderPrice{
				"a currency the plan is not priced in": {Account: AccountID, Currency: "GBP", ProviderPriceID: "price_gbp"},
				"an account nobody has":                {Account: "no-such-account", Currency: "USD", ProviderPriceID: "price_x"},
			} {
				if _, err := w.PutProviderPrice(Ctx(), change, md.DefaultRealm, "pack", pp); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("%s: PutProviderPrice = %v, want ErrInvalid", name, err)
				}
			}
			if got := planOf(t, w, md.DefaultRealm, "pack"); len(got.ProviderPrices) != 0 {
				t.Errorf("a refused mapping was stored: %+v", got.ProviderPrices)
			}
		}},
		{"an account is created and replaced, inactive ones are listed, and its provider never changes", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			before, err := w.Accounts(Ctx())
			if err != nil {
				t.Fatal(err)
			}
			a := accountOf("stripe-new", "fake", md.DefaultRealm)
			created, err := w.PutAccount(Ctx(), change, a)
			if err != nil || !created {
				t.Fatalf("PutAccount = %v, %v; want created", created, err)
			}
			got, ok, err := w.Account(Ctx(), "stripe-new")
			if err != nil || !ok || jsonOf(t, got) != jsonOf(t, a) {
				t.Fatalf("Account = %s, %v, %v; want %s", jsonOf(t, got), ok, err, jsonOf(t, a))
			}
			a.Mode, a.Active, a.Realms, a.SecretRef = "live", false, []md.Realm{md.DefaultRealm, otherRealm}, "file:/run/secrets/stripe"
			created, err = w.PutAccount(Ctx(), change, a)
			if err != nil || created {
				t.Fatalf("PutAccount again = %v, %v; want replaced, not created", created, err)
			}
			if got, _, _ := w.Account(Ctx(), "stripe-new"); jsonOf(t, got) != jsonOf(t, a) {
				t.Fatalf("the replacement is %s, want %s", jsonOf(t, got), jsonOf(t, a))
			}
			after, _ := w.Accounts(Ctx())
			if len(after) != len(before)+1 {
				t.Fatalf("Accounts = %d, want %d: an inactive account is the admin's to see", len(after), len(before)+1)
			}
			moved := a
			moved.Provider = "elsewhere"
			if _, err := w.PutAccount(Ctx(), change, moved); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("changing an account's provider = %v, want ErrInvalid: its webhooks arrive at a route that names the provider", err)
			}
			if got, _, _ := w.Account(Ctx(), "stripe-new"); got.Provider != "fake" {
				t.Fatalf("the provider is now %q", got.Provider)
			}
			if _, ok, _ := w.Account(Ctx(), "nobody"); ok {
				t.Fatal("an unknown account was found")
			}
		}},
		{"an account that is not valid is refused and stored nowhere", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			bad := accountOf("stripe-bad", "fake", md.DefaultRealm)
			bad.SecretRef = "sk_live_" + "51Habcdef" // a raw secret where a reference belongs
			if _, err := w.PutAccount(Ctx(), change, bad); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("PutAccount = %v, want ErrInvalid", err)
			}
			if _, ok, _ := w.Account(Ctx(), "stripe-bad"); ok {
				t.Fatal("the refused account was stored")
			}
		}},
		{"a customer links a scope to an existing provider customer, and is never re-pointed", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			sc := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w9"}
			c := domain.Customer{Scope: sc, Account: AccountID, ProviderCustomerID: "cus_9", BillingEmail: "old@example.test"}
			created, err := w.PutCustomer(Ctx(), change, c)
			if err != nil || !created {
				t.Fatalf("PutCustomer = %v, %v; want created", created, err)
			}
			created, err = w.PutCustomer(Ctx(), change, c)
			if err != nil || created {
				t.Fatalf("PutCustomer again = %v, %v; want a no-op", created, err)
			}
			c.BillingEmail = "new@example.test"
			if _, err := w.PutCustomer(Ctx(), change, c); err != nil {
				t.Fatalf("moving the email: %v", err)
			}
			repointed := c
			repointed.ProviderCustomerID = "cus_other"
			if _, err := w.PutCustomer(Ctx(), change, repointed); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("a different provider customer for the scope = %v, want ErrAlreadyExists: the webhooks of cus_9 would be orphaned", err)
			}
			got, ok, err := w.Customer(Ctx(), sc, AccountID)
			if err != nil || !ok || got.ProviderCustomerID != "cus_9" || got.BillingEmail != "new@example.test" {
				t.Fatalf("Customer = %+v, %v, %v; want cus_9 with the new email", got, ok, err)
			}
			if _, ok, _ := w.Customer(Ctx(), md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "nobody"}, AccountID); ok {
				t.Fatal("a scope that was never linked has a customer")
			}
		}},
		{"a provider customer already linked to another scope is refused", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			a := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "wa"}
			b := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "wb"}
			if _, err := w.PutCustomer(Ctx(), change, domain.Customer{Scope: a, Account: AccountID, ProviderCustomerID: "cus_shared"}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.PutCustomer(Ctx(), change, domain.Customer{Scope: b, Account: AccountID, ProviderCustomerID: "cus_shared"}); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("one provider customer for two scopes = %v, want ErrAlreadyExists: the payments of one would land in the other's books", err)
			}
			if _, ok, _ := w.Customer(Ctx(), b, AccountID); ok {
				t.Fatal("the second scope was linked")
			}
		}},
		{"a customer of an account that does not exist is refused", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			c := domain.Customer{Scope: md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}, Account: "no-such-account", ProviderCustomerID: "cus_1"}
			if _, err := w.PutCustomer(Ctx(), change, c); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("PutCustomer = %v, want ErrInvalid", err)
			}
		}},
		{"however many create one plan code at once, exactly one wins", func(t testing.TB, mk ConfigWriterFactory) {
			w := mk(t)
			const racers = 8
			var wg sync.WaitGroup
			errs := make([]error, racers)
			for i := range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					p := adminPack(md.DefaultRealm, "contested", i)
					p.Name = fmt.Sprintf("racer %d", i)
					_, errs[i] = w.CreatePlan(Ctx(), change, p)
				}()
			}
			wg.Wait()
			wins := 0
			for _, err := range errs {
				switch {
				case err == nil:
					wins++
				case !errors.Is(err, md.ErrAlreadyExists):
					t.Errorf("a loser got %v, want ErrAlreadyExists", err)
				}
			}
			if wins != 1 {
				t.Fatalf("%d of %d creations won, want exactly 1", wins, racers)
			}
		}},
	}
}

// ConfigWriterContract is the specification of the configuration writer behind the admin API.
func ConfigWriterContract(t *testing.T, mk ConfigWriterFactory) {
	t.Helper()
	runStoreCases(t, ConfigWriterCases(), mk)
}
