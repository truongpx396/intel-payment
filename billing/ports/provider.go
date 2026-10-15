package ports

import (
	"context"
	"net/http"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
)

// PaymentProvider is the one provider-specific seam. Stripe, Polar and PayPal are interchangeable adapters behind
// it, and no code outside billing/adapters/driven/<provider>/ may import a provider SDK.
type PaymentProvider interface {
	Name() domain.Provider

	// EnsureCustomer creates the provider's customer for a scope — once, idempotent on in.IdemKey — and returns its id.
	// It is called BEFORE a checkout, so the mapping from provider customer to scope exists before any payment can
	// arrive: a webhook learns whom it is about only from our own customer records, never from metadata.
	EnsureCustomer(ctx context.Context, acct domain.ProviderAccount, in CustomerInput) (providerCustomerID string, err error)

	// CreateCheckout starts a hosted checkout and returns a URL to hand off to. Mode "payment" and
	// "subscription" sell a plan; mode "setup" only saves a payment method (for auto top-up). It MUST NOT grant
	// anything: fulfilment arrives later on a verified webhook (invariant 2).
	CreateCheckout(ctx context.Context, acct domain.ProviderAccount, in CheckoutInput) (redirectURL string, err error)

	// CreatePortalSession returns a provider-hosted management portal (payment methods, invoices,
	// cancellation). The best card-data posture is never to touch it.
	CreatePortalSession(ctx context.Context, acct domain.ProviderAccount, customerID, returnURL string) (portalURL string, err error)

	// VerifyWebhook authenticates the RAW body with the account's secret and returns the normalized event.
	// CRITICAL. It MUST operate on the unmodified body, compare in CONSTANT TIME, enforce a timestamp
	// tolerance, and return an error — never a partial Event — on any failure: domain.ErrInvalidSignature when
	// the delivery is not authentic, domain.ErrVerificationUnavailable when verification itself cannot run. It
	// MAY perform I/O (PayPal verifies by API call), so callers give it a deadline.
	VerifyWebhook(ctx context.Context, acct domain.ProviderAccount, rawBody []byte, headers http.Header) (domain.Event, error)

	// FetchSubscription returns the provider's CURRENT subscription object and the provider's timestamp of it.
	// The processor applies this, not the event payload, so delivery order cannot matter (invariant 15); the
	// drift sweep uses it to repair missed webhooks.
	FetchSubscription(ctx context.Context, acct domain.ProviderAccount, providerSubID string) (domain.Sub, time.Time, error)

	// ChangePlan applies an upgrade/downgrade under the deployment's PlanChangePolicy.
	ChangePlan(ctx context.Context, acct domain.ProviderAccount, providerSubID, providerPriceID string, policy domain.PlanChangePolicy) error

	// CancelAtPeriodEnd sets the flag at the provider. Local status changes only when the resulting webhook is
	// processed (invariant 12).
	CancelAtPeriodEnd(ctx context.Context, acct domain.ProviderAccount, providerSubID string) error

	// ChargeSaved charges a saved payment method off-session — auto top-up. Idempotent on in.IdemKey at the
	// provider. It returns the provider payment id; credits still arrive only on the verified payment webhook
	// (invariant 18).
	ChargeSaved(ctx context.Context, acct domain.ProviderAccount, in OffSessionInput) (providerPaymentID string, err error)
}

// CheckoutInput is a hosted checkout to create. Everything in it was resolved by the deployment (the catalogue,
// the customer record); none of it is the caller's say-so.
type CheckoutInput struct {
	Scope           domain.Scope
	Mode            string // "payment" | "subscription" | "setup"
	PlanCode        string
	Currency        string
	ProviderPriceID string
	CustomerID      string // existing provider customer, or "" to create
	BillingEmail    string
	SuccessURL      string
	CancelURL       string
	IdemKey         string            // a double-clicked buy button creates one checkout
	Metadata        map[string]string // echoed back; correlation ONLY, never authority (invariant 4)
}

// CustomerInput is a provider customer to create for a scope.
type CustomerInput struct {
	Scope        domain.Scope
	BillingEmail string
	IdemKey      string // customer:<account>:<scope tag> — two first checkouts at once create one customer
}

// OffSessionInput is a charge of a saved payment method.
type OffSessionInput struct {
	CustomerID       string
	PaymentMethodRef string
	Amount           domain.Money
	PlanCode         string
	IdemKey          string // recharge:<realm>/<scope>:<day>:<n> — at most max_per_day distinct keys
}

// SecretResolver turns a reference (`env:STRIPE_SECRET_KEY`) into the secret it names, at the moment of use. A
// secret is never stored, logged or cached beyond a call (invariant 10).
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// Accounts serves provider accounts, which are configuration data.
type Accounts interface {
	// Account returns one account. found=false for an unknown id; the caller decides whether an inactive one
	// is also a 404.
	Account(ctx context.Context, id string) (domain.ProviderAccount, bool, error)
	// ForRealm lists the active accounts that serve a realm, by id.
	ForRealm(ctx context.Context, realm domain.Realm) ([]domain.ProviderAccount, error)
}

// Providers resolves a provider adapter by name.
type Providers map[domain.Provider]PaymentProvider
