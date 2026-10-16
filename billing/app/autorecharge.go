package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// DefaultRechargeStale is how long a claimed charge slot is held before another trigger may replace it: longer than a
// provider's webhook takes, short enough that a process that died holding one does not stop a customer being charged.
const DefaultRechargeStale = 15 * time.Minute

// The reasons an instruction's slot fails, recorded as why the instruction was switched off if it is.
const (
	ReasonChargeDeclined     = "charge_declined"
	ReasonPlanNotSold        = "plan_not_sold"
	ReasonNotAPack           = "plan_is_not_a_credit_pack"
	ReasonAccountUnavailable = "provider_account_unavailable"
	ReasonNoCustomer         = "no_customer_on_the_account"
	ReasonUnsupported        = "provider_cannot_charge_saved_methods"
)

// AutoRechargeDeps are the ports the handler needs.
type AutoRechargeDeps struct {
	Instructions ports.AutoRecharges
	Accounts     ports.Accounts
	Providers    ports.Providers
	Catalog      ports.Catalog
	Customers    ports.Customers
	Payments     ports.Payments
	// Balances is how the handler checks a trigger: the message is a hint, and the balance is the fact.
	Balances mports.BalanceReader
	Clock    mports.Clock
	Metrics  mports.Metrics
	// Bus is optional: with one, switching an instruction off announces billing.payment.<tag> with the reason.
	Bus mports.Bus
	Log *slog.Logger

	// SubjectPrefix is the namespace of the low-balance messages it listens to and the announcements it makes (default
	// DefaultSubjectPrefix; PAYMENT_SUBJECT_PREFIX): the writer publishes <prefix>.balance.low.<tag>.
	SubjectPrefix string

	// StaleAfter is how long a claimed slot is held before another trigger replaces it (default DefaultRechargeStale).
	StaleAfter time.Duration
}

// AutoRecharge is the handler of billing.balance.low.<tag>: when a scope's pool falls below its standing instruction's
// threshold it charges the saved payment method, off-session, for one pack — and does nothing else. Credits arrive when the
// verified payment is processed, like any purchase (payments invariant 18).
//
//	parse the hint → read the instruction → check the balance → claim a slot → charge under the slot's key
//
// The slot is what bounds it: at most max_per_day per UTC day however many times the trigger fires, and a duplicate resumes the
// slot in flight so the provider sees one key and charges once. A refusal by the provider counts toward switching the
// instruction off; any other failure leaves the outcome unknown, so the message is delivered again and the same key is tried.
type AutoRecharge struct{ d AutoRechargeDeps }

// NewAutoRecharge validates the dependencies and returns the handler.
func NewAutoRecharge(d AutoRechargeDeps) (*AutoRecharge, error) {
	var errs []error
	need := func(ok bool, what string) {
		if !ok {
			errs = append(errs, errors.New(what+" is required"))
		}
	}
	need(d.Instructions != nil, "the AutoRecharges store")
	need(d.Accounts != nil, "the Accounts store")
	need(d.Providers != nil, "the provider registry")
	need(d.Catalog != nil, "the Catalog")
	need(d.Customers != nil, "the Customers store")
	need(d.Payments != nil, "the Payments store")
	need(d.Balances != nil, "a BalanceReader")
	need(d.Clock != nil, "a Clock")
	need(d.Metrics != nil, "Metrics")
	need(d.Log != nil, "a logger")
	if d.StaleAfter < 0 {
		errs = append(errs, errors.New("the staleness window cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("billing: auto top-up: %w", err)
	}
	if d.StaleAfter == 0 {
		d.StaleAfter = DefaultRechargeStale
	}
	if d.SubjectPrefix == "" {
		d.SubjectPrefix = DefaultSubjectPrefix
	}
	return &AutoRecharge{d: d}, nil
}

func (a *AutoRecharge) count(result string) {
	a.d.Metrics.Count(ports.MetricAutoRecharge, 1, mports.Labels{"result": result})
}

// trigger is what a low-balance message says: which scope and which pool's watch was crossed.
type trigger struct {
	scope domain.Scope
	pool  domain.Pool
}

// parseTrigger reads the message the writer publishes: {"realm", "scope" (the tag), "watch" ("<pool>:<threshold>"), "seq"}.
// It trusts nothing it cannot cross-check: the subject, the realm and the tag must agree.
func parseTrigger(prefix, subject string, payload []byte) (trigger, error) {
	var m struct {
		Realm string `json:"realm"`
		Scope string `json:"scope"`
		Watch string `json:"watch"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return trigger{}, fmt.Errorf("%w: a low-balance message that is not JSON: %w", md.ErrInvalid, err)
	}
	scope, err := domain.ParseScopeTag(m.Scope)
	if err != nil {
		return trigger{}, err
	}
	if m.Realm != string(scope.Realm) {
		return trigger{}, fmt.Errorf("%w: a low-balance message names realm %q for a scope of realm %q", md.ErrInvalid, m.Realm, scope.Realm)
	}
	if want := prefix + ".balance.low." + scope.SubjectToken(); subject != want {
		return trigger{}, fmt.Errorf("%w: a low-balance message on %q about %s, which is published on %q", md.ErrInvalid, subject, scope.Tag(), want)
	}
	pool, threshold, ok := cutLast(m.Watch, ":")
	if n, err := strconv.ParseInt(threshold, 10, 64); !ok || pool == "" || err != nil || n < 1 {
		return trigger{}, fmt.Errorf("%w: a low-balance message with the watch %q, which is not <pool>:<threshold>", md.ErrInvalid, m.Watch)
	}
	return trigger{scope: scope, pool: domain.Pool(pool)}, nil
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// Handle implements the bus handler: nil acknowledges the message, an error has it delivered again.
func (a *AutoRecharge) Handle(ctx context.Context, subject string, payload []byte) error {
	t, err := parseTrigger(a.d.SubjectPrefix, subject, payload)
	if err != nil {
		return err
	}
	row, found, err := a.d.Instructions.Get(ctx, t.scope)
	if err != nil {
		a.count("error")
		return fmt.Errorf("read the auto top-up of %s: %w", t.scope.Tag(), err)
	}
	if !found || !row.Enabled || row.Pool.Or() != t.pool.Or() {
		a.count("ignored") // no instruction, a switched-off one, or the host's own watch on another pool
		return nil
	}
	// The message is a hint, and may be a duplicate delivered after a recharge already restored the balance: the balance is
	// the fact. Reading it before a slot is taken means a hint that is not acted on costs nothing.
	balances, err := a.d.Balances.Balance(ctx, t.scope)
	if err != nil {
		a.count("error")
		return fmt.Errorf("read the balance of %s: %w", t.scope.Tag(), err)
	}
	if int64(balances[row.Pool.Or()]) >= row.Threshold {
		a.count("ignored")
		return nil
	}
	claim, refusal, err := a.d.Instructions.Claim(ctx, t.scope, a.d.Clock.Now(), a.d.StaleAfter)
	if err != nil {
		a.count("error")
		return fmt.Errorf("claim a slot for %s: %w", t.scope.Tag(), err)
	}
	switch refusal {
	case ports.RechargeOK:
	case ports.RechargeDisabled:
		a.count("ignored")
		return nil
	default:
		a.count("refused")
		return nil
	}
	return a.charge(ctx, row, claim)
}

// charge buys the pack the instruction names under the slot's key.
func (a *AutoRecharge) charge(ctx context.Context, row domain.AutoRecharge, claim ports.RechargeClaim) error {
	scope := row.Scope
	key := domain.RechargeKey(scope, claim.Day, claim.N)

	acct, found, err := a.d.Accounts.Account(ctx, row.Account)
	if err != nil {
		a.count("error")
		return fmt.Errorf("read provider account %s: %w", row.Account, err)
	}
	if !found || !acct.Active || !slices.Contains(acct.Realms, scope.Realm.Or()) {
		return a.failSlot(ctx, row, claim, ReasonAccountUnavailable, domain.Money{})
	}
	provider, ok := a.d.Providers[acct.Provider]
	if !ok {
		a.count("error")
		return fmt.Errorf("%w: no adapter for provider %s", domain.ErrUnknownAccount, acct.Provider)
	}
	plan, _, err := a.d.Catalog.Resolve(ctx, scope.Realm.Or(), row.PlanCode, row.Currency, acct.ID)
	switch {
	case errors.Is(err, domain.ErrUnknownPrice):
		return a.failSlot(ctx, row, claim, ReasonPlanNotSold, domain.Money{})
	case err != nil:
		a.count("error")
		return fmt.Errorf("resolve plan %s: %w", row.PlanCode, err)
	}
	// The price is the catalogue's today (invariant 4): the instruction names a plan and never an amount.
	price, priced := plan.Prices[row.Currency]
	if !priced {
		return a.failSlot(ctx, row, claim, ReasonPlanNotSold, domain.Money{})
	}
	if plan.Kind != domain.OneTime || plan.CreditAllotment == nil {
		return a.failSlot(ctx, row, claim, ReasonNotAPack, price)
	}
	cust, found, err := a.d.Customers.Get(ctx, scope, acct.ID)
	if err != nil {
		a.count("error")
		return fmt.Errorf("read the customer of %s at %s: %w", scope.Tag(), acct.ID, err)
	}
	if !found {
		return a.failSlot(ctx, row, claim, ReasonNoCustomer, price)
	}

	paymentID, err := provider.ChargeSaved(ctx, acct, ports.OffSessionInput{CustomerID: cust.ProviderCustomerID, PaymentMethodRef: row.PaymentMethodRef,
		Amount: price, PlanCode: plan.Code, IdemKey: key})
	switch {
	case errors.Is(err, domain.ErrChargeDeclined):
		a.count("declined")
		return a.failSlot(ctx, row, claim, ReasonChargeDeclined, price)
	case errors.Is(err, domain.ErrUnsupported):
		return a.failSlot(ctx, row, claim, ReasonUnsupported, price)
	case err != nil:
		// The outcome is not known. The slot stays held, so the message delivered again resumes it, names the same key, and
		// the provider charges at most once.
		a.count("error")
		return fmt.Errorf("charge %s under %s: %w", scope.Tag(), key, err)
	}

	// The charge was accepted: record the payment it made, waiting for its webhook, keyed on the slot — which is how the
	// processor tells it is an auto top-up and which slot it settles. Nothing is granted here.
	now := a.d.Clock.Now()
	var settled *bool
	if _, err := a.d.Payments.Update(ctx, acct.ID, paymentID, func(cur domain.Payment, found bool) (domain.Payment, error) {
		if !found {
			return domain.Payment{Scope: scope, Account: acct.ID, ProviderPaymentID: paymentID, PlanCode: plan.Code, Kind: "auto_recharge", Pool: plan.Pool.Or(),
				Amount: price, Status: domain.Pending, IdemKey: key, CreatedAt: now}, nil
		}
		// The provider's webhook beat the handler: the processor already met the payment, granted under its own id, and
		// will not settle the slot. The outcome is known, so the slot is settled here.
		switch cur.Status {
		case domain.Succeeded:
			ok := true
			settled = &ok
		case domain.Failed:
			ok := false
			settled = &ok
		}
		return cur, nil
	}); err != nil {
		a.count("error")
		return fmt.Errorf("record the payment %s: %w", paymentID, err)
	}
	if settled != nil {
		reason := ""
		if !*settled {
			reason = "payment_failed"
		}
		a.settle(ctx, row, claim, *settled, reason, price)
	}
	if claim.Resumed {
		a.count("resumed")
	} else {
		a.count("charged")
	}
	return nil
}

// failSlot ends the slot with a failure the instruction's owner can fix. It is acknowledged, not retried: retrying a refused
// card or a plan nobody sells would only fail again under the same key.
func (a *AutoRecharge) failSlot(ctx context.Context, row domain.AutoRecharge, claim ports.RechargeClaim, reason string, price domain.Money) error {
	if reason != ReasonChargeDeclined {
		a.count("failed")
	}
	disabled, err := a.d.Instructions.Settle(ctx, row.Scope, claim, false, reason, a.d.Clock.Now())
	if err != nil {
		return fmt.Errorf("settle the slot of %s: %w", row.Scope.Tag(), err)
	}
	a.announceIfSwitchedOff(ctx, row, disabled, reason, price)
	return nil
}

// settle reports the outcome of a slot whose payment was already known, and announces if that switched the instruction off.
func (a *AutoRecharge) settle(ctx context.Context, row domain.AutoRecharge, claim ports.RechargeClaim, ok bool, reason string, price domain.Money) {
	disabled, err := a.d.Instructions.Settle(ctx, row.Scope, claim, ok, reason, a.d.Clock.Now())
	if err != nil {
		a.d.Log.Warn("auto top-up: the slot of a settled payment could not be settled; it is released when it goes stale", "scope", row.Scope.Tag(), "error", err.Error())
		return
	}
	a.announceIfSwitchedOff(ctx, row, disabled, reason, price)
}

// announceIfSwitchedOff tells the host, once, that the instruction was switched off and why, so it can ask the customer to
// update their card. Best effort: the instruction's own disabled_reason is the record, and a message that is not delivered must
// not cause a settled slot to be settled twice.
func (a *AutoRecharge) announceIfSwitchedOff(ctx context.Context, row domain.AutoRecharge, disabled bool, reason string, price domain.Money) {
	if !disabled {
		return
	}
	a.count("disabled")
	if a.d.Bus == nil {
		return
	}
	body, err := json.Marshal(paymentNote{Realm: string(row.Scope.Realm.Or()), Scope: scopeRef{row.Scope.Kind, row.Scope.ID}, Event: "failed",
		Amount: price.MinorUnits, Currency: price.Currency, Reason: reason})
	if err == nil {
		err = a.d.Bus.Publish(ctx, a.d.SubjectPrefix+".payment."+row.Scope.SubjectToken(), body)
	}
	if err != nil {
		a.d.Log.Warn("auto top-up: the switching-off was not announced", "scope", row.Scope.Tag(), "error", err.Error())
	}
}
