package polar

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// APIError is Polar refusing a call. It never carries the access token: a message that contains it is scrubbed.
type APIError struct {
	Status  int
	Type    string // ResourceNotFound, AlreadyCanceledSubscription, …; empty for a validation error
	Message string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("polar: HTTP %d", e.Status)
	if e.Type != "" {
		s += " " + e.Type
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is makes a missing resource the engine's ErrNotFound.
func (e *APIError) Is(target error) bool {
	return target == md.ErrNotFound && e.Status == http.StatusNotFound
}

// call makes one API request as the account: the token comes from its reference at this moment, goes in one header, and
// is scrubbed from anything that is said about the response.
func (p *Provider) call(ctx context.Context, acct domain.ProviderAccount, method, path string, in any) (http.Header, []byte, error) {
	target, err := p.apiURL(acct, path)
	if err != nil {
		return nil, nil, err
	}
	token, err := p.cfg.Secrets.Resolve(ctx, acct.SecretRef)
	if err != nil {
		return nil, nil, fmt.Errorf("polar: resolve the access token of account %s: %w", acct.ID, err)
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, nil, fmt.Errorf("polar: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, fmt.Errorf("polar: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "intel-payment")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return nil, nil, transportError(method+" "+path, err, token)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, transportError("read "+method+" "+path, err, token)
	}
	if resp.StatusCode/100 != 2 {
		return resp.Header, raw, apiError(resp.StatusCode, raw, token)
	}
	return resp.Header, raw, nil
}

// apiError reads Polar's two error shapes: {"error": "ResourceNotFound", "detail": "…"} and a validation error whose
// detail is a list of {"loc", "msg", "type"}.
func apiError(status int, raw []byte, token string) *APIError {
	ae := &APIError{Status: status}
	var env struct {
		Error  string          `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return ae
	}
	ae.Type = env.Error
	var msg string
	var list []struct {
		Msg string `json:"msg"`
	}
	switch {
	case json.Unmarshal(env.Detail, &msg) == nil:
		ae.Message = scrub(msg, token)
	case json.Unmarshal(env.Detail, &list) == nil:
		var msgs []string
		for _, d := range list {
			msgs = append(msgs, d.Msg)
		}
		ae.Message = scrub(strings.Join(msgs, "; "), token)
	}
	return ae
}

func scrub(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// scrubbed is a transport error with the token taken out of what it says, and its cause kept: a caller still asks
// errors.Is(err, context.Canceled).
type scrubbed struct {
	msg   string
	cause error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.cause }

func transportError(what string, err error, token string) error {
	return &scrubbed{msg: "polar: " + what + ": " + scrub(err.Error(), token), cause: err}
}

// decode reads the fields of a response the caller names.
func decode(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("polar: unreadable response: %w", err)
	}
	return nil
}

// externalID is the id Polar keeps a customer under for our key. It goes in a URL path to find the customer again, and
// a key such as customer:polar-a:default/workspace:w1 has a slash in it; so it is the hex of the key's SHA-256: the same
// for the same key, different for a different one, and safe in a path. Which scope it is for is in the metadata.
func externalID(idemKey string) string {
	sum := sha256.Sum256([]byte(idemKey))
	return hex.EncodeToString(sum[:])
}

// EnsureCustomer implements ports.PaymentProvider: one customer per idempotency key. Polar rejects a second customer
// with the same external id, and the same key finds the first; but it also rejects an email already used in the
// organization, and then there is no customer of this key to find — the error is returned as it is, and the scope is not
// handed another scope's customer.
func (p *Provider) EnsureCustomer(ctx context.Context, acct domain.ProviderAccount, in ports.CustomerInput) (string, error) {
	if in.BillingEmail == "" || in.IdemKey == "" {
		return "", fmt.Errorf("%w: a Polar customer needs a billing email and the key it is made under", md.ErrInvalid)
	}
	ext := externalID(in.IdemKey)
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/customers/", map[string]any{
		"email": in.BillingEmail, "external_id": ext,
		"metadata": map[string]string{"realm": string(in.Scope.Realm.Or()), "scope_kind": in.Scope.Kind, "scope_id": in.Scope.ID},
	})
	if err != nil {
		var api *APIError
		if !errors.As(err, &api) || api.Status != http.StatusUnprocessableEntity {
			return "", err
		}
		if id, found := p.customerByExternalID(ctx, acct, ext); found {
			return id, nil
		}
		return "", err
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := decode(raw, &c); err != nil || c.ID == "" {
		return "", errors.Join(err, errors.New("polar: a created customer has no id"))
	}
	return c.ID, nil
}

func (p *Provider) customerByExternalID(ctx context.Context, acct domain.ProviderAccount, ext string) (string, bool) {
	_, raw, err := p.call(ctx, acct, http.MethodGet, "/v1/customers/external/"+url.PathEscape(ext), nil)
	if err != nil {
		return "", false
	}
	var c struct {
		ID string `json:"id"`
	}
	if decode(raw, &c) != nil || c.ID == "" {
		return "", false
	}
	return c.ID, true
}

// CreateCheckout implements ports.PaymentProvider. Polar decides from the product whether a purchase recurs, so the
// mode is only checked. The catalogue's price is sent as well as the product: Polar honours it for a product whose price
// is set by the caller and ignores it for a fixed one, and in both the order that follows is what fulfils.
func (p *Provider) CreateCheckout(ctx context.Context, acct domain.ProviderAccount, in ports.CheckoutInput) (string, error) {
	switch in.Mode {
	case "payment", "subscription":
	case "setup":
		return "", fmt.Errorf("%w: Polar has no checkout that only saves a payment method", domain.ErrUnsupported)
	default:
		return "", fmt.Errorf("%w: checkout mode %q is not payment, subscription or setup", md.ErrInvalid, in.Mode)
	}
	if in.ProviderPriceID == "" {
		return "", fmt.Errorf("%w: a %s checkout needs a Polar product", md.ErrInvalid, in.Mode)
	}
	body := map[string]any{"products": []string{in.ProviderPriceID}, "success_url": in.SuccessURL}
	if in.CancelURL != "" {
		body["return_url"] = in.CancelURL
	}
	if in.CustomerID != "" {
		body["customer_id"] = in.CustomerID
	} else if in.BillingEmail != "" {
		body["customer_email"] = in.BillingEmail
	}
	if len(in.Metadata) > 0 {
		body["metadata"] = in.Metadata
	}
	if in.Amount.MinorUnits > 0 {
		body["amount"] = in.Amount.MinorUnits
		body["currency"] = strings.ToLower(in.Amount.Currency)
	}
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/checkouts/", body)
	if err != nil {
		return "", err
	}
	var s struct {
		URL string `json:"url"`
	}
	if err := decode(raw, &s); err != nil || s.URL == "" {
		return "", errors.Join(err, errors.New("polar: a checkout has no url"))
	}
	return s.URL, nil
}

// CreatePortalSession implements ports.PaymentProvider.
func (p *Provider) CreatePortalSession(ctx context.Context, acct domain.ProviderAccount, customerID, returnURL string) (string, error) {
	_, raw, err := p.call(ctx, acct, http.MethodPost, "/v1/customer-sessions/", map[string]any{"customer_id": customerID, "return_url": returnURL})
	if err != nil {
		return "", err
	}
	var s struct {
		URL string `json:"customer_portal_url"`
	}
	if err := decode(raw, &s); err != nil || s.URL == "" {
		return "", errors.Join(err, errors.New("polar: a customer session has no portal url"))
	}
	return s.URL, nil
}

// subscriptionStatus maps Polar's statuses onto the engine's six; they are Stripe's. unpaid is past due that Polar has
// stopped retrying; incomplete_expired is a subscription that never started.
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

// FetchSubscription implements ports.PaymentProvider: Polar's current object, in the engine's terms, and the instant it
// is current as of — the Date of Polar's own response, so state is ordered by the provider's clock and never ours.
func (p *Provider) FetchSubscription(ctx context.Context, acct domain.ProviderAccount, providerSubID string) (domain.Sub, time.Time, error) {
	hdr, raw, err := p.call(ctx, acct, http.MethodGet, "/v1/subscriptions/"+url.PathEscape(providerSubID), nil)
	if err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	var s subscriptionObj
	if err := decode(raw, &s); err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	start, end, err := s.period()
	if err != nil {
		return domain.Sub{}, time.Time{}, err
	}
	sub := domain.Sub{Account: acct.ID, ProviderSubscriptionID: s.ID, Status: subscriptionStatus(s.Status), CancelAtPeriodEnd: s.CancelAtPeriodEnd}
	if start != nil {
		sub.CurrentPeriodStart = *start
	}
	if end != nil {
		sub.CurrentPeriodEnd = *end
	}
	if product := s.product(); product != "" {
		plan, ok, err := p.cfg.Catalog.PlanForPrice(ctx, acct.ID, product)
		if err != nil {
			return domain.Sub{}, time.Time{}, fmt.Errorf("polar: map product %s to a plan: %w", product, err)
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

// ChangePlan implements ports.PaymentProvider. Proration is Polar's math; the policy only chooses when and whether it
// applies. Polar can change the plan at the end of the paid period (next_period, a pending change) and now with the
// difference prorated (prorate); its only way to change it now without proration restarts the billing cycle at the full
// price of the new plan, which is not what that policy says, so that policy is refused and nothing is approximated.
func (p *Provider) ChangePlan(ctx context.Context, acct domain.ProviderAccount, providerSubID, providerPriceID string, policy domain.PlanChangePolicy) error {
	if !policy.Valid() {
		return fmt.Errorf("%w: plan change policy %q", md.ErrInvalid, policy)
	}
	if providerPriceID == "" {
		return fmt.Errorf("%w: a plan change needs the Polar product to change to", md.ErrInvalid)
	}
	var behavior string
	switch policy {
	case domain.AtPeriodEnd:
		behavior = "next_period"
	case domain.ImmediateProrate:
		behavior = "prorate"
	default:
		return fmt.Errorf("%w: Polar cannot change a plan now without prorating it", domain.ErrUnsupported)
	}
	_, _, err := p.call(ctx, acct, http.MethodPatch, "/v1/subscriptions/"+url.PathEscape(providerSubID),
		map[string]any{"product_id": providerPriceID, "proration_behavior": behavior})
	return err
}

// CancelAtPeriodEnd implements ports.PaymentProvider: the flag is Polar's, and the local status changes when the
// webhook that follows is processed. Polar refuses to cancel a subscription that is already canceled or set to cancel;
// the state asked for holds, so that is not an error.
func (p *Provider) CancelAtPeriodEnd(ctx context.Context, acct domain.ProviderAccount, providerSubID string) error {
	_, _, err := p.call(ctx, acct, http.MethodPatch, "/v1/subscriptions/"+url.PathEscape(providerSubID), map[string]any{"cancel_at_period_end": true})
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusForbidden && api.Type == "AlreadyCanceledSubscription" {
		return nil
	}
	return err
}

// ChargeSaved implements ports.PaymentProvider: Polar has no off-session charge of a saved method.
func (p *Provider) ChargeSaved(context.Context, domain.ProviderAccount, ports.OffSessionInput) (string, error) {
	return "", fmt.Errorf("%w: Polar cannot charge a saved payment method", domain.ErrUnsupported)
}
