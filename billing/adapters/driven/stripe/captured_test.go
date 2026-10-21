package stripe_test

import (
	"testing"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/contracts/stripefix"
	"github.com/truongpx396/intel-payment/billing/domain"
)

// What the adapter reads is only as good as the bodies it is tried against, and until the sandbox rehearsal every one was built
// from the documentation. These are not: each is a delivery a real Stripe sandbox endpoint, pinned to the adapter's API version,
// sent while the whole flow was run for real (identities, links and keys scrubbed, the sandbox's price ids replaced by the contract
// world's). A shape Stripe changes, or that the documentation had wrong, fails here before it fails on a customer's payment.

type capturedWant struct {
	typ                domain.EventType
	customer, payment  string // "" is "none"
	price              string
	amount, refunded   int64
	status, dispute    string
	subscription       string
	method             string
	fails              string
	hasPeriod, cancels bool
}

func TestRealStripeDeliveriesAreReadAsTheEngineNeedsThem(t *testing.T) {
	t.Parallel()
	for name, w := range map[string]capturedWant{
		// A one-time pack: the invoice it creates is what fulfils it, keyed on the payment intent, with the price on its line.
		"invoice_paid_pack": {typ: domain.PaymentSucceeded, customer: "cus_VSBjkZ2Tt7fTaf", payment: "pi_3URHMHPJXgtZX5D624wwgobF", price: contracts.PackPriceID, amount: 1000},
		// A subscription's first invoice: the same event, plus the subscription it belongs to.
		"invoice_paid_subscription": {typ: domain.PaymentSucceeded, customer: "cus_VSBkHFhLOTQY38", payment: "pi_3URHNFPJXgtZX5D61AD8OEee", price: contracts.ProPriceID, amount: 2000, subscription: "sub_1URHNFPJXgtZX5D6WGWlwIYN"},
		"invoice_payment_failed":    {typ: domain.PaymentFailed, customer: "cus_VSBlHJqUHlHvBv", payment: "pi_3URHNgPJXgtZX5D62bZFy9Vg", price: contracts.ProPriceID, amount: 2000, subscription: "sub_1URHNgPJXgtZX5D6ZFr8MXiI", fails: "payment_failed"},
		// An auto top-up's charge is a bare payment intent: an amount and a saved method, and NO price (the engine fulfils it from the
		// payment its handler recorded, never from the event).
		"payment_intent_succeeded_offsession": {typ: domain.PaymentSucceeded, customer: "cus_VSBlzvgYKOnwFu", payment: "pi_3URHOJPJXgtZX5D62Nc1R1mm", amount: 1000, method: "pm_1URHNxPJXgtZX5D6ks6UWK2Y"},
		// ... while the intent of an invoice is the invoice's business: reading it too would grant twice.
		"payment_intent_succeeded_invoice": {typ: domain.Ignored},
		// A refund reports what has been refunded to date, and the charge's own amount.
		"charge_refunded_partial": {typ: domain.PaymentRefunded, customer: "cus_VSBjkZ2Tt7fTaf", payment: "pi_3URHMHPJXgtZX5D624wwgobF", amount: 1000, refunded: 400},
		// A dispute names the payment and its own id, and no customer: the engine finds the customer from the payment.
		"charge_dispute_created":          {typ: domain.DisputeOpened, payment: "pi_3URHMZPJXgtZX5D61kGGPEe3", dispute: "du_1URHMbPJXgtZX5D6cXLVlNd2"},
		"charge_dispute_funds_withdrawn":  {typ: domain.DisputeFundsWithdrawn, payment: "pi_3URHMZPJXgtZX5D61kGGPEe3", dispute: "du_1URHMbPJXgtZX5D6cXLVlNd2"},
		"charge_dispute_closed_won":       {typ: domain.DisputeClosed, payment: "pi_3URHMZPJXgtZX5D61kGGPEe3", dispute: "du_1URHMbPJXgtZX5D6cXLVlNd2", status: "won"},
		"charge_dispute_funds_reinstated": {typ: domain.DisputeFundsReinstated, payment: "pi_3URHMZPJXgtZX5D61kGGPEe3", dispute: "du_1URHMbPJXgtZX5D6cXLVlNd2"},
		// A subscription's state, with its price and its period.
		"customer_subscription_created": {typ: domain.SubscriptionUpdated, customer: "cus_VSBkHFhLOTQY38", price: contracts.ProPriceID, subscription: "sub_1URHNFPJXgtZX5D6WGWlwIYN", status: "active", hasPeriod: true},
		"customer_subscription_updated": {typ: domain.SubscriptionUpdated, customer: "cus_VSBkHFhLOTQY38", price: contracts.ProPriceID, subscription: "sub_1URHNFPJXgtZX5D6WGWlwIYN", status: "active", hasPeriod: true},
		// The dozens of types nobody subscribed to are Ignored: normal traffic, not an error.
		"customer_created": {typ: domain.Ignored},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, fx := setup(t)
			body := stripefix.Captured(t, name)
			e, err := p.VerifyWebhook(ctx(), fx.Account(), body, fx.Sign(stripefix.WebhookSecA, fx.Now(), body))
			if err != nil {
				t.Fatalf("a real delivery was refused: %v", err)
			}
			o := e.Object
			got := capturedWant{typ: e.Type, customer: o.ProviderCustomerID, payment: o.ProviderPaymentID, price: o.ProviderPriceID, status: o.Status, dispute: o.DisputeID,
				subscription: o.ProviderSubscriptionID, method: o.PaymentMethodRef, fails: o.FailureReason, hasPeriod: o.PeriodStart != nil && o.PeriodEnd != nil, cancels: o.CancelAtPeriodEnd}
			if o.Amount != nil {
				got.amount = o.Amount.MinorUnits
				if o.Amount.Currency != "USD" {
					t.Errorf("currency = %q, want USD", o.Amount.Currency)
				}
			}
			if o.AmountRefunded != nil {
				got.refunded = o.AmountRefunded.MinorUnits
			}
			if got != w {
				t.Errorf("read as\n  %+v\nwant\n  %+v", got, w)
			}
			if e.ProviderEventID == "" || e.CreatedAt.IsZero() || e.RawHash == "" {
				t.Errorf("event %+v lacks its id, its provider time or the hash of its body", e)
			}
		})
	}
}

// All four events of one chargeback name the same payment and the same dispute, which is what lets a won dispute give back
// exactly what the chargeback took.
func TestTheFourEventsOfOneRealChargebackAgree(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	var payment, dispute string
	for _, name := range []string{"charge_dispute_created", "charge_dispute_funds_withdrawn", "charge_dispute_closed_won", "charge_dispute_funds_reinstated"} {
		body := stripefix.Captured(t, name)
		e, err := p.VerifyWebhook(ctx(), fx.Account(), body, fx.Sign(stripefix.WebhookSecA, fx.Now(), body))
		if err != nil {
			t.Fatal(err)
		}
		if payment == "" {
			payment, dispute = e.Object.ProviderPaymentID, e.Object.DisputeID
		}
		if e.Object.ProviderPaymentID != payment || e.Object.DisputeID != dispute {
			t.Errorf("%s names payment %q dispute %q, the others %q %q", name, e.Object.ProviderPaymentID, e.Object.DisputeID, payment, dispute)
		}
	}
}

// A real body is only believed with its signature: the same delivery altered by a byte, or signed for another account, is refused.
func TestARealDeliveryIsOnlyBelievedWithItsSignature(t *testing.T) {
	t.Parallel()
	p, fx := setup(t)
	body := stripefix.Captured(t, "invoice_paid_pack")
	sig := fx.Sign(stripefix.WebhookSecA, fx.Now(), body)
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0x01
	if _, err := p.VerifyWebhook(ctx(), fx.Account(), tampered, sig); err == nil {
		t.Error("a real delivery altered by one byte verified")
	}
	if _, err := p.VerifyWebhook(ctx(), fx.OtherAccount(), body, sig); err == nil {
		t.Error("a real delivery signed for account A verified for account B")
	}
}
