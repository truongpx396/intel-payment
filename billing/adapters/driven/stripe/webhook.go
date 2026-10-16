package stripe

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
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// VerifyWebhook implements ports.PaymentProvider: the raw body is authenticated with the account's webhook secret —
// HMAC-SHA256 over `timestamp.body`, compared in constant time, with the timestamp inside the tolerance — and only
// then parsed. It returns an error and never a partial Event.
func (p *Provider) VerifyWebhook(ctx context.Context, acct domain.ProviderAccount, body []byte, headers http.Header) (domain.Event, error) {
	secret, err := p.cfg.Secrets.Resolve(ctx, acct.WebhookSecretRef)
	if err != nil {
		// Our misconfiguration: neither a forged delivery nor an outage of Stripe's. The error names the reference,
		// never the value.
		return domain.Event{}, fmt.Errorf("stripe: resolve the webhook secret of account %s: %w", acct.ID, err)
	}
	ts, sigs := parseSignature(headers.Get("Stripe-Signature"))
	if ts <= 0 || len(sigs) == 0 {
		return domain.Event{}, fmt.Errorf("%w: no usable Stripe-Signature", domain.ErrInvalidSignature)
	}
	age := ports.ReceivedAt(ctx, p.cfg.Clock.Now()).Sub(time.Unix(ts, 0)) // as of when it arrived: a parked delivery is verified later
	if age < 0 {
		age = -age
	}
	if age > p.cfg.Tolerance {
		return domain.Event{}, fmt.Errorf("%w: the signature is %s from now, beyond the %s tolerance", domain.ErrInvalidSignature, age.Round(time.Second), p.cfg.Tolerance)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	want := mac.Sum(nil)
	matched := false
	for _, sig := range sigs {
		if hmac.Equal(sig, want) {
			matched = true
		}
	}
	if !matched {
		return domain.Event{}, fmt.Errorf("%w: no signature matches", domain.ErrInvalidSignature)
	}
	return p.normalize(acct, body)
}

// parseSignature reads `t=<unix>,v1=<hex>[,v1=<hex>…]`. Stripe sends several v1 signatures while a secret is being
// rolled; v0 (test mode) and unknown schemes are ignored. A value that is not hex is skipped, so a header cannot make
// the comparison panic.
func parseSignature(h string) (ts int64, sigs [][]byte) {
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			if ts == 0 {
				ts, _ = strconv.ParseInt(v, 10, 64)
			}
		case "v1":
			if raw, err := hex.DecodeString(v); err == nil && len(raw) > 0 {
				sigs = append(sigs, raw)
			}
		}
	}
	return ts, sigs
}

// ---- Stripe's event shapes: only the fields the engine needs ----

// ref is an id Stripe sends as a string, or as the whole object when it is expanded.
type ref string

func (r *ref) UnmarshalJSON(b []byte) error {
	switch {
	case len(b) == 0 || string(b) == "null":
		*r = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*r = ref(s)
	case b[0] == '{':
		var o struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		*r = ref(o.ID)
	default:
		return fmt.Errorf("an id is a string or an object, not %s", b)
	}
	return nil
}

type priceRef struct {
	Price   ref `json:"price"`
	Pricing *struct {
		PriceDetails struct {
			Price ref `json:"price"`
		} `json:"price_details"`
	} `json:"pricing"`
}

func (p priceRef) id() string {
	if p.Price != "" {
		return string(p.Price)
	}
	if p.Pricing != nil {
		return string(p.Pricing.PriceDetails.Price)
	}
	return ""
}

type invoiceObj struct {
	ID                string `json:"id"`
	Customer          ref    `json:"customer"`
	Subscription      ref    `json:"subscription"`
	PaymentIntent     ref    `json:"payment_intent"`
	AmountPaid        int64  `json:"amount_paid"`
	AmountDue         int64  `json:"amount_due"`
	Currency          string `json:"currency"`
	Tax               *int64 `json:"tax"`
	HostedInvoiceURL  string `json:"hosted_invoice_url"`
	DefaultPaymentRef ref    `json:"default_payment_method"`
	Lines             struct {
		Data []priceRef `json:"data"`
	} `json:"lines"`
}

type paymentIntentObj struct {
	ID               string `json:"id"`
	Customer         ref    `json:"customer"`
	Amount           int64  `json:"amount"`
	Currency         string `json:"currency"`
	Invoice          ref    `json:"invoice"`
	PaymentMethod    ref    `json:"payment_method"`
	LastPaymentError *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"last_payment_error"`
}

type chargeObj struct {
	ID             string `json:"id"`
	Customer       ref    `json:"customer"`
	PaymentIntent  ref    `json:"payment_intent"`
	Invoice        ref    `json:"invoice"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
	ReceiptURL     string `json:"receipt_url"`
	Refunds        struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	} `json:"refunds"`
}

type disputeObj struct {
	ID            string `json:"id"`
	Charge        ref    `json:"charge"`
	PaymentIntent ref    `json:"payment_intent"`
	Status        string `json:"status"`
}

type subscriptionObj struct {
	ID                 string `json:"id"`
	Customer           ref    `json:"customer"`
	Status             string `json:"status"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	Items              struct {
		Data []struct {
			ID string `json:"id"`
			priceRef
		} `json:"data"`
	} `json:"items"`
}

func money(minor int64, currency string) *domain.Money {
	return &domain.Money{MinorUnits: minor, Currency: strings.ToUpper(currency)}
}

func unixPtr(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

// normalize turns a verified body into the engine's event. A body that verified and is not an event is our adapter's
// problem to alert on: an error, never a partial Event.
func (p *Provider) normalize(acct domain.ProviderAccount, body []byte) (domain.Event, error) {
	var env struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created *int64 `json:"created"`
		Data    *struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return domain.Event{}, fmt.Errorf("stripe: a verified body is not a JSON event: %w", err)
	}
	if env.ID == "" || env.Created == nil || env.Data == nil || len(env.Data.Object) == 0 {
		return domain.Event{}, fmt.Errorf("stripe: a verified body is not an event: it has no id, created or data.object")
	}
	sum := sha256.Sum256(body)
	e := domain.Event{Provider: p.Name(), Account: acct.ID, ProviderEventID: env.ID, CreatedAt: time.Unix(*env.Created, 0).UTC(),
		Type: domain.Ignored, RawHash: hex.EncodeToString(sum[:])}

	decode := func(v any) error {
		if err := json.Unmarshal(env.Data.Object, v); err != nil {
			return fmt.Errorf("stripe: event %s (%s) has an unreadable object: %w", env.ID, env.Type, err)
		}
		return nil
	}
	switch env.Type {
	case "invoice.paid", "invoice.payment_succeeded", "invoice.payment_failed":
		var inv invoiceObj
		if err := decode(&inv); err != nil {
			return domain.Event{}, err
		}
		e.Object = invoiceObject(inv, env.Type == "invoice.payment_failed")
		e.Type = domain.PaymentSucceeded
		if env.Type == "invoice.payment_failed" {
			e.Type = domain.PaymentFailed
		}
	case "payment_intent.succeeded", "payment_intent.payment_failed":
		var pi paymentIntentObj
		if err := decode(&pi); err != nil {
			return domain.Event{}, err
		}
		if pi.Invoice != "" {
			return e, nil // the invoice names this payment and fulfils it once
		}
		o := domain.EventObject{ProviderCustomerID: string(pi.Customer), ProviderPaymentID: pi.ID, Amount: money(pi.Amount, pi.Currency), PaymentMethodRef: string(pi.PaymentMethod)}
		e.Type = domain.PaymentSucceeded
		if env.Type == "payment_intent.payment_failed" {
			e.Type, o.FailureReason = domain.PaymentFailed, "payment_failed"
			if pi.LastPaymentError != nil && pi.LastPaymentError.Code != "" {
				o.FailureReason = pi.LastPaymentError.Code
			}
		}
		e.Object = o
	case "charge.refunded":
		var ch chargeObj
		if err := decode(&ch); err != nil {
			return domain.Event{}, err
		}
		o := domain.EventObject{ProviderCustomerID: string(ch.Customer), ProviderPaymentID: firstNonEmpty(string(ch.PaymentIntent), string(ch.Invoice), ch.ID),
			Amount: money(ch.Amount, ch.Currency), AmountRefunded: money(ch.AmountRefunded, ch.Currency), ReceiptURL: ch.ReceiptURL}
		if len(ch.Refunds.Data) > 0 {
			o.ProviderRefundID = ch.Refunds.Data[0].ID // newest first
		}
		e.Type, e.Object = domain.PaymentRefunded, o
	case "charge.dispute.created", "charge.dispute.funds_withdrawn", "charge.dispute.funds_reinstated", "charge.dispute.closed":
		var d disputeObj
		if err := decode(&d); err != nil {
			return domain.Event{}, err
		}
		o := domain.EventObject{ProviderPaymentID: firstNonEmpty(string(d.PaymentIntent), string(d.Charge)), DisputeID: d.ID}
		switch env.Type {
		case "charge.dispute.created":
			e.Type = domain.DisputeOpened
		case "charge.dispute.funds_withdrawn":
			e.Type = domain.DisputeFundsWithdrawn
		case "charge.dispute.funds_reinstated":
			e.Type = domain.DisputeFundsReinstated
		default:
			e.Type = domain.DisputeClosed
			o.Status = "won" // warning_closed and charge_refunded lose the merchant nothing
			if d.Status == "lost" {
				o.Status = "lost"
			}
		}
		e.Object = o
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.paused", "customer.subscription.resumed", "customer.subscription.deleted":
		var s subscriptionObj
		if err := decode(&s); err != nil {
			return domain.Event{}, err
		}
		o := domain.EventObject{ProviderCustomerID: string(s.Customer), ProviderSubscriptionID: s.ID, Status: s.Status, CancelAtPeriodEnd: s.CancelAtPeriodEnd,
			PeriodStart: unixPtr(s.CurrentPeriodStart), PeriodEnd: unixPtr(s.CurrentPeriodEnd)}
		if len(s.Items.Data) > 0 {
			o.ProviderPriceID = s.Items.Data[0].id()
		}
		e.Type, e.Object = domain.SubscriptionUpdated, o
		if env.Type == "customer.subscription.deleted" {
			e.Type = domain.SubscriptionDeleted
		}
	}
	// Anything else — including checkout.session.completed, which fulfils nothing — is Ignored: normal traffic.
	return e, nil
}

func invoiceObject(inv invoiceObj, failed bool) domain.EventObject {
	amount := inv.AmountPaid
	if failed {
		amount = inv.AmountDue
	}
	o := domain.EventObject{ProviderCustomerID: string(inv.Customer), ProviderSubscriptionID: string(inv.Subscription),
		ProviderPaymentID: firstNonEmpty(string(inv.PaymentIntent), inv.ID), Amount: money(amount, inv.Currency), ReceiptURL: inv.HostedInvoiceURL,
		PaymentMethodRef: string(inv.DefaultPaymentRef)}
	if len(inv.Lines.Data) > 0 {
		o.ProviderPriceID = inv.Lines.Data[0].id()
	}
	if inv.Tax != nil {
		o.TaxAmount = money(*inv.Tax, inv.Currency)
	}
	if failed {
		o.FailureReason = "payment_failed"
	}
	return o
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
