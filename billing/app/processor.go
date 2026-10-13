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
	plan, ok, err := p.d.Catalog.PlanForPrice(ctx, acct.ID, obj.ProviderPriceID)
	if err != nil {
		return "", fmt.Errorf("catalogue: price %s: %w", obj.ProviderPriceID, err)
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

	_, err = p.d.Payments.Update(ctx, acct.ID, obj.ProviderPaymentID, func(cur domain.Payment, found bool) (domain.Payment, error) {
		pay := cur
		if !found {
			pay = domain.Payment{Scope: scope, Account: acct.ID, ProviderPaymentID: obj.ProviderPaymentID, PlanCode: plan.Code,
				Kind: paymentKind(plan), Pool: plan.Pool.Or(), IdemKey: obj.ProviderPaymentID, CreatedAt: e.CreatedAt}
			if plan.CreditAllotment != nil {
				pay.CreditsGranted = *plan.CreditAllotment
			}
		}
		if !found || pay.Status == "" || pay.Status == domain.Pending || pay.Status == domain.Failed {
			pay.Amount, pay.TaxAmount, pay.ReceiptURL = *obj.Amount, obj.TaxAmount, obj.ReceiptURL
			pay.Status, pay.FailureReason = domain.Succeeded, ""
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
	return outcomeProcessed, nil
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
		if err := p.announcePayment(ctx, *announce, "refunded", announce.RefundedMinor); err != nil {
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
		if err := p.announcePayment(ctx, *announce, note, announce.Amount.MinorUnits); err != nil {
			return "", err
		}
	}
	return outcomeProcessed, nil
}

// paymentNote is the body of billing.payment.<tag>: facts for outbound events and the host, never an instruction.
type paymentNote struct {
	Realm string `json:"realm"`
	Scope struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"scope"`
	Event      string     `json:"event"` // succeeded | failed | refunded | disputed | charged_back | renewed | canceled
	Payment    string     `json:"payment"`
	Amount     int64      `json:"amount"` // minor units
	Currency   string     `json:"currency"`
	GraceUntil *time.Time `json:"grace_until,omitempty"`
	ReceiptURL string     `json:"receipt_url,omitempty"`
}

// announcePayment publishes billing.payment.<tag>. It happens before the event is marked processed and a failure
// retries the event — every step before it is idempotent — so a notification is never lost to a bus that blinked.
func (p *Processor) announcePayment(ctx context.Context, pay domain.Payment, event string, amount int64) error {
	n := paymentNote{Realm: string(pay.Scope.Realm.Or()), Event: event, Payment: pay.ProviderPaymentID, Amount: amount,
		Currency: pay.Amount.Currency, ReceiptURL: pay.ReceiptURL}
	n.Scope.Kind, n.Scope.ID = pay.Scope.Kind, pay.Scope.ID
	return p.publish(ctx, "billing.payment."+pay.Scope.SubjectToken(), n)
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
