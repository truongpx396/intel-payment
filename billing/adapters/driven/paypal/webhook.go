package paypal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

const verifyPath = "/v1/notifications/verify-webhook-signature"

// unavailable is a verification that could not run: the delivery is neither genuine nor forged, and is parked.
func unavailable(err error) error {
	return fmt.Errorf("%w: %w", domain.ErrVerificationUnavailable, err)
}

// VerifyWebhook implements ports.PaymentProvider. PayPal signs with a certificate this package never sees, so PayPal
// verifies: the delivery's headers, the account's webhook id and the event are sent to its verify call. Before that, what
// can be refused without PayPal is — and the event is sent only if it is exactly one JSON value. Nothing is parsed until
// PayPal has said SUCCESS. It returns an error and never a partial Event.
func (p *Provider) VerifyWebhook(ctx context.Context, acct domain.ProviderAccount, body []byte, headers http.Header) (domain.Event, error) {
	webhookID, err := p.cfg.Secrets.Resolve(ctx, acct.WebhookSecretRef)
	if err != nil || webhookID == "" {
		// Our misconfiguration: neither a forged delivery nor an outage of PayPal's. The error names the reference,
		// never the value.
		return domain.Event{}, misconfigured{fmt.Errorf("paypal: resolve the webhook id of account %s: %w", acct.ID, errors.Join(err, errors.New("it must not be empty")))}
	}
	algo, certURL := headers.Get("PAYPAL-AUTH-ALGO"), headers.Get("PAYPAL-CERT-URL")
	id, sig, sent := headers.Get("PAYPAL-TRANSMISSION-ID"), headers.Get("PAYPAL-TRANSMISSION-SIG"), headers.Get("PAYPAL-TRANSMISSION-TIME")
	if algo == "" || certURL == "" || id == "" || sig == "" || sent == "" {
		return domain.Event{}, fmt.Errorf("%w: a PAYPAL-* header is missing", domain.ErrInvalidSignature)
	}
	at, err := time.Parse(time.RFC3339Nano, sent)
	if err != nil {
		return domain.Event{}, fmt.Errorf("%w: PAYPAL-TRANSMISSION-TIME is not a time", domain.ErrInvalidSignature)
	}
	// Measured from when the delivery arrived: one parked while PayPal was down is verified later, and the provider will not
	// send it again.
	age := ports.ReceivedAt(ctx, p.cfg.Clock.Now()).Sub(at)
	if age < 0 {
		age = -age
	}
	if age > p.cfg.Tolerance {
		return domain.Event{}, fmt.Errorf("%w: the delivery was transmitted %s from when it arrived, beyond the %s tolerance", domain.ErrInvalidSignature, age.Round(time.Second), p.cfg.Tolerance)
	}
	// The event is spliced into PayPal's request as it is, so it must be exactly one JSON value: a body such as
	// `null,"webhook_id":"…"` would put a webhook id of the sender's choosing into the request, and PayPal would vouch for an
	// event it signed for the sender's own webhook. This checks syntax only; nothing is read out of the body yet.
	if !json.Valid(body) {
		return domain.Event{}, fmt.Errorf("%w: the body is not a single JSON value", domain.ErrInvalidSignature)
	}
	if err := p.verifyRemotely(ctx, acct, verifyRequest(algo, certURL, id, sig, sent, webhookID, body)); err != nil {
		return domain.Event{}, err
	}
	return p.normalize(ctx, acct, body)
}

// verifyRequest is the body of PayPal's verify call. The event is the delivery's bytes, untouched: PayPal checks the
// signature of exactly what it sent, so it is not decoded and encoded again.
func verifyRequest(algo, certURL, transmissionID, sig, sent, webhookID string, event []byte) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for _, kv := range [][2]string{{"auth_algo", algo}, {"cert_url", certURL}, {"transmission_id", transmissionID}, {"transmission_sig", sig},
		{"transmission_time", sent}, {"webhook_id", webhookID}} {
		k, _ := json.Marshal(kv[0])
		v, _ := json.Marshal(kv[1])
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
		b.WriteByte(',')
	}
	b.WriteString(`"webhook_event":`)
	b.Write(event)
	b.WriteByte('}')
	return b.Bytes()
}

// verifyRemotely asks PayPal. A verdict that is not SUCCESS, and a request PayPal cannot make sense of, are a delivery that
// does not verify. Everything else — PayPal down, slow, rate-limiting, refusing our token — is a verification that could
// not run, and the delivery waits. A failure that is ours is returned as it is.
func (p *Provider) verifyRemotely(ctx context.Context, acct domain.ProviderAccount, req []byte) error {
	_, raw, err := p.call(ctx, acct, http.MethodPost, verifyPath, req, nil)
	if err != nil {
		var cfg misconfigured
		var tok tokenError
		var api *APIError
		switch {
		case errors.As(err, &cfg), errors.Is(err, md.ErrInvalid):
			return err
		case !errors.As(err, &tok) && errors.As(err, &api) && (api.Status == http.StatusBadRequest || api.Status == http.StatusUnprocessableEntity):
			return fmt.Errorf("%w: PayPal could not make sense of the delivery: %w", domain.ErrInvalidSignature, err)
		}
		return unavailable(err)
	}
	var verdict struct {
		Status string `json:"verification_status"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return unavailable(fmt.Errorf("paypal: unreadable verdict: %w", err))
	}
	if verdict.Status != "SUCCESS" {
		return fmt.Errorf("%w: PayPal does not vouch for the delivery (%q)", domain.ErrInvalidSignature, verdict.Status)
	}
	return nil
}

// ---- PayPal's event shapes: only the fields the engine needs ----

type amountObj struct {
	CurrencyCode string `json:"currency_code"`
	Value        string `json:"value"`
}

func (a *amountObj) money() (*domain.Money, error) {
	if a == nil {
		return nil, fmt.Errorf("%w: there is no amount", md.ErrInvalid)
	}
	return moneyOf(a.Value, a.CurrencyCode)
}

func moneyOf(value, currency string) (*domain.Money, error) {
	minor, err := ParseAmount(value, currency)
	if err != nil {
		return nil, err
	}
	return &domain.Money{MinorUnits: minor, Currency: strings.ToUpper(currency)}, nil
}

type link struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
}

// capturePaymentObj is a capture, a refund of one, or the reversal of one: they share the fields the engine reads.
type capturePaymentObj struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Amount    *amountObj `json:"amount"`
	CustomID  string     `json:"custom_id"`
	Links     []link     `json:"links"`
	Breakdown struct {
		TotalRefunded *amountObj `json:"total_refunded_amount"`
	} `json:"seller_payable_breakdown"`
}

// captureID is the capture a refund or a reversal is of: the resource's "up" link, which names the transaction it
// belongs to.
func (c capturePaymentObj) captureID() string {
	for _, l := range c.Links {
		if _, rest, ok := strings.Cut(l.Href, "/captures/"); ok && l.Rel == "up" {
			id, _, _ := strings.Cut(rest, "?")
			return strings.Trim(id, "/")
		}
	}
	return ""
}

type saleObj struct {
	ID                 string `json:"id"`
	BillingAgreementID string `json:"billing_agreement_id"`
	Amount             struct {
		Total    string `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

type disputeObj struct {
	DisputeID    string `json:"dispute_id"`
	Transactions []struct {
		SellerTransactionID string `json:"seller_transaction_id"`
	} `json:"disputed_transactions"`
	Outcome struct {
		Code string `json:"outcome_code"`
	} `json:"dispute_outcome"`
}

// splitCustom reads the custom_id an order or subscription was made with: `<customer>~<product>`, the customer having no
// tilde. Anything else is not something this service made, and is not guessed at.
func splitCustom(s string) (customer, product string, ours bool) {
	customer, product, found := strings.Cut(s, "~")
	return customer, product, found && customer != "" && product != ""
}

// disputeOutcome is who PayPal decided for. An outcome this adapter has not been told about is not guessed: money rides
// on it.
func disputeOutcome(code string) (string, bool) {
	switch code {
	case "RESOLVED_BUYER_FAVOUR", "ACCEPTED":
		return "lost", true
	case "RESOLVED_SELLER_FAVOUR", "RESOLVED_WITH_PAYOUT", "CANCELED_BY_BUYER", "DENIED":
		return "won", true
	}
	return "", false
}

// normalize turns a verified body into the engine's event. A body that verified and is not an event is our adapter's
// problem to alert on: an error, never a partial Event. The id PayPal gave the event is the event's id, and a redelivery of
// it carries the same one.
func (p *Provider) normalize(ctx context.Context, acct domain.ProviderAccount, body []byte) (domain.Event, error) {
	var env struct {
		ID         string          `json:"id"`
		EventType  string          `json:"event_type"`
		CreateTime string          `json:"create_time"`
		Resource   json.RawMessage `json:"resource"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return domain.Event{}, fmt.Errorf("paypal: a verified body is not a JSON event: %w", err)
	}
	if env.ID == "" || env.EventType == "" || env.CreateTime == "" || len(env.Resource) == 0 || env.Resource[0] != '{' {
		return domain.Event{}, errors.New("paypal: a verified body is not an event: it has no id, event_type, create_time or resource object")
	}
	created, err := parseTime(env.CreateTime)
	if err != nil {
		return domain.Event{}, fmt.Errorf("paypal: event %s has an unreadable create_time: %w", env.ID, err)
	}
	sum := sha256.Sum256(body)
	e := domain.Event{Provider: p.Name(), Account: acct.ID, ProviderEventID: env.ID, CreatedAt: *created, Type: domain.Ignored, RawHash: hex.EncodeToString(sum[:])}

	decode := func(v any) error {
		if err := json.Unmarshal(env.Resource, v); err != nil {
			return fmt.Errorf("paypal: event %s (%s) has an unreadable resource: %w", env.ID, env.EventType, err)
		}
		return nil
	}
	switch env.EventType {
	case "PAYMENT.CAPTURE.COMPLETED", "PAYMENT.CAPTURE.DENIED", "PAYMENT.CAPTURE.DECLINED":
		var c capturePaymentObj
		if err := decode(&c); err != nil {
			return domain.Event{}, err
		}
		customer, product, ours := splitCustom(c.CustomID)
		if !ours {
			return e, nil // a sale of this PayPal account's that this service did not make
		}
		amount, err := c.Amount.money()
		if err != nil || c.ID == "" {
			return domain.Event{}, fmt.Errorf("paypal: event %s (%s) has a capture with no id or no readable amount: %w", env.ID, env.EventType, errors.Join(err, md.ErrInvalid))
		}
		obj := domain.EventObject{ProviderCustomerID: customer, ProviderPaymentID: c.ID, ProviderPriceID: product, Amount: amount}
		if env.EventType == "PAYMENT.CAPTURE.COMPLETED" {
			if c.Status != "COMPLETED" {
				return domain.Event{}, fmt.Errorf("paypal: event %s says capture %s completed and the capture says %q", env.ID, c.ID, c.Status)
			}
			e.Type, e.Object = domain.PaymentSucceeded, obj
		} else {
			obj.FailureReason = strings.ToLower(strings.TrimPrefix(env.EventType, "PAYMENT.CAPTURE."))
			e.Type, e.Object = domain.PaymentFailed, obj
		}
	case "PAYMENT.CAPTURE.REFUNDED":
		var r capturePaymentObj
		if err := decode(&r); err != nil {
			return domain.Event{}, err
		}
		total, err := r.Breakdown.TotalRefunded.money()
		capture := r.captureID()
		if err != nil || r.ID == "" || capture == "" {
			return domain.Event{}, fmt.Errorf("paypal: event %s has a refund with no id, no capture or no total refunded to date: %w", env.ID, errors.Join(err, md.ErrInvalid))
		}
		// The total refunded from the capture to date, not this refund alone: a per-refund figure would revoke too little.
		e.Type = domain.PaymentRefunded
		e.Object = domain.EventObject{ProviderPaymentID: capture, ProviderRefundID: r.ID, AmountRefunded: total}
	case "PAYMENT.CAPTURE.REVERSED":
		var c capturePaymentObj
		if err := decode(&c); err != nil {
			return domain.Event{}, err
		}
		capture := c.captureID()
		if capture == "" {
			capture = c.ID
		}
		if capture == "" {
			return domain.Event{}, fmt.Errorf("paypal: event %s reverses a capture it does not name", env.ID)
		}
		e.Type = domain.DisputeFundsWithdrawn
		e.Object = domain.EventObject{ProviderPaymentID: capture, DisputeID: disputeKey(capture)}
	case "PAYMENT.SALE.COMPLETED":
		var s saleObj
		if err := decode(&s); err != nil {
			return domain.Event{}, err
		}
		if s.BillingAgreementID == "" {
			return e, nil // a sale that is not a subscription's payment
		}
		// A sale names its subscription and nothing else: the customer and the plan are PayPal's to say.
		_, sub, err := p.getSubscription(ctx, acct, s.BillingAgreementID)
		if err != nil {
			return domain.Event{}, p.readFailure(err, fmt.Sprintf("sale %s names subscription %s", s.ID, s.BillingAgreementID))
		}
		customer, _, ours := splitCustom(sub.CustomID)
		if !ours {
			return e, nil
		}
		amount, err := moneyOf(s.Amount.Total, s.Amount.Currency)
		if err != nil || s.ID == "" {
			return domain.Event{}, fmt.Errorf("paypal: event %s has a sale with no id or no readable amount: %w", env.ID, errors.Join(err, md.ErrInvalid))
		}
		e.Type = domain.PaymentSucceeded
		e.Object = domain.EventObject{ProviderCustomerID: customer, ProviderPaymentID: s.ID, ProviderSubscriptionID: sub.ID, ProviderPriceID: sub.PlanID, Amount: amount}
	case "BILLING.SUBSCRIPTION.CREATED", "BILLING.SUBSCRIPTION.ACTIVATED", "BILLING.SUBSCRIPTION.UPDATED", "BILLING.SUBSCRIPTION.SUSPENDED",
		"BILLING.SUBSCRIPTION.RE-ACTIVATED", "BILLING.SUBSCRIPTION.CANCELLED", "BILLING.SUBSCRIPTION.EXPIRED", "BILLING.SUBSCRIPTION.PAYMENT.FAILED":
		var s subscriptionObj
		if err := decode(&s); err != nil {
			return domain.Event{}, err
		}
		if s.ID == "" {
			return domain.Event{}, fmt.Errorf("paypal: event %s (%s) has a subscription with no id", env.ID, env.EventType)
		}
		customer, _, ours := splitCustom(s.CustomID)
		if !ours {
			return e, nil
		}
		start, end, err := s.period()
		if err != nil {
			return domain.Event{}, err
		}
		obj := domain.EventObject{ProviderCustomerID: customer, ProviderSubscriptionID: s.ID, ProviderPriceID: s.PlanID, Status: s.Status, PeriodStart: start, PeriodEnd: end}
		switch env.EventType {
		case "BILLING.SUBSCRIPTION.PAYMENT.FAILED":
			obj.FailureReason = firstNonEmpty(s.Billing.LastFailed.ReasonCode, "payment_failed")
			e.Type = domain.PaymentFailed
		case "BILLING.SUBSCRIPTION.CANCELLED", "BILLING.SUBSCRIPTION.EXPIRED":
			e.Type = domain.SubscriptionDeleted
		default:
			e.Type = domain.SubscriptionUpdated
		}
		e.Object = obj
	case "CUSTOMER.DISPUTE.CREATED", "CUSTOMER.DISPUTE.RESOLVED":
		var d disputeObj
		if err := decode(&d); err != nil {
			return domain.Event{}, err
		}
		if len(d.Transactions) == 0 || d.Transactions[0].SellerTransactionID == "" {
			return domain.Event{}, fmt.Errorf("paypal: event %s (%s) has a dispute that names no transaction", env.ID, env.EventType)
		}
		tx := d.Transactions[0].SellerTransactionID
		obj := domain.EventObject{ProviderPaymentID: tx, DisputeID: disputeKey(tx)}
		if env.EventType == "CUSTOMER.DISPUTE.CREATED" {
			e.Type = domain.DisputeOpened
		} else {
			status, known := disputeOutcome(d.Outcome.Code)
			if !known {
				return domain.Event{}, fmt.Errorf("paypal: event %s resolves dispute %s with outcome %q, which this adapter has not been told how to settle", env.ID, d.DisputeID, d.Outcome.Code)
			}
			e.Type, obj.Status = domain.DisputeClosed, status
		}
		e.Object = obj
	}
	// Anything else — an approved order (a checkout return never fulfils), a pending capture, a dispute update, a refund of a
	// subscription's payment (PayPal reports no total), a catalogue change — is Ignored: normal traffic.
	return e, nil
}

// disputeKey is the id every phase of a dispute shares. PayPal's own dispute id is on only some of the events of one
// dispute, so the id is the transaction's, and the clawback and the reinstatement find each other.
func disputeKey(transaction string) string { return "dp:" + transaction }

// readFailure classifies a failure to read something while normalizing a verified delivery: PayPal not answering parks the
// delivery to be tried again, and the rest is ours or PayPal's to explain.
func (p *Provider) readFailure(err error, what string) error {
	var cfg misconfigured
	var api *APIError
	switch {
	case errors.As(err, &cfg), errors.Is(err, md.ErrInvalid):
		return err
	case errors.As(err, &api) && api.Status == http.StatusNotFound:
		return fmt.Errorf("paypal: %s, which PayPal does not have: %w", what, err)
	}
	return unavailable(err)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
