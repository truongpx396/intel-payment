package polar

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// VerifyWebhook implements ports.PaymentProvider: the raw body is authenticated with the account's webhook secret —
// HMAC-SHA256 over `message-id.timestamp.body`, compared in constant time, with the timestamp inside the tolerance — and
// only then parsed. It returns an error and never a partial Event.
func (p *Provider) VerifyWebhook(ctx context.Context, acct domain.ProviderAccount, body []byte, headers http.Header) (domain.Event, error) {
	secret, err := p.cfg.Secrets.Resolve(ctx, acct.WebhookSecretRef)
	if err != nil {
		// Our misconfiguration: neither a forged delivery nor an outage of Polar's. The error names the reference,
		// never the value.
		return domain.Event{}, fmt.Errorf("polar: resolve the webhook secret of account %s: %w", acct.ID, err)
	}
	id, stamp := headers.Get("webhook-id"), headers.Get("webhook-timestamp")
	ts, perr := strconv.ParseInt(stamp, 10, 64)
	sigs := parseSignatures(headers.Get("webhook-signature"))
	if id == "" || perr != nil || ts <= 0 || len(sigs) == 0 {
		return domain.Event{}, fmt.Errorf("%w: no usable webhook-id, webhook-timestamp and webhook-signature", domain.ErrInvalidSignature)
	}
	age := ports.ReceivedAt(ctx, p.cfg.Clock.Now()).Sub(time.Unix(ts, 0)) // as of when it arrived: a parked delivery is verified later
	if age < 0 {
		age = -age
	}
	if age > p.cfg.Tolerance {
		return domain.Event{}, fmt.Errorf("%w: the signature is %s from now, beyond the %s tolerance", domain.ErrInvalidSignature, age.Round(time.Second), p.cfg.Tolerance)
	}
	matched := false
	for _, key := range signingKeys(secret) {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(id + "." + stamp + "."))
		mac.Write(body)
		want := mac.Sum(nil)
		for _, sig := range sigs {
			if hmac.Equal(sig, want) {
				matched = true
			}
		}
	}
	if !matched {
		return domain.Event{}, fmt.Errorf("%w: no signature matches", domain.ErrInvalidSignature)
	}
	return p.normalize(acct, id, time.Unix(ts, 0).UTC(), body)
}

// signingKeys are the HMAC keys a webhook secret may have signed with. A secret made on or after 8 September 2026 is
// Standard Webhooks': its key is the base64 after the `whsec_` prefix, decoded. An older one is signed with the UTF-8
// bytes of the whole string. The dashboard does not say which a secret is, so both are tried — and a key that is empty
// is never one, so a secret that decodes to nothing cannot be signed for by anyone.
func signingKeys(secret string) [][]byte {
	var keys [][]byte
	if secret != "" {
		keys = append(keys, []byte(secret))
	}
	if rest, ok := strings.CutPrefix(secret, "whsec_"); ok {
		if k, err := base64.StdEncoding.DecodeString(rest); err == nil && len(k) > 0 {
			keys = append(keys, k)
		}
	}
	return keys
}

// parseSignatures reads the space-separated `v1,<base64>` entries of a webhook-signature header. Other versions (v1a is
// asymmetric) are not ours to check and are skipped, and so is a value that is not base64, so a header cannot make the
// comparison panic.
func parseSignatures(h string) [][]byte {
	var sigs [][]byte
	for _, part := range strings.Fields(h) {
		version, value, ok := strings.Cut(part, ",")
		if !ok || version != "v1" {
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(value); err == nil && len(raw) > 0 {
			sigs = append(sigs, raw)
		}
	}
	return sigs
}

// ---- Polar's event shapes: only the fields the engine needs ----

type orderObj struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	Paid              bool   `json:"paid"`
	TotalAmount       *int64 `json:"total_amount"`
	TaxAmount         *int64 `json:"tax_amount"`
	RefundedAmount    int64  `json:"refunded_amount"`
	RefundedTaxAmount int64  `json:"refunded_tax_amount"`
	Currency          string `json:"currency"`
	CustomerID        string `json:"customer_id"`
	ProductID         string `json:"product_id"`
	SubscriptionID    string `json:"subscription_id"`
}

func (o orderObj) isPaid() bool { return o.Paid || o.Status == "paid" }

type subscriptionObj struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	CustomerID         string `json:"customer_id"`
	ProductID          string `json:"product_id"`
	CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
	CurrentPeriodStart string `json:"current_period_start"`
	CurrentPeriodEnd   string `json:"current_period_end"`
	Product            *struct {
		ID string `json:"id"`
	} `json:"product"`
}

func (s subscriptionObj) product() string {
	if s.ProductID != "" {
		return s.ProductID
	}
	if s.Product != nil {
		return s.Product.ID
	}
	return ""
}

// period reads the two ends of the current period; an absent one is nil and an unreadable one is an error.
func (s subscriptionObj) period() (start, end *time.Time, err error) {
	if start, err = parseTime(s.CurrentPeriodStart); err != nil {
		return nil, nil, fmt.Errorf("polar: subscription %s: current_period_start: %w", s.ID, err)
	}
	if end, err = parseTime(s.CurrentPeriodEnd); err != nil {
		return nil, nil, fmt.Errorf("polar: subscription %s: current_period_end: %w", s.ID, err)
	}
	return start, end, nil
}

func parseTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, err
	}
	t = t.UTC()
	return &t, nil
}

func money(minor int64, currency string) *domain.Money {
	return &domain.Money{MinorUnits: minor, Currency: strings.ToUpper(currency)}
}

// normalize turns a verified body into the engine's event. A body that verified and is not an event is our adapter's
// problem to alert on: an error, never a partial Event. The message id Polar signed is the event's id: the body has none,
// and a redelivery of the same message carries the same one.
func (p *Provider) normalize(acct domain.ProviderAccount, messageID string, signedAt time.Time, body []byte) (domain.Event, error) {
	var env struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return domain.Event{}, fmt.Errorf("polar: a verified body is not a JSON event: %w", err)
	}
	if env.Type == "" || len(env.Data) == 0 || env.Data[0] != '{' {
		return domain.Event{}, errors.New("polar: a verified body is not an event: it has no type or no data object")
	}
	created := signedAt // the timestamp the signature covers, for an event that carries none of its own
	if t, err := parseTime(env.Timestamp); err != nil {
		return domain.Event{}, fmt.Errorf("polar: event %s has an unreadable timestamp: %w", messageID, err)
	} else if t != nil {
		created = *t
	}
	sum := sha256.Sum256(body)
	e := domain.Event{Provider: p.Name(), Account: acct.ID, ProviderEventID: messageID, CreatedAt: created, Type: domain.Ignored, RawHash: hex.EncodeToString(sum[:])}

	decode := func(v any) error {
		if err := json.Unmarshal(env.Data, v); err != nil {
			return fmt.Errorf("polar: event %s (%s) has an unreadable object: %w", messageID, env.Type, err)
		}
		return nil
	}
	switch env.Type {
	case "order.created", "order.paid", "order.updated", "order.refunded":
		var o orderObj
		if err := decode(&o); err != nil {
			return domain.Event{}, err
		}
		if o.ID == "" || o.Currency == "" || o.TotalAmount == nil || *o.TotalAmount < 0 || o.RefundedAmount < 0 || o.RefundedTaxAmount < 0 {
			return domain.Event{}, fmt.Errorf("polar: event %s (%s) has an order with no id, currency or total, or a negative amount", messageID, env.Type)
		}
		switch env.Type {
		case "order.created", "order.paid":
			if !o.isPaid() {
				if env.Type == "order.paid" {
					return domain.Event{}, fmt.Errorf("polar: event %s says order %s is paid and the order says it is %s", messageID, o.ID, o.Status)
				}
				return e, nil // an order created and not yet paid: order.paid will say when it is
			}
			e.Type, e.Object = domain.PaymentSucceeded, orderPayment(o)
		default:
			// A refund can arrive as order.refunded or as an update of the order; either way the order says what it has
			// been refunded to date, the money and its sales tax apart, and the customer got back both.
			refunded := o.RefundedAmount + o.RefundedTaxAmount
			if refunded == 0 {
				return e, nil
			}
			e.Type = domain.PaymentRefunded
			e.Object = domain.EventObject{ProviderCustomerID: o.CustomerID, ProviderPaymentID: o.ID, Amount: money(*o.TotalAmount, o.Currency), AmountRefunded: money(refunded, o.Currency)}
		}
	case "subscription.created", "subscription.active", "subscription.updated", "subscription.canceled", "subscription.uncanceled", "subscription.past_due",
		"subscription.paused", "subscription.resumed", "subscription.cycled", "subscription.revoked":
		var s subscriptionObj
		if err := decode(&s); err != nil {
			return domain.Event{}, err
		}
		if s.ID == "" {
			return domain.Event{}, fmt.Errorf("polar: event %s (%s) has a subscription with no id", messageID, env.Type)
		}
		start, end, err := s.period()
		if err != nil {
			return domain.Event{}, err
		}
		e.Type = domain.SubscriptionUpdated
		if env.Type == "subscription.revoked" {
			e.Type = domain.SubscriptionDeleted
		}
		e.Object = domain.EventObject{ProviderCustomerID: s.CustomerID, ProviderSubscriptionID: s.ID, ProviderPriceID: s.product(), Status: s.Status,
			CancelAtPeriodEnd: s.CancelAtPeriodEnd, PeriodStart: start, PeriodEnd: end}
	}
	// Anything else — a checkout, a refund object, a benefit grant, a customer — is Ignored: normal traffic. The order a
	// refund belongs to carries the cumulative total, and a checkout never fulfils.
	return e, nil
}

// orderPayment is a paid order as the engine's payment: keyed on the order, for what the customer was charged with the
// tax in it, and for the product (which is what the catalogue maps to a plan).
func orderPayment(o orderObj) domain.EventObject {
	obj := domain.EventObject{ProviderCustomerID: o.CustomerID, ProviderSubscriptionID: o.SubscriptionID, ProviderPaymentID: o.ID,
		ProviderPriceID: o.ProductID, Amount: money(*o.TotalAmount, o.Currency)}
	if o.TaxAmount != nil {
		obj.TaxAmount = money(*o.TaxAmount, o.Currency)
	}
	return obj
}
