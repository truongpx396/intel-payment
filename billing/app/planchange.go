package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// SubscriptionsDeps are the ports plan changes need. Subs is read, never written: this service has no way to set a
// subscription's status, and that is the point (invariant 12).
type SubscriptionsDeps struct {
	Providers ports.Providers
	Accounts  ports.Accounts
	Catalog   ports.Catalog
	Subs      ports.Subs
	Payments  ports.Payments
	// Policy is when a change takes effect (D11): at the period's end, now with proration, or now without.
	Policy domain.PlanChangePolicy
	// Guard says whether a change that takes effect now would take a quota below what the scope uses. The
	// entitlement layer implements it; with none wired, nothing is below usage.
	Guard ports.PlanChangeGuard
	Log   *slog.Logger
}

// PlanChanges moves a subscription to another plan, or ends it at the period's end, by asking the provider and
// waiting for the webhook. Proration is the provider's math — it already owns the invoice.
type PlanChanges struct{ d SubscriptionsDeps }

var _ ports.Subscriptions = (*PlanChanges)(nil)

// NewSubscriptions validates its dependencies and returns the service.
func NewSubscriptions(d SubscriptionsDeps) (*PlanChanges, error) {
	var errs []error
	if len(d.Providers) == 0 || d.Accounts == nil || d.Catalog == nil || d.Subs == nil || d.Payments == nil {
		errs = append(errs, errors.New("the Providers, Accounts, Catalog, Subs and Payments are required"))
	}
	if !d.Policy.Valid() {
		errs = append(errs, fmt.Errorf("the plan change policy %q is not at_period_end, immediate_prorate or immediate_no_prorate", d.Policy))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("billing subscriptions: %w", err)
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &PlanChanges{d: d}, nil
}

// live finds the scope's subscription and the account and provider it lives at.
func (s *PlanChanges) live(ctx context.Context, scope domain.Scope) (domain.Sub, domain.ProviderAccount, ports.PaymentProvider, error) {
	if err := scope.Validate(); err != nil {
		return domain.Sub{}, domain.ProviderAccount{}, nil, err
	}
	sub, found, err := s.d.Subs.Get(ctx, scope.Normalized())
	if err != nil {
		return domain.Sub{}, domain.ProviderAccount{}, nil, fmt.Errorf("read the subscription of %s: %w", scope.Tag(), err)
	}
	if !found || !sub.Live() {
		return domain.Sub{}, domain.ProviderAccount{}, nil, fmt.Errorf("%w: %s has no live subscription", md.ErrNotFound, scope.Tag())
	}
	acct, ok, err := s.d.Accounts.Account(ctx, sub.Account)
	if err != nil {
		return domain.Sub{}, domain.ProviderAccount{}, nil, fmt.Errorf("provider account %s: %w", sub.Account, err)
	}
	provider, have := s.d.Providers[acct.Provider]
	if !ok || !acct.Active || !have {
		return domain.Sub{}, domain.ProviderAccount{}, nil, fmt.Errorf("%w: the account %s of this subscription is not active", domain.ErrUnknownAccount, sub.Account)
	}
	return sub, acct, provider, nil
}

// currency is the currency a subscription is billed in: the one of its latest invoice.
func (s *PlanChanges) currency(ctx context.Context, scope domain.Scope, account string) (string, error) {
	latest, _, err := s.d.Payments.List(ctx, scope.Normalized(), "", 20)
	if err != nil {
		return "", fmt.Errorf("read the payments of %s: %w", scope.Tag(), err)
	}
	for _, p := range latest {
		if p.Kind == "subscription_invoice" && p.Account == account && p.Amount.Currency != "" {
			return p.Amount.Currency, nil
		}
	}
	return "", fmt.Errorf("%w: %s has no subscription invoice yet, so the currency of a plan change is not known", md.ErrInvalid, scope.Tag())
}

// Change implements ports.Subscriptions.
func (s *PlanChanges) Change(ctx context.Context, scope domain.Scope, planCode string) error {
	sub, acct, provider, err := s.live(ctx, scope)
	if err != nil {
		return err
	}
	if planCode == sub.PlanCode {
		return fmt.Errorf("%w: %s is already on plan %s", md.ErrInvalid, scope.Tag(), planCode)
	}
	currency, err := s.currency(ctx, scope, acct.ID)
	if err != nil {
		return err
	}
	realm := scope.Realm.Or()
	to, priceID, err := s.d.Catalog.Resolve(ctx, realm, planCode, currency, acct.ID)
	if err != nil {
		return fmt.Errorf("catalogue: %s in %s: %w", planCode, currency, err)
	}
	if to.Code != planCode || !to.Active || to.Realm.Or() != realm || priceID == "" {
		return fmt.Errorf("%w: %s in %s is not sold", domain.ErrUnknownPrice, planCode, currency)
	}
	if to.Kind != domain.Subscription {
		return fmt.Errorf("%w: plan %s is a one-time purchase and cannot be moved to", md.ErrInvalid, planCode)
	}

	// A change that takes effect now must not take a quota below what the scope uses. One deferred to the period's
	// end is allowed whatever the usage: metering's ceilings answer for the new plan when it begins.
	if s.d.Policy != domain.AtPeriodEnd && s.d.Guard != nil {
		from, _, err := s.d.Catalog.Resolve(ctx, realm, sub.PlanCode, currency, acct.ID)
		if err != nil && !errors.Is(err, domain.ErrUnknownPrice) {
			return fmt.Errorf("catalogue: %s in %s: %w", sub.PlanCode, currency, err)
		}
		if from.Code == "" {
			from = domain.Plan{Realm: realm, Code: sub.PlanCode} // a plan since retired is still the plan it was
		}
		if err := s.d.Guard.CheckChange(ctx, scope.Normalized(), from, to); err != nil {
			return fmt.Errorf("plan change %s → %s: %w", from.Code, to.Code, err)
		}
	}
	if err := provider.ChangePlan(ctx, acct, sub.ProviderSubscriptionID, priceID, s.d.Policy); err != nil {
		return fmt.Errorf("change plan at %s: %w", acct.ID, err)
	}
	return nil
}

// CancelAtPeriodEnd implements ports.Subscriptions. The local status changes when the resulting webhook is
// processed, not before.
func (s *PlanChanges) CancelAtPeriodEnd(ctx context.Context, scope domain.Scope) error {
	sub, acct, provider, err := s.live(ctx, scope)
	if err != nil {
		return err
	}
	if err := provider.CancelAtPeriodEnd(ctx, acct, sub.ProviderSubscriptionID); err != nil {
		return fmt.Errorf("cancel at period end at %s: %w", acct.ID, err)
	}
	return nil
}
