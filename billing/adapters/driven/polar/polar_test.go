package polar_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driven/polar"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/polarfix"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

func setup(t *testing.T, mutate ...func(*polar.Config)) (*polar.Provider, *polarfix.Fixtures) {
	t.Helper()
	fx := polarfix.New(t)
	cfg := polar.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, BaseURL: fx.APIURL()}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := polar.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, fx
}

func ctx() context.Context { return context.Background() }

// fixedSecrets resolves every reference to the value it is given, so a test can hold the adapter to a secret of any shape.
type fixedSecrets map[string]string

func (m fixedSecrets) Resolve(_ context.Context, ref string) (string, error) {
	if v, ok := m[ref]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no secret behind %q", ref)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------- the contract --

func TestPolarHoldsTheProviderContract(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	contracts.ProviderContract(t, p, fx)
}

func TestPolarIsNamedPolar(t *testing.T) {
	t.Parallel()
	p, _ := setup(t)
	if p.Name() != "polar" {
		t.Fatalf("Name = %q", p.Name())
	}
}

func TestPolarIsBuiltOnlyWithWhatItNeeds(t *testing.T) {
	t.Parallel()
	fx := polarfix.New(t)
	ok := polar.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock}
	if _, err := polar.New(ok); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*polar.Config){
		"no secret resolver":         func(c *polar.Config) { c.Secrets = nil },
		"no catalogue":               func(c *polar.Config) { c.Catalog = nil },
		"no clock":                   func(c *polar.Config) { c.Clock = nil },
		"a negative tolerance":       func(c *polar.Config) { c.Tolerance = -time.Second },
		"a base URL that is not one": func(c *polar.Config) { c.BaseURL = "api.polar.sh" },
	} {
		c := ok
		mutate(&c)
		if _, err := polar.New(c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// ------------------------------------------------------------------ signatures --

func paidOrder(id string) map[string]any {
	return polarfix.Order(id, "cus_1", contracts.PackPriceID, 1000, "paid", "")
}

func TestTheTimestampToleranceIsExactlyItsBound(t *testing.T) {
	t.Parallel()
	p, fx := setup(t) // the default, five minutes
	acct := fx.Account()
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	verify := func(offset time.Duration) error {
		_, err := p.VerifyWebhook(ctx(), acct, body, polarfix.Sign(polarfix.WebhookSecA, "msg_t", fx.Now().Add(offset), body))
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
	p, fx := setup(t, func(c *polar.Config) { c.Tolerance = 30 * time.Second })
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_t", fx.Now().Add(-30*time.Second), body)); err != nil {
		t.Errorf("at 30 s: %v", err)
	}
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_t", fx.Now().Add(-31*time.Second), body)); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("at 31 s: err = %v", err)
	}
}

func TestAnAccountsDeliveriesAreVerifiedOnlyWithItsOwnSecret(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	a, b := fx.Account(), fx.OtherAccount()
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	forA, forB := polarfix.Sign(polarfix.WebhookSecA, "msg_s", fx.Now(), body), polarfix.Sign(polarfix.WebhookSecB, "msg_s", fx.Now(), body)
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

// Polar signs with the Standard Webhooks key for a secret made on or after 8 September 2026, and with the UTF-8 bytes of
// the whole `whsec_…` string for an older one. The secret in the dashboard does not say which it is, so both are accepted.
func TestASecretIsAcceptedUnderBothOfPolarsKeys(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	for name, hdr := range map[string]http.Header{
		"the Standard Webhooks key": polarfix.Sign(polarfix.WebhookSecA, "msg_k", fx.Now(), body),
		"the older Polar key":       polarfix.SignLegacy(polarfix.WebhookSecA, "msg_k", fx.Now(), body),
	} {
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Neither key of ANOTHER secret verifies, and neither does a key that is only the secret's prefix or its base64 text.
	for name, hdr := range map[string]http.Header{
		"the other secret, Standard": polarfix.Sign(polarfix.WebhookSecB, "msg_k", fx.Now(), body),
		"the other secret, older":    polarfix.SignLegacy(polarfix.WebhookSecB, "msg_k", fx.Now(), body),
		"the base64 text without the prefix": polarfix.Headers("msg_k", fx.Now(),
			polarfix.SignatureWith([]byte(strings.TrimPrefix(polarfix.WebhookSecA, "whsec_")), "msg_k", fx.Now(), body)),
		"an empty key": polarfix.Headers("msg_k", fx.Now(), polarfix.SignatureWith(nil, "msg_k", fx.Now(), body)),
	} {
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, hdr); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}

func TestASecretThatIsNotInPolarsFormIsUsedAsItIsAndNeverPanics(t *testing.T) {
	t.Parallel()
	for name, secret := range map[string]string{"no prefix": "plain-secret", "a prefix and not base64": "whsec_!!!not-base64!!!", "only the prefix": "whsec_"} {
		secrets := fixedSecrets{"env:POLAR_WEBHOOK_SECRET_A": secret}
		p, fx := setup(t, func(c *polar.Config) { c.Secrets = secrets })
		body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.SignLegacy(secret, "msg_k", fx.Now(), body)); err != nil {
			t.Errorf("%s: a delivery signed with the secret's own bytes: %v", name, err)
		}
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.SignLegacy("another-secret", "msg_k", fx.Now(), body)); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: another secret's signature: err = %v", name, err)
		}
		// A secret that decodes to no key must not become a key anyone can sign with.
		empty := polarfix.Headers("msg_k", fx.Now(), polarfix.SignatureWith(nil, "msg_k", fx.Now(), body))
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, empty); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: a signature under an empty key: err = %v", name, err)
		}
	}
}

func TestSeveralSignaturesInOneHeaderAreAcceptedIfAnyMatches(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	good := polarfix.SignatureWith(polarfix.StandardKey(polarfix.WebhookSecA), "msg_r", fx.Now(), body)
	old := polarfix.SignatureWith(polarfix.StandardKey(polarfix.WebhookSecB), "msg_r", fx.Now(), body)
	_, goodSig, _ := strings.Cut(good, ",")
	for name, sigs := range map[string][]string{
		"the rotated-out secret's signature first": {old, good},
		"the new one first":                        {good, old},
		"an unknown version beside it":             {"v2," + goodSig, good},
		"an asymmetric signature beside it":        {"v1a," + goodSig, good},
	} {
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Headers("msg_r", fx.Now(), sigs...)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, sigs := range map[string][]string{
		"only the rotated-out secret's":       {old},
		"only a signature of another version": {"v1a," + goodSig},
		"the right bytes under no version":    {goodSig},
	} {
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Headers("msg_r", fx.Now(), sigs...)); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}

func TestMalformedSignatureHeadersAreInvalidNotAPanic(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	good := polarfix.Sign(polarfix.WebhookSecA, "msg_m", fx.Now(), body)
	with := func(k, v string) http.Header {
		h := good.Clone()
		if v == "" {
			h.Del(k)
		} else {
			h.Set(k, v)
		}
		return h
	}
	for name, h := range map[string]http.Header{
		"no headers":                       nil,
		"no message id":                    with("webhook-id", ""),
		"no timestamp":                     with("webhook-timestamp", ""),
		"no signature":                     with("webhook-signature", ""),
		"a timestamp that is not a number": with("webhook-timestamp", "yesterday"),
		"a zero timestamp":                 with("webhook-timestamp", "0"),
		"a timestamp with a fraction":      with("webhook-timestamp", fmt.Sprintf("%d.5", fx.Now().Unix())),
		"a signature that is not base64":   with("webhook-signature", "v1,!!!"),
		"a version with no signature":      with("webhook-signature", "v1,"),
		"a signature with no version":      with("webhook-signature", "v1"),
		"garbage":                          with("webhook-signature", ",,, v1,, ===  v1"),
	} {
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, h)
		if !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
		if e.ProviderEventID != "" || e.Type != "" {
			t.Errorf("%s: an error came with a partial event: %+v", name, e)
		}
	}
}

// The signature covers `id.timestamp.body`: a captured delivery cannot be replayed under another message id (which
// would make it a new event to the inbox) or another time (which would make it fresh).
func TestTheMessageIdAndTheTimestampAreCoveredByTheSignature(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	good := polarfix.Sign(polarfix.WebhookSecA, "msg_a", fx.Now(), body)
	otherID := good.Clone()
	otherID.Set("webhook-id", "msg_b")
	otherTime := good.Clone()
	otherTime.Set("webhook-timestamp", fmt.Sprint(fx.Now().Add(time.Second).Unix()))
	for name, h := range map[string]http.Header{"another message id": otherID, "another timestamp": otherTime} {
		if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, h); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}

func TestTheBodyIsVerifiedAsReceivedNotAsReencoded(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := fx.Event("order.paid", fx.Now(), paidOrder("pay-1"))
	// The same JSON with a space added: parsing and re-encoding it would still be valid JSON, and would break the signature.
	spaced := []byte(strings.Replace(string(body), `"type":`, `"type" :`, 1))
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), spaced, polarfix.Sign(polarfix.WebhookSecA, "msg_w", fx.Now(), body)); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("a reformatted body verified (err = %v): the signature covers the raw bytes", err)
	}
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), spaced, polarfix.Sign(polarfix.WebhookSecA, "msg_w", fx.Now(), spaced)); err != nil {
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
	if strings.Contains(err.Error(), polarfix.WebhookSecA) {
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

func verify(t *testing.T, p *polar.Provider, fx *polarfix.Fixtures, id, typ string, data map[string]any) domain.Event {
	t.Helper()
	body := fx.Event(typ, fx.Now(), data)
	e, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, id, fx.Now(), body))
	if err != nil {
		t.Fatalf("%s: %v", typ, err)
	}
	if e.Provider != "polar" || e.Account != fx.Account().ID || e.ProviderEventID != id || !e.CreatedAt.Equal(fx.Now()) || e.RawHash == "" {
		t.Fatalf("%s: envelope = %+v", typ, e)
	}
	return e
}

func money(minor int64) *domain.Money { return &domain.Money{MinorUnits: minor, Currency: "USD"} }

func TestEachPolarEventIsNormalizedIntoTheEnginesVocabulary(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, end := fx.Now().Add(-24*time.Hour), fx.Now().Add(29*24*time.Hour)
	order := func(id string, total int64, status, sub string) map[string]any {
		return polarfix.Order(id, "cus_1", contracts.PackPriceID, total, status, sub)
	}

	t.Run("a paid order is a payment keyed on the order, with what the customer was charged and the tax in it", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "msg_1", "order.paid", order("ord_9", 1000, "paid", ""))
		o := e.Object
		if e.Type != domain.PaymentSucceeded || o.ProviderPaymentID != "ord_9" || o.ProviderCustomerID != "cus_1" || o.ProviderSubscriptionID != "" ||
			o.ProviderPriceID != contracts.PackPriceID || o.Amount == nil || *o.Amount != *money(1000) || o.TaxAmount == nil || *o.TaxAmount != *money(81) {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
	})
	t.Run("a subscription's renewal is a payment that names the subscription", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "msg_2", "order.paid", polarfix.Order("ord_2", "cus_1", contracts.ProPriceID, 2000, "paid", "sub_1"))
		o := e.Object
		if e.Type != domain.PaymentSucceeded || o.ProviderSubscriptionID != "sub_1" || o.ProviderPriceID != contracts.ProPriceID || o.Amount.MinorUnits != 2000 {
			t.Fatalf("event = %+v object = %+v", e, o)
		}
	})
	t.Run("an order created already paid is the same payment as the order.paid that follows it", func(t *testing.T) {
		t.Parallel()
		a := verify(t, p, fx, "msg_3a", "order.created", order("ord_3", 1000, "paid", ""))
		b := verify(t, p, fx, "msg_3b", "order.paid", order("ord_3", 1000, "paid", ""))
		if a.Type != domain.PaymentSucceeded || b.Type != domain.PaymentSucceeded || a.Object.ProviderPaymentID != b.Object.ProviderPaymentID || a.Object.ProviderPaymentID != "ord_3" {
			t.Fatalf("grants are keyed on the payment, never the event: %+v vs %+v", a, b)
		}
	})
	t.Run("an order created and not yet paid grants nothing", func(t *testing.T) {
		t.Parallel()
		for _, status := range []string{"pending", "draft", "void"} {
			if e := verify(t, p, fx, "msg_4"+status, "order.created", order("ord_4", 2000, status, "sub_1")); e.Type != domain.Ignored {
				t.Fatalf("an unpaid (%s) order created = %q", status, e.Type)
			}
		}
	})
	t.Run("an order.updated that refunds nothing is ignored: order.paid already fulfilled it", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "msg_5", "order.updated", order("ord_5", 1000, "paid", "")); e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a refunded order reports what it has been refunded to date: the money and its tax together", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "msg_6", "order.refunded", polarfix.Refunded(order("ord_6", 1000, "partially_refunded", ""), 460, 40))
		o := e.Object
		if e.Type != domain.PaymentRefunded || o.ProviderPaymentID != "ord_6" || o.AmountRefunded == nil || *o.AmountRefunded != *money(500) || *o.Amount != *money(1000) {
			t.Fatalf("event = %+v object = %+v: refunded_amount is before tax and refunded_tax_amount is the tax; the customer got back both", e, o)
		}
	})
	t.Run("a refund that reaches the order as an update is still a refund", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "msg_7", "order.updated", polarfix.Refunded(order("ord_7", 1000, "partially_refunded", ""), 230, 20))
		if e.Type != domain.PaymentRefunded || *e.Object.AmountRefunded != *money(250) {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a fully refunded order is a refund of the whole", func(t *testing.T) {
		t.Parallel()
		e := verify(t, p, fx, "msg_8", "order.refunded", polarfix.Refunded(order("ord_8", 1000, "refunded", ""), 919, 81))
		if e.Type != domain.PaymentRefunded || *e.Object.AmountRefunded != *money(1000) {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("a refund event that refunded nothing revokes nothing", func(t *testing.T) {
		t.Parallel()
		if e := verify(t, p, fx, "msg_9", "order.refunded", order("ord_9", 1000, "paid", "")); e.Type != domain.Ignored {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("an order.paid whose order is not paid is an error, never a grant", func(t *testing.T) {
		t.Parallel()
		body := fx.Event("order.paid", fx.Now(), order("ord_10", 1000, "pending", ""))
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_10", fx.Now(), body))
		if err == nil || errors.Is(err, domain.ErrInvalidSignature) || e.ProviderEventID != "" {
			t.Fatalf("event = %+v err = %v: an authentic event that contradicts itself is the adapter's to alert on", e, err)
		}
	})
	t.Run("subscription events carry the subscription, the customer, the product and the period", func(t *testing.T) {
		t.Parallel()
		for typ, want := range map[string]domain.EventType{
			"subscription.created": domain.SubscriptionUpdated, "subscription.active": domain.SubscriptionUpdated, "subscription.updated": domain.SubscriptionUpdated,
			"subscription.canceled": domain.SubscriptionUpdated, "subscription.uncanceled": domain.SubscriptionUpdated, "subscription.past_due": domain.SubscriptionUpdated,
			"subscription.paused": domain.SubscriptionUpdated, "subscription.resumed": domain.SubscriptionUpdated, "subscription.cycled": domain.SubscriptionUpdated,
			"subscription.revoked": domain.SubscriptionDeleted,
		} {
			e := verify(t, p, fx, "msg_s"+typ, typ, polarfix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "past_due", start, end, true))
			o := e.Object
			if e.Type != want || o.ProviderSubscriptionID != "sub_1" || o.ProviderCustomerID != "cus_1" || o.ProviderPriceID != contracts.ProPriceID || o.Status != "past_due" ||
				!o.CancelAtPeriodEnd || o.PeriodStart == nil || !o.PeriodStart.Equal(start) || o.PeriodEnd == nil || !o.PeriodEnd.Equal(end) {
				t.Fatalf("%s → %+v / %+v", typ, e, o)
			}
		}
	})
	t.Run("what Polar settles itself or does not charge for grants and revokes nothing", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"refund.created", "refund.updated", "checkout.created", "checkout.updated", "checkout.expired", "customer.created", "customer.state_changed",
			"benefit_grant.created", "benefit_grant.revoked", "subscription.migrated", "order.something_new"} {
			if e := verify(t, p, fx, "msg_i"+typ, typ, map[string]any{"id": "x"}); e.Type != domain.Ignored || e.Object != (domain.EventObject{}) {
				t.Fatalf("%s → %+v: the order that was refunded carries the cumulative total, and a checkout never fulfils (invariant 2)", typ, e)
			}
		}
	})
	t.Run("a redelivery under another message id names the same payment", func(t *testing.T) {
		t.Parallel()
		a := verify(t, p, fx, "msg_x1", "order.paid", order("ord_x", 1000, "paid", ""))
		b := verify(t, p, fx, "msg_x2", "order.paid", order("ord_x", 1000, "paid", ""))
		if a.Object.ProviderPaymentID != b.Object.ProviderPaymentID || a.ProviderEventID == b.ProviderEventID {
			t.Fatalf("one payment, two events: %q/%q vs %q/%q", a.ProviderEventID, a.Object.ProviderPaymentID, b.ProviderEventID, b.Object.ProviderPaymentID)
		}
	})
	t.Run("the event is dated by its own timestamp, and by the signed header when it has none", func(t *testing.T) {
		t.Parallel()
		at := fx.Now().Add(-90 * time.Second)
		body := fx.Event("order.paid", at, order("ord_d", 1000, "paid", ""))
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_d", fx.Now(), body))
		if err != nil || !e.CreatedAt.Equal(at) {
			t.Fatalf("created = %v (%v), want the event's own %v", e.CreatedAt, err, at)
		}
		bare := []byte(`{"type":"order.paid","data":` + string(mustJSON(order("ord_d", 1000, "paid", ""))) + `}`)
		e, err = p.VerifyWebhook(ctx(), fx.Account(), bare, polarfix.Sign(polarfix.WebhookSecA, "msg_d2", fx.Now(), bare))
		if err != nil || !e.CreatedAt.Equal(fx.Now()) {
			t.Fatalf("created = %v (%v), want the signed header's %v", e.CreatedAt, err, fx.Now())
		}
	})
}

func TestAVerifiedBodyThatIsNotAnEventIsAnErrorNotAPartialEvent(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	order := string(mustJSON(paidOrder("pay-1")))
	for name, body := range map[string][]byte{
		"not JSON":                         []byte(`<html>`),
		"an array":                         []byte(`[]`),
		"no type":                          []byte(`{"timestamp":"2026-03-01T00:00:00Z","data":` + order + `}`),
		"no data":                          []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z"}`),
		"data that is not an object":       []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z","data":"x"}`),
		"a timestamp that is not a time":   []byte(`{"type":"order.paid","timestamp":"soon","data":` + order + `}`),
		"an order with no id":              []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z","data":{"status":"paid","paid":true,"total_amount":1,"currency":"usd"}}`),
		"a subscription with no id":        []byte(`{"type":"subscription.updated","timestamp":"2026-03-01T00:00:00Z","data":{"status":"active"}}`),
		"an order with no currency":        []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z","data":{"id":"o","status":"paid","paid":true,"total_amount":1}}`),
		"a negative refund":                []byte(`{"type":"order.refunded","timestamp":"2026-03-01T00:00:00Z","data":{"id":"o","status":"paid","paid":true,"total_amount":1000,"refunded_amount":-5,"currency":"usd"}}`),
		"a negative refunded tax":          []byte(`{"type":"order.refunded","timestamp":"2026-03-01T00:00:00Z","data":{"id":"o","status":"paid","paid":true,"total_amount":1000,"refunded_tax_amount":-5,"currency":"usd"}}`),
		"a negative total":                 []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z","data":{"id":"o","status":"paid","paid":true,"total_amount":-1,"currency":"usd"}}`),
		"an order with no total":           []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z","data":{"id":"o","status":"paid","paid":true,"currency":"usd"}}`),
		"an amount that is not an integer": []byte(`{"type":"order.paid","timestamp":"2026-03-01T00:00:00Z","data":{"id":"o","status":"paid","paid":true,"total_amount":10.5,"currency":"usd"}}`),
	} {
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_n", fx.Now(), body))
		if err == nil || errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: err = %v: a delivery that verified and is nonsense is our adapter's problem, not a forgery", name, err)
		}
		if e.ProviderEventID != "" {
			t.Errorf("%s: a partial event came with the error: %+v", name, e)
		}
	}
}

// ------------------------------------------------------------------ the API --

func TestEnsureCustomerCreatesOneCustomerPerScopeKeyedOnTheExternalId(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := ports.CustomerInput{Scope: md.Scope{Realm: "default", Kind: "workspace", ID: "w1"}, BillingEmail: "buyer@example.test", IdemKey: "customer:polar-a:default/workspace:w1"}
	id1, err := p.EnsureCustomer(ctx(), fx.Account(), in)
	if err != nil || id1 == "" {
		t.Fatalf("EnsureCustomer = %q, %v", id1, err)
	}
	req, ok := fx.Last(http.MethodPost, "/v1/customers/")
	if !ok || req.Body["email"] != "buyer@example.test" || req.Header.Get("Authorization") != "Bearer "+polarfix.TokenA {
		t.Fatalf("request = %+v", req)
	}
	// The key is looked up in a URL path, and this one has a slash in it: Polar is given an id that is safe there, the same
	// for the same key and different for another.
	ext, _ := req.Body["external_id"].(string)
	if ext == "" || strings.ContainsAny(ext, "/?#% :") || strings.Contains(ext, in.IdemKey) {
		t.Fatalf("external_id = %q: it must be path-safe", ext)
	}
	if meta, _ := req.Body["metadata"].(map[string]any); meta["scope_id"] != "w1" || meta["scope_kind"] != "workspace" || meta["realm"] != "default" {
		t.Fatalf("metadata = %v: it says whom the customer was made for, for a human reading Polar's dashboard", req.Body["metadata"])
	}
	// The same key again — two first checkouts at once — finds the customer the first call made.
	if id2, err := p.EnsureCustomer(ctx(), fx.Account(), in); err != nil || id2 != id1 {
		t.Fatalf("the same key gave %q then %q (%v): two first checkouts at once would leave two customers", id1, id2, err)
	}
	other := in
	other.IdemKey, other.BillingEmail = "customer:polar-a:default/workspace:w9", "w9@example.test"
	if _, err := p.EnsureCustomer(ctx(), fx.Account(), other); err != nil {
		t.Fatal(err)
	}
	if req2, _ := fx.Last(http.MethodPost, "/v1/customers/"); req2.Body["external_id"] == ext {
		t.Fatalf("two keys share the external id %q", ext)
	}
	if _, err := p.EnsureCustomer(ctx(), fx.OtherAccount(), ports.CustomerInput{Scope: in.Scope, BillingEmail: "b@example.test", IdemKey: "customer:polar-b:default/workspace:w1"}); err != nil {
		t.Fatal(err)
	}
	if req, _ := fx.Last(http.MethodPost, "/v1/customers/"); req.Header.Get("Authorization") != "Bearer "+polarfix.TokenB {
		t.Fatalf("account B's call used %q: each account has its own token", req.Header.Get("Authorization"))
	}
}

// Polar keeps an email unique within an organization. A second scope that bills to an address already taken cannot get
// a customer of its own, and must not be handed the first scope's: the mapping from customer to scope is one to one.
func TestAScopeWhoseEmailIsTakenGetsAnErrorNotAnotherScopesCustomer(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	first := ports.CustomerInput{Scope: md.Scope{Realm: "default", Kind: "workspace", ID: "w1"}, BillingEmail: "shared@example.test", IdemKey: "customer:polar-a:default/workspace:w1"}
	second := ports.CustomerInput{Scope: md.Scope{Realm: "default", Kind: "workspace", ID: "w2"}, BillingEmail: "shared@example.test", IdemKey: "customer:polar-a:default/workspace:w2"}
	id1, err := p.EnsureCustomer(ctx(), fx.Account(), first)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := p.EnsureCustomer(ctx(), fx.Account(), second)
	var api *polar.APIError
	if !errors.As(err, &api) || api.Status != 422 || id2 != "" || id2 == id1 {
		t.Fatalf("EnsureCustomer = %q, %v; want an *APIError with status 422 and no customer", id2, err)
	}
}

func TestACustomerNeedsABillingEmailAndPolarIsNotAskedWithout(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	if _, err := p.EnsureCustomer(ctx(), fx.Account(), ports.CustomerInput{IdemKey: "k"}); !errors.Is(err, md.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("Polar was called: %v", fx.Requests())
	}
}

func TestCreateCheckoutAsksForTheProductTheCataloguesPriceAndTheReturnPages(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	for _, mode := range []string{"payment", "subscription"} {
		in := fx.Checkout()
		in.Mode = mode
		url, err := p.CreateCheckout(ctx(), fx.Account(), in)
		if err != nil || !strings.HasPrefix(url, "https://buy.polar.test/checkout/") {
			t.Fatalf("%s: %q, %v", mode, url, err)
		}
		req, _ := fx.Last(http.MethodPost, "/v1/checkouts/")
		b := req.Body
		products, _ := b["products"].([]any)
		meta, _ := b["metadata"].(map[string]any)
		if len(products) != 1 || products[0] != contracts.PackPriceID || b["customer_id"] != "cus_1" || b["success_url"] != in.SuccessURL || b["return_url"] != in.CancelURL ||
			meta["plan"] != contracts.PackCode || b["amount"] != float64(1000) || b["currency"] != "usd" {
			t.Fatalf("%s: body = %v: the catalogue's price and currency are told to Polar, which charges them for a price it lets the caller set", mode, b)
		}
		if req.Header.Get("Authorization") != "Bearer "+polarfix.TokenA || req.Header.Get("Idempotency-Key") != "" {
			t.Fatalf("%s: header = %v (Polar has no idempotency header; a second checkout page fulfils nothing)", mode, req.Header)
		}
	}
}

func TestACheckoutWithoutACustomerNamesTheEmailInstead(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := fx.Checkout()
	in.CustomerID = ""
	if _, err := p.CreateCheckout(ctx(), fx.Account(), in); err != nil {
		t.Fatal(err)
	}
	req, _ := fx.Last(http.MethodPost, "/v1/checkouts/")
	if _, has := req.Body["customer_id"]; has || req.Body["customer_email"] != "buyer@example.test" {
		t.Fatalf("body = %v", req.Body)
	}
}

func TestACheckoutThatSellsNothingIsRefusedBeforePolarIsAsked(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	noProduct := fx.Checkout()
	noProduct.ProviderPriceID = ""
	badMode := fx.Checkout()
	badMode.Mode = "barter"
	for name, in := range map[string]ports.CheckoutInput{"no product": noProduct, "a mode nobody defined": badMode} {
		if _, err := p.CreateCheckout(ctx(), fx.Account(), in); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("Polar was called: %v", fx.Requests())
	}
}

// Polar has no checkout that only saves a payment method and no off-session charge. It says so, rather than selling
// something else under the name of a setup or pretending a charge happened.
func TestSavingAMethodAndChargingItAreUnsupportedAndPolarIsNotCalled(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	in := fx.Checkout()
	in.Mode = "setup"
	if _, err := p.CreateCheckout(ctx(), fx.Account(), in); !errors.Is(err, domain.ErrUnsupported) {
		t.Errorf("setup: err = %v", err)
	}
	id, err := p.ChargeSaved(ctx(), fx.Account(), ports.OffSessionInput{CustomerID: "cus_1", PaymentMethodRef: "pm_1", Amount: *money(1000), IdemKey: "k"})
	if !errors.Is(err, domain.ErrUnsupported) || id != "" {
		t.Errorf("ChargeSaved = %q, %v", id, err)
	}
	if len(fx.Requests()) != 0 {
		t.Fatalf("Polar was called: %v", fx.Requests())
	}
}

func TestPortalSessionIsPolarsPageForTheCustomer(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	url, err := p.CreatePortalSession(ctx(), fx.Account(), "cus_1", "https://app.example.test/billing")
	req, _ := fx.Last(http.MethodPost, "/v1/customer-sessions/")
	if err != nil || !strings.HasSuffix(url, "/cus_1") || req.Body["customer_id"] != "cus_1" || req.Body["return_url"] != "https://app.example.test/billing" {
		t.Fatalf("%q %v %v", url, err, req.Body)
	}
}

func TestFetchSubscriptionIsPolarsCurrentObjectInTheEnginesTerms(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, end := fx.Now().Add(-24*time.Hour).Truncate(time.Second), fx.Now().Add(29*24*time.Hour).Truncate(time.Second)
	fx.Subscriptions["sub_1"] = polarfix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "unpaid", start, end, true)
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
	if req, _ := fx.Last(http.MethodGet, "/v1/subscriptions/sub_1"); req.Header.Get("Authorization") != "Bearer "+polarfix.TokenA {
		t.Fatalf("header = %v", req.Header)
	}
	for polarStatus, want := range map[string]domain.SubStatus{"trialing": domain.Trialing, "active": domain.Active, "past_due": domain.PastDue, "unpaid": domain.PastDue,
		"canceled": domain.Canceled, "incomplete": domain.Incomplete, "incomplete_expired": domain.Canceled, "paused": domain.Paused} {
		fx.Subscriptions["sub_2"] = polarfix.Subscription("sub_2", "cus_1", contracts.ProPriceID, polarStatus, start, end, false)
		if got, _, _ := p.FetchSubscription(ctx(), fx.Account(), "sub_2"); got.Status != want {
			t.Errorf("Polar's %q = %q, want %q", polarStatus, got.Status, want)
		}
	}
	fx.Subscriptions["sub_3"] = polarfix.Subscription("sub_3", "cus_1", "product_nobody_sells", "active", start, end, false)
	if got, _, err := p.FetchSubscription(ctx(), fx.Account(), "sub_3"); err != nil || got.PlanCode != "" {
		t.Errorf("a product the catalogue does not sell: %+v, %v; want no plan code, which the processor ignores rather than guesses", got, err)
	}
	if _, _, err := p.FetchSubscription(ctx(), fx.Account(), "sub_missing"); !errors.Is(err, md.ErrNotFound) {
		t.Errorf("a subscription Polar does not have: %v, want ErrNotFound", err)
	}
}

func TestFetchedStateIsDatedByPolarsClockNotOurs(t *testing.T) {
	t.Parallel()
	fx := polarfix.New(t)
	ours := mcontracts.NewFakeClock(fx.Now().Add(7 * 24 * time.Hour)) // a clock a week ahead of Polar's
	p, err := polar.New(polar.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: ours, BaseURL: fx.APIURL()})
	if err != nil {
		t.Fatal(err)
	}
	fx.Subscriptions["sub_1"] = polarfix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "active", fx.Now(), fx.Now().Add(30*24*time.Hour), false)
	_, at, err := p.FetchSubscription(ctx(), fx.Account(), "sub_1")
	if err != nil || !at.Equal(fx.Now()) {
		t.Fatalf("at = %v (%v), want Polar's own %v: a subscription is ordered by the provider's time, never ours", at, err, fx.Now())
	}
}

func TestChangePlanFollowsThePolicyAtPolarOrSaysItCannot(t *testing.T) {
	t.Parallel()
	start, end := contracts.Start.Add(-24*time.Hour), contracts.Start.Add(29*24*time.Hour)
	seed := func(fx *polarfix.Fixtures) {
		fx.Subscriptions["sub_1"] = polarfix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "active", start, end, false)
	}
	for name, tc := range map[string]struct {
		policy   domain.PlanChangePolicy
		behavior string
	}{
		"at the period's end, as a pending change": {domain.AtPeriodEnd, "next_period"},
		"now, with Polar's proration":              {domain.ImmediateProrate, "prorate"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, fx := setup(t)
			seed(fx)
			if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.TeamPriceID, tc.policy); err != nil {
				t.Fatal(err)
			}
			req, ok := fx.Last(http.MethodPatch, "/v1/subscriptions/sub_1")
			if !ok || req.Body["product_id"] != contracts.TeamPriceID || req.Body["proration_behavior"] != tc.behavior {
				t.Fatalf("body = %v", req.Body)
			}
		})
	}
	t.Run("now with no proration has no Polar equivalent: switching now without prorating would restart the billing cycle at the full price", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		seed(fx)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.TeamPriceID, domain.ImmediateNoProrate); !errors.Is(err, domain.ErrUnsupported) {
			t.Fatalf("err = %v", err)
		}
		if len(fx.Requests()) != 0 {
			t.Fatalf("Polar was called: %v", fx.Requests())
		}
	})
	t.Run("a change to no product is refused before Polar is asked", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		seed(fx)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", "", domain.AtPeriodEnd); !errors.Is(err, md.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
		if len(fx.Requests()) != 0 {
			t.Fatalf("Polar was called: %v", fx.Requests())
		}
	})
	t.Run("a policy nobody defined is refused before Polar is asked", func(t *testing.T) {
		t.Parallel()
		p, fx := setup(t)
		if err := p.ChangePlan(ctx(), fx.Account(), "sub_1", contracts.TeamPriceID, "whenever"); !errors.Is(err, md.ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
		if len(fx.Requests()) != 0 {
			t.Fatalf("Polar was called: %v", fx.Requests())
		}
	})
}

func TestCancelAtPeriodEndSetsTheFlagAtPolarAndAskingTwiceIsFine(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	start, end := contracts.Start.Add(-24*time.Hour), contracts.Start.Add(29*24*time.Hour)
	fx.Subscriptions["sub_1"] = polarfix.Subscription("sub_1", "cus_1", contracts.ProPriceID, "active", start, end, false)
	if err := p.CancelAtPeriodEnd(ctx(), fx.Account(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if req, ok := fx.Last(http.MethodPatch, "/v1/subscriptions/sub_1"); !ok || req.Body["cancel_at_period_end"] != true {
		t.Fatalf("request = %+v", req)
	}
	// Polar refuses to cancel what is already canceled or set to cancel. The state asked for holds, so it is not an error.
	fx.Subscriptions["sub_1"]["cancel_at_period_end"] = true
	if err := p.CancelAtPeriodEnd(ctx(), fx.Account(), "sub_1"); err != nil {
		t.Fatalf("cancelling twice: %v", err)
	}
	if err := p.CancelAtPeriodEnd(ctx(), fx.Account(), "sub_missing"); !errors.Is(err, md.ErrNotFound) {
		t.Fatalf("a subscription Polar does not have: %v", err)
	}
}

func TestAnAPIFailureIsAnErrorAndNeverTheToken(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	fx.FailAll = true // the fake echoes the token it was given into the error, as a careless upstream page might
	for name, call := range map[string]func() error{
		"customer": func() error {
			_, err := p.EnsureCustomer(ctx(), fx.Account(), ports.CustomerInput{BillingEmail: "a@example.test", IdemKey: "k"})
			return err
		},
		"checkout": func() error { _, err := p.CreateCheckout(ctx(), fx.Account(), fx.Checkout()); return err },
		"portal": func() error {
			_, err := p.CreatePortalSession(ctx(), fx.Account(), "cus_1", "https://app.example.test/")
			return err
		},
		"cancel": func() error { return p.CancelAtPeriodEnd(ctx(), fx.Account(), "sub_1") },
		"fetch": func() error {
			_, _, err := p.FetchSubscription(ctx(), fx.Account(), "sub_1")
			return err
		},
	} {
		err := call()
		var api *polar.APIError
		if !errors.As(err, &api) || api.Status != 500 || api.Type != "InternalServerError" {
			t.Errorf("%s: err = %v, want an *APIError with status 500", name, err)
		}
		if err != nil && strings.Contains(err.Error(), polarfix.TokenA) {
			t.Errorf("%s: the error carries the token: %v", name, err)
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
		t.Fatal("a cancelled context still reached Polar")
	}
}

// roundTrip answers every request with a canned response and remembers where it was sent.
type roundTrip struct{ hosts []string }

func (r *roundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	r.hosts = append(r.hosts, req.URL.Scheme+"://"+req.URL.Host)
	return &http.Response{StatusCode: 201, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"cus_1"}`)), Request: req}, nil
}

func TestTheAPIHostFollowsTheAccountsModeAndNothingElse(t *testing.T) {
	t.Parallel()
	fx := polarfix.New(t)
	rt := &roundTrip{}
	p, err := polar.New(polar.Config{Secrets: fx.Secrets(), Catalog: fx.Catalog(), Clock: fx.Clock, HTTP: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	in := ports.CustomerInput{BillingEmail: "a@example.test", IdemKey: "k"}
	for mode, want := range map[string]string{"test": polar.SandboxURL, "live": polar.ProductionURL} {
		acct := fx.Account()
		acct.Mode = mode
		rt.hosts = nil
		if _, err := p.EnsureCustomer(ctx(), acct, in); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if len(rt.hosts) != 1 || rt.hosts[0] != want {
			t.Errorf("mode %q went to %v, want %s: a test-mode account must never reach production, nor a live one the sandbox", mode, rt.hosts, want)
		}
	}
	acct := fx.Account()
	acct.Mode = "staging"
	rt.hosts = nil
	if _, err := p.EnsureCustomer(ctx(), acct, in); !errors.Is(err, md.ErrInvalid) || len(rt.hosts) != 0 {
		t.Errorf("an account of unknown mode: err = %v, called %v", err, rt.hosts)
	}
}
