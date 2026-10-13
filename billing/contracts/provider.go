package contracts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// ProviderCase is one subtest of ProviderContract. The cases are exported so that a suite can be shown to have
// teeth: run a case against a deliberately broken provider with Probe and require that it is rejected, by name.
type ProviderCase struct {
	Name string
	Run  func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures)
}

// ProviderCases are the subtests of ProviderContract, in order.
func ProviderCases() []ProviderCase {
	return []ProviderCase{
		{"rejects a tampered body", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, hdr := fx.TamperedPaymentSucceeded()
			e, err := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr)
			if err == nil {
				t.Fatal("a tampered body must never verify (invariant 1)")
			}
			if !errors.Is(err, domain.ErrInvalidSignature) {
				t.Fatalf("a tampered body is domain.ErrInvalidSignature, got %v", err)
			}
			if !reflect.DeepEqual(e, domain.Event{}) {
				t.Fatalf("an error must come with no Event at all, never a partial one: %+v", e)
			}
		}},
		{"rejects a missing signature", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, _ := fx.SignedPaymentSucceeded()
			e, err := p.VerifyWebhook(Ctx(), fx.Account(), body, nil)
			if !errors.Is(err, domain.ErrInvalidSignature) {
				t.Fatalf("an unsigned delivery must be domain.ErrInvalidSignature, got %v", err)
			}
			if !reflect.DeepEqual(e, domain.Event{}) {
				t.Fatalf("an error must come with no Event at all, never a partial one: %+v", e)
			}
		}},
		{"rejects a stale signature", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, hdr := fx.SignedAt(fx.Now().Add(-48 * time.Hour))
			if _, err := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr); !errors.Is(err, domain.ErrInvalidSignature) {
				t.Fatalf("a captured payload must not verify forever, got %v", err)
			}
			body, hdr = fx.SignedAt(fx.Now().Add(48 * time.Hour))
			if _, err := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr); !errors.Is(err, domain.ErrInvalidSignature) {
				t.Fatalf("a signature from the future is as suspect as one from the past, got %v", err)
			}
		}},
		{"rejects a body signed for another account", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, hdr := fx.SignedWithSecretOf(fx.OtherAccount())
			if _, err := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr); !errors.Is(err, domain.ErrInvalidSignature) {
				t.Fatalf("secrets are per account: a body signed with account B's must fail on account A, got %v", err)
			}
		}},
		{"verifies and normalizes with the provider's timestamp and a body hash", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			acct := fx.Account()
			body, hdr := fx.SignedPaymentSucceeded()
			e, err := p.VerifyWebhook(Ctx(), acct, body, hdr)
			if err != nil {
				t.Fatalf("a genuine delivery must verify: %v", err)
			}
			if e.Type != domain.PaymentSucceeded || e.ProviderEventID == "" || e.CreatedAt.IsZero() {
				t.Fatalf("the adapter must normalize the provider's shape: %+v", e)
			}
			if e.Provider != p.Name() || e.Account != acct.ID {
				t.Fatalf("the event names provider %q account %q, want %q %q", e.Provider, e.Account, p.Name(), acct.ID)
			}
			if a := e.Object.Amount; a == nil || a.MinorUnits != 1000 || a.Currency != "USD" {
				t.Fatalf("amount = %+v, want 1000 USD in integer minor units", a)
			}
			if e.Object.ProviderCustomerID != "cus_1" || e.Object.ProviderPaymentID != "pay-1" || e.Object.ProviderPriceID != PackPriceID {
				t.Fatalf("the provider's ids must reach the event: %+v", e.Object)
			}
			sum := sha256.Sum256(body)
			if e.RawHash != hex.EncodeToString(sum[:]) {
				t.Fatalf("RawHash = %q, want the SHA-256 of the verified body: the body itself is not stored", e.RawHash)
			}
			if err := e.Validate(); err != nil {
				t.Fatalf("a verified event must be one the inbox accepts: %v", err)
			}
		}},
		{"verifying a delivery twice gives the same event", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, hdr := fx.SignedPaymentSucceeded()
			a, err1 := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr)
			b, err2 := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr)
			if err1 != nil || err2 != nil {
				t.Fatalf("a redelivery must verify as the original did: %v / %v", err1, err2)
			}
			if a.ProviderEventID != b.ProviderEventID || a.RawHash != b.RawHash || !a.CreatedAt.Equal(b.CreatedAt) || a.Type != b.Type {
				t.Fatalf("a redelivery normalized differently — it would be a different event to the inbox:\n%+v\n%+v", a, b)
			}
		}},
		{"refund amounts are normalized as cumulative", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, hdr := fx.SignedCumulativeRefund(300, 200) // two refunds: 300, then 200
			e, err := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr)
			if err != nil {
				t.Fatal(err)
			}
			if e.Type != domain.PaymentRefunded {
				t.Fatalf("type = %q, want payment_refunded", e.Type)
			}
			if e.Object.AmountRefunded == nil || e.Object.AmountRefunded.MinorUnits != 500 {
				t.Fatalf("want a cumulative 500, got %+v — a per-event figure would revoke too little (invariant 16)", e.Object.AmountRefunded)
			}
		}},
		{"unsubscribed types are Ignored, not errors", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			body, hdr := fx.SignedUnknownType()
			e, err := p.VerifyWebhook(Ctx(), fx.Account(), body, hdr)
			if err != nil {
				t.Fatalf("a type nobody subscribed to is normal traffic, not an error: %v", err)
			}
			if e.Type != domain.Ignored {
				t.Fatalf("want Ignored, got %q", e.Type)
			}
		}},
		{"checkout returns a hosted page and carries no secret", func(t testing.TB, p ports.PaymentProvider, fx ProviderFixtures) {
			acct := fx.Account()
			for _, mode := range []string{"payment", "subscription", "setup"} {
				in := fx.Checkout()
				in.Mode = mode
				url, err := p.CreateCheckout(Ctx(), acct, in)
				if err != nil {
					t.Fatalf("mode %s: %v", mode, err)
				}
				if !strings.HasPrefix(url, "https://") {
					t.Fatalf("mode %s: redirect %q is not a hosted https page", mode, url)
				}
				if strings.Contains(url, acct.SecretRef) || strings.Contains(url, acct.WebhookSecretRef) {
					t.Fatalf("mode %s: the redirect leaks a secret reference: %s", mode, url)
				}
			}
		}},
	}
}

// ProviderContract is the specification of a provider adapter (FR-018, FR-019, SC-004): it verifies on the raw
// body or not at all, takes its time and its secret from the right place, normalizes in the engine's vocabulary
// — refunds as cumulative totals — and treats traffic nobody subscribed to as a first-class outcome. It runs
// against EVERY adapter; a new adapter is not registered until it passes.
func ProviderContract(t *testing.T, p ports.PaymentProvider, fx ProviderFixtures) {
	t.Helper()
	for _, c := range ProviderCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, p, fx)
		})
	}
}
