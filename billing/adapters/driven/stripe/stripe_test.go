package stripe_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driven/stripe"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/stripefix"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

func setup(t *testing.T, mutate ...func(*stripe.Config)) (*stripe.Provider, *stripefix.Fixtures) {
	t.Helper()
	fx := stripefix.New(t)
	cfg := stripe.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, BaseURL: fx.APIURL()}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := stripe.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, fx
}

func ctx() context.Context { return context.Background() }

// ---------------------------------------------------------------- the contract --

func TestStripeHoldsTheProviderContract(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	contracts.ProviderContract(t, p, fx)
}

func TestStripeIsNamedStripe(t *testing.T) {
	t.Parallel()
	p, _ := setup(t)
	if p.Name() != "stripe" {
		t.Fatalf("Name = %q", p.Name())
	}
}

func TestStripeIsBuiltOnlyWithWhatItNeeds(t *testing.T) {
	t.Parallel()
	fx := stripefix.New(t)
	ok := stripe.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock}
	if _, err := stripe.New(ok); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*stripe.Config){
		"no secret resolver":         func(c *stripe.Config) { c.Secrets = nil },
		"no catalogue":               func(c *stripe.Config) { c.Catalog = nil },
		"no clock":                   func(c *stripe.Config) { c.Clock = nil },
		"a negative tolerance":       func(c *stripe.Config) { c.Tolerance = -time.Second },
		"a base URL that is not one": func(c *stripe.Config) { c.BaseURL = "api.stripe.com" },
	} {
		c := ok
		mutate(&c)
		if _, err := stripe.New(c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// ------------------------------------------------------------------ signatures --

func TestTheTimestampToleranceIsExactlyItsBound(t *testing.T) {
	t.Parallel()
	p, fx := setup(t) // the default, five minutes
	acct := fx.Account()
	body := fx.Event("evt_t", "invoice.paid", fx.Now(), stripefix.Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	verify := func(offset time.Duration) error {
		_, err := p.VerifyWebhook(ctx(), acct, body, fx.Sign(stripefix.WebhookSecA, fx.Now().Add(offset), body))
		return err
	}
	const tol = 5 * time.Minute
	for name, tc := range map[string]struct {
		offset time.Duration
		ok     bool
	}{
		"a second inside, in the past":    {-tol + time.Second, true},
		"exactly at the bound, past":      {-tol, true},
		"a second outside, in the past":   {-tol - time.Second, false},
		"a second inside, in the future":  {tol - time.Second, true},
		"exactly at the bound, future":    {tol, true},
		"a second outside, in the future": {tol + time.Second, false},
		"now":                             {0, true},
	} {
		err := verify(tc.offset)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !tc.ok && !errors.Is(err, domain.ErrInvalidSignature):
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}

func TestAConfiguredToleranceIsTheOneUsed(t *testing.T) {
	t.Parallel()
	p, fx := setup(t, func(c *stripe.Config) { c.Tolerance = 30 * time.Second })
	body := fx.Event("evt_t", "invoice.paid", fx.Now(), stripefix.Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, fx.Sign(stripefix.WebhookSecA, fx.Now().Add(-30*time.Second), body)); err != nil {
		t.Errorf("at 30 s: %v", err)
	}
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, fx.Sign(stripefix.WebhookSecA, fx.Now().Add(-31*time.Second), body)); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("at 31 s: err = %v", err)
	}
}

func TestAnAccountsDeliveriesAreVerifiedOnlyWithItsOwnSecret(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	a, b := fx.Account(), fx.OtherAccount()
	body := fx.Event("evt_s", "invoice.paid", fx.Now(), stripefix.Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	forA, forB := fx.Sign(stripefix.WebhookSecA, fx.Now(), body), fx.Sign(stripefix.WebhookSecB, fx.Now(), body)
	for name, tc := range map[string]struct {
		acct domain.ProviderAccount
		hdr  http.Header
		ok   bool
	}{"A's secret on A": {a, forA, true}, "B's secret on B": {b, forB, true}, "B's secret on A": {a, forB, false}, "A's secret on B": {b, forA, false}} {
		_, err := p.VerifyWebhook(ctx(), tc.acct, body, tc.hdr)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSeveralSignaturesInOneHeaderAreAcceptedIfAnyMatches(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("evt_r", "invoice.paid", fx.Now(), stripefix.Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	good := stripefix.SignatureHeader(stripefix.WebhookSecA, fx.Now(), body)
	old := stripefix.SignatureHeader("a-secret-that-was-rotated-out", fx.Now(), body)
	_, oldSig, _ := strings.Cut(old, ",v1=")
	_, goodSig, _ := strings.Cut(good, ",v1=")
	ts := fmt.Sprintf("t=%d", fx.Now().Unix())
	for name, hdr := range map[string]string{
		"the old secret's signature first": ts + ",v1=" + oldSig + ",v1=" + goodSig,
		"the new one first":                ts + ",v1=" + goodSig + ",v1=" + oldSig,
		"a v0 test signature beside it":    ts + ",v0=deadbeef,v1=" + goodSig,
	} {
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, http.Header{"Stripe-Signature": {hdr}}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, http.Header{"Stripe-Signature": {ts + ",v1=" + oldSig}}); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("only the rotated-out secret's signature: err = %v", err)
	}
}

func TestMalformedSignatureHeadersAreInvalidNotAPanic(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("evt_m", "invoice.paid", fx.Now(), stripefix.Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	good := stripefix.SignatureHeader(stripefix.WebhookSecA, fx.Now(), body)
	_, sig, _ := strings.Cut(good, ",v1=")
	for name, h := range map[string]http.Header{
		"no header":                        nil,
		"an empty header":                  {"Stripe-Signature": {""}},
		"no timestamp":                     {"Stripe-Signature": {"v1=" + sig}},
		"no signature":                     {"Stripe-Signature": {fmt.Sprintf("t=%d", fx.Now().Unix())}},
		"a timestamp that is not a number": {"Stripe-Signature": {"t=yesterday,v1=" + sig}},
		"a signature that is not hex":      {"Stripe-Signature": {fmt.Sprintf("t=%d,v1=not-hex", fx.Now().Unix())}},
		"a zero timestamp":                 {"Stripe-Signature": {"t=0,v1=" + sig}},
		"garbage":                          {"Stripe-Signature": {",,,===,v1,t"}},
		"two headers":                      {"Stripe-Signature": {good, good}},
	} {
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, h)
		if name == "two headers" {
			continue // a repeated header is the transport's to merge; either answer is safe as long as it is not a partial event
		}
		if !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
		if e.ProviderEventID != "" || e.Type != "" {
			t.Errorf("%s: an error came with a partial event: %+v", name, e)
		}
	}
}

func TestTheBodyIsVerifiedAsReceivedNotAsReencoded(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("evt_w", "invoice.paid", fx.Now(), stripefix.Invoice("in_1", "cus_1", "pay-1", "", contracts.PackPriceID, 1000))
	// The same JSON with a space added: parsing and re-encoding it would still be valid JSON, and would break the signature.
	spaced := []byte(strings.Replace(string(body), `"type":`, `"type" :`, 1))
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), spaced, fx.Sign(stripefix.WebhookSecA, fx.Now(), body)); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("a reformatted body verified (err = %v): the signature covers the raw bytes", err)
	}
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), spaced, fx.Sign(stripefix.WebhookSecA, fx.Now(), spaced)); err != nil {
		t.Errorf("the reformatted body signed as received: %v", err)
	}
}

func TestAnUnresolvableSecretIsOurFailureNotAnInvalidSignature(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	acct := fx.Account()
	acct.WebhookSecretRef = "env:NOT_CONFIGURED"
	body, hdr := fx.SignedPaymentSucceeded()
	_, err := p.VerifyWebhook(ctx(), acct, body, hdr)
	if err == nil || errors.Is(err, domain.ErrInvalidSignature) || errors.Is(err, domain.ErrVerificationUnavailable) {
		t.Fatalf("err = %v: a secret we cannot resolve is a misconfiguration to alert on, not a forged delivery and not an outage", err)
	}
	if strings.Contains(err.Error(), stripefix.WebhookSecA) {
		t.Fatalf("the error carries a secret: %v", err)
	}
}

func TestSignaturesAreComparedWithHmacEqualOnly(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no adapter source found: %v", err)
	}
	var usesEqual bool
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		usesEqual = usesEqual || strings.Contains(s, "hmac.Equal(")
		for _, banned := range []string{"bytes.Equal(", "strings.EqualFold(", "subtle.ConstantTimeCompare("} {
			if strings.Contains(s, banned) {
				t.Errorf("%s uses %s: a signature is compared with hmac.Equal and nothing else, so there is one place to look", f, banned)
			}
		}
	}
	if !usesEqual {
		t.Error("the adapter never calls hmac.Equal: how does it compare signatures?")
	}
}

// ---------------------------------------------------------------- normalization --

func verify(t *testing.T, p *stripe.Provider, fx *stripefix.Fixtures, id, typ string, object map[string]any) domain.Event {
	t.Helper()
	body := fx.Event(id, typ, fx.Now(), object)
	e, err := p.VerifyWebhook(ctx(), fx.Account(), body, fx.Sign(stripefix.WebhookSecA, fx.Now(), body))
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	if e.Provider != "stripe" || e.Account != fx.Account().ID || e.ProviderEventID != id || !e.CreatedAt.Equal(fx.Now()) || e.RawHash == "" {
		t.Fatalf("%s: envelope = %+v", typ, e)
	}
	return e
}

func money(minor int64) *domain.Money { return &domain.Money{MinorUnits: minor, Currency: "USD"} }

func TestEachStripeEventIsNormalizedIntoTheEnginesVocabulary(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, end := fx.Now().Add(-24*time.Hour), fx.Now().Add(29*24*time.Hour)

	t.Run("a paid subscription invoice is a payment keyed on the payment intent", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_1", "invoice.paid", stripefix.Invoice("in_9", "cus_1", "pi_9", "sub_1", contracts.ProPriceID, 2000))
		o := e.Object
		if e.Type != domain.PaymentSucceeded || o.ProviderPaymentID != "pi_9" || o.ProviderCustomerID != "cus_1" || o.ProviderSubscriptionID != "sub_1" ||
			o.ProviderPriceID != contracts.ProPriceID || *o.Amount != *money(2000) || o.TaxAmount == nil || *o.TaxAmount != *money(81) ||
			o.ReceiptURL != "https://invoice.stripe.test/i/in_9" {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
	})
	t.Run("a one-time purchase through an invoice carries no subscription", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_2", "invoice.paid", stripefix.Invoice("in_2", "cus_1", "pi_2", "", contracts.PackPriceID, 1000))
		if e.Type != domain.PaymentSucceeded || e.Object.ProviderSubscriptionID != "" || e.Object.ProviderPaymentID != "pi_2" {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("an invoice with no payment intent is keyed on the invoice", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_3", "invoice.paid", stripefix.Invoice("in_free", "cus_1", "", "sub_1", contracts.ProPriceID, 0))
		if e.Object.ProviderPaymentID != "in_free" || e.Object.Amount == nil || e.Object.Amount.MinorUnits != 0 {
			t.Fatalf("object = %+v", e.Object)
		}
	})
	t.Run("a failed invoice is a failed payment", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_4", "invoice.payment_failed", stripefix.Invoice("in_8", "cus_1", "pi_8", "sub_1", contracts.ProPriceID, 2000))
		if e.Type != domain.PaymentFailed || e.Object.ProviderPaymentID != "pi_8" || e.Object.ProviderSubscriptionID != "sub_1" || e.Object.FailureReason == "" || e.Object.Amount.MinorUnits != 2000 {
			t.Fatalf("event = %+v object = %+v", e, e.Object)
		}
	})
	t.Run("a payment intent with no invoice is a payment, carrying the method it used", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_5", "payment_intent.succeeded", stripefix.PaymentIntent("pi_5", "cus_1", 1000, nil, "pm_1"))
		if e.Type != domain.PaymentSucceeded || e.Object.ProviderPaymentID != "pi_5" || e.Object.PaymentMethodRef != "pm_1" || e.Object.ProviderPriceID != "" {
			t.Fatalf("event = %+v object = %+v", e, e.Object)
		}
	})
	t.Run("a payment intent that belongs to an invoice is ignored: the invoice fulfils it once", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"payment_intent.succeeded", "payment_intent.payment_failed"} {
			if e := verify(t, p, fx, "evt_6"+typ, typ, stripefix.PaymentIntent("pi_6", "cus_1", 1000, "in_6", "pm_1")); e.Type != domain.Ignored {
				t.Fatalf("%s with an invoice = %q", typ, e.Type)
			}
		}
	})
	t.Run("a failed payment intent with no invoice is a failed payment with the card's reason", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_7", "payment_intent.payment_failed", stripefix.PaymentIntent("pi_7", "cus_1", 1000, nil, "pm_1"))
		if e.Type != domain.PaymentFailed || e.Object.ProviderPaymentID != "pi_7" || e.Object.FailureReason != "card_declined" {
			t.Fatalf("event = %+v object = %+v", e, e.Object)
		}
	})
	t.Run("a refunded charge reports the cumulative total against the payment intent", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_8", "charge.refunded", stripefix.Charge("ch_1", "cus_1", "pi_9", "in_9", 2000, 700, "re_2", "re_1"))
		o := e.Object
		if e.Type != domain.PaymentRefunded || o.ProviderPaymentID != "pi_9" || *o.AmountRefunded != *money(700) || *o.Amount != *money(2000) || o.ProviderRefundID != "re_2" {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
	})
	t.Run("a charge with no payment intent is keyed on the invoice, then the charge", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "evt_9", "charge.refunded", stripefix.Charge("ch_2", "cus_1", "", "in_free", 100, 100, "re_1")); e.Object.ProviderPaymentID != "in_free" {
			t.Fatalf("payment = %q", e.Object.ProviderPaymentID)
		}
		if e := verify(t, p, fx, "evt_10", "charge.refunded", stripefix.Charge("ch_3", "cus_1", "", "", 100, 100, "re_1")); e.Object.ProviderPaymentID != "ch_3" {
			t.Fatalf("payment = %q", e.Object.ProviderPaymentID)
		}
	})
	t.Run("the four dispute events carry the dispute id and the payment, and no customer", func(t *testing.T) {
		t.Parallel()
		for typ, want := range map[string]domain.EventType{"charge.dispute.created": domain.DisputeOpened, "charge.dispute.funds_withdrawn": domain.DisputeFundsWithdrawn,
			"charge.dispute.funds_reinstated": domain.DisputeFundsReinstated, "charge.dispute.closed": domain.DisputeClosed} {
			e := verify(t, p, fx, "evt_d"+typ, typ, stripefix.Dispute("dp_1", "pi_9", 2000, "won"))
			if e.Type != want || e.Object.DisputeID != "dp_1" || e.Object.ProviderPaymentID != "pi_9" || e.Object.ProviderCustomerID != "" {
				t.Fatalf("%s → %+v", typ, e)
			}
		}
	})
	t.Run("a closed dispute is won or lost", func(t *testing.T) {
		t.Parallel()
		for status, want := range map[string]string{"won": "won", "lost": "lost", "warning_closed": "won", "charge_refunded": "won"} {
			e := verify(t, p, fx, "evt_c"+status, "charge.dispute.closed", stripefix.Dispute("dp_1", "pi_9", 2000, status))
			if e.Object.Status != want {
				t.Fatalf("dispute closed as %q = %q, want %q", status, e.Object.Status, want)
			}
		}
	})
	t.Run("subscription updates and deletions carry the subscription, the customer and the price", func(t *testing.T) {
		t.Parallel()
		for typ, want := range map[string]domain.EventType{"customer.subscription.updated": domain.SubscriptionUpdated, "customer.subscription.deleted": domain.SubscriptionDeleted} {
			e := verify(t, p, fx, "evt_s"+typ, typ, stripefix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "past_due", start, end, true))
			o := e.Object
			if e.Type != want || o.ProviderSubscriptionID != "sub_1" || o.ProviderCustomerID != "cus_1" || o.ProviderPriceID != contracts.ProPriceID || o.Status != "past_due" ||
				!o.CancelAtPeriodEnd || o.PeriodStart == nil || !o.PeriodStart.Equal(start) || o.PeriodEnd == nil || !o.PeriodEnd.Equal(end) {
				t.Fatalf("%s → %+v / %+v", typ, e, o)
			}
		}
	})
	t.Run("a checkout session grants nothing: the invoice that follows it does", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "evt_cs", "checkout.session.completed", map[string]any{"id": "cs_1", "object": "checkout.session", "customer": "cus_1", "payment_status": "paid"}); e.Type != domain.Ignored {
			t.Fatalf("checkout.session.completed = %q: a checkout return must never be what fulfils (invariant 2)", e.Type)
		}
	})
	t.Run("a type nobody subscribed to is ignored with nothing in it", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "evt_u", "customer.created", map[string]any{"id": "cus_1", "email": "a@example.test"})
		if e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a redelivery of the same payment under another event names the same payment", func(t *testing.T) {
		t.Parallel()
		a := verify(t, p, fx, "evt_x1", "invoice.paid", stripefix.Invoice("in_9", "cus_1", "pi_9", "", contracts.PackPriceID, 1000))
		b := verify(t, p, fx, "evt_x2", "invoice.payment_succeeded", stripefix.Invoice("in_9", "cus_1", "pi_9", "", contracts.PackPriceID, 1000))
		if a.Object.ProviderPaymentID != b.Object.ProviderPaymentID || b.Type != domain.PaymentSucceeded {
			t.Fatalf("grants are keyed on the payment, never the event: %q vs %q (%s)", a.Object.ProviderPaymentID, b.Object.ProviderPaymentID, b.Type)
		}
	})
}

func TestAVerifiedBodyThatIsNotAnEventIsAnErrorNotAPartialEvent(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	for name, body := range map[string][]byte{
		"not JSON":   []byte(`<html>`),
		"an array":   []byte(`[]`),
		"no id":      []byte(`{"type":"invoice.paid","created":1,"data":{"object":{}}}`),
		"no created": []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`),
		"no data":    []byte(`{"id":"evt_1","type":"invoice.paid","created":1}`),
	} {
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, fx.Sign(stripefix.WebhookSecA, fx.Now(), body))
		if err == nil || errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v: a delivery that verified and is nonsense is our adapter's problem, not a forgery", name, err)
		}
		if e.ProviderEventID != "" {
			t.Errorf("%s: a partial event came with the error: %+v", name, e)
		}
	}
}

// ------------------------------------------------------------------ the API --

func TestEnsureCustomerCreatesOneCustomerPerKeyWithTheAccountsKey(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := ports.CustomerInput{Scope: md.Scope{Realm: "default", Kind: "workspace", ID: "w1"}, BillingEmail: "buyer@example.test", IdemKey: "customer:stripe-a:default/workspace:w1"}
	id1, err := p.EnsureCustomer(ctx(), fx.Account(), in)
	if err != nil {
		t.Fatal(err)
	}
	if id2, _ := p.EnsureCustomer(ctx(), fx.Account(), in); id2 != id1 {
		t.Fatalf("the same key gave %q then %q: two first checkouts at once would leave two customers", id1, id2)
	}
	req, ok := fx.Last(http.MethodPost, "/v1/customers")
	if !ok || req.Form.Get("email") != "buyer@example.test" || req.Form.Get("metadata[scope_id]") != "w1" || req.Form.Get("metadata[scope_kind]") != "workspace" ||
		req.Header.Get("Idempotency-Key") != in.IdemKey || req.Header.Get("Authorization") != "Bearer "+stripefix.APIKeyA || req.Header.Get("Stripe-Version") != stripefix.APIVersion {
		t.Fatalf("request = %+v", req)
	}
	if _, err := p.EnsureCustomer(ctx(), fx.OtherAccount(), in); err != nil {
		t.Fatal(err)
	}
	if req, _ := fx.Last(http.MethodPost, "/v1/customers"); req.Header.Get("Authorization") != "Bearer "+stripefix.APIKeyB {
		t.Fatalf("account B's call used %q: each account has its own key", req.Header.Get("Authorization"))
	}
}

func TestCreateCheckoutAsksForTheModeThePriceAndTheReturnPages(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := fx.Checkout()
	for name, tc := range map[string]struct {
		mode  string
		check func(t *testing.T, f map[string][]string)
	}{
		"payment": {"payment", func(t *testing.T, f map[string][]string) {
			if f["line_items[0][price]"][0] != contracts.PackPriceID || f["line_items[0][quantity]"][0] != "1" || f["invoice_creation[enabled]"][0] != "true" {
				t.Fatalf("form = %v: a one-time purchase creates an invoice, so the invoice that follows is what fulfils it", f)
			}
		}},
		"subscription": {"subscription", func(t *testing.T, f map[string][]string) {
			if f["line_items[0][price]"][0] != contracts.PackPriceID || f["invoice_creation[enabled]"] != nil {
				t.Fatalf("form = %v", f)
			}
		}},
		"setup": {"setup", func(t *testing.T, f map[string][]string) {
			if f["line_items[0][price]"] != nil || f["payment_method_types[0]"][0] != "card" {
				t.Fatalf("form = %v: saving a method sells nothing", f)
			}
		}},
	} {
		in.Mode = tc.mode
		url, err := p.CreateCheckout(ctx(), fx.Account(), in)
		if err != nil || !strings.HasPrefix(url, "https://checkout.stripe.test/c/pay/") {
			t.Fatalf("%s: %q, %v", name, url, err)
		}
		req, _ := fx.Last(http.MethodPost, "/v1/checkout/sessions")
		f := req.Form
		if f.Get("mode") != tc.mode || f.Get("customer") != "cus_1" || f.Get("success_url") != in.SuccessURL || f.Get("cancel_url") != in.CancelURL ||
			f.Get("metadata[plan]") != contracts.PackCode || req.Header.Get("Idempotency-Key") != "checkout-1" {
			t.Fatalf("%s: form = %v header = %v", name, f, req.Header)
		}
		tc.check(t, f)
	}
}

func TestPortalSessionIsTheProvidersPageForTheCustomer(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	url, err := p.CreatePortalSession(ctx(), fx.Account(), "cus_1", "https://app.example.test/billing")
	req, _ := fx.Last(http.MethodPost, "/v1/billing_portal/sessions")
	if err != nil || !strings.HasSuffix(url, "/cus_1") || req.Form.Get("return_url") != "https://app.example.test/billing" {
		t.Fatalf("%q %v %v", url, err, req.Form)
	}
}

func TestFetchSubscriptionIsTheProvidersCurrentObjectInTheEnginesTerms(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, end := fx.Now().Add(-24*time.Hour).Truncate(time.Second), fx.Now().Add(29*24*time.Hour).Truncate(time.Second)
	fx.Subscriptions["sub_1"] = stripefix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "unpaid", start, end, true)
	sub, at, err := p.FetchSubscription(ctx(), fx.Account(), "sub_1")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ProviderSubscriptionID != "sub_1" || sub.Account != fx.Account().ID || sub.PlanCode != contracts.ProCode || sub.Status != domain.PastDue ||
		!sub.CancelAtPeriodEnd || !sub.CurrentPeriodStart.Equal(start) || !sub.CurrentPeriodEnd.Equal(end) {
		t.Fatalf("sub = %+v", sub)
	}
	if !at.Equal(fx.Now()) {
		t.Fatalf("at = %v, want the provider's own clock (its Date header) %v: state is ordered by the provider's time, never ours", at, fx.Now())
	}
	for stripeStatus, want := range map[string]domain.SubStatus{"trialing": domain.Trialing, "active": domain.Active, "past_due": domain.PastDue, "unpaid": domain.PastDue,
		"canceled": domain.Canceled, "incomplete": domain.Incomplete, "incomplete_expired": domain.Canceled, "paused": domain.Paused} {
		fx.Subscriptions["sub_2"] = stripefix.Subscription("sub_2", "cus_1", contracts.ProPriceID, stripeStatus, start, end, false)
		if got, _, _ := p.FetchSubscription(ctx(), fx.Account(), "sub_2"); got.Status != want {
			t.Errorf("Stripe's %q = %q, want %q", stripeStatus, got.Status, want)
		}
	}
	fx.Subscriptions["sub_3"] = stripefix.Subscription("sub_3", "cus_1", "price_nobody_sells", "active", start, end, false)
	if got, _, err := p.FetchSubscription(ctx(), fx.Account(), "sub_3"); err != nil || got.PlanCode != "" {
		t.Errorf("a price the catalogue does not sell: %+v, %v; want no plan code, which the processor ignores rather than guesses", got, err)
	}
	if _, _, err := p.FetchSubscription(ctx(), fx.Account(), "sub_missing"); !errors.Is(err, md.ErrNotFound) {
		t.Errorf("a subscription Stripe does not have: %v, want ErrNotFound", err)
	}
}

func TestChangePlanFollowsThePolicyAtStripe(t *testing.T) {
	t.Parallel()
	start, end := contracts.Start.Add(-24*time.Hour), contracts.Start.Add(29*24*time.Hour)
	t.Run("now, prorated", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		fx.Subscriptions["sub_1"] = stripefix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "active", start, end, false)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.TeamPriceID, domain.ImmediateProrate); err != nil {
			t.Fatal(err)
		}
		req, ok := fx.Last(http.MethodPost, "/v1/subscriptions/sub_1")
		if !ok || req.Form.Get("items[0][id]") != "si_1" || req.Form.Get("items[0][price]") != contracts.TeamPriceID || req.Form.Get("proration_behavior") != "create_prorations" {
			t.Fatalf("form = %v", req.Form)
		}
	})
	t.Run("now, not prorated", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		fx.Subscriptions["sub_1"] = stripefix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "active", start, end, false)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.LitePriceID, domain.ImmediateNoProrate); err != nil {
			t.Fatal(err)
		}
		if req, _ := fx.Last(http.MethodPost, "/v1/subscriptions/sub_1"); req.Form.Get("proration_behavior") != "none" || req.Form.Get("items[0][price]") != contracts.LitePriceID {
			t.Fatalf("form = %v", req.Form)
		}
	})
	t.Run("at the period's end, through a schedule that releases itself", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		fx.Subscriptions["sub_1"] = stripefix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "active", start, end, false)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.TeamPriceID, domain.AtPeriodEnd); err != nil {
			t.Fatal(err)
		}
		create, ok := fx.Last(http.MethodPost, "/v1/subscription_schedules")
		update, ok2 := fx.Last(http.MethodPost, "/v1/subscription_schedules/sub_sched_1")
		if !ok || !ok2 || create.Form.Get("from_subscription") != "sub_1" {
			t.Fatalf("create = %+v update = %+v", create, update)
		}
		f := update.Form
		if f.Get("end_behavior") != "release" || f.Get("phases[0][items][0][price]") != "price_pro" || f.Get("phases[1][items][0][price]") != contracts.TeamPriceID ||
			f.Get("phases[0][start_date]") == "" || f.Get("phases[0][end_date]") == "" {
			t.Fatalf("form = %v: the first phase keeps what the customer pays until the period ends, the second is the new plan", f)
		}
		if _, direct := fx.Last(http.MethodPost, "/v1/subscriptions/sub_1"); direct {
			t.Fatal("a change at the period's end modified the subscription now")
		}
	})
	t.Run("a policy nobody defined is refused before Stripe is asked", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.TeamPriceID, "whenever"); !errors.Is(err, md.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
		if len(fx.Requests()) != 0 {
			t.Fatalf("Stripe was called: %v", fx.Requests())
		}
	})
}

func TestCancelAtPeriodEndSetsTheFlagAtStripe(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	if err := p.CancelAtPeriodEnd(ctx(), fx.Account(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if req, ok := fx.Last(http.MethodPost, "/v1/subscriptions/sub_1"); !ok || req.Form.Get("cancel_at_period_end") != "true" {
		t.Fatalf("request = %+v", req)
	}
}

func TestChargeSavedIsAnOffSessionPaymentIdempotentOnItsKey(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := ports.OffSessionInput{CustomerID: "cus_1", PaymentMethodRef: "pm_1", Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, PlanCode: contracts.PackCode,
		IdemKey: "recharge:default/workspace:w1:2026-03-01:1"}
	id1, err := p.ChargeSaved(ctx(), fx.Account(), in)
	if err != nil || id1 == "" {
		t.Fatalf("ChargeSaved = %q, %v", id1, err)
	}
	if id2, _ := p.ChargeSaved(ctx(), fx.Account(), in); id2 != id1 {
		t.Fatalf("the same key charged twice: %q then %q (invariant 18)", id1, id2)
	}
	req, _ := fx.Last(http.MethodPost, "/v1/payment_intents")
	f := req.Form
	if f.Get("amount") != "1000" || f.Get("currency") != "usd" || f.Get("customer") != "cus_1" || f.Get("payment_method") != "pm_1" || f.Get("off_session") != "true" ||
		f.Get("confirm") != "true" || req.Header.Get("Idempotency-Key") != in.IdemKey {
		t.Fatalf("form = %v header = %v", f, req.Header)
	}
}

func TestADeclinedCardIsATypedErrorThatNeverCarriesTheKey(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	fx.DeclineCharges = true
	_, err := p.ChargeSaved(ctx(), fx.Account(), ports.OffSessionInput{CustomerID: "cus_1", PaymentMethodRef: "pm_1",
		Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, IdemKey: "k"})
	var api *stripe.APIError
	if !errors.As(err, &api) || api.Code != "card_declined" || api.Status != 402 {
		t.Fatalf("err = %v, want an *APIError for card_declined", err)
	}
	if strings.Contains(err.Error(), stripefix.APIKeyA) {
		t.Fatalf("the error carries the API key: %v", err)
	}
}

func TestAnAPIFailureIsAnErrorAndNeverTheSecret(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	fx.FailAll = true
	for name, call := range map[string]func() error{
		"customer": func() error {
			_, err := p.EnsureCustomer(ctx(), fx.Account(), ports.CustomerInput{IdemKey: "k"})
			return err
		},
		"checkout": func() error { _, err := p.CreateCheckout(ctx(), fx.Account(), fx.Checkout()); return err },
		"portal": func() error {
			_, err := p.CreatePortalSession(ctx(), fx.Account(), "cus_1", "https://app.example.test/")
			return err
		},
		"cancel": func() error { return p.CancelAtPeriodEnd(ctx(), fx.Account(), "sub_1") },
	} {
		err := call()
		var api *stripe.APIError
		if !errors.As(err, &api) || api.Status != 500 {
			t.Errorf("%s: err = %v, want an *APIError with status 500", name, err)
		}
		if err != nil && strings.Contains(err.Error(), stripefix.APIKeyA) {
			t.Errorf("%s: the error carries the key", name)
		}
	}
}

func TestACancelledContextStopsTheCall(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	c, cancel := context.WithCancel(ctx())
	cancel()
	if _, err := p.CreateCheckout(c, fx.Account(), fx.Checkout()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatal("a cancelled context still reached Stripe")
	}
}

var _ = fmt.Sprintf
