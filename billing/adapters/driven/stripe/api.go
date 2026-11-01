package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// APIError is Stripe refusing a call. It never carries the API key: a message that contains it is scrubbed.
type APIError struct {
	Status    int
	Type      string // card_error, invalid_request_error, api_error, …
	Code      string // card_declined, resource_missing, …
	Message   string
	RequestID string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("stripe: HTTP %d %s", e.Status, e.Type)
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.RequestID != "" {
		s += " [request " + e.RequestID + "]"
	}
	return s
}

// Is makes a missing resource the engine's ErrNotFound.
func (e *APIError) Is(target error) bool {
	return target == md.ErrNotFound && e.Status == http.StatusNotFound
}

// call makes one API request as the account: the key comes from its reference at this moment, goes in one header, and
// is scrubbed from anything that is said about the response.
func (p *Provider) call(ctx context.Context, acct domain.ProviderAccount, method, path string, form url.Values, idemKey string) (http.Header, []byte, error) {
	key, err := p.cfg.Secrets.Resolve(ctx, acct.SecretRef)
	if err != nil {
		return nil, nil, fmt.Errorf("stripe: resolve the API key of account %s: %w", acct.ID, err)
	}
	var body io.Reader
	target := p.apiURL(path)
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, fmt.Errorf("stripe: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Stripe-Version", APIVersion)
	req.Header.Set("User-Agent", "intel-payment")
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return nil, nil, transportError(method+" "+path, err, key)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, transportError("read "+method+" "+path, err, key)
	}
	if resp.StatusCode/100 != 2 {
		ae := &APIError{Status: resp.StatusCode, RequestID: resp.Header.Get("Request-Id")}
		var env struct {
			Error struct {
				Type, Code, Message string
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			ae.Type, ae.Code, ae.Message = env.Error.Type, env.Error.Code, scrub(env.Error.Message, key)
		}
		return resp.Header, raw, ae
	}
	return resp.Header, raw, nil
}

func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// scrubbed is a transport error with the key taken out of what it says, and its cause kept: a caller still asks
// errors.Is(err, context.Canceled).
type scrubbed struct {
	msg   string
	cause error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.cause }

func transportError(what string, err error, key string) error {
	return &scrubbed{msg: "stripe: " + what + ": " + scrub(err.Error(), key), cause: err}
}

// decode reads the fields of a response the caller names.
func decode(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("stripe: unreadable response: %w", err)
	}
	return nil
}

// EnsureCustomer implements ports.PaymentProvider: one customer per idempotency key, whose metadata says which scope it
// was made for (correlation for a human reading Stripe's dashboard, never authority: the mapping is ours).
func (p *Provider) EnsureCustomer(ctx context.Context, acct domain.ProviderAccount, in ports.CustomerInput) (string, error) {
	f := url.Values{}
	if in.BillingEmail != "" {
		f.Set("email", in.BillingEmail)
	}
	f.Set("metadata[realm]", string(in.Scope.Realm.Or()))
	f.Set("metadata[scope_kind]", in.Scope.Kind)
	f.Set("metadata[scope_id]", in.Scope.ID)
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/customers", f, in.IdemKey)
	if err != nil {
		return "", err
	}
	var c struct{ ID string }
	if err := decode(raw, &c); err != nil || c.ID == "" {
		return "", errors.Join(err, errors.New("stripe: a created customer has no id"))
	}
	return c.ID, nil
}

// CreateCheckout implements ports.PaymentProvider. A one-time purchase asks Stripe for an invoice, so the invoice that
// follows payment is what fulfils it — the same event as a subscription renewal, and never the session.
func (p *Provider) CreateCheckout(ctx context.Context, acct domain.ProviderAccount, in ports.CheckoutInput) (string, error) {
	f := url.Values{}
	switch in.Mode {
	case "payment", "subscription":
		if in.ProviderPriceID == "" {
			return "", fmt.Errorf("%w: a %s checkout needs a provider price", md.ErrInvalid, in.Mode)
		}
		f.Set("line_items[0][price]", in.ProviderPriceID)
		f.Set("line_items[0][quantity]", "1")
		if in.Mode == "payment" {
			f.Set("invoice_creation[enabled]", "true")
		}
	case "setup":
		f.Set("payment_method_types[0]", "card")
	default:
		return "", fmt.Errorf("%w: checkout mode %q is not payment, subscription or setup", md.ErrInvalid, in.Mode)
	}
	f.Set("mode", in.Mode)
	f.Set("success_url", in.SuccessURL)
	f.Set("cancel_url", in.CancelURL)
	if in.CustomerID != "" {
		f.Set("customer", in.CustomerID)
	} else if in.BillingEmail != "" {
		f.Set("customer_email", in.BillingEmail)
	}
	for k, v := range in.Metadata {
		f.Set("metadata["+k+"]", v)
	}
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/checkout/sessions", f, in.IdemKey)
	if managedPaymentsRefusal(err) {
		// A new account has Managed Payments (Stripe as the merchant of record) on by default, and it needs an API version
		// later than the one this adapter is written against. Whether Stripe is the merchant of record is the deployment's
		// decision, made by its tax strategy, not something the pinned version can express — so the session opts out, and only
		// when Stripe says it must, which keeps the parameter from an account that may not know it. The retry is a different
		// request and so gets a key of its own, one that still names the first: Stripe refuses one key for two requests.
		f.Set("managed_payments[enabled]", "false")
		retryKey := ""
		if in.IdemKey != "" {
			retryKey = in.IdemKey + ":no-managed-payments"
		}
		_, raw, err = p.call(ctx, acct, http.MethodPost, "/v1/checkout/sessions", f, retryKey)
	}
	if err != nil {
		return "", err
	}
	var s struct{ URL string }
	if err := decode(raw, &s); err != nil || s.URL == "" {
		return "", errors.Join(err, errors.New("stripe: a checkout session has no url"))
	}
	return s.URL, nil
}

// managedPaymentsRefusal reports whether Stripe refused a call because the account has Managed Payments on. Stripe gives such a
// refusal no code and no param, only a message.
func managedPaymentsRefusal(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusBadRequest && ae.Type == "invalid_request_error" && strings.Contains(ae.Message, "Managed Payments")
}

// CreatePortalSession implements ports.PaymentProvider.
func (p *Provider) CreatePortalSession(ctx context.Context, acct domain.ProviderAccount, customerID, returnURL string) (string, error) {
	f := url.Values{"customer": {customerID}, "return_url": {returnURL}}
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/billing_portal/sessions", f, "")
	if err != nil {
		return "", err
	}
	var s struct{ URL string }
	if err := decode(raw, &s); err != nil || s.URL == "" {
		return "", errors.Join(err, errors.New("stripe: a portal session has no url"))
	}
	return s.URL, nil
}

// subscriptionStatus maps Stripe's statuses onto the engine's six. unpaid is past due that Stripe has stopped
// retrying; incomplete_expired is a subscription that never started.
func subscriptionStatus(s string) domain.SubStatus {
	switch s {
	case "trialing":
		return domain.Trialing
	case "active":
		return domain.Active
	case "past_due", "unpaid":
		return domain.PastDue
	case "paused":
		return domain.Paused
	case "canceled", "incomplete_expired":
		return domain.Canceled
	}
	return domain.Incomplete
}

// FetchSubscription implements ports.PaymentProvider: Stripe's current object, in the engine's terms, and the instant
// it is current as of — the Date of Stripe's own response, so state is ordered by the provider's clock and never ours.
func (p *Provider) FetchSubscription(ctx context.Context, acct domain.ProviderAccount, providerSubID string) (domain.Sub, time.Time, error) {
	hdr, raw, err := p.call(ctx, acct, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(providerSubID), nil, "")
	if err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	var s subscriptionObj
	if err := decode(raw, &s); err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	sub := domain.Sub{Account: acct.ID, ProviderSubscriptionID: s.ID, Status: subscriptionStatus(s.Status), CancelAtPeriodEnd: s.CancelAtPeriodEnd}
	if t := unixPtr(s.CurrentPeriodStart); t != nil {
		sub.CurrentPeriodStart = *t
	}
	if t := unixPtr(s.CurrentPeriodEnd); t != nil {
		sub.CurrentPeriodEnd = *t
	}
	if len(s.Items.Data) > 0 {
		plan, ok, err := p.cfg.Catalog.PlanForPrice(ctx, acct.ID, s.Items.Data[0].id())
		if err != nil {
			return domain.Sub{}, time.Time{}, fmt.Errorf("stripe: map price %s to a plan: %w", s.Items.Data[0].id(), err)
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

// ChangePlan implements ports.PaymentProvider. Proration is Stripe's math; the policy only chooses when and whether it
// applies: now with proration, now without, or at the end of the paid period through a schedule that releases itself.
func (p *Provider) ChangePlan(ctx context.Context, acct domain.ProviderAccount, providerSubID, providerPriceID string, policy domain.PlanChangePolicy) error {
	if !policy.Valid() {
		return fmt.Errorf("%w: plan change policy %q", md.ErrInvalid, policy)
	}
	path := "/v1/subscriptions/" + url.PathEscape(providerSubID)
	if policy == domain.AtPeriodEnd {
		return p.changeAtPeriodEnd(ctx, acct, providerSubID, providerPriceID)
	}
	_, raw, err := p.call(ctx, acct, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	var s subscriptionObj
	if err := decode(raw, &s); err != nil {
		return err
	}
	if len(s.Items.Data) == 0 || s.Items.Data[0].ID == "" {
		return fmt.Errorf("stripe: subscription %s has no item to change", providerSubID)
	}
	f := url.Values{"items[0][id]": {s.Items.Data[0].ID}, "items[0][price]": {providerPriceID}, "proration_behavior": {"create_prorations"}}
	if policy == domain.ImmediateNoProrate {
		f.Set("proration_behavior", "none")
	}
	_, _, err = p.call(ctx, acct, http.MethodPost, path, f, "")
	return err
}

// changeAtPeriodEnd schedules the change for when the paid period ends: the schedule keeps the current price for the
// phase already running and moves to the new one after it, then releases the subscription back to normal.
func (p *Provider) changeAtPeriodEnd(ctx context.Context, acct domain.ProviderAccount, subID, priceID string) error {
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/subscription_schedules", url.Values{"from_subscription": {subID}}, "")
	if err != nil {
		return err
	}
	var sched struct {
		ID     string `json:"id"`
		Phases []struct {
			StartDate int64 `json:"start_date"`
			EndDate   int64 `json:"end_date"`
			Items     []struct {
				Price ref `json:"price"`
			} `json:"items"`
		} `json:"phases"`
	}
	if err := decode(raw, &sched); err != nil {
		return err
	}
	if sched.ID == "" || len(sched.Phases) == 0 || len(sched.Phases[0].Items) == 0 {
		return fmt.Errorf("stripe: the schedule of %s has no current phase", subID)
	}
	cur := sched.Phases[0]
	f := url.Values{
		"end_behavior":                  {"release"},
		"phases[0][start_date]":         {strconv.FormatInt(cur.StartDate, 10)},
		"phases[0][end_date]":           {strconv.FormatInt(cur.EndDate, 10)},
		"phases[0][items][0][price]":    {string(cur.Items[0].Price)},
		"phases[0][items][0][quantity]": {"1"},
		"phases[1][items][0][price]":    {priceID},
		"phases[1][items][0][quantity]": {"1"},
	}
	_, _, err = p.call(ctx, acct, http.MethodPost, "/v1/subscription_schedules/"+url.PathEscape(sched.ID), f, "")
	return err
}

// CancelAtPeriodEnd implements ports.PaymentProvider: the flag is Stripe's, and the local status changes when the
// webhook that follows is processed.
func (p *Provider) CancelAtPeriodEnd(ctx context.Context, acct domain.ProviderAccount, providerSubID string) error {
	_, _, err := p.call(ctx, acct, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(providerSubID), url.Values{"cancel_at_period_end": {"true"}}, "")
	return err
}

// ChargeSaved implements ports.PaymentProvider: an off-session charge of a saved method, idempotent on the key so a
// duplicated trigger charges once. The credits arrive only when the verified payment is processed.
func (p *Provider) ChargeSaved(ctx context.Context, acct domain.ProviderAccount, in ports.OffSessionInput) (string, error) {
	f := url.Values{
		"amount": {strconv.FormatInt(in.Amount.MinorUnits, 10)}, "currency": {strings.ToLower(in.Amount.Currency)},
		"customer": {in.CustomerID}, "payment_method": {in.PaymentMethodRef}, "off_session": {"true"}, "confirm": {"true"},
	}
	if in.PlanCode != "" {
		f.Set("metadata[plan]", in.PlanCode)
	}
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/payment_intents", f, in.IdemKey)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && refusal(api.Status) {
			return "", fmt.Errorf("%w: %w", domain.ErrChargeDeclined, err)
		}
		return "", err
	}
	var pi struct {
		ID, Status string
	}
	if err := decode(raw, &pi); err != nil || pi.ID == "" {
		return "", errors.Join(err, errors.New("stripe: a payment intent has no id"))
	}
	switch pi.Status {
	case "succeeded", "processing":
		return pi.ID, nil
	}
	return pi.ID, fmt.Errorf("%w: payment intent %s is %s: the saved method could not be charged without the customer", domain.ErrChargeDeclined, pi.ID, pi.Status)
}

// refusal reports whether an HTTP status from Stripe is the provider refusing a request for good: a client error that
// resending the same request will not change. A 401 is our key and not their card, a 408 timed out, a 409 is the same
// idempotency key still being processed, a 429 is a rate limit, and a 5xx is Stripe's own failure; none of those says the
// charge was refused, so none is.
func refusal(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
}
