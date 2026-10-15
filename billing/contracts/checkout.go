package contracts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ReturnHost is the one host the checkout under test accepts return URLs for.
const ReturnHost = "app.example.test"

// FulfilmentWindow is how long after a payment began the server reports it as the scope's current purchase.
const FulfilmentWindow = 30 * time.Minute

// CheckoutFactory builds the checkout under test over a harness's collaborators: its providers, accounts, catalogue,
// customers and payments, ReturnHost as the only host a return URL may name, and the tax strategy given (nil is
// provider_managed). It is given no Meter: a checkout cannot grant.
type CheckoutFactory func(t testing.TB, h *Harness, tax ports.TaxStrategy) ports.Checkout

// CheckoutCase is one subtest of CheckoutContract.
type CheckoutCase struct {
	Name string
	Run  func(t testing.TB, mk CheckoutFactory)
}

// CheckoutRequest is a valid request for a plan.
func (h *Harness) CheckoutRequest(planCode string) ports.CheckoutRequest {
	return ports.CheckoutRequest{Scope: h.Scope, PlanCode: planCode, Currency: "USD", SuccessURL: "https://" + ReturnHost + "/ok",
		CancelURL: "https://" + ReturnHost + "/no", BillingEmail: "buyer@example.test", IdemKey: "checkout-1"}
}

func (h *Harness) lastCheckout(t testing.TB) ports.CheckoutInput {
	t.Helper()
	all := h.Provider.Checkouts()
	if len(all) == 0 {
		t.Fatal("the provider was never asked for a checkout")
	}
	return all[len(all)-1]
}

func startErr(t testing.TB, c ports.Checkout, req ports.CheckoutRequest, want error) {
	t.Helper()
	url, err := c.Start(Ctx(), req)
	if !errors.Is(err, want) {
		t.Fatalf("Start(%+v) = %q, %v; want %v", req, url, err, want)
	}
	if url != "" {
		t.Fatalf("a refused checkout must return no URL, got %q", url)
	}
}

// CheckoutCases are the subtests of CheckoutContract (invariants 2, 4; FR-021, FR-022).
func CheckoutCases() []CheckoutCase {
	return []CheckoutCase{
		{"payment, subscription and setup each return a hosted page and grant nothing (2)", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			for _, tc := range []struct {
				name, plan, mode, wantMode, wantPrice string
			}{
				{"a pack", PackCode, "", "payment", PackPriceID},
				{"a subscription", ProCode, "", "subscription", ProPriceID},
				{"a pack, mode named", PackCode, "payment", "payment", PackPriceID},
				{"saving a method", "", "setup", "setup", ""},
			} {
				req := h.CheckoutRequest(tc.plan)
				req.Mode = tc.mode
				url, err := c.Start(Ctx(), req)
				if err != nil || !strings.HasPrefix(url, "https://") {
					t.Fatalf("%s: Start = %q, %v", tc.name, url, err)
				}
				in := h.lastCheckout(t)
				if in.Mode != tc.wantMode || in.ProviderPriceID != tc.wantPrice || in.PlanCode != tc.plan || in.Scope != h.Scope ||
					in.SuccessURL != req.SuccessURL || in.CancelURL != req.CancelURL {
					t.Fatalf("%s: the provider was asked for %+v", tc.name, in)
				}
			}
			if h.Meter.GrantCalls() != 0 || h.Payments.Count() != 0 {
				t.Fatalf("a checkout moved credits or recorded a payment: %d grant calls, %d payments", h.Meter.GrantCalls(), h.Payments.Count())
			}
			if touched := h.Log.Touched("meter.", "payments.", "subs."); len(touched) != 0 {
				t.Fatalf("starting a checkout touched %v: fulfilment is the webhook's", touched)
			}
		}},
		{"the price and credits come from the catalogue, never from the request (4)", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			inactive := domain.Plan{Realm: md.DefaultRealm, Code: "old", Name: "Retired", Kind: domain.OneTime, Pool: md.GeneralPool, Active: false,
				Prices: map[string]domain.Money{"USD": {MinorUnits: 100, Currency: "USD"}}}
			h.Catalog.Add(inactive, AccountID, "USD", "price_old")
			for name, mutate := range map[string]func(*ports.CheckoutRequest){
				"a plan the catalogue does not sell": func(r *ports.CheckoutRequest) { r.PlanCode = "free-money" },
				"a currency the plan is not sold in": func(r *ports.CheckoutRequest) { r.Currency = "JPY" },
				"a retired plan":                     func(r *ports.CheckoutRequest) { r.PlanCode = "old" },
			} {
				req := h.CheckoutRequest(PackCode)
				mutate(&req)
				url, err := c.Start(Ctx(), req)
				if !errors.Is(err, domain.ErrUnknownPrice) || url != "" {
					t.Fatalf("%s: Start = %q, %v; want domain.ErrUnknownPrice and no URL — never a default plan", name, url, err)
				}
			}
			req := h.CheckoutRequest(PackCode)
			req.Currency = "usd"
			startErr(t, c, req, md.ErrInvalid)
			if n := len(h.Provider.Checkouts()); n != 0 {
				t.Fatalf("the provider was asked for %d checkouts the catalogue does not sell", n)
			}
			if _, err := c.Start(Ctx(), h.CheckoutRequest(PackCode)); err != nil {
				t.Fatal(err)
			}
			if in := h.lastCheckout(t); in.ProviderPriceID != PackPriceID || in.Currency != "USD" {
				t.Fatalf("input = %+v: the price is the catalogue's mapping for this account and currency", in)
			}
		}},
		{"the mode must match the plan", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			for name, tc := range map[string][2]string{
				"a subscription of a pack":       {PackCode, "subscription"},
				"a payment for a recurring plan": {ProCode, "payment"},
				"a mode nobody offers":           {PackCode, "installments"},
				"a plan with a setup":            {PackCode, "setup"},
			} {
				req := h.CheckoutRequest(tc[0])
				req.Mode = tc[1]
				if url, err := c.Start(Ctx(), req); !errors.Is(err, md.ErrInvalid) || url != "" {
					t.Errorf("%s: Start = %q, %v; want ErrInvalid", name, url, err)
				}
			}
			if len(h.Provider.Checkouts()) != 0 {
				t.Fatal("a checkout whose mode contradicts its plan reached the provider")
			}
		}},
		{"return URLs name an https page on the configured host", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			bad := []string{"", "/relative", "javascript:alert(1)", "http://" + ReturnHost + "/ok", "https://evil.example/ok",
				"https://" + ReturnHost + "@evil.example/ok", "https://" + ReturnHost + ".evil.example/ok", "//evil.example/x", "https:///nohost",
				"https://user:pw@" + ReturnHost + "/ok", "https://" + ReturnHost + ":8443/ok"}
			for _, u := range bad {
				req := h.CheckoutRequest(PackCode)
				req.SuccessURL = u
				if url, err := c.Start(Ctx(), req); !errors.Is(err, md.ErrInvalid) || url != "" {
					t.Errorf("success URL %q: Start = %q, %v; want ErrInvalid", u, url, err)
				}
				req = h.CheckoutRequest(PackCode)
				req.CancelURL = u
				if _, err := c.Start(Ctx(), req); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("cancel URL %q: err = %v; want ErrInvalid", u, err)
				}
			}
			if len(h.Provider.Checkouts()) != 0 {
				t.Fatal("a checkout with a return URL off the allow-list reached the provider")
			}
			req := h.CheckoutRequest(PackCode)
			req.SuccessURL = "https://" + ReturnHost + "/ok?from=checkout#done"
			if _, err := c.Start(Ctx(), req); err != nil {
				t.Fatalf("a query and a fragment are fine: %v", err)
			}
		}},
		{"a scope must be valid and in a realm the account serves", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			req := h.CheckoutRequest(PackCode)
			req.Scope = md.Scope{Realm: md.DefaultRealm, Kind: "", ID: "w1"}
			startErr(t, c, req, md.ErrInvalid)
			req = h.CheckoutRequest(PackCode)
			req.Scope = h.Foreign // realm "other": no account serves it
			startErr(t, c, req, domain.ErrUnknownAccount)
			if len(h.Provider.Checkouts()) != 0 || h.Provider.EnsureCalls() != 0 {
				t.Fatal("a scope no account serves reached the provider")
			}
		}},
		{"a named account must exist, be active, serve the realm and sell the plan", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			for name, tc := range map[string]struct {
				account string
				want    error
			}{
				"an account that does not exist": {"nope", domain.ErrUnknownAccount},
				"an inactive account":            {InactiveID, domain.ErrUnknownAccount},
				"another provider's account":     {ElsewhereID, domain.ErrUnknownAccount},
				"an account that sells nothing":  {OtherAccountID, domain.ErrUnknownPrice},
			} {
				req := h.CheckoutRequest(PackCode)
				req.Account = tc.account
				if url, err := c.Start(Ctx(), req); !errors.Is(err, tc.want) || url != "" {
					t.Errorf("%s: Start = %q, %v; want %v", name, url, err, tc.want)
				}
			}
			req := h.CheckoutRequest(PackCode)
			req.Account = AccountID
			if _, err := c.Start(Ctx(), req); err != nil {
				t.Fatal(err)
			}
		}},
		{"an unnamed account is the one that sells the plan", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			credits := PackCredits
			h.Catalog.Add(domain.Plan{Realm: md.DefaultRealm, Code: "pack-b", Name: "Pack sold through B", Kind: domain.OneTime, CreditAllotment: &credits,
				Pool: md.GeneralPool, Active: true, Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}}}, OtherAccountID, "USD", "price_pack_b")
			url, err := c.Start(Ctx(), h.CheckoutRequest("pack-b"))
			if err != nil || !strings.Contains(url, "/"+OtherAccountID+"/") {
				t.Fatalf("Start = %q, %v: want a checkout on account %s, the only one that sells it", url, err, OtherAccountID)
			}
			if in := h.lastCheckout(t); in.ProviderPriceID != "price_pack_b" {
				t.Fatalf("input = %+v", in)
			}
		}},
		{"a customer is created once and mapped to its scope before a payment can arrive", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			fresh := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w3"}
			req := h.CheckoutRequest(PackCode)
			req.Scope = fresh
			if _, err := c.Start(Ctx(), req); err != nil {
				t.Fatal(err)
			}
			cust, ok, _ := h.Customers.Get(Ctx(), fresh, AccountID)
			if !ok || cust.ProviderCustomerID == "" || cust.BillingEmail != "buyer@example.test" {
				t.Fatalf("customer = %+v, %v: the mapping must exist before the buyer reaches the provider's page", cust, ok)
			}
			if got, ok, _ := h.Customers.ResolveScope(Ctx(), AccountID, cust.ProviderCustomerID); !ok || got != fresh {
				t.Fatalf("a webhook about %s would resolve to %+v, %v", cust.ProviderCustomerID, got, ok)
			}
			if in := h.lastCheckout(t); in.CustomerID != cust.ProviderCustomerID {
				t.Fatalf("the checkout was created for customer %q, the mapping says %q", in.CustomerID, cust.ProviderCustomerID)
			}
			if _, err := c.Start(Ctx(), req); err != nil {
				t.Fatal(err)
			}
			if h.Provider.EnsureCalls() != 1 {
				t.Fatalf("EnsureCustomer was called %d times for one scope", h.Provider.EnsureCalls())
			}

			// An existing customer is reused: cus_1 is already mapped to h.Scope.
			if _, err := c.Start(Ctx(), h.CheckoutRequest(PackCode)); err != nil {
				t.Fatal(err)
			}
			if in := h.lastCheckout(t); in.CustomerID != "cus_1" || h.Provider.EnsureCalls() != 1 {
				t.Fatalf("input customer %q, %d ensure calls: a mapped scope is not created again", in.CustomerID, h.Provider.EnsureCalls())
			}
		}},
		{"two first checkouts at once create one customer", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			fresh := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w4"}
			var wg sync.WaitGroup
			errs := make([]error, 8)
			for i := range errs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req := h.CheckoutRequest(PackCode)
					req.Scope = fresh
					_, errs[i] = c.Start(Ctx(), req)
				}()
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("checkout %d: %v", i, err)
				}
			}
			if n := h.Provider.CustomersCreated(); n != 1 {
				t.Fatalf("%d provider customers for one scope: the others are customers whose payments no webhook can place", n)
			}
			cust, _, _ := h.Customers.Get(Ctx(), fresh, AccountID)
			for _, in := range h.Provider.Checkouts() {
				if in.CustomerID != cust.ProviderCustomerID {
					t.Fatalf("a checkout was created for customer %q, the mapping says %q", in.CustomerID, cust.ProviderCustomerID)
				}
			}
		}},
		{"the request's idempotency key and the scope reach the provider, as correlation and not authority", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			if _, err := c.Start(Ctx(), h.CheckoutRequest(PackCode)); err != nil {
				t.Fatal(err)
			}
			in := h.lastCheckout(t)
			if in.IdemKey != "checkout-1" {
				t.Fatalf("idem key = %q: a double-clicked buy button must create one checkout", in.IdemKey)
			}
			if in.Metadata["plan"] != PackCode || in.Metadata["scope_id"] != "w1" {
				t.Fatalf("metadata = %v: it echoes back for correlation", in.Metadata)
			}
		}},
		{"a failing provider fails the checkout and leaves credits alone", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			h.Provider.FailCheckout(errors.New("provider is down"))
			if url, err := c.Start(Ctx(), h.CheckoutRequest(PackCode)); err == nil || url != "" {
				t.Fatalf("Start = %q, %v: a provider failure must be an error", url, err)
			}
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("credits moved on a failed checkout")
			}
		}},
		{"a tax strategy that fails fails the checkout closed", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			boom := errors.New("tax service is down")
			c := mk(t, h, failingTax{boom})
			url, err := c.Start(Ctx(), h.CheckoutRequest(PackCode))
			if !errors.Is(err, boom) || url != "" {
				t.Fatalf("Start = %q, %v: an unpriced tax is not a checkout", url, err)
			}
			if len(h.Provider.Checkouts()) != 0 {
				t.Fatal("a checkout whose tax could not be settled reached the provider")
			}
		}},
		{"a return from checkout grants nothing, and fulfilment is the server's report (2)", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			if _, err := c.Start(Ctx(), h.CheckoutRequest(PackCode)); err != nil {
				t.Fatal(err)
			}
			// The buyer comes back to the success URL and the page asks what happened — over and over.
			for range 5 {
				f, err := c.Fulfilment(Ctx(), h.Scope)
				if err != nil || f.Kind != ports.FulfilmentNone {
					t.Fatalf("Fulfilment = %+v, %v: with no payment recorded nothing has been granted", f, err)
				}
			}
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("a checkout return granted credits (invariant 2)")
			}

			put := func(status domain.PaymentStatus, reason string, credits md.Credits, scope md.Scope) {
				p := domain.Payment{Scope: scope, Account: AccountID, ProviderPaymentID: "pay-f", Kind: "one_time", Amount: domain.Money{MinorUnits: 1000, Currency: "USD"},
					CreditsGranted: credits, Pool: md.GeneralPool, Status: status, FailureReason: reason, ReceiptURL: "https://pay.fake.test/receipt/1", IdemKey: "pay-f", CreatedAt: h.Clock.Now()}
				if err := h.Payments.Upsert(Ctx(), p); err != nil {
					t.Fatal(err)
				}
			}
			put(domain.Succeeded, "", 0, h.Scope2) // another scope's payment is nobody else's business
			if f, _ := c.Fulfilment(Ctx(), h.Scope); f.Kind != ports.FulfilmentNone {
				t.Fatalf("another scope's payment leaked into this one's fulfilment: %+v", f)
			}
			put(domain.Pending, "", 0, h.Scope)
			if f, _ := c.Fulfilment(Ctx(), h.Scope); f.Kind != ports.FulfilmentProcessing || !f.Since.Equal(h.Clock.Now()) {
				t.Fatalf("a pending payment is processing since it began: %+v", f)
			}
			put(domain.Failed, "card_declined", 0, h.Scope)
			if f, _ := c.Fulfilment(Ctx(), h.Scope); f.Kind != ports.FulfilmentFailed || f.Reason != "card_declined" {
				t.Fatalf("a failed payment is failed, with the provider's reason: %+v", f)
			}
			put(domain.Succeeded, "", PackCredits, h.Scope)
			f, _ := c.Fulfilment(Ctx(), h.Scope)
			if f.Kind != ports.FulfilmentGranted || f.Amount != PackCredits || f.ReceiptURL == "" {
				t.Fatalf("a succeeded payment is granted what the books were given: %+v", f)
			}
			h.Clock.Advance(FulfilmentWindow + time.Minute)
			if f, _ := c.Fulfilment(Ctx(), h.Scope); f.Kind != ports.FulfilmentNone {
				t.Fatalf("a purchase from long ago is not the one the buyer just made: %+v", f)
			}
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("reading fulfilment granted credits")
			}
		}},
		{"the portal is the provider's page for a customer who has bought before", func(t testing.TB, mk CheckoutFactory) {
			h := NewHarness()
			c := mk(t, h, nil)
			url, err := c.Portal(Ctx(), h.Scope, "", "https://"+ReturnHost+"/billing")
			if err != nil || !strings.Contains(url, "/portal/cus_1") {
				t.Fatalf("Portal = %q, %v", url, err)
			}
			nobody := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "never-bought"}
			if url, err := c.Portal(Ctx(), nobody, "", "https://"+ReturnHost+"/billing"); !errors.Is(err, md.ErrNotFound) || url != "" {
				t.Fatalf("a scope with no customer: Portal = %q, %v; want ErrNotFound", url, err)
			}
			if _, err := c.Portal(Ctx(), h.Scope, "", "https://evil.example/billing"); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("a return URL off the allow-list: err = %v; want ErrInvalid", err)
			}
		}},
	}
}

// failingTax is a tax strategy whose service is down.
type failingTax struct{ err error }

func (failingTax) Mode() domain.TaxMode { return domain.TaxExternalHook }
func (f failingTax) Annotate(ctx context.Context, _ *ports.CheckoutInput) error {
	return fmt.Errorf("tax: %w", f.err)
}

// CheckoutContract is the specification of the checkout (invariants 2 and 4; FR-021, FR-022): it sells a plan the
// catalogue has, at the catalogue's price, through an account that serves the realm, after mapping the customer to
// its scope — and it grants nothing, on a return from the provider or ever. Credits arrive when the verified
// payment is processed, and the only way a UI learns that is Fulfilment.
func CheckoutContract(t *testing.T, mk CheckoutFactory) {
	t.Helper()
	for _, c := range CheckoutCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}
