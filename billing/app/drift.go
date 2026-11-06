package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// DefaultDriftPage is how many live subscriptions a sweep reads from the books at a time.
const DefaultDriftPage = 100

var _ ports.SubscriptionDrift = (*Processor)(nil)

// SweepSubscriptions is the backstop for a webhook that never arrived (R110a): for every live subscription of the account it asks the
// provider for the current object and applies it through applySubscription — the door the webhook processor uses, so status keeps its
// one writer (payments invariant 12) and a state the books already hold is not written, not announced and not called a repair. A
// subscription the provider cancelled is cancelled here, and the entitlement layer and the customer are told once.
//
// One subscription that fails does not stop the others: the sweep carries on and returns the failures together, by id, with the report of
// what it did. Nothing is held between pages: the books are read by keyset, so a subscription that ends mid-sweep is neither skipped nor
// read twice.
func (p *Processor) SweepSubscriptions(ctx context.Context, account string) (ports.DriftReport, error) {
	acct, err := p.driftAccount(ctx, account)
	if err != nil {
		return ports.DriftReport{}, err
	}
	rep, err := p.sweepAccount(ctx, acct)
	rep.Accounts = 1
	return rep, err
}

// SweepAllSubscriptions sweeps every active account that holds a live subscription. An account switched off since its subscriptions were
// written is left alone, as a sweep of it by name would be refused. An account that cannot be swept does not stop the next.
func (p *Processor) SweepAllSubscriptions(ctx context.Context) (ports.DriftReport, error) {
	ids, err := p.d.Subs.LiveAccounts(ctx)
	if err != nil {
		return ports.DriftReport{}, fmt.Errorf("list the accounts with a live subscription: %w", err)
	}
	var all ports.DriftReport
	var errs []error
	for _, id := range ids {
		acct, ok, err := p.d.Accounts.Account(ctx, id)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("read account %s: %w", id, err))
			continue
		case !ok || !acct.Active:
			continue
		}
		rep, err := p.sweepAccount(ctx, acct)
		if err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", id, err))
		}
		all.Accounts++
		all.Checked += rep.Checked
		all.Repaired += rep.Repaired
		all.Unchanged += rep.Unchanged
		all.Skipped += rep.Skipped
		all.Failed += rep.Failed
	}
	return all, errors.Join(errs...)
}

// driftAccount is the active account a sweep was asked for. One that does not exist, and one that is switched off, are the same refusal:
// a sweep of either would hold a provider call for credentials the operator has withdrawn or never gave.
func (p *Processor) driftAccount(ctx context.Context, id string) (domain.ProviderAccount, error) {
	acct, ok, err := p.d.Accounts.Account(ctx, id)
	switch {
	case err != nil:
		return domain.ProviderAccount{}, fmt.Errorf("read account %s: %w", id, err)
	case !ok:
		return domain.ProviderAccount{}, fmt.Errorf("%w: there is no provider account %q", domain.ErrUnknownAccount, id)
	case !acct.Active:
		return domain.ProviderAccount{}, fmt.Errorf("%w: provider account %q is switched off", domain.ErrUnknownAccount, id)
	}
	return acct, nil
}

func (p *Processor) sweepAccount(ctx context.Context, acct domain.ProviderAccount) (ports.DriftReport, error) {
	var rep ports.DriftReport
	var errs []error
	count := func(outcome string) {
		p.d.Metrics.Count(ports.MetricSubdrift, 1, mports.Labels{"outcome": outcome})
	}
	after := ""
	for {
		page, err := p.d.Subs.LiveOf(ctx, acct.ID, after, p.d.DriftPage)
		if err != nil {
			return rep, errors.Join(append(errs, fmt.Errorf("list the live subscriptions of %s: %w", acct.ID, err))...)
		}
		if len(page) > 0 && page[0].ProviderSubscriptionID <= after {
			// A store that does not move its cursor would be asked for the same page for ever, and the provider with it.
			return rep, errors.Join(append(errs, fmt.Errorf("the live subscriptions of %s were read from after %q and came back from %q: the cursor did not move",
				acct.ID, after, page[0].ProviderSubscriptionID))...)
		}
		for _, sub := range page {
			if err := ctx.Err(); err != nil {
				return rep, errors.Join(append(errs, err)...)
			}
			rep.Checked++
			res, err := p.applySubscription(ctx, acct, sub.Scope, domain.Event{Provider: acct.Provider}, sub.ProviderSubscriptionID)
			switch {
			case err != nil:
				rep.Failed++
				count("failed")
				errs = append(errs, fmt.Errorf("subscription %s: %w", sub.ProviderSubscriptionID, err))
			case res.skipped:
				rep.Skipped++
				count("skipped")
			case res.applied:
				rep.Repaired++
				count("repaired")
			default:
				rep.Unchanged++
				count("unchanged")
			}
		}
		if len(page) < p.d.DriftPage {
			return rep, errors.Join(errs...)
		}
		after = page[len(page)-1].ProviderSubscriptionID
	}
}
