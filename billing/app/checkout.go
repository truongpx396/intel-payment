package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// DefaultFulfilmentWindow is how long after a payment began the server reports it as the scope's current purchase.
const DefaultFulfilmentWindow = 30 * time.Minute

// CheckoutDeps are the ports the checkout needs. It is given no Meter, and that is the point: it cannot grant.
type CheckoutDeps struct {
	Providers ports.Providers
	Accounts  ports.Accounts
	Catalog   ports.Catalog
	Customers ports.Customers
	Payments  ports.Payments
	// Tax names who computes tax; nil is provider_managed, which adds nothing.
	Tax   ports.TaxStrategy
	Clock mports.Clock
	// ReturnHosts are the hosts a return URL may name (host or host:port, as the URL spells it). Empty accepts any
	// https host; a deployment that knows its hosts should list them, because a return URL is where a buyer is sent.
	ReturnHosts []string
	Window      time.Duration // default DefaultFulfilmentWindow
	Log         *slog.Logger
}

// Checkout sells a plan the catalogue has, at the catalogue's price, through an account that serves the realm, after
// mapping the customer to its scope — and grants nothing. Credits arrive when the verified payment is processed
// (payments invariants 2 and 4).
type Checkout struct {
	d     CheckoutDeps
	hosts map[string]bool
}

var _ ports.Checkout = (*Checkout)(nil)

// NewCheckout validates its dependencies and returns the checkout.
func NewCheckout(d CheckoutDeps) (*Checkout, error) {
	var errs []error
	if len(d.Providers) == 0 || d.Accounts == nil || d.Catalog == nil || d.Customers == nil || d.Payments == nil {
		errs = append(errs, errors.New("the Providers, Accounts, Catalog, Customers and Payments are required"))
	}
	if d.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: the core reads no wall clock"))
	}
	if d.Window < 0 {
		errs = append(errs, errors.New("the fulfilment window cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("billing checkout: %w", err)
	}
	if d.Tax == nil {
		d.Tax = passthroughTax{domain.TaxProviderManaged}
	}
	if d.Window == 0 {
		d.Window = DefaultFulfilmentWindow
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	hosts := map[string]bool{}
	for _, h := range d.ReturnHosts {
		hosts[strings.ToLower(h)] = true
	}
	return &Checkout{d: d, hosts: hosts}, nil
}

// returnURL refuses a URL a buyer should not be sent to: not https, with credentials in it, naming no host, or a
// host outside the configured list.
func (c *Checkout) returnURL(what, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || raw == "":
		return nil, fmt.Errorf("%w: %s is not a URL", md.ErrInvalid, what)
	case u.Scheme != "https" || u.Opaque != "":
		return nil, fmt.Errorf("%w: %s must be an https URL", md.ErrInvalid, what)
	case u.User != nil:
		return nil, fmt.Errorf("%w: %s must not carry credentials", md.ErrInvalid, what)
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("%w: %s names no host", md.ErrInvalid, what)
	case len(c.hosts) > 0 && !c.hosts[strings.ToLower(u.Host)]:
		return nil, fmt.Errorf("%w: %s names a host this deployment does not send buyers to", md.ErrInvalid, what)
	}
	return u, nil
}

// account picks the provider account a checkout goes through: the one named, if it is live and serves the realm.
func (c *Checkout) account(ctx context.Context, realm domain.Realm, named string) ([]domain.ProviderAccount, error) {
	if named != "" {
		a, found, err := c.d.Accounts.Account(ctx, named)
		if err != nil {
			return nil, fmt.Errorf("provider account %s: %w", named, err)
		}
		if !found || !a.Serves(realm) {
			return nil, fmt.Errorf("%w: %s does not serve realm %s", domain.ErrUnknownAccount, named, realm.Or())
		}
		if _, ok := c.d.Providers[a.Provider]; !ok {
			return nil, fmt.Errorf("%w: %s belongs to provider %s, which this deployment does not run", domain.ErrUnknownAccount, named, a.Provider)
		}
		return []domain.ProviderAccount{a}, nil
	}
	all, err := c.d.Accounts.ForRealm(ctx, realm)
	if err != nil {
		return nil, fmt.Errorf("provider accounts of realm %s: %w", realm.Or(), err)
	}
	live := all[:0:0]
	for _, a := range all {
		if _, ok := c.d.Providers[a.Provider]; ok {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("%w: no account serves realm %s", domain.ErrUnknownAccount, realm.Or())
	}
	return live, nil
}

// Start implements ports.Checkout.
func (c *Checkout) Start(ctx context.Context, req ports.CheckoutRequest) (string, error) {
	if err := req.Scope.Validate(); err != nil {
		return "", err
	}
	scope := req.Scope.Normalized()
	switch req.Mode {
	case "", "payment", "subscription", "setup":
	default:
		return "", fmt.Errorf("%w: checkout mode %q is not payment, subscription or setup", md.ErrInvalid, req.Mode)
	}
	if req.Mode == "setup" && req.PlanCode != "" {
		return "", fmt.Errorf("%w: a setup checkout saves a payment method and sells no plan", md.ErrInvalid)
	}
	if req.Mode != "setup" && req.PlanCode == "" {
		return "", fmt.Errorf("%w: a checkout needs a plan code", md.ErrInvalid)
	}
	if req.Currency != "" || req.Mode != "setup" {
		if err := (md.Money{Currency: req.Currency}).Validate(); err != nil {
			return "", err
		}
	}
	if _, err := c.returnURL("the success URL", req.SuccessURL); err != nil {
		return "", err
	}
	if _, err := c.returnURL("the cancel URL", req.CancelURL); err != nil {
		return "", err
	}

	candidates, err := c.account(ctx, scope.Realm, req.Account)
	if err != nil {
		return "", err
	}
	acct, mode, plan, priceID, err := c.sell(ctx, scope.Realm, candidates, req)
	if err != nil {
		return "", err
	}
	provider, ok := c.d.Providers[acct.Provider]
	if !ok {
		return "", fmt.Errorf("%w: no adapter for provider %s", domain.ErrUnknownAccount, acct.Provider)
	}

	customerID, err := c.customer(ctx, provider, acct, scope, req.BillingEmail)
	if err != nil {
		return "", err
	}
	in := ports.CheckoutInput{Scope: scope, Mode: mode, PlanCode: plan.Code, Currency: req.Currency, ProviderPriceID: priceID, CustomerID: customerID,
		BillingEmail: req.BillingEmail, SuccessURL: req.SuccessURL, CancelURL: req.CancelURL, IdemKey: req.IdemKey,
		// Correlation only, echoed back by the provider; the webhook never trusts it (invariant 4).
		Metadata: map[string]string{"realm": string(scope.Realm), "scope_kind": scope.Kind, "scope_id": scope.ID, "plan": plan.Code}}
	if err := c.d.Tax.Annotate(ctx, &in); err != nil {
		return "", fmt.Errorf("tax (%s): %w", c.d.Tax.Mode(), err)
	}
	redirect, err := provider.CreateCheckout(ctx, acct, in)
	if err != nil {
		return "", fmt.Errorf("create checkout at %s: %w", acct.ID, err)
	}
	return redirect, nil
}

// sell resolves what is being sold, and through which account, from the catalogue and nothing else.
func (c *Checkout) sell(ctx context.Context, realm domain.Realm, candidates []domain.ProviderAccount, req ports.CheckoutRequest) (domain.ProviderAccount, string, domain.Plan, string, error) {
	if req.Mode == "setup" {
		return candidates[0], "setup", domain.Plan{}, "", nil
	}
	for _, acct := range candidates {
		plan, priceID, err := c.d.Catalog.Resolve(ctx, realm, req.PlanCode, req.Currency, acct.ID)
		switch {
		case errors.Is(err, domain.ErrUnknownPrice):
			continue
		case err != nil:
			return domain.ProviderAccount{}, "", domain.Plan{}, "", fmt.Errorf("catalogue: %s in %s: %w", req.PlanCode, req.Currency, err)
		}
		// Trust, but check: what came back must be the plan that was asked for, live, in this realm.
		if plan.Code != req.PlanCode || !plan.Active || plan.Realm.Or() != realm.Or() || priceID == "" {
			continue
		}
		mode := "payment"
		if plan.Kind == domain.Subscription {
			mode = "subscription"
		}
		if req.Mode != "" && req.Mode != mode {
			return domain.ProviderAccount{}, "", domain.Plan{}, "", fmt.Errorf("%w: plan %s is %s and cannot be bought as a %s", md.ErrInvalid, plan.Code, plan.Kind, req.Mode)
		}
		return acct, mode, plan, priceID, nil
	}
	return domain.ProviderAccount{}, "", domain.Plan{}, "", fmt.Errorf("%w: %s in %s is not sold", domain.ErrUnknownPrice, req.PlanCode, req.Currency)
}

// customer returns the provider customer of a scope, creating and MAPPING it first if there is none: the mapping must
// exist before the buyer reaches the provider's page, or the payment they make arrives as a webhook about a customer
// nobody knows. The first writer wins; a second checkout racing the first reads back the winner's id.
func (c *Checkout) customer(ctx context.Context, provider ports.PaymentProvider, acct domain.ProviderAccount, scope domain.Scope, email string) (string, error) {
	cust, found, err := c.d.Customers.Get(ctx, scope, acct.ID)
	if err != nil {
		return "", fmt.Errorf("read the customer of %s: %w", scope.Tag(), err)
	}
	if found && (email == "" || cust.BillingEmail == email) {
		return cust.ProviderCustomerID, nil
	}
	if !found {
		id, err := provider.EnsureCustomer(ctx, acct, ports.CustomerInput{Scope: scope, BillingEmail: email, IdemKey: "customer:" + acct.ID + ":" + scope.Tag()})
		if err != nil {
			return "", fmt.Errorf("create the customer of %s at %s: %w", scope.Tag(), acct.ID, err)
		}
		cust = domain.Customer{Scope: scope, Account: acct.ID, ProviderCustomerID: id}
	}
	cust.BillingEmail = email
	if err := c.d.Customers.Upsert(ctx, cust); err != nil {
		return "", fmt.Errorf("map the customer of %s: %w", scope.Tag(), err)
	}
	stored, ok, err := c.d.Customers.Get(ctx, scope, acct.ID)
	if err != nil {
		return "", fmt.Errorf("read the customer of %s: %w", scope.Tag(), err)
	}
	if !ok {
		return "", fmt.Errorf("the customer of %s was not stored: a payment could not be placed", scope.Tag())
	}
	return stored.ProviderCustomerID, nil
}

// Portal implements ports.Checkout.
func (c *Checkout) Portal(ctx context.Context, scope domain.Scope, account, returnTo string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	scope = scope.Normalized()
	if _, err := c.returnURL("the return URL", returnTo); err != nil {
		return "", err
	}
	candidates, err := c.account(ctx, scope.Realm, account)
	if err != nil {
		return "", err
	}
	for _, acct := range candidates {
		cust, found, err := c.d.Customers.Get(ctx, scope, acct.ID)
		if err != nil {
			return "", fmt.Errorf("read the customer of %s: %w", scope.Tag(), err)
		}
		if !found {
			continue
		}
		provider, ok := c.d.Providers[acct.Provider]
		if !ok {
			return "", fmt.Errorf("%w: no adapter for provider %s", domain.ErrUnknownAccount, acct.Provider)
		}
		u, err := provider.CreatePortalSession(ctx, acct, cust.ProviderCustomerID, returnTo)
		if err != nil {
			return "", fmt.Errorf("create the portal session at %s: %w", acct.ID, err)
		}
		return u, nil
	}
	return "", fmt.Errorf("%w: %s has no billing account yet", md.ErrNotFound, scope.Tag())
}

// Fulfilment implements ports.Checkout: the server's report on the scope's latest payment, from the payment record
// the processor writes. A UI that has returned from checkout shows "processing" and asks this; it is the only
// sanctioned way to learn that credits arrived (invariant 2). Reading it grants nothing.
func (c *Checkout) Fulfilment(ctx context.Context, scope domain.Scope) (ports.Fulfilment, error) {
	if err := scope.Validate(); err != nil {
		return ports.Fulfilment{}, err
	}
	latest, _, err := c.d.Payments.List(ctx, scope.Normalized(), "", 1)
	if err != nil {
		return ports.Fulfilment{}, fmt.Errorf("read the payments of %s: %w", scope.Tag(), err)
	}
	if len(latest) == 0 || c.d.Clock.Now().Sub(latest[0].CreatedAt) > c.d.Window {
		return ports.Fulfilment{Kind: ports.FulfilmentNone}, nil
	}
	p := latest[0]
	switch p.Status {
	case domain.Pending:
		return ports.Fulfilment{Kind: ports.FulfilmentProcessing, Since: p.CreatedAt}, nil
	case domain.Failed:
		return ports.Fulfilment{Kind: ports.FulfilmentFailed, Reason: p.FailureReason}, nil
	case domain.Succeeded, domain.PartiallyRefunded, domain.Disputed:
		return ports.Fulfilment{Kind: ports.FulfilmentGranted, Amount: p.CreditsGranted, ReceiptURL: p.ReceiptURL}, nil
	}
	return ports.Fulfilment{Kind: ports.FulfilmentNone}, nil
}
