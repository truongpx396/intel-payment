package domain_test

import (
	"errors"
	"strings"
	"testing"

	domain "github.com/truongpx396/intel-payment/billing/domain"
	mdomain "github.com/truongpx396/intel-payment/metering/domain"
)

func credits(n int64) *domain.Credits { c := domain.Credits(n); return &c }

func pack() domain.Plan {
	return domain.Plan{
		Realm: "acme", Code: "pack_10", Name: "10 credits", Kind: domain.OneTime, CreditAllotment: credits(1000), Pool: "general", Active: true,
		Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}},
	}
}

func proPlan() domain.Plan {
	month := domain.Monthly
	p := pack()
	p.Code, p.Name, p.Kind, p.Interval = "pro", "Pro", domain.Subscription, &month
	return p
}

// R100: a plan is what a customer is sold, and its code is the only identifier a client ever sees, so what a plan may be is
// decided once, in the domain, before any store is asked. A plan that is wrongly shaped is refused — it is never repaired into
// one that sells something nobody meant to sell.
func TestAPlanIsRefusedUnlessItIsOneThingAndPricedInWholeMinorUnits(t *testing.T) {
	t.Parallel()
	for name, p := range map[string]domain.Plan{"a pack": pack(), "a subscription": proPlan()} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
	free := pack()
	free.CreditAllotment = credits(0)
	negotiated := pack()
	negotiated.CreditAllotment = nil
	unpriced := pack()
	unpriced.Prices = nil
	for name, p := range map[string]domain.Plan{"a free pack": free, "a negotiated allotment": negotiated, "no price yet": unpriced} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}

	cases := map[string]func(*domain.Plan){
		"no code":                     func(p *domain.Plan) { p.Code = "" },
		"a code a path cannot carry":  func(p *domain.Plan) { p.Code = "pack/10" },
		"a code with a space":         func(p *domain.Plan) { p.Code = "pack 10" },
		"an upper-case code":          func(p *domain.Plan) { p.Code = "Pack10" },
		"a code too long":             func(p *domain.Plan) { p.Code = strings.Repeat("a", 64) },
		"a blank name":                func(p *domain.Plan) { p.Name = "  " },
		"a malformed realm":           func(p *domain.Plan) { p.Realm = "Not A Realm" },
		"a kind nobody defined":       func(p *domain.Plan) { p.Kind = "lifetime" },
		"a negative allotment":        func(p *domain.Plan) { p.CreditAllotment = credits(-1) },
		"a pool that is not a name":   func(p *domain.Plan) { p.Pool = "Bad Pool" },
		"a pack that renews":          func(p *domain.Plan) { m := domain.Monthly; p.Interval = &m },
		"a subscription that never":   func(p *domain.Plan) { *p = proPlan(); p.Interval = nil },
		"an interval nobody defined":  func(p *domain.Plan) { *p = proPlan(); w := domain.Interval("week"); p.Interval = &w },
		"a lower-case currency":       func(p *domain.Plan) { p.Prices = map[string]domain.Money{"usd": {MinorUnits: 1, Currency: "usd"}} },
		"a two-letter currency":       func(p *domain.Plan) { p.Prices = map[string]domain.Money{"US": {MinorUnits: 1, Currency: "US"}} },
		"a price filed under another": func(p *domain.Plan) { p.Prices = map[string]domain.Money{"USD": {MinorUnits: 1, Currency: "EUR"}} },
		"a negative price":            func(p *domain.Plan) { p.Prices = map[string]domain.Money{"USD": {MinorUnits: -1, Currency: "USD"}} },
		"a price with a hidden second": func(p *domain.Plan) {
			p.Prices = map[string]domain.Money{"USD": {MinorUnits: 1, Currency: "USD"}, "": {}}
		},
	}
	for name, mutate := range cases {
		p := pack()
		mutate(&p)
		err := p.Validate()
		if !errors.Is(err, mdomain.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

func TestAProviderPriceNamesAnAccountACurrencyAndTheProvidersPrice(t *testing.T) {
	t.Parallel()
	good := domain.ProviderPrice{Account: "stripe-live", Currency: "USD", ProviderPriceID: "price_1Pabc"}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good mapping was refused: %v", err)
	}
	for name, mutate := range map[string]func(*domain.ProviderPrice){
		"no account":                        func(p *domain.ProviderPrice) { p.Account = "" },
		"an account the route cannot carry": func(p *domain.ProviderPrice) { p.Account = "a/b" },
		"no currency":                       func(p *domain.ProviderPrice) { p.Currency = "" },
		"a lower-case currency":             func(p *domain.ProviderPrice) { p.Currency = "usd" },
		"no price id":                       func(p *domain.ProviderPrice) { p.ProviderPriceID = "" },
		"a price id with a space":           func(p *domain.ProviderPrice) { p.ProviderPriceID = "price 1" },
		"a price id past the cap":           func(p *domain.ProviderPrice) { p.ProviderPriceID = strings.Repeat("p", 256) },
	} {
		p := good
		mutate(&p)
		if err := p.Validate(); !errors.Is(err, mdomain.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

// A customer row is where a payment lands: it decides which scope's books a verified event is booked to (invariant 4).
func TestACustomerNamesAScopeAnAccountAndTheProvidersCustomer(t *testing.T) {
	t.Parallel()
	good := domain.Customer{Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Account: "stripe-live", ProviderCustomerID: "cus_123", BillingEmail: "a@example.com"}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good customer was refused: %v", err)
	}
	noEmail := good
	noEmail.BillingEmail = ""
	if err := noEmail.Validate(); err != nil {
		t.Errorf("a customer with no email was refused: %v", err)
	}
	for name, mutate := range map[string]func(*domain.Customer){
		"no scope kind":                    func(c *domain.Customer) { c.Scope.Kind = "" },
		"no scope id":                      func(c *domain.Customer) { c.Scope.ID = "" },
		"no account":                       func(c *domain.Customer) { c.Account = "" },
		"no provider customer":             func(c *domain.Customer) { c.ProviderCustomerID = "" },
		"a provider customer with a space": func(c *domain.Customer) { c.ProviderCustomerID = "cus 1" },
		"an email that is not":             func(c *domain.Customer) { c.BillingEmail = "not an address" },
	} {
		c := good
		mutate(&c)
		if err := c.Validate(); !errors.Is(err, mdomain.ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
}

// A secret reference names where a secret lives, and a scheme the deployment cannot read is a webhook answered 500 for ever —
// so an account is only accepted with the schemes the deployment can resolve.
func TestASecretReferenceMustUseASchemeTheDeploymentResolves(t *testing.T) {
	t.Parallel()
	a := account()
	if err := a.ValidateRefs("env", "file"); err != nil {
		t.Fatalf("env references were refused: %v", err)
	}
	a.SecretRef = "file:/run/secrets/stripe_key"
	if err := a.ValidateRefs("env", "file"); err != nil {
		t.Fatalf("a file reference was refused: %v", err)
	}
	for name, mutate := range map[string]func(*domain.ProviderAccount){
		"vault, which nothing resolves":    func(a *domain.ProviderAccount) { a.SecretRef = "vault:secret/stripe" },
		"a webhook ref nobody resolves":    func(a *domain.ProviderAccount) { a.WebhookSecretRef = "aws:arn:thing" },
		"an env name that is not a name":   func(a *domain.ProviderAccount) { a.SecretRef = "env:NOT A NAME" },
		"an env name with a leading digit": func(a *domain.ProviderAccount) { a.SecretRef = "env:1KEY" },
		"a relative file":                  func(a *domain.ProviderAccount) { a.SecretRef = "file:secrets/key" },
		"a climbing file":                  func(a *domain.ProviderAccount) { a.SecretRef = "file:/run/../etc/passwd" },
		"a file with a NUL":                func(a *domain.ProviderAccount) { a.SecretRef = "file:/run/secrets/a\x00b" },
	} {
		b := account()
		mutate(&b)
		if err := b.ValidateRefs("env", "file"); !errors.Is(err, mdomain.ErrInvalid) {
			t.Errorf("%s: ValidateRefs = %v, want ErrInvalid", name, err)
		}
	}
	if err := account().ValidateRefs(); err == nil {
		t.Error("a deployment that resolves no scheme accepted an account")
	}
}
