package contracts

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The world every suite runs in, so an expectation is a number and not a formula.
const (
	ProviderName   = domain.Provider("fake")
	AccountID      = "acct"          // serves the default realm
	OtherAccountID = "acct-b"        // another account of the same provider, with a secret of its own
	InactiveID     = "acct-inactive" // exists, and is switched off
	ElsewhereID    = "acct-x"        // belongs to another provider
	PackCode       = "pack"          // a one-time pack: $10.00 for 10,000,000 credits
	ProCode        = "pro"           // a monthly subscription: $20.00 for 5,000,000 credits into the `plan` pool
	PackCredits    = md.Credits(10_000_000)
	ProCredits     = md.Credits(5_000_000)
	PackPriceID    = "price_pack"
	ProPriceID     = "price_pro"
	// MaxWebhookBody is the largest delivery the ingress is built to accept.
	MaxWebhookBody = 1 << 20
)

// Harness is one isolated world: a clock, a provider, and every store, with the call log they share. Each case
// builds its own, so cases run in parallel without sharing a balance.
type Harness struct {
	Clock     *mcontracts.FakeClock
	Log       *CallLog
	Provider  *FakeProvider
	Accounts  *MemAccounts
	Inbox     *MemInbox
	Payments  *MemPayments
	Subs      *MemSubs
	Customers *MemCustomers
	Catalog   *MemCatalog
	Meter     *FakeMeter
	Lots      *FakeLots
	Metrics   *RecordingMetrics

	// Scope is the customer cus_1's scope; Scope2 is cus_2's. Foreign is cus_foreign's, in a realm the account
	// does not serve.
	Scope, Scope2, Foreign md.Scope
}

// Start is the harness clock's first instant.
var Start = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// NewHarness builds the world: three accounts of the provider "fake" (and one of another provider), the catalogue,
// and three customers.
func NewHarness() *Harness {
	log := &CallLog{}
	clock := mcontracts.NewFakeClock(Start)
	acct := func(id string, p domain.Provider, active bool) domain.ProviderAccount {
		return domain.ProviderAccount{ID: id, Provider: p, Realms: []domain.Realm{md.DefaultRealm}, Mode: "test",
			SecretRef: "env:FAKE_SECRET_" + id, WebhookSecretRef: "env:FAKE_WEBHOOK_" + id, Active: active}
	}
	accts := []domain.ProviderAccount{acct(AccountID, ProviderName, true), acct(OtherAccountID, ProviderName, true),
		acct(InactiveID, ProviderName, false), acct(ElsewhereID, "elsewhere", true)}
	h := &Harness{
		Clock: clock, Log: log,
		Provider: NewFakeProvider(ProviderName, clock, log, accts...),
		Accounts: NewMemAccounts(log, accts...),
		Inbox:    NewMemInbox(clock, log), Payments: NewMemPayments(log), Subs: NewMemSubs(log),
		Customers: NewMemCustomers(log), Catalog: NewMemCatalog(log), Meter: NewFakeMeter(log), Lots: NewFakeLots(log),
		Metrics: &RecordingMetrics{},
		Scope:   md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"},
		Scope2:  md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w2"},
		Foreign: md.Scope{Realm: "other", Kind: "workspace", ID: "x1"},
	}
	credits, plan := PackCredits, ProCredits
	month := domain.Monthly
	h.Catalog.Add(domain.Plan{Realm: md.DefaultRealm, Code: PackCode, Name: "Credit pack", Kind: domain.OneTime, CreditAllotment: &credits,
		Pool: md.GeneralPool, Active: true, Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}}}, AccountID, "USD", PackPriceID)
	h.Catalog.Add(domain.Plan{Realm: md.DefaultRealm, Code: ProCode, Name: "Pro", Kind: domain.Subscription, CreditAllotment: &plan,
		Pool: "plan", Interval: &month, Active: true, SortOrder: 1, Prices: map[string]domain.Money{"USD": {MinorUnits: 2000, Currency: "USD"}}}, AccountID, "USD", ProPriceID)
	for id, s := range map[string]md.Scope{"cus_1": h.Scope, "cus_2": h.Scope2, "cus_foreign": h.Foreign} {
		_ = h.Customers.Upsert(context.Background(), domain.Customer{Scope: s, Account: AccountID, ProviderCustomerID: id})
	}
	log.Reset()
	return h
}

// Ctx is the context every suite call uses.
func Ctx() context.Context { return context.Background() }

// Account is the account a suite delivers to.
func (h *Harness) Account() domain.ProviderAccount {
	a, _, _ := h.Accounts.Account(Ctx(), AccountID)
	h.Log.Reset()
	return a
}

// Providers is the adapter registry the ingress is given.
func (h *Harness) Providers() ports.Providers {
	return ports.Providers{ProviderName: h.Provider}
}

// Body encodes an event the way the fake provider sends it.
func Body(fe FakeEvent) []byte {
	b, err := json.Marshal(fe)
	if err != nil {
		panic(err)
	}
	return b
}

// Deliver signs an event for the account, now, and returns what the provider would POST.
func (h *Harness) Deliver(fe FakeEvent) ([]byte, http.Header) {
	b := Body(fe)
	return b, h.Provider.Sign(AccountID, b, h.Clock.Now())
}

// Verified is an event that has genuinely been through VerifyWebhook: normalized by the provider, not built by hand.
func (h *Harness) Verified(fe FakeEvent) domain.Event {
	body, hdr := h.Deliver(fe)
	e, err := h.Provider.VerifyWebhook(Ctx(), h.Account(), body, hdr)
	if err != nil {
		panic("harness: " + err.Error())
	}
	h.Log.Reset()
	return e
}

// Persisted puts an event in the inbox as the ingress would have, before the crash a test is about.
func (h *Harness) Persisted(fe FakeEvent) domain.Event {
	e := h.Verified(fe)
	h.Inbox.Add(e)
	return e
}

// Now is the harness clock as a provider timestamp.
func (h *Harness) Now() int64 { return h.Clock.Now().Unix() }

// PaymentSucceeded is a verified payment of the pack for cus_1: $10.00.
func (h *Harness) PaymentSucceeded(eventID, paymentID string) FakeEvent {
	return FakeEvent{ID: eventID, Type: string(domain.PaymentSucceeded), Created: h.Now(), Customer: "cus_1", Payment: paymentID,
		Price: PackPriceID, Amount: 1000, Currency: "USD"}
}

// ---------------------------------------------------------------- fixtures --

// ProviderFixtures is what ProviderContract needs from an adapter's test code: a way to make deliveries the
// provider would really sign, and the ones it would not. Every adapter brings its own, in its own scheme.
type ProviderFixtures interface {
	Account() domain.ProviderAccount
	OtherAccount() domain.ProviderAccount // the same provider, a secret of its own
	// Now is the instant the adapter under test believes it is.
	Now() time.Time
	// SignedPaymentSucceeded is a genuine payment_succeeded of $10.00 (1000 minor units, USD) for customer
	// cus_1, payment pay-1, price price_pack.
	SignedPaymentSucceeded() ([]byte, http.Header)
	// TamperedPaymentSucceeded is that delivery with its amount changed after signing.
	TamperedPaymentSucceeded() ([]byte, http.Header)
	// SignedAt is a genuine delivery whose signature carries the instant `at`.
	SignedAt(at time.Time) ([]byte, http.Header)
	// SignedWithSecretOf is a genuine delivery signed with another account's secret.
	SignedWithSecretOf(other domain.ProviderAccount) ([]byte, http.Header)
	// SignedCumulativeRefund is the event for a payment refunded `first` and then `second` more: the provider
	// reports how much it has refunded in total, first+second, or tells the adapter enough to know it.
	SignedCumulativeRefund(first, second int64) ([]byte, http.Header)
	// SignedUnknownType is a genuine delivery of a type nobody subscribed to.
	SignedUnknownType() ([]byte, http.Header)
	// Checkout is a valid checkout to create.
	Checkout() ports.CheckoutInput
}

// Fixtures is the fake provider's.
func (h *Harness) Fixtures() ProviderFixtures { return fakeFixtures{h} }

type fakeFixtures struct{ h *Harness }

func (f fakeFixtures) Account() domain.ProviderAccount { return f.h.Account() }
func (f fakeFixtures) OtherAccount() domain.ProviderAccount {
	a, _, _ := f.h.Accounts.Account(Ctx(), OtherAccountID)
	return a
}
func (f fakeFixtures) Now() time.Time { return f.h.Clock.Now() }

func (f fakeFixtures) signed(fe FakeEvent, secretOf string, at time.Time) ([]byte, http.Header) {
	b := Body(fe)
	return b, f.h.Provider.Sign(secretOf, b, at)
}

func (f fakeFixtures) SignedPaymentSucceeded() ([]byte, http.Header) {
	return f.signed(f.h.PaymentSucceeded("evt_ok", "pay-1"), AccountID, f.h.Clock.Now())
}

func (f fakeFixtures) TamperedPaymentSucceeded() ([]byte, http.Header) {
	b, hdr := f.SignedPaymentSucceeded()
	return []byte(strings.Replace(string(b), `"amount":1000`, `"amount":100000`, 1)), hdr
}

func (f fakeFixtures) SignedAt(at time.Time) ([]byte, http.Header) {
	fe := f.h.PaymentSucceeded("evt_at", "pay-1")
	fe.Created = at.Unix()
	return f.signed(fe, AccountID, at)
}

func (f fakeFixtures) SignedWithSecretOf(other domain.ProviderAccount) ([]byte, http.Header) {
	return f.signed(f.h.PaymentSucceeded("evt_other", "pay-1"), other.ID, f.h.Clock.Now())
}

func (f fakeFixtures) SignedCumulativeRefund(first, second int64) ([]byte, http.Header) {
	fe := FakeEvent{ID: "evt_refund", Type: string(domain.PaymentRefunded), Created: f.h.Now(), Customer: "cus_1", Payment: "pay-1",
		Refund: "re_2", RefundAmount: second, Refunded: first + second, Amount: 1000, Currency: "USD"}
	return f.signed(fe, AccountID, f.h.Clock.Now())
}

func (f fakeFixtures) SignedUnknownType() ([]byte, http.Header) {
	return f.signed(FakeEvent{ID: "evt_unknown", Type: "customer.created", Created: f.h.Now(), Customer: "cus_1"}, AccountID, f.h.Clock.Now())
}

func (f fakeFixtures) Checkout() ports.CheckoutInput {
	return ports.CheckoutInput{Scope: f.h.Scope, Mode: "payment", PlanCode: PackCode, Currency: "USD", ProviderPriceID: PackPriceID,
		BillingEmail: "buyer@example.test", SuccessURL: "https://app.example.test/ok", CancelURL: "https://app.example.test/no", IdemKey: "checkout-1"}
}
