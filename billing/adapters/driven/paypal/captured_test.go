package paypal_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/billing/contracts/paypalfix"
	"github.com/truongpx396/intel-payment/billing/domain"
)

// What the adapter reads is only as good as the bodies it is tried against, and until the sandbox rehearsal every one was built
// from the documentation. These are deliveries PayPal's webhook simulator sent to a real endpoint, verified through PayPal's
// real verify call. The simulator is not a buyer: it sends sample bodies (some of them the legacy v1 shape), for sales and
// subscriptions this service never made. So they prove the envelope, the verification round trip and what is deliberately
// not read — not the fields of a real capture, which needs a buyer to approve an order in a browser and is still unrehearsed.

func verifyCaptured(t *testing.T, name string) (domain.Event, error) {
	t.Helper()
	p, fx := setup(t)
	body := paypalfix.Captured(t, name)
	e, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-"+name, fx.Now(), body))
	return e, err
}

func TestRealPayPalDeliveriesAreReadAsTheEngineNeedsThem(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]domain.Event{
		// A dispute names the transaction it is about, and the engine keys the dispute on it, so that the four steps of one
		// chargeback — and its resolution — meet on one record.
		"simulated_customer_dispute_created":  {Type: domain.DisputeOpened, Object: domain.EventObject{ProviderPaymentID: "00D10444LD479031K", DisputeID: "dp:00D10444LD479031K"}},
		"simulated_customer_dispute_resolved": {Type: domain.DisputeClosed, Object: domain.EventObject{ProviderPaymentID: "00D10444LD479031K", DisputeID: "dp:00D10444LD479031K", Status: "won"}},
		// A capture, or a subscription, whose custom_id this service did not set is another system's sale on the same PayPal
		// account: normal traffic, Ignored — never a grant to a customer nobody asked about.
		"simulated_payment_capture_completed":      {Type: domain.Ignored},
		"simulated_payment_capture_denied":         {Type: domain.Ignored},
		"simulated_billing_subscription_created":   {Type: domain.Ignored},
		"simulated_billing_subscription_cancelled": {Type: domain.Ignored},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, err := verifyCaptured(t, name)
			if err != nil {
				t.Fatalf("a real delivery was refused: %v", err)
			}
			got := domain.Event{Type: e.Type, Object: domain.EventObject{ProviderPaymentID: e.Object.ProviderPaymentID, DisputeID: e.Object.DisputeID, Status: e.Object.Status}}
			if got != want {
				t.Errorf("read as\n  %+v\nwant\n  %+v", got, want)
			}
			if e.ProviderEventID == "" || e.CreatedAt.IsZero() || e.RawHash == "" {
				t.Errorf("event %+v lacks its id, its provider time or the hash of its body", e)
			}
		})
	}
}

// A refund the adapter cannot read is an error that is raised, never an event that quietly refunds nothing. The simulator's
// refund is the legacy sale shape, with no capture and no total refunded to date; the delivery is retried and the operator is
// told, which is what the rehearsal saw (a 500, not a lost refund).
func TestARefundInTheShapeTheAdapterDoesNotReadFailsLoudly(t *testing.T) {
	t.Parallel()
	e, err := verifyCaptured(t, "simulated_payment_capture_refunded")
	if err == nil {
		t.Fatalf("a refund with no capture and no total was read as %+v", e)
	}
	if errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("err = %v; the delivery verified, so this is the adapter's problem to alert on, not a forged request", err)
	}
	if e.Type != "" {
		t.Errorf("event %+v came with the error: never a partial event", e)
	}
}

// PayPal vouches for the signature of exactly the bytes it sent, so the adapter hands over the delivery untouched — whitespace
// and all — and a delivery altered by one byte is refused.
func TestARealPayPalDeliveryIsAskedAboutAsReceivedAndOnlyBelievedWithItsSignature(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := paypalfix.Captured(t, "simulated_customer_dispute_created")
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-1", fx.Now(), body)); err != nil {
		t.Fatal(err)
	}
	asked, ok := fx.Last("POST", verifyPath)
	if !ok || !bytes.Contains(asked.Raw, body) {
		t.Fatalf("PayPal was asked about %q, which does not contain the delivery as it arrived", asked.Raw)
	}

	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0x01
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), tampered, paypalfix.Headers(paypalfix.WebhookIDA, "tx-1", fx.Now(), body)); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("a delivery altered by one byte = %v; want ErrInvalidSignature", err)
	}
	if _, err := p.VerifyWebhook(ctx(), fx.OtherAccount(), body, paypalfix.Headers(paypalfix.WebhookIDA, "tx-1", fx.Now(), body)); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("a delivery for webhook A verified on account B = %v; want ErrInvalidSignature", err)
	}
}
