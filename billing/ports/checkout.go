package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
)

// CheckoutRequest asks to sell a plan to a scope. It names WHAT is wanted — a plan code and a currency — and nothing
// about what it costs: the amount, the currency's price and the credit quantity come from the catalogue
// (invariant 4).
type CheckoutRequest struct {
	Scope    domain.Scope
	PlanCode string // required for "payment" and "subscription"; "setup" saves a method and sells nothing
	Currency string
	// Mode is "payment", "subscription" or "setup". Empty is whatever the plan is: a pack is "payment", a
	// recurring plan "subscription".
	Mode string
	// Account names the provider account to sell through. Empty is the one of the realm that sells the plan in the
	// currency.
	Account      string
	SuccessURL   string
	CancelURL    string
	BillingEmail string
	IdemKey      string // a double-clicked buy button creates one checkout
}

// FulfilmentKind is what the server says about the credits a purchase should bring.
type FulfilmentKind string

// The fulfilment states. `granted` is a state the SERVER reports from a processed webhook — never one a client
// infers from having returned from a checkout (invariant 2).
const (
	FulfilmentNone       FulfilmentKind = "none"
	FulfilmentProcessing FulfilmentKind = "processing"
	FulfilmentGranted    FulfilmentKind = "granted"
	FulfilmentFailed     FulfilmentKind = "failed"
)

// Fulfilment is the server's report on a scope's most recent payment.
type Fulfilment struct {
	Kind       FulfilmentKind
	Since      time.Time      // processing: when the payment began
	Amount     domain.Credits // granted: what arrived
	ReceiptURL string
	Reason     string // failed: the provider's reason
}

// Checkout is the driving port behind /v1/checkout, /v1/portal and /v1/fulfilment. It grants nothing: it has no
// Meter, and the credits arrive when the verified payment is processed.
type Checkout interface {
	// Start creates a hosted checkout and returns the URL to hand off to.
	Start(ctx context.Context, req CheckoutRequest) (redirectURL string, err error)
	// Portal returns the provider's hosted management page for a scope that has bought before.
	Portal(ctx context.Context, scope domain.Scope, account, returnURL string) (portalURL string, err error)
	// Fulfilment reports on the scope's latest payment. Reading it is the only sanctioned way for a UI to learn that
	// credits arrived.
	Fulfilment(ctx context.Context, scope domain.Scope) (Fulfilment, error)
}

// PlanChangeGuard says whether a plan change would take an entitlement below what the scope uses now. The entitlement
// layer implements it; billing only asks.
type PlanChangeGuard interface {
	// CheckChange returns domain.ErrBelowUsage (wrapped, with what is over) when the change cannot be made now.
	CheckChange(ctx context.Context, scope domain.Scope, from, to domain.Plan) error
}

// Subscriptions is the driving port behind /v1/subscription/change and /v1/subscription/cancel. Neither writes
// subscription status: they call the provider, and status changes when the resulting webhook is processed
// (invariant 12).
type Subscriptions interface {
	// Change moves the scope's subscription to another plan under the deployment's PlanChangePolicy.
	Change(ctx context.Context, scope domain.Scope, planCode string) error
	// CancelAtPeriodEnd asks the provider to end the subscription when the paid period does.
	CancelAtPeriodEnd(ctx context.Context, scope domain.Scope) error
}
