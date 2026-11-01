package polar_test

import (
	"testing"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/polarfix"
	"github.com/truongpx396/intel-payment/billing/domain"
)

// What the adapter reads is only as good as the bodies it is tried against, and until the sandbox rehearsal every one was built
// from the documentation. These are not: each is a delivery the Polar sandbox sent to a real endpoint while a checkout was
// completed for real (identities, links and keys scrubbed, the sandbox's product id replaced by the contract world's). The
// order is a free one — a sandbox with no card on file cannot take payment without a buyer at a browser — so it proves the
// shape and the signature, not a charge.

func TestRealPolarDeliveriesAreReadAsTheEngineNeedsThem(t *testing.T) {
	t.Parallel()
	const customer, order = "63ff574e-0cc3-466c-b092-304af6c7406a", "753e1606-29bf-439b-bca6-bf943303ec5d"
	for name, want := range map[string]domain.EventType{
		// An order is the payment, keyed on its own id, with the customer and the product (the catalogue's price) on it.
		"order_created": domain.PaymentSucceeded,
		"order_paid":    domain.PaymentSucceeded,
		// An order's later edits and the checkout and customer chatter are not payments: Ignored, normal traffic, not an error.
		"order_updated":    domain.Ignored,
		"checkout_updated": domain.Ignored,
		"customer_created": domain.Ignored,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, fx := setup(t)
			body := polarfix.Captured(t, name)
			e, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_cap", fx.Now(), body))
			if err != nil {
				t.Fatalf("a real delivery was refused: %v", err)
			}
			if e.Type != want {
				t.Fatalf("read as %q, want %q", e.Type, want)
			}
			if e.RawHash == "" || e.CreatedAt.IsZero() {
				t.Errorf("event %+v lacks the hash of its body or its provider time", e)
			}
			if want != domain.PaymentSucceeded {
				return
			}
			o := e.Object
			if o.ProviderPaymentID != order || o.ProviderCustomerID != customer || o.ProviderPriceID != contracts.PackPriceID || o.ProviderSubscriptionID != "" {
				t.Errorf("object = %+v; want order %s of customer %s for the pack, no subscription", o, order, customer)
			}
			if o.Amount == nil || o.Amount.MinorUnits != 0 || o.Amount.Currency != "USD" {
				t.Errorf("amount = %+v; the sandbox order was free, in USD", o.Amount)
			}
		})
	}
}

// order.created and order.paid are two deliveries of one payment, which is what keeps a purchase from granting twice.
func TestTheTwoRealDeliveriesOfOneOrderAreOnePayment(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	var ids [2]string
	for i, name := range []string{"order_created", "order_paid"} {
		body := polarfix.Captured(t, name)
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, polarfix.Sign(polarfix.WebhookSecA, "msg_"+name, fx.Now(), body))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = e.Object.ProviderPaymentID
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Errorf("payment keys %q and %q; grants are keyed on the payment, so they must agree", ids[0], ids[1])
	}
}

// A real body is only believed with its signature: the same delivery altered by a byte, or signed for another account, is refused.
func TestARealPolarDeliveryIsOnlyBelievedWithItsSignature(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := polarfix.Captured(t, "order_paid")
	sig := polarfix.Sign(polarfix.WebhookSecA, "msg_cap", fx.Now(), body)
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0x01
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), tampered, sig); err == nil {
		t.Error("a real delivery altered by one byte verified")
	}
	if _, err := p.VerifyWebhook(ctx(), fx.OtherAccount(), body, sig); err == nil {
		t.Error("a real delivery signed for account A verified for account B")
	}
}
