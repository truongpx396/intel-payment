package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Defaults of the ingress.
const (
	DefaultMaxBody       = 1 << 20          // 1 MiB: a webhook is a few kilobytes; more is not one
	DefaultVerifyTimeout = 10 * time.Second // PayPal verifies by API call; a slow one is parked, not waited for
	retryBatch           = 100
	// DefaultSubjectPrefix is the namespace of every subject the payment layer publishes or listens on, as the engine's
	// own (PAYMENT_SUBJECT_PREFIX): billing.payment.<tag>, billing.entitlement.<tag>, billing.balance.low.<tag> …
	DefaultSubjectPrefix = "billing"
	// webhookReceived is the latency hint published after a delivery is persisted, under the prefix. The inbox sweep is the
	// guarantee; the hint only makes the common case fast.
	webhookReceived = ".webhook.received"
)

// IngressDeps are the ports stage 1 needs.
type IngressDeps struct {
	Providers ports.Providers
	Accounts  ports.Accounts
	Inbox     ports.Inbox
	Metrics   mports.Metrics
	Clock     mports.Clock
	// Bus is optional: with one, a persisted delivery publishes <prefix>.webhook.received. A failed hint never
	// fails the acknowledgement — the delivery is durable either way.
	Bus mports.Bus
	Log *slog.Logger

	// SubjectPrefix is the namespace the hint goes out under (default DefaultSubjectPrefix; PAYMENT_SUBJECT_PREFIX).
	SubjectPrefix string

	MaxBody       int           // default DefaultMaxBody
	VerifyTimeout time.Duration // default DefaultVerifyTimeout
	// ParkBackoff is how long a parked delivery waits after its nth failed attempt (default: 30 s doubling, 1 h cap).
	ParkBackoff func(attempts int) time.Duration
}

// Ingress is stage 1 of the webhook flow, synchronous and provider-facing:
//
//  1. the body is already read, size-limited, and not parsed;
//  2. the route names the account, so the secret and the realms it may touch are known first;
//  3. the provider verifies the RAW body — nothing is parsed, looked up or logged before it does;
//  4. the verified, normalized event is persisted; a duplicate is a no-op that still succeeds;
//  5. and only then is the delivery acknowledged.
//
// Nothing here touches money. The only way a verified delivery is lost is a failure before step 4 commits — and
// then the provider has not received 200 and retries it (payments contract, § Stage 1).
type Ingress struct {
	d IngressDeps
}

var _ ports.WebhookIngress = (*Ingress)(nil)

// NewIngress validates its dependencies and returns the ingress.
func NewIngress(d IngressDeps) (*Ingress, error) {
	var errs []error
	if len(d.Providers) == 0 || d.Accounts == nil || d.Inbox == nil {
		errs = append(errs, errors.New("the Providers, Accounts and Inbox are required"))
	}
	if d.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: the core reads no wall clock"))
	}
	if d.MaxBody < 0 || d.VerifyTimeout < 0 {
		errs = append(errs, errors.New("MaxBody and VerifyTimeout cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("billing ingress: %w", err)
	}
	if d.Metrics == nil {
		d.Metrics = noMetrics{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.SubjectPrefix == "" {
		d.SubjectPrefix = DefaultSubjectPrefix
	}
	if d.MaxBody == 0 {
		d.MaxBody = DefaultMaxBody
	}
	if d.VerifyTimeout == 0 {
		d.VerifyTimeout = DefaultVerifyTimeout
	}
	if d.ParkBackoff == nil {
		d.ParkBackoff = defaultParkBackoff
	}
	return &Ingress{d: d}, nil
}

func defaultParkBackoff(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

type noMetrics struct{}

func (noMetrics) Count(string, int64, mports.Labels)     {}
func (noMetrics) Gauge(string, int64, mports.Labels)     {}
func (noMetrics) Observe(string, float64, mports.Labels) {}

func result(status int, code string) ports.IngressResult {
	return ports.IngressResult{Status: status, Code: code}
}

// resolve finds the provider and the live account a route names. The account must exist, be active, and belong
// to this provider: a route is not a way to verify a body against some other account's secret.
func (g *Ingress) resolve(ctx context.Context, provider, account string) (ports.PaymentProvider, domain.ProviderAccount, error) {
	p, ok := g.d.Providers[domain.Provider(provider)]
	if !ok {
		return nil, domain.ProviderAccount{}, domain.ErrUnknownAccount
	}
	acct, found, err := g.d.Accounts.Account(ctx, account)
	if err != nil {
		return nil, domain.ProviderAccount{}, err
	}
	if !found || !acct.Active || acct.Provider != p.Name() {
		return nil, domain.ProviderAccount{}, domain.ErrUnknownAccount
	}
	return p, acct, nil
}

// verify runs the provider's verification under a deadline of its own. A verification that ran out of time is
// unavailable, not invalid: the delivery may be perfectly genuine.
func (g *Ingress) verify(ctx context.Context, p ports.PaymentProvider, acct domain.ProviderAccount, body []byte, header http.Header) (domain.Event, error) {
	vctx, cancel := context.WithTimeout(ctx, g.d.VerifyTimeout)
	defer cancel()
	e, err := p.VerifyWebhook(vctx, acct, body, header)
	if err != nil && ctx.Err() == nil && errors.Is(vctx.Err(), context.DeadlineExceeded) &&
		!errors.Is(err, domain.ErrInvalidSignature) && !errors.Is(err, domain.ErrVerificationUnavailable) {
		err = fmt.Errorf("%w: %w", domain.ErrVerificationUnavailable, err)
	}
	if err != nil {
		return domain.Event{}, err
	}
	// The event's existence is the proof of verification; one an adapter built wrongly is a defect to refuse, not
	// to file under a secret it was not verified with.
	if verr := e.Validate(); verr != nil {
		return domain.Event{}, fmt.Errorf("adapter %s returned an event verification could not have produced: %w", p.Name(), verr)
	}
	if e.Account != acct.ID || e.Provider != p.Name() {
		return domain.Event{}, fmt.Errorf("adapter %s verified for account %s and returned an event for %s/%s", p.Name(), acct.ID, e.Provider, e.Account)
	}
	return e, nil
}

func (g *Ingress) count(name, provider, result string) {
	g.d.Metrics.Count(name, 1, mports.Labels{"provider": provider, "result": result})
}

func (g *Ingress) verifyFailed(provider, account string, err error) {
	g.d.Metrics.Count(ports.MetricWebhookVerifyFailed, 1, mports.Labels{"provider": provider})
	// The payload is not logged: nothing about an unverified body is trusted, and it may hold PII.
	g.d.Log.Warn("webhook signature verification failed (security event)", "provider", provider, "account", account, "error", err.Error())
}

// Handle implements ports.WebhookIngress.
func (g *Ingress) Handle(ctx context.Context, provider, account string, body []byte, header http.Header) ports.IngressResult {
	if len(body) > g.d.MaxBody {
		return result(http.StatusRequestEntityTooLarge, "payload_too_large")
	}
	p, acct, err := g.resolve(ctx, provider, account)
	switch {
	case errors.Is(err, domain.ErrUnknownAccount):
		return result(http.StatusNotFound, "unknown_account")
	case err != nil:
		g.d.Log.Error("webhook: provider account lookup failed", "error", err.Error())
		return result(http.StatusServiceUnavailable, "internal")
	}

	// From here the provider is a registered one, so it is a bounded metric label; what the route typed never is.
	label := string(p.Name())
	e, err := g.verify(ctx, p, acct, body, header)
	switch {
	case errors.Is(err, domain.ErrInvalidSignature):
		g.verifyFailed(label, acct.ID, err)
		return result(http.StatusBadRequest, "invalid_signature")
	case errors.Is(err, domain.ErrVerificationUnavailable):
		return g.park(ctx, p.Name(), label, acct.ID, body, header)
	case err != nil:
		// Verification could not run for a reason that is ours (a secret that does not resolve, an adapter that
		// returned nonsense). Not a 2xx — the provider must retry — and not an invalid signature.
		g.d.Log.Error("webhook: verification failed to run", "provider", label, "account", acct.ID, "error", err.Error())
		return result(http.StatusInternalServerError, "internal")
	}

	inserted, err := g.d.Inbox.Persist(ctx, e)
	if err != nil {
		g.d.Log.Error("webhook: persisting a verified delivery failed", "provider", label, "account", acct.ID, "event", e.ProviderEventID, "error", err.Error())
		return result(http.StatusServiceUnavailable, "persist_failed")
	}
	if inserted {
		g.count(ports.MetricWebhookReceived, label, "new")
		g.hint(ctx, e)
	} else {
		g.count(ports.MetricWebhookReceived, label, "duplicate")
	}
	return result(http.StatusOK, "")
}

// park keeps a delivery that could be neither verified nor rejected, raw body included, for billing.webhook.tick
// to verify again. It is named by the hash of its bytes, because nothing in it may be parsed yet.
func (g *Ingress) park(ctx context.Context, provider domain.Provider, label, account string, body []byte, header http.Header) ports.IngressResult {
	sum := sha256.Sum256(body)
	if err := g.d.Inbox.Park(ctx, provider, account, "parked:"+hex.EncodeToString(sum[:]), body, header); err != nil {
		g.d.Log.Error("webhook: parking an unverifiable delivery failed", "provider", label, "account", account, "error", err.Error())
		return result(http.StatusServiceUnavailable, "persist_failed")
	}
	g.count(ports.MetricWebhookReceived, label, "parked")
	g.d.Log.Warn("webhook parked: verification is unavailable", "provider", label, "account", account)
	return result(http.StatusAccepted, "")
}

// hint publishes <prefix>.webhook.received. It is a latency optimisation only: the inbox sweep is the guarantee.
func (g *Ingress) hint(ctx context.Context, e domain.Event) {
	if g.d.Bus == nil {
		return
	}
	payload := fmt.Sprintf(`{"account":%q,"event_id":%q}`, e.Account, e.ProviderEventID)
	if err := g.d.Bus.Publish(ctx, g.d.SubjectPrefix+webhookReceived, []byte(payload)); err != nil {
		g.d.Log.Warn("webhook: the received hint was not published; the inbox sweep will find the event", "error", err.Error())
	}
}

// Retry implements ports.WebhookIngress: it verifies the parked deliveries again.
func (g *Ingress) Retry(ctx context.Context) error {
	parked, err := g.d.Inbox.LeaseParked(ctx, retryBatch, 2*g.d.VerifyTimeout)
	if err != nil {
		return fmt.Errorf("lease parked deliveries: %w", err)
	}
	var errs []error
	for _, d := range parked {
		if err := g.retryOne(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("parked delivery %s: %w", d.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (g *Ingress) retryOne(ctx context.Context, d ports.ParkedDelivery) error {
	later := func() error {
		return g.d.Inbox.RetryParked(ctx, d.ID, g.d.Clock.Now().Add(g.d.ParkBackoff(d.Attempts)))
	}

	p, acct, err := g.resolve(ctx, string(d.Provider), d.Account)
	switch {
	case errors.Is(err, domain.ErrUnknownAccount):
		return later() // the account was switched off: an operator may switch it back on; the delivery waits
	case err != nil:
		return err
	}
	label := string(p.Name())
	// A signature's age is judged as of when the delivery arrived: the provider was told 202 and will not send it again,
	// so an outage longer than the tolerance must not turn a genuine delivery into a replay.
	e, err := g.verify(ports.WithReceivedAt(ctx, d.ReceivedAt), p, acct, d.Body, d.Header)
	switch {
	case errors.Is(err, domain.ErrInvalidSignature):
		g.verifyFailed(label, acct.ID, err)
		return g.d.Inbox.Reject(ctx, d.ID, "invalid_signature")
	case errors.Is(err, domain.ErrVerificationUnavailable):
		return later()
	case err != nil:
		return err
	}
	inserted, err := g.d.Inbox.Promote(ctx, d.ID, e)
	if err != nil {
		return err
	}
	if inserted {
		g.count(ports.MetricWebhookReceived, label, "new")
		g.hint(ctx, e)
	} else {
		g.count(ports.MetricWebhookReceived, label, "duplicate")
	}
	return nil
}
