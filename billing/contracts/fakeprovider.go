package contracts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// FakeDefect is a deliberate flaw in a FakeProvider. Each is one way a real adapter goes wrong, and ProviderContract
// must reject every one by name — a suite that cannot fail has not been shown to test anything.
type FakeDefect string

// The flaws.
const (
	DefectNone             FakeDefect = ""
	DefectSkipVerification FakeDefect = "skip-verification" // parses and trusts the body
	DefectIgnoreTimestamp  FakeDefect = "ignore-timestamp"  // a captured payload verifies forever
	DefectSharedSecret     FakeDefect = "shared-secret"     // any account's secret verifies for every account
	DefectNoRawHash        FakeDefect = "no-raw-hash"       // the verified body is not hashed
	DefectPerEventRefund   FakeDefect = "per-event-refund"  // reports this refund, not the cumulative total
	DefectNoIgnore         FakeDefect = "no-ignore"         // an unsubscribed type becomes a payment
	DefectPartialOnError   FakeDefect = "partial-on-error"  // returns a half-filled Event beside the error
	DefectIgnoreArrival    FakeDefect = "ignore-arrival"    // judges a signature's age by when it is verified, not when the delivery arrived
)

// FakeEvent is the body the fake provider's webhooks carry.
type FakeEvent struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Created      int64             `json:"created"`
	Customer     string            `json:"customer,omitempty"`
	Payment      string            `json:"payment,omitempty"`
	Subscription string            `json:"subscription,omitempty"`
	Price        string            `json:"price,omitempty"`
	Amount       int64             `json:"amount,omitempty"`
	Currency     string            `json:"currency,omitempty"`
	RefundAmount int64             `json:"refund_amount,omitempty"` // this refund alone
	Refunded     int64             `json:"refunded,omitempty"`      // cumulative, as a provider reports it
	Refund       string            `json:"refund,omitempty"`
	Dispute      string            `json:"dispute,omitempty"`
	Status       string            `json:"status,omitempty"`
	PeriodStart  int64             `json:"period_start,omitempty"`
	PeriodEnd    int64             `json:"period_end,omitempty"`
	CancelAtEnd  bool              `json:"cancel_at_end,omitempty"`
	Method       string            `json:"method,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// SignatureHeader is where the fake provider puts its signature: `t=<unix seconds>,v1=<hex HMAC-SHA256(secret, t.body)>`.
const SignatureHeader = "X-Fake-Signature"

// FakeProvider is a PaymentProvider with an honest signature scheme, so the suites have something real to verify
// and something to break. It is safe for concurrent use.
type FakeProvider struct {
	// Tolerance is how far the signature's timestamp may be from now (default 5 minutes).
	Tolerance time.Duration

	name    domain.Provider
	clock   mports.Clock
	log     *CallLog
	defect  FakeDefect
	secrets map[string]string // account id → webhook secret

	down         atomic.Bool
	wrongAcct    atomic.Bool
	mu           sync.Mutex
	subs         map[string]fetchedSub
	checkouts    []ports.CheckoutInput
	customers    map[string]string // idem key → provider customer id
	ensureCalls  int
	failCheckout error
	declined     map[string]error // checkout mode → the error it is declined with
	// NonIdempotentCustomers makes EnsureCustomer create a new customer on every call: a provider that ignores the
	// key, so two first checkouts at once leave two customers and one of them unmapped.
	NonIdempotentCustomers bool
	charges                map[string]string // idem key → provider payment id
	chargeCalls            []ports.OffSessionInput
	changes                []string
	cancels                []string
	verifyCalls            int
}

type fetchedSub struct {
	sub domain.Sub
	at  time.Time
}

// NewFakeProvider serves the accounts given, each with a secret of its own.
func NewFakeProvider(name domain.Provider, clock mports.Clock, log *CallLog, accts ...domain.ProviderAccount) *FakeProvider {
	p := &FakeProvider{Tolerance: 5 * time.Minute, name: name, clock: clock, log: log, secrets: map[string]string{},
		subs: map[string]fetchedSub{}, charges: map[string]string{}, customers: map[string]string{}}
	for _, a := range accts {
		p.secrets[a.ID] = "whsec-" + a.ID
	}
	return p
}

// WithDefect returns a provider that behaves like p except for one flaw. It shares p's secrets and clock.
func (p *FakeProvider) WithDefect(d FakeDefect) *FakeProvider {
	q := NewFakeProvider(p.name, p.clock, p.log)
	q.Tolerance, q.defect = p.Tolerance, d
	for k, v := range p.secrets {
		q.secrets[k] = v
	}
	return q
}

// SetDown makes verification unavailable (as PayPal's API being down does) until cleared.
func (p *FakeProvider) SetDown(down bool) { p.down.Store(down) }

// ReturnWrongAccount makes the provider return events that name another account than the one verified — an
// adapter defect the ingress must refuse rather than file under the wrong secret.
func (p *FakeProvider) ReturnWrongAccount(v bool) { p.wrongAcct.Store(v) }

// VerifyCalls is how many times VerifyWebhook was called.
func (p *FakeProvider) VerifyCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verifyCalls
}

// Name implements ports.PaymentProvider.
func (p *FakeProvider) Name() domain.Provider { return p.name }

// SecretOf is the webhook secret the provider holds for an account: what a fixture signs with.
func (p *FakeProvider) SecretOf(accountID string) string { return p.secrets[accountID] }

func sign(secret string, at time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", at.Unix())
	_, _ = mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", at.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// Sign returns the headers a delivery of body to the account carries when signed at `at`.
func (p *FakeProvider) Sign(accountID string, body []byte, at time.Time) http.Header {
	h := http.Header{}
	h.Set(SignatureHeader, sign(p.secrets[accountID], at, body))
	return h
}

func (p *FakeProvider) authentic(ctx context.Context, acct domain.ProviderAccount, body []byte, headers http.Header) error {
	sig := headers.Get(SignatureHeader)
	if sig == "" {
		return fmt.Errorf("%w: no signature", domain.ErrInvalidSignature)
	}
	var ts int64
	var v1 string
	for _, part := range strings.Split(sig, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			v1 = v
		}
	}
	if ts == 0 || v1 == "" {
		return fmt.Errorf("%w: malformed signature", domain.ErrInvalidSignature)
	}
	if p.defect != DefectIgnoreTimestamp {
		now := p.clock.Now()
		if p.defect != DefectIgnoreArrival {
			now = ports.ReceivedAt(ctx, now)
		}
		age := now.Sub(time.Unix(ts, 0))
		if age < 0 {
			age = -age
		}
		if age > p.Tolerance {
			return fmt.Errorf("%w: timestamp outside the tolerance", domain.ErrInvalidSignature)
		}
	}
	candidates := []string{p.secrets[acct.ID]}
	if p.defect == DefectSharedSecret {
		candidates = nil
		for _, s := range p.secrets {
			candidates = append(candidates, s)
		}
	}
	got, err := hex.DecodeString(v1)
	if err != nil {
		return fmt.Errorf("%w: malformed signature", domain.ErrInvalidSignature)
	}
	for _, secret := range candidates {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = fmt.Fprintf(mac, "%d.", ts)
		_, _ = mac.Write(body)
		if hmac.Equal(got, mac.Sum(nil)) {
			return nil
		}
	}
	return fmt.Errorf("%w: signature mismatch", domain.ErrInvalidSignature)
}

// VerifyWebhook implements ports.PaymentProvider.
func (p *FakeProvider) VerifyWebhook(ctx context.Context, acct domain.ProviderAccount, body []byte, headers http.Header) (domain.Event, error) {
	p.log.Add("provider.VerifyWebhook")
	p.mu.Lock()
	p.verifyCalls++
	p.mu.Unlock()
	if p.down.Load() {
		return domain.Event{}, fmt.Errorf("%w: verification service down", domain.ErrVerificationUnavailable)
	}
	if p.defect != DefectSkipVerification {
		if err := p.authentic(ctx, acct, body, headers); err != nil {
			if p.defect == DefectPartialOnError {
				return domain.Event{Provider: p.name, Account: acct.ID}, err
			}
			return domain.Event{}, err
		}
	}
	var fe FakeEvent
	if err := json.Unmarshal(body, &fe); err != nil {
		return domain.Event{}, fmt.Errorf("fake provider: verified body is not JSON: %w", err)
	}
	e := p.normalize(acct, fe)
	if p.defect != DefectNoRawHash {
		sum := sha256.Sum256(body)
		e.RawHash = hex.EncodeToString(sum[:])
	}
	if p.wrongAcct.Load() {
		e.Account = acct.ID + "-elsewhere"
	}
	return e, nil
}

func (p *FakeProvider) normalize(acct domain.ProviderAccount, fe FakeEvent) domain.Event {
	money := func(minor int64) *domain.Money {
		if minor == 0 && fe.Currency == "" {
			return nil
		}
		return &domain.Money{MinorUnits: minor, Currency: fe.Currency}
	}
	e := domain.Event{Provider: p.name, Account: acct.ID, ProviderEventID: fe.ID, CreatedAt: time.Unix(fe.Created, 0).UTC(),
		Type: domain.EventType(fe.Type)}
	obj := domain.EventObject{ProviderCustomerID: fe.Customer, ProviderPaymentID: fe.Payment, ProviderSubscriptionID: fe.Subscription,
		ProviderPriceID: fe.Price, ProviderRefundID: fe.Refund, DisputeID: fe.Dispute, Status: fe.Status, CancelAtPeriodEnd: fe.CancelAtEnd,
		FailureReason: fe.Reason, PaymentMethodRef: fe.Method}
	if fe.Amount != 0 {
		obj.Amount = money(fe.Amount)
	}
	if fe.Refunded != 0 || fe.RefundAmount != 0 {
		cum := fe.Refunded
		if p.defect == DefectPerEventRefund {
			cum = fe.RefundAmount
		}
		obj.AmountRefunded = money(cum)
	}
	if fe.PeriodStart != 0 {
		t := time.Unix(fe.PeriodStart, 0).UTC()
		obj.PeriodStart = &t
	}
	if fe.PeriodEnd != 0 {
		t := time.Unix(fe.PeriodEnd, 0).UTC()
		obj.PeriodEnd = &t
	}
	e.Object = obj
	if !e.Type.Valid() || e.Type == domain.Ignored {
		e.Type = domain.Ignored
		e.Object = domain.EventObject{}
		if p.defect == DefectNoIgnore {
			e.Type, e.Object = domain.PaymentSucceeded, obj
		}
	}
	return e
}

// EnsureCustomer implements ports.PaymentProvider: idempotent on the key, as a provider's is.
func (p *FakeProvider) EnsureCustomer(_ context.Context, _ domain.ProviderAccount, in ports.CustomerInput) (string, error) {
	p.log.Add("provider.EnsureCustomer")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureCalls++
	if id, ok := p.customers[in.IdemKey]; ok && !p.NonIdempotentCustomers {
		return id, nil
	}
	id := "cus_new_" + strconv.Itoa(p.ensureCalls)
	p.customers[in.IdemKey+"#"+id] = id
	if !p.NonIdempotentCustomers {
		p.customers[in.IdemKey] = id
	}
	return id, nil
}

// EnsureCalls is how many times EnsureCustomer was called, retries included.
func (p *FakeProvider) EnsureCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureCalls
}

// CustomersCreated is how many distinct customers the provider holds.
func (p *FakeProvider) CustomersCreated() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range p.customers {
		seen[id] = true
	}
	return len(seen)
}

// FailCheckout makes CreateCheckout fail with err; nil clears it.
func (p *FakeProvider) FailCheckout(err error) {
	p.mu.Lock()
	p.failCheckout = err
	p.mu.Unlock()
}

// DeclineMode makes CreateCheckout answer err for one mode ("payment", "subscription" or "setup"), as a provider that
// cannot do it does; nil clears it.
func (p *FakeProvider) DeclineMode(mode string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.declined == nil {
		p.declined = map[string]error{}
	}
	if err == nil {
		delete(p.declined, mode)
		return
	}
	p.declined[mode] = err
}

// CreateCheckout implements ports.PaymentProvider. It returns a URL and nothing else: fulfilment is the webhook's.
func (p *FakeProvider) CreateCheckout(_ context.Context, acct domain.ProviderAccount, in ports.CheckoutInput) (string, error) {
	p.log.Add("provider.CreateCheckout")
	p.mu.Lock()
	err := p.failCheckout
	if err == nil {
		err = p.declined[in.Mode]
	}
	if err == nil {
		p.checkouts = append(p.checkouts, in)
	}
	p.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "https://pay.fake.test/" + acct.ID + "/checkout/" + in.IdemKey, nil
}

// Checkouts is the checkouts created so far.
func (p *FakeProvider) Checkouts() []ports.CheckoutInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ports.CheckoutInput(nil), p.checkouts...)
}

// CreatePortalSession implements ports.PaymentProvider.
func (p *FakeProvider) CreatePortalSession(_ context.Context, acct domain.ProviderAccount, customerID, _ string) (string, error) {
	p.log.Add("provider.CreatePortalSession")
	return "https://pay.fake.test/" + acct.ID + "/portal/" + customerID, nil
}

// SetSubscription sets the provider's current object for a subscription, and the instant it is current as of.
func (p *FakeProvider) SetSubscription(id string, s domain.Sub, at time.Time) {
	p.mu.Lock()
	p.subs[id] = fetchedSub{s, at}
	p.mu.Unlock()
}

// FetchSubscription implements ports.PaymentProvider.
func (p *FakeProvider) FetchSubscription(_ context.Context, _ domain.ProviderAccount, id string) (domain.Sub, time.Time, error) {
	p.log.Add("provider.FetchSubscription")
	p.mu.Lock()
	defer p.mu.Unlock()
	f, ok := p.subs[id]
	if !ok {
		return domain.Sub{}, time.Time{}, fmt.Errorf("fake provider: no subscription %s", id)
	}
	return f.sub, f.at, nil
}

// ChangePlan implements ports.PaymentProvider.
func (p *FakeProvider) ChangePlan(_ context.Context, _ domain.ProviderAccount, subID, priceID string, policy domain.PlanChangePolicy) error {
	p.log.Add("provider.ChangePlan")
	p.mu.Lock()
	p.changes = append(p.changes, fmt.Sprintf("%s>%s/%s", subID, priceID, policy))
	p.mu.Unlock()
	return nil
}

// Changes is the plan changes requested, as "sub>price/policy".
func (p *FakeProvider) Changes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.changes...)
}

// CancelAtPeriodEnd implements ports.PaymentProvider.
func (p *FakeProvider) CancelAtPeriodEnd(_ context.Context, _ domain.ProviderAccount, subID string) error {
	p.log.Add("provider.CancelAtPeriodEnd")
	p.mu.Lock()
	p.cancels = append(p.cancels, subID)
	p.mu.Unlock()
	return nil
}

// Cancels is the subscriptions asked to cancel at period end.
func (p *FakeProvider) Cancels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.cancels...)
}

var errCardDeclined = fmt.Errorf("%w: fake provider: the card was declined", domain.ErrChargeDeclined)

// DeclineCharges makes ChargeSaved fail, as a declined card does.
func (p *FakeProvider) DeclineCharges(v bool) {
	p.mu.Lock()
	if v {
		p.charges["\x00decline"] = "1"
	} else {
		delete(p.charges, "\x00decline")
	}
	p.mu.Unlock()
}

// ChargeSaved implements ports.PaymentProvider: idempotent on the key, as a provider's is.
func (p *FakeProvider) ChargeSaved(_ context.Context, _ domain.ProviderAccount, in ports.OffSessionInput) (string, error) {
	p.log.Add("provider.ChargeSaved")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chargeCalls = append(p.chargeCalls, in)
	if id, ok := p.charges[in.IdemKey]; ok {
		return id, nil
	}
	if _, declined := p.charges["\x00decline"]; declined {
		return "", errCardDeclined
	}
	id := "pi_" + strconv.Itoa(len(p.charges)+1)
	p.charges[in.IdemKey] = id
	return id, nil
}

// ChargeCalls is every ChargeSaved call, retries included.
func (p *FakeProvider) ChargeCalls() []ports.OffSessionInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ports.OffSessionInput(nil), p.chargeCalls...)
}

// Charges is the distinct charges the provider holds, by idempotency key.
func (p *FakeProvider) Charges() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for k, v := range p.charges {
		if !strings.HasPrefix(k, "\x00") {
			out[k] = v
		}
	}
	return out
}

var _ ports.PaymentProvider = (*FakeProvider)(nil)
