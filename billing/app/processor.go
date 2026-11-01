package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Defaults of the processor.
const (
	DefaultBatch    = 50
	DefaultLeaseTTL = 2 * time.Minute
	DefaultGrace    = 7 * 24 * time.Hour
)

// The outcomes an inbox row ends in.
const (
	outcomeProcessed = "processed"
	outcomeIgnored   = "ignored"
)

// ProcessorDeps are the ports stage 2 needs.
type ProcessorDeps struct {
	Inbox     ports.Inbox
	Accounts  ports.Accounts
	Providers ports.Providers
	Customers ports.Customers
	Catalog   ports.Catalog
	Payments  ports.Payments
	Subs      ports.Subs
	// Recharges is optional: with it, the outcome of an auto top-up's payment settles the charge slot it was made in.
	Recharges ports.AutoRecharges
	// Meter is the ONLY way credits are created (invariant 3); Lots says how much of a payment's credits expiry
	// has already taken, so a refund never takes them twice (invariant 20).
	Meter   mports.Meter
	Lots    mports.LotReader
	Clock   mports.Clock
	Metrics mports.Metrics
	// Bus is optional: with one, a processed event announces billing.payment.<tag> and billing.entitlement.<tag>.
	Bus mports.Bus
	Log *slog.Logger

	Batch    int           // events leased per tick (default DefaultBatch)
	LeaseTTL time.Duration // how long a lease holds (default DefaultLeaseTTL)
	// Backoff is how long a failed event waits after its nth failed attempt (default: 15 s doubling, 30 min cap).
	Backoff func(attempts int) time.Duration
	// Grace is how long entitlements hold after a failed payment (default DefaultGrace; PAYMENT_DUNNING_GRACE_DAYS).
	Grace time.Duration
	// SubjectPrefix is the namespace the announcements go out under (default DefaultSubjectPrefix; PAYMENT_SUBJECT_PREFIX).
	SubjectPrefix string
}

// Processor is stage 2 of the webhook flow, run by payment-worker: it fulfils verified events from the inbox.
//
//	Inbox.Lease → resolve the scope from the VERIFIED customer → dispatch → mark processed;
//	a failure → MarkFailed with a backoff → dead past the attempt cap, and a page.
//
// Every step is idempotent — Meter.Grant is keyed on a provider id, Payments.Update is serialized per payment,
// Subs.ApplyIfNewer is ordered by the provider's clock — which is what makes lease-and-retry safe: a processor
// that crashes mid-event leaves a lease that expires, and the next one re-runs the event with every completed
// step a no-op (payments contract, § Stage 2).
type Processor struct {
	d ProcessorDeps
}

var _ ports.WebhookProcessor = (*Processor)(nil)

// NewProcessor validates its dependencies and returns the processor.
func NewProcessor(d ProcessorDeps) (*Processor, error) {
	var errs []error
	if d.Inbox == nil || d.Accounts == nil || d.Customers == nil || d.Catalog == nil || d.Payments == nil || d.Subs == nil {
		errs = append(errs, errors.New("the Inbox, Accounts, Customers, Catalog, Payments and Subs ports are required"))
	}
	if len(d.Providers) == 0 {
		errs = append(errs, errors.New("the Providers are required: subscription state is the provider's current object, never the event"))
	}
	if d.Meter == nil {
		errs = append(errs, errors.New("a Meter is required: credits are created only through it"))
	}
	if d.Lots == nil {
		errs = append(errs, errors.New("a LotReader is required: a refund must know what expiry already took"))
	}
	if d.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: the core reads no wall clock"))
	}
	if d.Batch < 0 || d.LeaseTTL < 0 || d.Grace < 0 {
		errs = append(errs, errors.New("the Batch, LeaseTTL and Grace cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("billing processor: %w", err)
	}
	if d.Metrics == nil {
		d.Metrics = noMetrics{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Batch == 0 {
		d.Batch = DefaultBatch
	}
	if d.LeaseTTL == 0 {
		d.LeaseTTL = DefaultLeaseTTL
	}
	if d.Grace == 0 {
		d.Grace = DefaultGrace
	}
	if d.SubjectPrefix == "" {
		d.SubjectPrefix = DefaultSubjectPrefix
	}
	if d.Backoff == nil {
		d.Backoff = defaultRetryBackoff
	}
	return &Processor{d: d}, nil
}

func defaultRetryBackoff(attempts int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < attempts && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// Tick leases the events that are due and fulfils them. It reports only what it could not handle itself: an
// event that fails is recorded on its row (and retried, then dead and paged), not returned.
func (p *Processor) Tick(ctx context.Context) error {
	leased, err := p.d.Inbox.Lease(ctx, p.d.Batch, p.d.LeaseTTL)
	if err != nil {
		return fmt.Errorf("lease webhook events: %w", err)
	}
	var errs []error
	for _, le := range leased {
		if ctx.Err() != nil {
			break // the leases expire; whoever ticks next finishes the rest
		}
		if err := p.process(ctx, le); err != nil {
			errs = append(errs, fmt.Errorf("event %s: %w", le.Event.ProviderEventID, err))
		}
	}
	if err := p.sweepGrace(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := p.publishInboxAge(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *Processor) publishInboxAge(ctx context.Context) error {
	at, ok, err := p.d.Inbox.Oldest(ctx)
	if err != nil {
		return fmt.Errorf("read the inbox age: %w", err)
	}
	var age int64
	if ok {
		age = max(int64(p.d.Clock.Now().Sub(at)/time.Second), 0)
	}
	p.d.Metrics.Gauge(ports.MetricWebhookInboxAge, age, nil)
	return nil
}

func (p *Processor) process(ctx context.Context, le ports.LeasedEvent) error {
	label := string(le.Event.Provider)
	outcome, err := p.handle(ctx, le)
	if err != nil {
		return p.fail(ctx, le, err)
	}
	if err := p.d.Inbox.MarkProcessed(ctx, le.ID, outcome); err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	p.d.Metrics.Count(ports.MetricWebhookProcessed, 1, mports.Labels{"provider": label, "outcome": outcome})
	return nil
}

// fail records a failed attempt. Past the attempt cap the event is dead, and that pages: it is a payment that
// will not be fulfilled until a human looks.
func (p *Processor) fail(ctx context.Context, le ports.LeasedEvent, cause error) error {
	label := string(le.Event.Provider)
	p.d.Metrics.Count(ports.MetricWebhookFailed, 1, mports.Labels{"provider": label})
	dead, err := p.d.Inbox.MarkFailed(ctx, le.ID, cause, p.d.Clock.Now().Add(p.d.Backoff(le.Attempts)))
	if err != nil {
		return fmt.Errorf("mark failed (%w): %w", cause, err)
	}
	if dead {
		p.d.Metrics.Count(ports.MetricWebhookDead, 1, mports.Labels{"provider": label})
		p.d.Log.Error("webhook event is dead: a verified payment event could not be fulfilled (page)",
			"provider", label, "account", le.Event.Account, "event", le.Event.ProviderEventID, "type", string(le.Event.Type), "error", cause.Error())
	} else {
		p.d.Log.Warn("webhook event failed; it will be retried", "provider", label, "event", le.Event.ProviderEventID,
			"attempt", le.Attempts, "error", cause.Error())
	}
	return nil
}

func (p *Processor) ignored(e domain.Event, reason string) string {
	p.d.Metrics.Count(ports.MetricWebhookIgnored, 1, mports.Labels{"provider": string(e.Provider), "reason": reason})
	return outcomeIgnored
}

// handle dispatches one event. It returns the outcome to record, or an error to retry.
func (p *Processor) handle(ctx context.Context, le ports.LeasedEvent) (string, error) {
	e := le.Event
	if e.Type == domain.Ignored {
		return p.ignored(e, "unsubscribed_type"), nil
	}
	acct, found, err := p.d.Accounts.Account(ctx, e.Account)
	if err != nil {
		return "", fmt.Errorf("provider account %s: %w", e.Account, err)
	}
	if !found || !acct.Active {
		// The event was verified when the account was live. It waits: an operator may switch the account back on.
		return "", fmt.Errorf("%w: provider account %s is not active", domain.ErrUnknownAccount, e.Account)
	}
	scope, ok, err := p.scopeOf(ctx, acct, e)
	if err != nil {
		return "", err
	}
	if !ok {
		return p.ignored(e, "unknown_customer"), nil
	}
	if !acct.Serves(scope.Realm) {
		// A verified event whose customer resolves to a realm the account does not serve: a mapping error or an
		// attack, and either way a security event (invariant 19).
		p.d.Metrics.Count(ports.MetricWebhookForeignRealm, 1, mports.Labels{"provider": string(e.Provider)})
		p.d.Log.Warn("webhook event resolves to a realm its account does not serve (security event)",
			"provider", string(e.Provider), "account", acct.ID, "event", e.ProviderEventID, "realm", string(scope.Realm.Or()))
		return outcomeIgnored, nil
	}

	switch e.Type {
	case domain.PaymentSucceeded:
		return p.paymentSucceeded(ctx, acct, scope, le)
	case domain.PaymentFailed:
		return p.paymentFailed(ctx, acct, scope, le)
	case domain.SubscriptionUpdated, domain.SubscriptionDeleted:
		return p.subscriptionChanged(ctx, acct, scope, le)
	case domain.PaymentRefunded:
		return p.paymentRefunded(ctx, acct, scope, le)
	case domain.DisputeOpened, domain.DisputeFundsWithdrawn, domain.DisputeFundsReinstated, domain.DisputeClosed:
		return p.dispute(ctx, acct, scope, le)
	default:
		return "", fmt.Errorf("%w: event type %s is not handled yet", domain.ErrNotImplemented, e.Type)
	}
}

// scopeOf resolves whom a verified event is about: the scope of the customer the PROVIDER named, through this
// deployment's own customer records — the only sanctioned way (invariant 4). An unknown customer is not an error:
// a provider account can carry customers this deployment never created (another environment, a manual test).
func (p *Processor) scopeOf(ctx context.Context, acct domain.ProviderAccount, e domain.Event) (domain.Scope, bool, error) {
	if c := e.Object.ProviderCustomerID; c != "" {
		s, ok, err := p.d.Customers.ResolveScope(ctx, acct.ID, c)
		if err != nil {
			return domain.Scope{}, false, fmt.Errorf("resolve customer %s: %w", c, err)
		}
		return s, ok, nil
	}
	// Some events (a dispute) name the payment and not the customer: the payment is ours, and carries its scope.
	if id := e.Object.ProviderPaymentID; id != "" {
		pay, found, err := p.d.Payments.Get(ctx, acct.ID, id)
		if err != nil {
			return domain.Scope{}, false, fmt.Errorf("read payment %s: %w", id, err)
		}
		if !found {
			return domain.Scope{}, false, fmt.Errorf("%w: payment %s is not recorded yet", md.ErrNotFound, id)
		}
		return pay.Scope, true, nil
	}
	return domain.Scope{}, false, nil
}

// paymentSucceeded records the payment and grants what it bought, once.
func (p *Processor) paymentSucceeded(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, le ports.LeasedEvent) (string, error) {
	e := le.Event
	obj := e.Object
	if obj.ProviderPaymentID == "" || obj.Amount == nil {
		return "", fmt.Errorf("%w: payment_succeeded %s carries no payment id or amount", md.ErrInvalid, e.ProviderEventID)
	}
	if err := obj.Amount.Validate(); err != nil {
		return "", err
	}
	plan, ok, err := p.planOf(ctx, acct, scope, obj)
	if err != nil {
		return "", err
	}
	if !ok {
		// Never a default plan (FR-021): a customer paid for something this deployment does not sell.
		p.d.Log.Warn("a verified payment is for a price the catalogue does not sell", "provider", string(e.Provider),
			"account", acct.ID, "payment", obj.ProviderPaymentID, "price", obj.ProviderPriceID)
		return p.ignored(e, "unknown_price"), nil
	}
	if plan.Realm.Or() != scope.Realm.Or() {
		return "", fmt.Errorf("%w: price %s is plan %s of realm %s and customer %s is in realm %s", md.ErrInvalid,
			obj.ProviderPriceID, plan.Code, plan.Realm.Or(), obj.ProviderCustomerID, scope.Realm.Or())
	}

	var announce *domain.Payment
	_, err = p.d.Payments.Update(ctx, acct.ID, obj.ProviderPaymentID, func(cur domain.Payment, found bool) (domain.Payment, error) {
		pay := cur
		if !found || pay.Status == "" || pay.Status == domain.Pending || pay.Status == domain.Failed {
			// A payment that has not succeeded yet takes the plan's terms now — including one that failed on an
			// earlier attempt and is paid at last under the same payment id.
			if !found {
				pay = domain.Payment{Scope: scope, Account: acct.ID, ProviderPaymentID: obj.ProviderPaymentID, IdemKey: obj.ProviderPaymentID, CreatedAt: e.CreatedAt}
			}
			pay.PlanCode, pay.Pool, pay.CreditsGranted = plan.Code, plan.Pool.Or(), 0
			if pay.Kind != kindAutoRecharge { // an auto top-up was made as one by the handler, which keyed it on its slot
				pay.Kind = paymentKind(plan)
			}
			if plan.CreditAllotment != nil {
				pay.CreditsGranted = *plan.CreditAllotment
			}
			pay.Amount, pay.TaxAmount, pay.ReceiptURL = *obj.Amount, obj.TaxAmount, obj.ReceiptURL
			pay.Status, pay.FailureReason = domain.Succeeded, ""
			announce = &pay
		}
		// The grant is keyed on the PAYMENT (or invoice) id, never on the event: invoice.paid and
		// checkout.session.completed can both arrive for one purchase. A replay is a no-op, so running it again
		// after a crash — or for a payment already recorded — is how a lost grant heals.
		if pay.CreditsGranted > 0 {
			if err := p.mint(ctx, md.Grant{Scope: pay.Scope, Pool: pay.Pool, Amount: pay.CreditsGranted, IdemKey: pay.IdemKey,
				Reason: grantReason(pay.Kind), Ref: refOf(acct, pay, e)}); err != nil {
				return cur, fmt.Errorf("grant credits for payment %s: %w", pay.ProviderPaymentID, err)
			}
		}
		return pay, nil
	})
	if err != nil {
		return "", err
	}
	if announce != nil {
		event := "succeeded"
		if announce.Kind == "subscription_invoice" {
			event = "renewed"
		}
		if err := p.announcePayment(ctx, announce.Scope, p.noteOf(*announce, event, announce.Amount.MinorUnits, nil)); err != nil {
			return "", err
		}
		// A confirmed good charge clears the failures of the slot it was made in and releases it (invariant 18).
		p.settleRecharge(ctx, *announce, true, "")
	}
	return outcomeProcessed, nil
}

// planOf finds what a verified payment bought. A purchase names the provider's price, and the catalogue maps it back to a
// plan. A charge this service made off-session for an auto top-up does not: the provider's event about it (a Stripe
// payment_intent) carries an amount and a method and no price, because a payment intent has none. What it bought was fixed when
// the handler charged it and recorded then, on the payment the handler left waiting for this event — so the plan is the one that
// payment names, and never one the event's metadata or amount points at. Such a charge whose plan is no longer sold is an error,
// not an ignore: the customer has been charged, and the event is retried and, in the end, pages a person rather than taking the
// money for nothing.
func (p *Processor) planOf(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, obj domain.EventObject) (domain.Plan, bool, error) {
	if obj.ProviderPriceID != "" {
		plan, ok, err := p.d.Catalog.PlanForPrice(ctx, acct.ID, obj.ProviderPriceID)
		if err != nil {
			return domain.Plan{}, false, fmt.Errorf("catalogue: price %s: %w", obj.ProviderPriceID, err)
		}
		return plan, ok, nil
	}
	pending, found, err := p.d.Payments.Get(ctx, acct.ID, obj.ProviderPaymentID)
	if err != nil {
		return domain.Plan{}, false, fmt.Errorf("read payment %s: %w", obj.ProviderPaymentID, err)
	}
	if !found || pending.Kind != kindAutoRecharge || pending.PlanCode == "" {
		return domain.Plan{}, false, nil
	}
	plan, _, err := p.d.Catalog.Resolve(ctx, scope.Realm.Or(), pending.PlanCode, pending.Amount.Currency, acct.ID)
	if err != nil {
		return domain.Plan{}, false, fmt.Errorf("auto top-up payment %s was charged for plan %s, which the catalogue no longer sells: %w", obj.ProviderPaymentID, pending.PlanCode, err)
	}
	return plan, true, nil
}

// kindAutoRecharge is the kind of a payment made by an auto top-up's charge.
const kindAutoRecharge = "auto_recharge"

// settleRecharge reports the outcome of an auto top-up's payment to the slot it was made in. The payment's idempotency key IS
// the slot's key, so it names the slot; the settlement releases that slot only. Only the first outcome of a payment reaches
// here, so a replayed event cannot count one failure three times: that is deliberately at-most-once, because an undercount
// only delays switching an instruction off while an overcount would switch a good card off. For the same reason a settlement
// that cannot be made is logged and does not fail the event — the payment is already recorded, a retry would not settle again,
// and the slot is replaced when it goes stale. A payment that is not an auto top-up, or a processor with no instructions,
// settles nothing. It reports whether this settling switched the instruction off.
func (p *Processor) settleRecharge(ctx context.Context, pay domain.Payment, ok bool, reason string) (disabled bool) {
	if p.d.Recharges == nil || pay.Kind != kindAutoRecharge {
		return false
	}
	tag, day, n, parsed := domain.ParseRechargeKey(pay.IdemKey)
	if !parsed || tag != pay.Scope.Tag() {
		p.d.Log.Warn("an auto top-up payment is not keyed on a charge slot; nothing was settled", "payment", pay.ProviderPaymentID, "key", pay.IdemKey)
		return false
	}
	disabled, err := p.d.Recharges.Settle(ctx, pay.Scope, ports.RechargeClaim{Day: day, N: n}, ok, reason, p.d.Clock.Now())
	switch {
	case errors.Is(err, md.ErrNotFound):
		return false // the instruction was deleted since: there is nothing left to settle
	case err != nil:
		p.d.Log.Warn("the charge slot of an auto top-up payment could not be settled; it is replaced when it goes stale",
			"payment", pay.ProviderPaymentID, "scope", pay.Scope.Tag(), "error", err.Error())
		return false
	}
	return disabled
}

func paymentKind(plan domain.Plan) string {
	if plan.Kind == domain.Subscription {
		return "subscription_invoice"
	}
	return "one_time"
}

// grantReason is the reason a payment's credits are granted for.
func grantReason(kind string) string {
	switch kind {
	case "subscription_invoice":
		return "subscription_grant"
	case "auto_recharge":
		return "auto_recharge"
	}
	return "purchase"
}

// refOf is what the books remember about where a grant came from: audit data, never authority.
func refOf(acct domain.ProviderAccount, pay domain.Payment, e domain.Event) map[string]string {
	return map[string]string{"payment": pay.ProviderPaymentID, "account": acct.ID, "plan": pay.PlanCode, "event": e.ProviderEventID}
}

// mint is the one place billing creates or removes credits, and it does so only through Meter.Grant (invariant 3).
func (p *Processor) mint(ctx context.Context, g md.Grant) error {
	receipt, err := p.d.Meter.Grant(ctx, g)
	if err != nil {
		return err
	}
	if receipt.Applied {
		p.d.Metrics.Count(ports.MetricGrantApplied, 1, mports.Labels{"realm": string(g.Scope.Realm.Or()), "reason": g.Reason})
	}
	return nil
}

// expired is how much of a payment's credits expiry has already taken (Σ lot.Expired under lots_fefo; nothing
// with expiry off). A refund or a chargeback is capped by it, so the customer is not charged for the same credits
// twice (invariant 20). An outage of the reader is an error, never a zero: taking credits back blind is worse
// than taking them back late.
func (p *Processor) expired(ctx context.Context, pay domain.Payment) (md.Credits, error) {
	lots, err := p.d.Lots.LotsOf(ctx, pay.Scope, pay.Pool, pay.IdemKey)
	if err != nil {
		return 0, fmt.Errorf("read the lots of payment %s: %w", pay.ProviderPaymentID, err)
	}
	var total md.Credits
	for _, l := range lots {
		if total, err = total.Add(l.Expired); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// refundKey names a refund's clawback: the provider's refund id, so a redelivery is the same key. A provider
// that names none gets the payment and the cumulative figure, which is the same refund report every time.
func refundKey(obj domain.EventObject) string {
	if obj.ProviderRefundID != "" {
		return "refund:" + obj.ProviderRefundID
	}
	return fmt.Sprintf("refund:%s@%d", obj.ProviderPaymentID, obj.AmountRefunded.MinorUnits)
}

// ownPayment refuses an event whose customer is not the payment's: the payment is the authority on whose it is.
func ownPayment(cur domain.Payment, found bool, id string, scope domain.Scope) error {
	if !found {
		return fmt.Errorf("%w: payment %s is not recorded yet", md.ErrNotFound, id)
	}
	if cur.Scope.Normalized() != scope.Normalized() {
		return fmt.Errorf("%w: payment %s belongs to %s, not %s", md.ErrInvalid, id, cur.Scope.Tag(), scope.Tag())
	}
	return nil
}

// paymentRefunded applies a CUMULATIVE refund report. The clawback is the difference between what the formula
// says should be revoked by now and what has been — so several partial refunds revoke what one refund of the
// total would, a replay revokes nothing, and a late, lower report is a no-op. It runs under the payment's lock:
// two reports for one payment must not both read the same cumulative figures.
func (p *Processor) paymentRefunded(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, le ports.LeasedEvent) (string, error) {
	e := le.Event
	obj := e.Object
	if obj.ProviderPaymentID == "" || obj.AmountRefunded == nil {
		return "", fmt.Errorf("%w: payment_refunded %s carries no payment id or refunded amount", md.ErrInvalid, e.ProviderEventID)
	}
	var announce *domain.Payment
	_, err := p.d.Payments.Update(ctx, acct.ID, obj.ProviderPaymentID, func(cur domain.Payment, found bool) (domain.Payment, error) {
		if err := ownPayment(cur, found, obj.ProviderPaymentID, scope); err != nil {
			return cur, err
		}
		expired, err := p.expired(ctx, cur)
		if err != nil {
			return cur, err
		}
		plan, err := cur.ApplyRefund(*obj.AmountRefunded, expired)
		if err != nil {
			return cur, err
		}
		if plan.Revoke > 0 {
			// Appends, never mutates (invariant 8): a negative grant beside the purchase, naming the payment whose
			// lot the books empty first.
			if err := p.mint(ctx, md.Grant{Scope: cur.Scope, Pool: cur.Pool, Amount: -plan.Revoke, SourceKey: cur.IdemKey,
				IdemKey: refundKey(obj), Reason: "refund", Ref: refOf(acct, cur, e)}); err != nil {
				return cur, fmt.Errorf("revoke credits for refund %s: %w", refundKey(obj), err)
			}
		}
		next := cur
		next.RefundedMinor, next.CreditsRevoked, next.Status = plan.RefundedMinor, plan.RevokedTotal, plan.Status
		if next != cur {
			announce = &next
		}
		return next, nil
	})
	if err != nil {
		return "", err
	}
	if announce != nil {
		if err := p.announcePayment(ctx, announce.Scope, p.noteOf(*announce, "refunded", announce.RefundedMinor, nil)); err != nil {
			return "", err
		}
	}
	return outcomeProcessed, nil
}

// dispute applies a phase of a dispute. Each phase that moves credits is a grant keyed on the dispute id and the
// phase, so a replay is one effect; the phases converge in either order (invariant 17).
func (p *Processor) dispute(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, le ports.LeasedEvent) (string, error) {
	e := le.Event
	obj := e.Object
	if obj.ProviderPaymentID == "" || obj.DisputeID == "" {
		return "", fmt.Errorf("%w: %s %s carries no payment or dispute id", md.ErrInvalid, e.Type, e.ProviderEventID)
	}
	var announce *domain.Payment
	var note string
	_, err := p.d.Payments.Update(ctx, acct.ID, obj.ProviderPaymentID, func(cur domain.Payment, found bool) (domain.Payment, error) {
		if err := ownPayment(cur, found, obj.ProviderPaymentID, scope); err != nil {
			return cur, err
		}
		var plan domain.DisputePlan
		switch e.Type {
		case domain.DisputeOpened:
			plan = domain.DisputePlan{Payment: cur.OpenDispute(obj.DisputeID)}
			note = "disputed"
		case domain.DisputeFundsWithdrawn:
			expired, err := p.expired(ctx, cur)
			if err != nil {
				return cur, err
			}
			if plan, err = cur.WithdrawFunds(obj.DisputeID, expired); err != nil {
				return cur, err
			}
			note = "charged_back"
		case domain.DisputeFundsReinstated:
			var err error
			if plan, err = cur.ReinstateFunds(obj.DisputeID); err != nil {
				return cur, err
			}
			note = "succeeded"
		default: // DisputeClosed
			var err error
			switch obj.Status {
			case "won":
				plan, err = cur.CloseDispute(obj.DisputeID, true)
				note = "succeeded"
			case "lost":
				plan, err = cur.CloseDispute(obj.DisputeID, false)
			default:
				err = fmt.Errorf("%w: dispute %s closed with status %q, want won or lost", md.ErrInvalid, obj.DisputeID, obj.Status)
			}
			if err != nil {
				return cur, err
			}
		}
		if plan.Delta != 0 {
			g := md.Grant{Scope: cur.Scope, Pool: cur.Pool, Amount: plan.Delta, IdemKey: plan.IdemKey, Reason: plan.Reason, Ref: refOf(acct, cur, e)}
			if plan.Delta < 0 {
				g.SourceKey = cur.IdemKey // a chargeback empties the payment's own lot first
			}
			if err := p.mint(ctx, g); err != nil {
				return cur, fmt.Errorf("apply %s for dispute %s: %w", plan.Reason, obj.DisputeID, err)
			}
		}
		if plan.Payment != cur {
			announce = &plan.Payment
		}
		return plan.Payment, nil
	})
	if err != nil {
		return "", err
	}
	if announce != nil && note != "" {
		if err := p.announcePayment(ctx, announce.Scope, p.noteOf(*announce, note, announce.Amount.MinorUnits, nil)); err != nil {
			return "", err
		}
	}
	return outcomeProcessed, nil
}

// scopeRef is a scope as a message names it.
type scopeRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// paymentNote is the body of billing.payment.<tag>: facts for outbound events and the host, never an instruction.
type paymentNote struct {
	Realm      string     `json:"realm"`
	Scope      scopeRef   `json:"scope"`
	Event      string     `json:"event"` // succeeded | failed | refunded | disputed | charged_back | renewed | canceled
	Payment    string     `json:"payment"`
	Amount     int64      `json:"amount"` // minor units
	Currency   string     `json:"currency"`
	GraceUntil *time.Time `json:"grace_until,omitempty"`
	ReceiptURL string     `json:"receipt_url,omitempty"`
	// Reason is why an auto top-up was switched off, when this announces that; empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// noteOf is the announcement of something that happened to a payment.
func (p *Processor) noteOf(pay domain.Payment, event string, amount int64, graceUntil *time.Time) paymentNote {
	return paymentNote{Realm: string(pay.Scope.Realm.Or()), Scope: scopeRef{pay.Scope.Kind, pay.Scope.ID}, Event: event,
		Payment: pay.ProviderPaymentID, Amount: amount, Currency: pay.Amount.Currency, GraceUntil: graceUntil, ReceiptURL: pay.ReceiptURL}
}

// announcePayment publishes <prefix>.payment.<tag>. It happens before the event is marked processed and a failure
// retries the event — every step before it is idempotent — so a notification is never lost to a bus that blinked.
func (p *Processor) announcePayment(ctx context.Context, scope domain.Scope, n paymentNote) error {
	return p.publish(ctx, p.d.SubjectPrefix+".payment."+scope.SubjectToken(), n)
}

// entitlementNote is the body of billing.entitlement.<tag>: a scope's entitlements may have changed, so evict it
// from every cache now rather than at its TTL — a customer who just paid must not wait.
type entitlementNote struct {
	Realm  string   `json:"realm"`
	Scope  scopeRef `json:"scope"`
	Reason string   `json:"reason"` // subscription | grace_expired
}

func (p *Processor) announceEntitlement(ctx context.Context, scope domain.Scope, reason string) error {
	return p.publish(ctx, p.d.SubjectPrefix+".entitlement."+scope.SubjectToken(),
		entitlementNote{Realm: string(scope.Realm.Or()), Scope: scopeRef{scope.Kind, scope.ID}, Reason: reason})
}

// publish sends a JSON body to the bus, if there is one.
func (p *Processor) publish(ctx context.Context, subject string, body any) error {
	if p.d.Bus == nil {
		return nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", subject, err)
	}
	if err := p.d.Bus.Publish(ctx, subject, raw); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// paymentFailed records a failed payment and, for a subscription's renewal, takes the provider's current state.
// A failed payment does not revoke anything (invariant 13): the grace window it may open holds entitlements until
// the instant GraceUntil, and a payment already settled is never undone by a failure that follows it.
func (p *Processor) paymentFailed(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, le ports.LeasedEvent) (string, error) {
	e := le.Event
	obj := e.Object
	var failed *domain.Payment
	transitioned := false // this event is what made the payment failed: the first outcome, not a replay of it
	if obj.ProviderPaymentID != "" && obj.Amount != nil {
		_, err := p.d.Payments.Update(ctx, acct.ID, obj.ProviderPaymentID, func(cur domain.Payment, found bool) (domain.Payment, error) {
			transitioned = false
			pay := cur
			if !found {
				pay = domain.Payment{Scope: scope, Account: acct.ID, ProviderPaymentID: obj.ProviderPaymentID, Kind: "one_time", Pool: md.GeneralPool,
					Amount: *obj.Amount, IdemKey: obj.ProviderPaymentID, CreatedAt: e.CreatedAt}
				plan, ok, err := p.d.Catalog.PlanForPrice(ctx, acct.ID, obj.ProviderPriceID)
				if err != nil {
					return cur, fmt.Errorf("catalogue: price %s: %w", obj.ProviderPriceID, err)
				}
				if ok && plan.Realm.Or() == scope.Realm.Or() {
					pay.PlanCode, pay.Kind, pay.Pool = plan.Code, paymentKind(plan), plan.Pool.Or()
				}
			}
			if !found || pay.Status == "" || pay.Status == domain.Pending || pay.Status == domain.Failed {
				transitioned = pay.Status != domain.Failed
				pay.Status, pay.FailureReason = domain.Failed, obj.FailureReason
			}
			failed = &pay
			return pay, nil
		})
		if err != nil {
			return "", err
		}
	}
	// A failed auto top-up counts against the instruction, once; the third in a row switches it off.
	reason, switchedOff := obj.FailureReason, false
	if reason == "" {
		reason = "payment_failed"
	}
	if transitioned && failed != nil {
		switchedOff = p.settleRecharge(ctx, *failed, false, reason)
	}
	var grace *time.Time
	if obj.ProviderSubscriptionID != "" {
		res, err := p.applySubscription(ctx, acct, scope, e, obj.ProviderSubscriptionID)
		if err != nil {
			return "", err
		}
		if !res.skipped {
			grace = res.sub.GraceUntil
		}
	}
	if failed != nil {
		note := p.noteOf(*failed, "failed", failed.Amount.MinorUnits, grace)
		if switchedOff {
			note.Reason = reason // so the host can tell the customer why their top-up stopped
		}
		if err := p.announcePayment(ctx, scope, note); err != nil {
			return "", err
		}
	}
	return outcomeProcessed, nil
}

// subscriptionChanged applies a subscription event. The event says THAT something changed; what the subscription
// is now comes from the provider's current object, so no delivery order can decide state (invariant 15).
func (p *Processor) subscriptionChanged(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, le ports.LeasedEvent) (string, error) {
	e := le.Event
	if e.Object.ProviderSubscriptionID == "" {
		return "", fmt.Errorf("%w: %s %s carries no subscription id", md.ErrInvalid, e.Type, e.ProviderEventID)
	}
	res, err := p.applySubscription(ctx, acct, scope, e, e.Object.ProviderSubscriptionID)
	if err != nil {
		return "", err
	}
	if res.skipped {
		return p.ignored(e, "unknown_price"), nil
	}
	return outcomeProcessed, nil
}

type subResult struct {
	sub     domain.Sub
	applied bool
	skipped bool // the provider's object names a plan the catalogue does not sell: nothing was written
}

// applySubscription fetches the provider's CURRENT subscription and writes it if it is newer than the one held,
// then tells the entitlement layer. Status has exactly one writer — here (invariant 12).
func (p *Processor) applySubscription(ctx context.Context, acct domain.ProviderAccount, scope domain.Scope, e domain.Event, subID string) (subResult, error) {
	provider, ok := p.d.Providers[e.Provider]
	if !ok {
		return subResult{}, fmt.Errorf("%w: no adapter for provider %s", domain.ErrUnknownAccount, e.Provider)
	}
	cur, at, err := provider.FetchSubscription(ctx, acct, subID)
	if err != nil {
		return subResult{}, fmt.Errorf("fetch subscription %s: %w", subID, err)
	}
	if cur.PlanCode == "" {
		p.d.Log.Warn("a subscription is on a price the catalogue does not sell", "provider", string(e.Provider), "account", acct.ID, "subscription", subID)
		return subResult{skipped: true}, nil
	}
	sub := cur
	sub.Scope, sub.Account, sub.ProviderSubscriptionID, sub.GraceLapsedAt = scope, acct.ID, subID, nil

	held, has, err := p.d.Subs.Get(ctx, scope)
	if err != nil {
		return subResult{}, fmt.Errorf("read the subscription of %s: %w", scope.Tag(), err)
	}
	same := has && held.ProviderSubscriptionID == subID
	switch {
	case sub.Status != domain.PastDue:
		sub.GraceUntil = nil // not past due at the provider: nothing to hold on to
	case same && held.Status == domain.PastDue && held.GraceUntil != nil:
		// Still the same trouble: the window opened when it began, and a newer notice of it does not move it.
		sub.GraceUntil, sub.GraceLapsedAt = held.GraceUntil, held.GraceLapsedAt
	default:
		g := at.Add(p.d.Grace) // the provider's clock, not ours: a retry computes the same instant
		sub.GraceUntil = &g
	}

	applied, err := p.d.Subs.ApplyIfNewer(ctx, sub, at)
	if err != nil {
		return subResult{}, fmt.Errorf("apply subscription %s: %w", subID, err)
	}
	if applied {
		if err := p.announceEntitlement(ctx, scope, "subscription"); err != nil {
			return subResult{}, err
		}
		if sub.Status == domain.Canceled && (!same || held.Status != domain.Canceled) {
			if err := p.announcePayment(ctx, scope, paymentNote{Realm: string(scope.Realm.Or()), Scope: scopeRef{scope.Kind, scope.ID}, Event: "canceled"}); err != nil {
				return subResult{}, err
			}
		}
	}
	return subResult{sub: sub, applied: applied}, nil
}

// sweepGrace announces, once, that a grace window has ended — so entitlement caches evict the scope at that
// instant instead of at their TTL. Entitlements themselves lapse by the clock (Sub.HoldsEntitlements); this only
// tells the others. The announcement is made before it is recorded, so a crash between them repeats it, which
// is harmless, instead of losing it.
func (p *Processor) sweepGrace(ctx context.Context) error {
	now := p.d.Clock.Now()
	due, err := p.d.Subs.DueForGraceExpiry(ctx, now)
	if err != nil {
		return fmt.Errorf("list ended grace windows: %w", err)
	}
	var errs []error
	for _, s := range due {
		if err := p.announceEntitlement(ctx, s.Scope, "grace_expired"); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := p.d.Subs.MarkGraceLapsed(ctx, s, now); err != nil {
			errs = append(errs, fmt.Errorf("record the end of the grace of %s: %w", s.ProviderSubscriptionID, err))
		}
	}
	return errors.Join(errs...)
}
