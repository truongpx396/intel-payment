package paypal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// APIError is PayPal refusing a call. It never carries the access token or the client secret.
type APIError struct {
	Status  int
	Name    string // RESOURCE_NOT_FOUND, INVALID_REQUEST, …; or the OAuth error, such as invalid_client
	Message string
	DebugID string // PayPal's id for the call, which is what its support asks for
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("paypal: HTTP %d", e.Status)
	if e.Name != "" {
		s += " " + e.Name
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.DebugID != "" {
		s += " [debug id " + e.DebugID + "]"
	}
	return s
}

// Is makes a missing resource the engine's ErrNotFound.
func (e *APIError) Is(target error) bool {
	return target == md.ErrNotFound && e.Status == http.StatusNotFound
}

// misconfigured is a failure that is ours and not PayPal's: a reference that does not resolve, credentials of the wrong
// shape. It is never an unavailable verification — retrying it would only grow the parked half — and it is the thing to
// alert on.
type misconfigured struct{ error }

func (e misconfigured) Unwrap() error { return e.error }

// tokenError is a failure to get an access token, as opposed to PayPal's answer to the call that needed one. A 400 from
// the verify call means PayPal could not make sense of the delivery; a 400 from the token call means nothing of the kind.
type tokenError struct{ error }

func (e tokenError) Unwrap() error { return e.error }

// scrubbed is a transport error with the secrets taken out of what it says, and its cause kept: a caller still asks
// errors.Is(err, context.Canceled).
type scrubbed struct {
	msg   string
	cause error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.cause }

func scrub(s string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

func transportError(what string, err error, secrets ...string) error {
	return &scrubbed{msg: "paypal: " + what + ": " + scrub(err.Error(), secrets...), cause: err}
}

// apiError reads PayPal's two error shapes: {"name", "message", "debug_id"} and the OAuth {"error", "error_description"}.
func apiError(status int, raw []byte, secrets ...string) *APIError {
	ae := &APIError{Status: status}
	var env struct {
		Name             string `json:"name"`
		Message          string `json:"message"`
		DebugID          string `json:"debug_id"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return ae
	}
	ae.Name, ae.Message, ae.DebugID = env.Name, scrub(env.Message, secrets...), env.DebugID
	if ae.Name == "" {
		ae.Name, ae.Message = env.Error, scrub(env.ErrorDescription, secrets...)
	}
	return ae
}

// credentials resolves an account's `client_id:client_secret`.
func (p *Provider) credentials(ctx context.Context, acct domain.ProviderAccount) (id, secret string, err error) {
	v, err := p.cfg.Secrets.Resolve(ctx, acct.SecretRef)
	if err != nil {
		return "", "", misconfigured{fmt.Errorf("paypal: resolve the credentials of account %s: %w", acct.ID, err)}
	}
	id, secret, ok := strings.Cut(v, ":")
	if !ok || id == "" || secret == "" {
		return "", "", misconfigured{fmt.Errorf("paypal: the credentials of account %s are not client_id:client_secret", acct.ID)}
	}
	return id, secret, nil
}

// accessToken is a token for the account: the one held, unless it is about to expire or force says it was refused.
func (p *Provider) accessToken(ctx context.Context, acct domain.ProviderAccount, force bool) (string, error) {
	id, secret, err := p.credentials(ctx, acct)
	if err != nil {
		return "", err
	}
	key := acct.ID + "|" + id
	if !force {
		p.mu.Lock()
		t, ok := p.tokens[key]
		p.mu.Unlock()
		if ok && p.cfg.Clock.Now().Add(tokenMargin).Before(t.expires) {
			return t.value, nil
		}
	}
	target, err := p.apiURL(acct, "/v1/oauth2/token")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", fmt.Errorf("paypal: build the token request: %w", err)
	}
	req.SetBasicAuth(id, secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return "", transportError("token request", err, secret, base64.StdEncoding.EncodeToString([]byte(id+":"+secret)))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", transportError("read the token response", err, secret)
	}
	if resp.StatusCode/100 != 2 {
		return "", apiError(resp.StatusCode, raw, secret)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" || tok.ExpiresIn <= 0 {
		return "", errors.New("paypal: the token response has no access token or no lifetime")
	}
	p.mu.Lock()
	p.tokens[key] = token{value: tok.AccessToken, expires: p.cfg.Clock.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
	p.mu.Unlock()
	return tok.AccessToken, nil
}

// call makes one API request as the account. A token PayPal refuses is replaced once and the call made again, not in a
// loop. The response headers come back with the body, because a subscription is dated by PayPal's own clock.
func (p *Provider) call(ctx context.Context, acct domain.ProviderAccount, method, path string, body []byte, extra http.Header) (http.Header, []byte, error) {
	target, err := p.apiURL(acct, path)
	if err != nil {
		return nil, nil, err
	}
	_, secret, err := p.credentials(ctx, acct)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; ; attempt++ {
		tok, err := p.accessToken(ctx, acct, attempt > 0)
		if err != nil {
			var cfg misconfigured
			if errors.As(err, &cfg) || errors.Is(err, md.ErrInvalid) {
				return nil, nil, err
			}
			return nil, nil, tokenError{err}
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return nil, nil, fmt.Errorf("paypal: build %s %s: %w", method, path, err)
		}
		for k, vs := range extra {
			req.Header[k] = vs
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "intel-payment")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.cfg.HTTP.Do(req)
		if err != nil {
			return nil, nil, transportError(method+" "+path, err, tok, secret)
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, nil, transportError("read "+method+" "+path, rerr, tok, secret)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue // the token was withdrawn: fetch another and ask again, once
		}
		if resp.StatusCode/100 != 2 {
			return resp.Header, raw, apiError(resp.StatusCode, raw, tok, secret)
		}
		return resp.Header, raw, nil
	}
}

func (p *Provider) postJSON(ctx context.Context, acct domain.ProviderAccount, path string, v any, idemKey string) (http.Header, []byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("paypal: encode %s: %w", path, err)
	}
	var extra http.Header
	if idemKey != "" {
		extra = http.Header{"Paypal-Request-Id": {idemKey}}
	}
	return p.call(ctx, acct, http.MethodPost, path, raw, extra)
}

// EnsureCustomer implements ports.PaymentProvider. PayPal has no customer object, so the customer is an opaque id made
// from the key: the same for the same key and different for another, with nothing of the scope in it. It is carried in the
// custom_id of an order or a subscription, and it is what the engine's own customer records map back to the scope.
func (p *Provider) EnsureCustomer(_ context.Context, _ domain.ProviderAccount, in ports.CustomerInput) (string, error) {
	if in.IdemKey == "" {
		return "", fmt.Errorf("%w: a PayPal customer is made from the key it is made under", md.ErrInvalid)
	}
	sum := sha256.Sum256([]byte(in.IdemKey))
	return "pp_" + hex.EncodeToString(sum[:])[:32], nil
}

// Limits PayPal puts on a custom_id.
const (
	maxOrderCustomID        = 255
	maxSubscriptionCustomID = 127
)

// CreateCheckout implements ports.PaymentProvider. A payment is an order the buyer approves and PayPal captures on
// approval; a subscription is a plan the buyer approves. Neither fulfils anything: the capture's or the sale's webhook does.
func (p *Provider) CreateCheckout(ctx context.Context, acct domain.ProviderAccount, in ports.CheckoutInput) (string, error) {
	switch in.Mode {
	case "payment", "subscription":
	case "setup":
		return "", fmt.Errorf("%w: PayPal has no checkout that only saves a payment method", domain.ErrUnsupported)
	default:
		return "", fmt.Errorf("%w: checkout mode %q is not payment, subscription or setup", md.ErrInvalid, in.Mode)
	}
	if in.CustomerID == "" || strings.Contains(in.CustomerID, "~") || in.ProviderPriceID == "" {
		return "", fmt.Errorf("%w: a PayPal checkout needs the customer EnsureCustomer made (no tilde in it) and the product: the capture carries back only these", md.ErrInvalid)
	}
	custom := in.CustomerID + "~" + in.ProviderPriceID
	var (
		path, rel string
		body      any
	)
	if in.Mode == "payment" {
		value, err := FormatAmount(in.Amount.MinorUnits, in.Amount.Currency)
		if err != nil || in.Amount.MinorUnits == 0 {
			return "", fmt.Errorf("%w: a payment is of something: %d %q", md.ErrInvalid, in.Amount.MinorUnits, in.Amount.Currency)
		}
		if len(custom) > maxOrderCustomID {
			return "", fmt.Errorf("%w: the custom_id of an order is at most %d characters", md.ErrInvalid, maxOrderCustomID)
		}
		unit := map[string]any{"custom_id": custom, "amount": map[string]any{"currency_code": strings.ToUpper(in.Amount.Currency), "value": value}}
		if in.PlanCode != "" {
			unit["reference_id"] = in.PlanCode
		}
		wallet := map[string]any{"experience_context": map[string]any{
			"return_url": in.SuccessURL, "cancel_url": in.CancelURL, "user_action": "PAY_NOW", "shipping_preference": "NO_SHIPPING"}}
		if in.BillingEmail != "" {
			wallet["email_address"] = in.BillingEmail
		}
		// ORDER_COMPLETE_ON_PAYMENT_APPROVAL: PayPal captures the order when the buyer approves it, so no call of ours is
		// needed between approval and payment. Without it a buyer could approve and never be charged.
		body = map[string]any{"intent": "CAPTURE", "processing_instruction": "ORDER_COMPLETE_ON_PAYMENT_APPROVAL",
			"purchase_units": []any{unit}, "payment_source": map[string]any{"paypal": wallet}}
		path, rel = "/v2/checkout/orders", "payer-action"
	} else {
		if len(custom) > maxSubscriptionCustomID {
			return "", fmt.Errorf("%w: the custom_id of a subscription is at most %d characters", md.ErrInvalid, maxSubscriptionCustomID)
		}
		sub := map[string]any{"plan_id": in.ProviderPriceID, "custom_id": custom, "application_context": map[string]any{
			"return_url": in.SuccessURL, "cancel_url": in.CancelURL, "user_action": "SUBSCRIBE_NOW", "shipping_preference": "NO_SHIPPING"}}
		if in.BillingEmail != "" {
			sub["subscriber"] = map[string]any{"email_address": in.BillingEmail}
		}
		body, path, rel = sub, "/v1/billing/subscriptions", "approve"
	}
	_, raw, err := p.postJSON(ctx, acct, path, body, in.IdemKey)
	if err != nil {
		return "", err
	}
	var created struct {
		Links []struct{ Href, Rel string } `json:"links"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("paypal: unreadable response: %w", err)
	}
	// An order made with a payment source is approved at payer-action; an older flow answers approve.
	for _, want := range []string{rel, "approve"} {
		for _, l := range created.Links {
			if l.Rel == want && l.Href != "" {
				return l.Href, nil
			}
		}
	}
	return "", errors.New("paypal: the order or subscription has no link to send the buyer to")
}

// CreatePortalSession implements ports.PaymentProvider: PayPal has no hosted page for a merchant to send a customer to.
func (p *Provider) CreatePortalSession(context.Context, domain.ProviderAccount, string, string) (string, error) {
	return "", fmt.Errorf("%w: PayPal has no customer portal", domain.ErrUnsupported)
}

// subscriptionStatus maps PayPal's statuses onto the engine's six. A subscription PayPal has suspended — for failed
// payments, or by the merchant or the buyer — is past due, which has a grace window and then lapses; one that was never
// approved or is not yet active is incomplete.
func subscriptionStatus(s string) domain.SubStatus {
	switch s {
	case "ACTIVE":
		return domain.Active
	case "SUSPENDED":
		return domain.PastDue
	case "CANCELLED", "EXPIRED":
		return domain.Canceled
	}
	return domain.Incomplete
}

type subscriptionObj struct {
	ID        string `json:"id"`
	PlanID    string `json:"plan_id"`
	Status    string `json:"status"`
	CustomID  string `json:"custom_id"`
	StartTime string `json:"start_time"`
	Billing   struct {
		NextBilling string `json:"next_billing_time"`
		LastPayment struct {
			Time string `json:"time"`
		} `json:"last_payment"`
		LastFailed struct {
			ReasonCode string `json:"reason_code"`
		} `json:"last_failed_payment"`
	} `json:"billing_info"`
}

// period is the current period of a subscription: from its last payment (or, before it has had one, its start) to its next
// billing.
func (s subscriptionObj) period() (start, end *time.Time, err error) {
	from := s.Billing.LastPayment.Time
	if from == "" {
		from = s.StartTime
	}
	if start, err = parseTime(from); err != nil {
		return nil, nil, fmt.Errorf("paypal: subscription %s: its period start: %w", s.ID, err)
	}
	if end, err = parseTime(s.Billing.NextBilling); err != nil {
		return nil, nil, fmt.Errorf("paypal: subscription %s: next_billing_time: %w", s.ID, err)
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

func (p *Provider) getSubscription(ctx context.Context, acct domain.ProviderAccount, id string) (http.Header, subscriptionObj, error) {
	hdr, raw, err := p.call(ctx, acct, http.MethodGet, "/v1/billing/subscriptions/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return nil, subscriptionObj{}, err
	}
	var s subscriptionObj
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, subscriptionObj{}, fmt.Errorf("paypal: unreadable response: %w", err)
	}
	return hdr, s, nil
}

// FetchSubscription implements ports.PaymentProvider: PayPal's current object, in the engine's terms, and the instant it
// is current as of — the Date of PayPal's own response, so state is ordered by the provider's clock and never ours.
func (p *Provider) FetchSubscription(ctx context.Context, acct domain.ProviderAccount, providerSubID string) (domain.Sub, time.Time, error) {
	hdr, s, err := p.getSubscription(ctx, acct, providerSubID)
	if err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	start, end, err := s.period()
	if err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	sub := domain.Sub{Account: acct.ID, ProviderSubscriptionID: s.ID, Status: subscriptionStatus(s.Status)}
	if start != nil {
		sub.CurrentPeriodStart = *start
	}
	if end != nil {
		sub.CurrentPeriodEnd = *end
	}
	if s.PlanID != "" {
		plan, ok, err := p.cfg.Catalog.PlanForPrice(ctx, acct.ID, s.PlanID)
		if err != nil {
			return domain.Sub{}, time.Time{}, fmt.Errorf("paypal: map plan %s to a plan: %w", s.PlanID, err)
		}
		if ok {
			sub.PlanCode = plan.Code
		}
	}
	at, err := http.ParseTime(hdr.Get("Date"))
	if err != nil {
		at = p.cfg.Clock.Now()
	}
	return sub, at.UTC(), nil
}

// ChangePlan implements ports.PaymentProvider. PayPal changes a subscription's plan by revising it, which needs the
// buyer's approval at a link the port has no way to hand back; so no policy can be honoured, and none is approximated.
func (p *Provider) ChangePlan(_ context.Context, _ domain.ProviderAccount, _, _ string, policy domain.PlanChangePolicy) error {
	if !policy.Valid() {
		return fmt.Errorf("%w: plan change policy %q", md.ErrInvalid, policy)
	}
	return fmt.Errorf("%w: PayPal changes a plan only with the buyer's approval", domain.ErrUnsupported)
}

// CancelAtPeriodEnd implements ports.PaymentProvider: PayPal cancels a subscription at once, never at the period's end.
func (p *Provider) CancelAtPeriodEnd(context.Context, domain.ProviderAccount, string) error {
	return fmt.Errorf("%w: PayPal cancels a subscription at once, not at the end of the period", domain.ErrUnsupported)
}

// ChargeSaved implements ports.PaymentProvider: this adapter saves no payment method.
func (p *Provider) ChargeSaved(context.Context, domain.ProviderAccount, ports.OffSessionInput) (string, error) {
	return "", fmt.Errorf("%w: PayPal cannot charge a saved payment method", domain.ErrUnsupported)
}
