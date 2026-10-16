package paymenthttp_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// adminStub embeds the port, so a method a test gave no behaviour panics.
type adminStub struct {
	ports.Admin
	plans       func(ctx context.Context, realm domain.Realm) ([]ports.PlanConfig, error)
	plan        func(ctx context.Context, realm domain.Realm, code string) (ports.PlanConfig, error)
	createPlan  func(ctx context.Context, ch ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error)
	patchPlan   func(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, code string, patch ports.PlanPatch) (ports.PlanConfig, error)
	putPrice    func(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, code string, pp domain.ProviderPrice) (bool, error)
	accounts    func(ctx context.Context) ([]domain.ProviderAccount, error)
	account     func(ctx context.Context, id string) (domain.ProviderAccount, error)
	putAccount  func(ctx context.Context, ch ports.ConfigChange, a domain.ProviderAccount) (bool, error)
	customer    func(ctx context.Context, s domain.Scope, account string) (domain.Customer, error)
	putCustomer func(ctx context.Context, ch ports.ConfigChange, c domain.Customer) (bool, error)
}

func (a adminStub) Plans(ctx context.Context, r domain.Realm) ([]ports.PlanConfig, error) {
	return a.plans(ctx, r)
}

func (a adminStub) Plan(ctx context.Context, r domain.Realm, code string) (ports.PlanConfig, error) {
	return a.plan(ctx, r, code)
}

func (a adminStub) CreatePlan(ctx context.Context, ch ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error) {
	return a.createPlan(ctx, ch, p)
}

func (a adminStub) PatchPlan(ctx context.Context, ch ports.ConfigChange, r domain.Realm, code string, p ports.PlanPatch) (ports.PlanConfig, error) {
	return a.patchPlan(ctx, ch, r, code, p)
}

func (a adminStub) PutProviderPrice(ctx context.Context, ch ports.ConfigChange, r domain.Realm, code string, pp domain.ProviderPrice) (bool, error) {
	return a.putPrice(ctx, ch, r, code, pp)
}

func (a adminStub) Accounts(ctx context.Context) ([]domain.ProviderAccount, error) {
	return a.accounts(ctx)
}

func (a adminStub) Account(ctx context.Context, id string) (domain.ProviderAccount, error) {
	return a.account(ctx, id)
}

func (a adminStub) PutAccount(ctx context.Context, ch ports.ConfigChange, acct domain.ProviderAccount) (bool, error) {
	return a.putAccount(ctx, ch, acct)
}

func (a adminStub) Customer(ctx context.Context, s domain.Scope, account string) (domain.Customer, error) {
	return a.customer(ctx, s, account)
}

func (a adminStub) PutCustomer(ctx context.Context, ch ports.ConfigChange, c domain.Customer) (bool, error) {
	return a.putCustomer(ctx, ch, c)
}

func adminDoor(t *testing.T, a adminStub) *door {
	t.Helper()
	return newDoor(t, paymenthttp.Config{Admin: a})
}

func proConfig() ports.PlanConfig {
	month := domain.Monthly
	p := pack()
	p.Code, p.Name, p.Kind, p.CreditAllotment, p.Interval, p.Pool = "pro", "Pro", domain.Subscription, credits(2000), &month, "plan"
	return ports.PlanConfig{Plan: p, ProviderPrices: []domain.ProviderPrice{{Account: "stripe-test", Currency: "USD", ProviderPriceID: "price_pro"}}}
}

func stripeAccount(realms ...domain.Realm) domain.ProviderAccount {
	return domain.ProviderAccount{ID: "stripe-test", Provider: "stripe", Realms: realms, Mode: "test", SecretRef: "env:STRIPE_SECRET_KEY", WebhookSecretRef: "env:STRIPE_WEBHOOK_SECRET", Active: true}
}

const createPlanBody = `{"actor":"alice","reason":"launch","code":"pro","name":"Pro","kind":"subscription","credit_allotment":2000,"pool":"plan","interval":"month",` +
	`"active":true,"sort_order":1,"prices":{"USD":2000}}`

// ------------------------------------------------------------------ attribution --

// Every write names a person and a reason, and the credential it came through is recorded beside them: the database's audit
// row is how anyone later answers "who changed the price, and why".
func TestAnAdminWriteIsAttributedToThePersonTheReasonAndTheCredential(t *testing.T) {
	t.Parallel()
	var got []ports.ConfigChange
	d := adminDoor(t, adminStub{createPlan: func(_ context.Context, ch ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error) {
		got = append(got, ch)
		return ports.PlanConfig{Plan: p}, nil
	}})
	if r := d.call(t, "POST", "/v1/admin/plans", createPlanBody); r.Status != http.StatusCreated {
		t.Fatalf("status = %d: %s", r.Status, r.Body)
	}
	if len(got) != 1 || got[0] != (ports.ConfigChange{Actor: "alice", Reason: "launch", Via: "test-credential"}) {
		t.Fatalf("the use case was told %+v", got)
	}
}

func TestAnAdminWriteWithoutAPersonOrAReasonIsRefusedBeforeTheUseCase(t *testing.T) {
	t.Parallel()
	var calls int
	count := func() { calls++ }
	d := adminDoor(t, adminStub{
		createPlan: func(context.Context, ports.ConfigChange, domain.Plan) (ports.PlanConfig, error) {
			count()
			return ports.PlanConfig{}, nil
		},
		patchPlan: func(context.Context, ports.ConfigChange, domain.Realm, string, ports.PlanPatch) (ports.PlanConfig, error) {
			count()
			return ports.PlanConfig{}, nil
		},
		putPrice: func(context.Context, ports.ConfigChange, domain.Realm, string, domain.ProviderPrice) (bool, error) {
			count()
			return true, nil
		},
		account: func(context.Context, string) (domain.ProviderAccount, error) {
			return domain.ProviderAccount{}, md.ErrNotFound
		},
		putAccount: func(context.Context, ports.ConfigChange, domain.ProviderAccount) (bool, error) {
			count()
			return true, nil
		},
		putCustomer: func(context.Context, ports.ConfigChange, domain.Customer) (bool, error) { count(); return true, nil },
	})
	attribution := func(body, actor, reason string) string {
		return strings.Replace(strings.Replace(body, `"actor":"alice"`, actor, 1), `"reason":"launch"`, reason, 1)
	}
	account := `{"actor":"alice","reason":"launch","provider":"stripe","realms":["acme"],"mode":"test","secret_ref":"env:K","webhook_secret_ref":"env:W","active":true}`
	customer := `{"actor":"alice","reason":"launch","scope":{"kind":"workspace","id":"w1"},"account":"stripe-test","provider_customer_id":"cus_1"}`
	for _, w := range []struct{ method, path, body string }{
		{"POST", "/v1/admin/plans", createPlanBody},
		{"PATCH", "/v1/admin/plans/pro", `{"actor":"alice","reason":"launch","name":"Pro 2"}`},
		{"PUT", "/v1/admin/plans/pro/provider-prices/stripe-test/USD", `{"actor":"alice","reason":"launch","provider_price_id":"price_x"}`},
		{"PUT", "/v1/admin/provider-accounts/stripe-test", account},
		{"PUT", "/v1/admin/customers", customer},
	} {
		for label, body := range map[string]string{
			"no actor":       attribution(w.body, `"x":0`, `"reason":"launch"`),
			"a blank actor":  attribution(w.body, `"actor":" "`, `"reason":"launch"`),
			"no reason":      attribution(w.body, `"actor":"alice"`, `"x":0`),
			"a blank reason": attribution(w.body, `"actor":"alice"`, `"reason":"  "`),
		} {
			r := d.call(t, w.method, w.path, body)
			if r.Status != http.StatusBadRequest {
				t.Errorf("%s %s with %s: %d %s, want 400", w.method, w.path, label, r.Status, r.Body)
			}
		}
	}
	if calls != 0 {
		t.Errorf("%d changes reached the use case without a person and a reason", calls)
	}
}

// ------------------------------------------------------------------------ plans --

func TestPlansAreListedAndReadWithTheirMappingsForTheCredentialsRealm(t *testing.T) {
	t.Parallel()
	var listed, read domain.Realm
	d := adminDoor(t, adminStub{
		plans: func(_ context.Context, r domain.Realm) ([]ports.PlanConfig, error) {
			listed = r
			off := proConfig()
			off.Plan.Active = false
			return []ports.PlanConfig{proConfig(), off}, nil
		},
		plan: func(_ context.Context, r domain.Realm, code string) (ports.PlanConfig, error) {
			read = r
			if code != "pro" {
				return ports.PlanConfig{}, md.ErrNotFound
			}
			return proConfig(), nil
		},
	})
	got := d.call(t, "GET", "/v1/admin/plans", "")
	if got.Status != http.StatusOK || listed != "acme" {
		t.Fatalf("list = %d (realm %q): %s", got.Status, listed, got.Body)
	}
	var list struct {
		Data []struct {
			Code            string           `json:"code"`
			Name            string           `json:"name"`
			Kind            string           `json:"kind"`
			CreditAllotment *int64           `json:"credit_allotment"`
			Pool            string           `json:"pool"`
			Interval        string           `json:"interval"`
			Active          bool             `json:"active"`
			SortOrder       int              `json:"sort_order"`
			Prices          map[string]int64 `json:"prices"`
			ProviderPrices  []struct {
				Account         string `json:"account"`
				Currency        string `json:"currency"`
				ProviderPriceID string `json:"provider_price_id"`
			} `json:"provider_prices"`
		} `json:"data"`
	}
	got.JSON(t, &list)
	if len(list.Data) != 2 || list.Data[1].Active {
		t.Fatalf("list = %s: an inactive plan is the admin's to see", got.Body)
	}
	p := list.Data[0]
	if p.Code != "pro" || p.Kind != "subscription" || *p.CreditAllotment != 2000 || p.Pool != "plan" || p.Interval != "month" || !p.Active ||
		p.Prices["USD"] != 1000 || len(p.ProviderPrices) != 1 || p.ProviderPrices[0].ProviderPriceID != "price_pro" || p.ProviderPrices[0].Account != "stripe-test" {
		t.Errorf("the plan came out as %+v: the admin sees the provider's price id, a client never does", p)
	}
	if r := d.call(t, "GET", "/v1/admin/plans/pro", ""); r.Status != http.StatusOK || read != "acme" {
		t.Errorf("read = %d (realm %q): %s", r.Status, read, r.Body)
	}
	if r := d.call(t, "GET", "/v1/admin/plans/ghost", ""); r.Status != http.StatusNotFound || r.ErrorCode(t) != "not_found" {
		t.Errorf("read of nothing = %d %s, want 404 not_found", r.Status, r.Body)
	}
}

func TestCreatingAPlanPassesExactlyWhatWasSaidAndAnswers201(t *testing.T) {
	t.Parallel()
	var got domain.Plan
	d := adminDoor(t, adminStub{createPlan: func(_ context.Context, _ ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error) {
		got = p
		return ports.PlanConfig{Plan: p}, nil
	}})
	r := d.call(t, "POST", "/v1/admin/plans", createPlanBody)
	if r.Status != http.StatusCreated {
		t.Fatalf("status = %d: %s", r.Status, r.Body)
	}
	month := domain.Monthly
	want := domain.Plan{Realm: "acme", Code: "pro", Name: "Pro", Kind: domain.Subscription, CreditAllotment: credits(2000), Pool: "plan", Interval: &month, Active: true, SortOrder: 1,
		Prices: map[string]domain.Money{"USD": {MinorUnits: 2000, Currency: "USD"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the use case was given %+v, want %+v", got, want)
	}
}

func TestCreatingAPlanCannotSetWhatTheDeploymentOwns(t *testing.T) {
	t.Parallel()
	d := adminDoor(t, adminStub{createPlan: func(context.Context, ports.ConfigChange, domain.Plan) (ports.PlanConfig, error) {
		t.Error("a plan was created from a request with a field the door does not have")
		return ports.PlanConfig{}, nil
	}})
	for _, field := range []string{`"realm":"other"`, `"entitlements":[]`, `"provider_price_id":"price_x"`, `"id":"x"`} {
		body := strings.Replace(createPlanBody, `{`, `{`+field+`,`, 1)
		r := d.call(t, "POST", "/v1/admin/plans", body)
		name := strings.Trim(strings.SplitN(field, ":", 2)[0], `"`)
		if r.Status != http.StatusBadRequest || !strings.Contains(string(r.Body), name) {
			t.Errorf("%s: %d %s, want 400 naming the field", field, r.Status, r.Body)
		}
	}
}

func TestAPatchNamesOnlyWhatMayChange(t *testing.T) {
	t.Parallel()
	var patch ports.PlanPatch
	d := adminDoor(t, adminStub{patchPlan: func(_ context.Context, _ ports.ConfigChange, _ domain.Realm, _ string, p ports.PlanPatch) (ports.PlanConfig, error) {
		patch = p
		return proConfig(), nil
	}})
	r := d.call(t, "PATCH", "/v1/admin/plans/pro", `{"actor":"alice","reason":"reprice","name":"Pro 2","active":false,"sort_order":4,"prices":{"USD":2500,"EUR":null}}`)
	if r.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", r.Status, r.Body)
	}
	if patch.Name == nil || *patch.Name != "Pro 2" || patch.Active == nil || *patch.Active || patch.SortOrder == nil || *patch.SortOrder != 4 {
		t.Errorf("patch = %+v", patch)
	}
	if len(patch.Prices) != 2 || patch.Prices["USD"] == nil || *patch.Prices["USD"] != 2500 || patch.Prices["EUR"] != nil {
		t.Errorf("prices = %+v, want USD 2500 and EUR removed", patch.Prices)
	}
	// What a plan grants is fixed when it is created: change it by selling a new plan and taking the old one off sale.
	for _, field := range []string{`"credit_allotment":1`, `"kind":"one_time"`, `"pool":"x"`, `"interval":"year"`, `"code":"other"`} {
		body := `{"actor":"alice","reason":"x",` + field + `}`
		name := strings.Trim(strings.SplitN(field, ":", 2)[0], `"`)
		if r := d.call(t, "PATCH", "/v1/admin/plans/pro", body); r.Status != http.StatusBadRequest || !strings.Contains(string(r.Body), name) {
			t.Errorf("%s: %d %s, want 400 naming the field: a customer who paid for 2000 credits must not be granted another quantity", field, r.Status, r.Body)
		}
	}
}

func TestAProviderPriceMappingIsMadeByThePathAndTheBodyNamesOnlyThePrice(t *testing.T) {
	t.Parallel()
	var got domain.ProviderPrice
	var realm domain.Realm
	var code string
	created := true
	d := adminDoor(t, adminStub{putPrice: func(_ context.Context, _ ports.ConfigChange, r domain.Realm, c string, pp domain.ProviderPrice) (bool, error) {
		got, realm, code = pp, r, c
		return created, nil
	}})
	r := d.call(t, "PUT", "/v1/admin/plans/pro/provider-prices/stripe-test/USD", `{"actor":"alice","reason":"wire it","provider_price_id":"price_pro"}`)
	if r.Status != http.StatusCreated || realm != "acme" || code != "pro" || got != (domain.ProviderPrice{Account: "stripe-test", Currency: "USD", ProviderPriceID: "price_pro"}) {
		t.Fatalf("created: %d %s; realm %q code %q mapping %+v", r.Status, r.Body, realm, code, got)
	}
	created = false
	if r := d.call(t, "PUT", "/v1/admin/plans/pro/provider-prices/stripe-test/USD", `{"actor":"alice","reason":"again","provider_price_id":"price_pro"}`); r.Status != http.StatusOK {
		t.Errorf("an identical repeat = %d %s, want 200", r.Status, r.Body)
	}
	if r := d.call(t, "PUT", "/v1/admin/plans/pro/provider-prices/stripe-test/USD", `{"actor":"alice","reason":"x","provider_price_id":"p","account":"other"}`); r.Status != http.StatusBadRequest {
		t.Errorf("an account in the body as well as the path = %d %s, want 400: the path is the identity", r.Status, r.Body)
	}
}

func TestAdminRefusalsAreStatusesAClientCanActOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"a plan code that exists", fmt.Errorf("%w: pro", md.ErrAlreadyExists), 409, "already_exists"},
		{"a mapping that would be replaced", fmt.Errorf("%w: pro is sold as price_a", md.ErrAlreadyExists), 409, "already_exists"},
		{"a shape the data refuses", fmt.Errorf("%w: no price in GBP", md.ErrInvalid), 400, "invalid_request"},
		{"no such plan", md.ErrNotFound, 404, "not_found"},
		{"anything else", errBoom, 500, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := adminDoor(t, adminStub{createPlan: func(context.Context, ports.ConfigChange, domain.Plan) (ports.PlanConfig, error) {
				return ports.PlanConfig{}, tc.err
			}})
			r := d.call(t, "POST", "/v1/admin/plans", createPlanBody)
			if r.Status != tc.status || r.ErrorCode(t) != tc.code {
				t.Errorf("%d %s, want %d %s", r.Status, r.Body, tc.status, tc.code)
			}
		})
	}
}

// ------------------------------------------------------------------------ accounts --

const accountBody = `{"actor":"alice","reason":"connect stripe","provider":"stripe","realms":["acme"],"mode":"test","secret_ref":"env:STRIPE_SECRET_KEY",` +
	`"webhook_secret_ref":"env:STRIPE_WEBHOOK_SECRET","active":true}`

func TestAnAccountIsStoredByReferenceAndAnswersWhatWasStored(t *testing.T) {
	t.Parallel()
	var got domain.ProviderAccount
	d := adminDoor(t, adminStub{
		account: func(context.Context, string) (domain.ProviderAccount, error) {
			return domain.ProviderAccount{}, md.ErrNotFound
		},
		putAccount: func(_ context.Context, _ ports.ConfigChange, a domain.ProviderAccount) (bool, error) {
			got = a
			return true, nil
		},
	})
	r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-test", accountBody)
	if r.Status != http.StatusCreated {
		t.Fatalf("status = %d: %s", r.Status, r.Body)
	}
	if !reflect.DeepEqual(got, stripeAccount("acme")) {
		t.Errorf("the use case was given %+v", got)
	}
	var out struct {
		ID               string   `json:"id"`
		Provider         string   `json:"provider"`
		Realms           []string `json:"realms"`
		Mode             string   `json:"mode"`
		SecretRef        string   `json:"secret_ref"`
		WebhookSecretRef string   `json:"webhook_secret_ref"`
		Active           bool     `json:"active"`
	}
	r.JSON(t, &out)
	if out.ID != "stripe-test" || out.Provider != "stripe" || len(out.Realms) != 1 || out.SecretRef != "env:STRIPE_SECRET_KEY" || !out.Active {
		t.Errorf("answered %+v", out)
	}
}

// `active` is part of the replacement: a PUT states the whole account, and an omitted flag must not silently switch a live
// account off (or an off one on).
func TestAnAccountReplacementStatesWhetherItIsActive(t *testing.T) {
	t.Parallel()
	d := adminDoor(t, adminStub{
		account: func(context.Context, string) (domain.ProviderAccount, error) {
			return domain.ProviderAccount{}, md.ErrNotFound
		},
		putAccount: func(context.Context, ports.ConfigChange, domain.ProviderAccount) (bool, error) {
			t.Error("stored without a stated flag")
			return true, nil
		},
	})
	body := strings.Replace(accountBody, `,"active":true`, ``, 1)
	if r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-test", body); r.Status != http.StatusBadRequest || !strings.Contains(string(r.Body), "active") {
		t.Errorf("= %d %s, want 400 naming active", r.Status, r.Body)
	}
}

// A credential administers the realms it covers. An account that serves a realm it does not cover is not its to read or to
// rewrite — and one that exists but is not its own is not found, so a stranger cannot probe for accounts.
func TestAnAdminOnlyTouchesAccountsOfRealmsItsCredentialCovers(t *testing.T) {
	t.Parallel()
	mine, theirs := stripeAccount("acme"), stripeAccount("someone-else")
	theirs.ID = "stripe-theirs"
	both := stripeAccount("acme", "someone-else")
	both.ID = "stripe-both"
	var puts int
	d := adminDoor(t, adminStub{
		accounts: func(context.Context) ([]domain.ProviderAccount, error) {
			return []domain.ProviderAccount{mine, theirs, both}, nil
		},
		account: func(_ context.Context, id string) (domain.ProviderAccount, error) {
			for _, a := range []domain.ProviderAccount{mine, theirs, both} {
				if a.ID == id {
					return a, nil
				}
			}
			return domain.ProviderAccount{}, md.ErrNotFound
		},
		putAccount: func(context.Context, ports.ConfigChange, domain.ProviderAccount) (bool, error) {
			puts++
			return false, nil
		},
	})
	r := d.call(t, "GET", "/v1/admin/provider-accounts", "")
	if r.Status != http.StatusOK || !strings.Contains(string(r.Body), `"stripe-test"`) || strings.Contains(string(r.Body), "stripe-theirs") {
		t.Errorf("list = %d %s, want only the accounts that serve a realm the credential covers", r.Status, r.Body)
	}
	if r := d.call(t, "GET", "/v1/admin/provider-accounts/stripe-theirs", ""); r.Status != http.StatusNotFound {
		t.Errorf("read of another realm's account = %d %s, want 404", r.Status, r.Body)
	}
	if r := d.call(t, "GET", "/v1/admin/provider-accounts/stripe-test", ""); r.Status != http.StatusOK {
		t.Errorf("read of my account = %d %s, want 200", r.Status, r.Body)
	}
	// Writing: the realms asked for must all be covered, and so must the realms the account serves now.
	other := strings.Replace(accountBody, `"realms":["acme"]`, `"realms":["acme","someone-else"]`, 1)
	if r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-test", other); r.Status != http.StatusForbidden || r.ErrorCode(t) != "realm_mismatch" {
		t.Errorf("granting an account a realm the credential lacks = %d %s, want 403 realm_mismatch", r.Status, r.Body)
	}
	if r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-both", accountBody); r.Status != http.StatusForbidden {
		t.Errorf("rewriting an account that serves a realm the credential lacks = %d %s, want 403", r.Status, r.Body)
	}
	if r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-theirs", accountBody); r.Status != http.StatusForbidden && r.Status != http.StatusNotFound {
		t.Errorf("rewriting another realm's account = %d %s, want 403 or 404", r.Status, r.Body)
	}
	if puts != 0 {
		t.Errorf("%d accounts were rewritten that the credential does not cover", puts)
	}
	if r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-test", accountBody); r.Status != http.StatusOK || puts != 1 {
		t.Errorf("rewriting my own = %d %s (puts %d), want 200", r.Status, r.Body, puts)
	}
}

func TestAnAccountIdInThePathIsTheOnlyId(t *testing.T) {
	t.Parallel()
	d := adminDoor(t, adminStub{account: func(context.Context, string) (domain.ProviderAccount, error) {
		return domain.ProviderAccount{}, md.ErrNotFound
	}})
	body := strings.Replace(accountBody, `"provider":"stripe"`, `"id":"other","provider":"stripe"`, 1)
	if r := d.call(t, "PUT", "/v1/admin/provider-accounts/stripe-test", body); r.Status != http.StatusBadRequest {
		t.Errorf("an id in the body = %d %s, want 400: the path is the identity", r.Status, r.Body)
	}
}

// ---------------------------------------------------------------------- customers --

func TestACustomerLinkNamesTheScopeTheAccountAndTheProvidersCustomer(t *testing.T) {
	t.Parallel()
	var got domain.Customer
	created := true
	d := adminDoor(t, adminStub{
		putCustomer: func(_ context.Context, _ ports.ConfigChange, c domain.Customer) (bool, error) {
			got = c
			return created, nil
		},
		customer: func(_ context.Context, s domain.Scope, account string) (domain.Customer, error) {
			if s.ID != "w1" {
				return domain.Customer{}, md.ErrNotFound
			}
			return domain.Customer{Scope: s, Account: account, ProviderCustomerID: "cus_1", BillingEmail: "a@example.com"}, nil
		},
	})
	body := `{"actor":"alice","reason":"migrate","scope":{"kind":"workspace","id":"w1"},"account":"stripe-test","provider_customer_id":"cus_1","billing_email":"a@example.com"}`
	r := d.call(t, "PUT", "/v1/admin/customers", body)
	want := domain.Customer{Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Account: "stripe-test", ProviderCustomerID: "cus_1", BillingEmail: "a@example.com"}
	if r.Status != http.StatusCreated || got != want {
		t.Fatalf("= %d %s; the use case was given %+v, want %+v", r.Status, r.Body, got, want)
	}
	created = false
	if r := d.call(t, "PUT", "/v1/admin/customers", body); r.Status != http.StatusOK {
		t.Errorf("an identical repeat = %d %s, want 200", r.Status, r.Body)
	}
	r = d.call(t, "GET", "/v1/admin/customers?scope_kind=workspace&scope_id=w1&account=stripe-test", "")
	if r.Status != http.StatusOK || !strings.Contains(string(r.Body), `"provider_customer_id":"cus_1"`) {
		t.Errorf("read = %d %s", r.Status, r.Body)
	}
	if r := d.call(t, "GET", "/v1/admin/customers?scope_kind=workspace&scope_id=nobody&account=stripe-test", ""); r.Status != http.StatusNotFound {
		t.Errorf("read of an unlinked scope = %d %s, want 404", r.Status, r.Body)
	}
	if r := d.call(t, "GET", "/v1/admin/customers?scope_kind=workspace&scope_id=w1", ""); r.Status != http.StatusBadRequest {
		t.Errorf("read with no account = %d %s, want 400", r.Status, r.Body)
	}
	other := strings.Replace(body, `"id":"w1"`, `"id":"w1","realm":"someone-else"`, 1)
	if r := d.call(t, "PUT", "/v1/admin/customers", other); r.Status != http.StatusForbidden || r.ErrorCode(t) != "realm_mismatch" {
		t.Errorf("a scope in a realm the credential lacks = %d %s, want 403 realm_mismatch", r.Status, r.Body)
	}
}
