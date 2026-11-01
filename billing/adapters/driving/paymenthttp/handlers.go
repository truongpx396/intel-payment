package paymenthttp

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func rfc3339Ptr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := rfc3339(*t)
	return &s
}

// ----------------------------------------------------------------------- plans --

type entitlementJSON struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Value any    `json:"value"`
}

type planJSON struct {
	Code            string            `json:"code"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	CreditAllotment *int64            `json:"credit_allotment"`
	Pool            string            `json:"pool"`
	Interval        *string           `json:"interval,omitempty"`
	SortOrder       int               `json:"sort_order"`
	Prices          map[string]int64  `json:"prices"`
	Entitlements    []entitlementJSON `json:"entitlements,omitempty"`
}

func planToJSON(p domain.Plan) planJSON {
	out := planJSON{Code: p.Code, Name: p.Name, Kind: string(p.Kind), Pool: string(p.Pool), SortOrder: p.SortOrder, Prices: make(map[string]int64, len(p.Prices))}
	if p.CreditAllotment != nil {
		v := int64(*p.CreditAllotment)
		out.CreditAllotment = &v
	}
	if p.Interval != nil {
		v := string(*p.Interval)
		out.Interval = &v
	}
	for cur, m := range p.Prices {
		out.Prices[cur] = m.MinorUnits
	}
	for _, e := range p.Entitlements {
		j := entitlementJSON{Key: e.Key, Kind: e.Kind}
		switch {
		case e.Bool != nil:
			j.Value = *e.Bool
		case e.Int != nil:
			j.Value = *e.Int
		case e.Text != nil:
			j.Value = *e.Text
		}
		out.Entitlements = append(out.Entitlements, j)
	}
	return out
}

// plans lists what the credential's realm sells in a currency: slugs, prices, allotments and entitlements — never a
// provider's price id.
func (d *Door) plans(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cur := q.Get("currency")
	if !currencyCode.MatchString(cur) {
		d.fail(w, r, "plans", invalid("currency is required, an upper-case ISO 4217 code such as USD, got %q", cur))
		return
	}
	scope, err := d.cfg.Caller.Resolve(r, domain.Scope{Realm: domain.Realm(q.Get("realm"))})
	if err != nil {
		d.fail(w, r, "plans", err)
		return
	}
	plans, err := d.cfg.Catalog.List(r.Context(), scope.Realm, cur)
	if err != nil {
		d.fail(w, r, "plans", err)
		return
	}
	out := struct {
		Data []planJSON `json:"data"`
	}{Data: make([]planJSON, 0, len(plans))}
	for _, p := range plans {
		out.Data = append(out.Data, planToJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// -------------------------------------------------------------------- checkout --

type checkoutRequest struct {
	Scope        *scopeJSON `json:"scope"`
	PlanCode     string     `json:"plan_code"`
	Currency     string     `json:"currency"`
	Mode         string     `json:"mode"`
	Account      string     `json:"account"`
	SuccessURL   string     `json:"success_url"`
	CancelURL    string     `json:"cancel_url"`
	BillingEmail string     `json:"billing_email"`
}

// checkout starts a hosted checkout and answers where to send the buyer. It never grants: the door has no Meter.
func (d *Door) checkout(w http.ResponseWriter, r *http.Request) {
	var req checkoutRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "checkout", err)
		return
	}
	key, err := idemKey(r)
	if err != nil {
		d.fail(w, r, "checkout", err)
		return
	}
	scope, err := d.scopeOf(r, req.Scope)
	if err != nil {
		d.fail(w, r, "checkout", err)
		return
	}
	redirect, err := d.cfg.Checkout.Start(r.Context(), ports.CheckoutRequest{
		Scope: scope, PlanCode: req.PlanCode, Currency: req.Currency, Mode: req.Mode, Account: req.Account,
		SuccessURL: req.SuccessURL, CancelURL: req.CancelURL, BillingEmail: req.BillingEmail, IdemKey: key,
	})
	if err != nil {
		d.fail(w, r, "checkout", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		RedirectURL string `json:"redirect_url"`
	}{redirect})
}

// portal is the provider's page for a scope that has bought before.
func (d *Door) portal(w http.ResponseWriter, r *http.Request) {
	scope, err := d.scopeFromQuery(r)
	if err != nil {
		d.fail(w, r, "portal", err)
		return
	}
	q := r.URL.Query()
	if q.Get("return_url") == "" {
		d.fail(w, r, "portal", required("return_url"))
		return
	}
	url, err := d.cfg.Checkout.Portal(r.Context(), scope, q.Get("account"), q.Get("return_url"))
	if err != nil {
		d.fail(w, r, "portal", err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		PortalURL string `json:"portal_url"`
	}{url})
}

type fulfilmentJSON struct {
	Kind       string  `json:"kind"`
	Since      *string `json:"since,omitempty"`
	Amount     *int64  `json:"amount,omitempty"`
	ReceiptURL string  `json:"receipt_url,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// fulfilment is the server's report on the scope's latest payment: the only sanctioned way a UI learns credits arrived.
func (d *Door) fulfilment(w http.ResponseWriter, r *http.Request) {
	scope, err := d.scopeFromQuery(r)
	if err != nil {
		d.fail(w, r, "fulfilment", err)
		return
	}
	f, err := d.cfg.Checkout.Fulfilment(r.Context(), scope)
	if err != nil {
		d.fail(w, r, "fulfilment", err)
		return
	}
	out := fulfilmentJSON{Kind: string(f.Kind), ReceiptURL: f.ReceiptURL, Reason: f.Reason}
	if !f.Since.IsZero() {
		out.Since = rfc3339Ptr(&f.Since)
	}
	if f.Kind == ports.FulfilmentGranted {
		v := int64(f.Amount)
		out.Amount = &v
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- subscription --

type subscriptionJSON struct {
	PlanCode          string  `json:"plan_code"`
	Status            string  `json:"status"`
	CurrentPeriodEnd  string  `json:"current_period_end"`
	CancelAtPeriodEnd bool    `json:"cancel_at_period_end"`
	GraceUntil        *string `json:"grace_until"`
}

// subscription is the scope's, or null: a scope with none is not an error.
func (d *Door) subscription(w http.ResponseWriter, r *http.Request) {
	scope, err := d.scopeFromQuery(r)
	if err != nil {
		d.fail(w, r, "subscription", err)
		return
	}
	sub, found, err := d.cfg.Subs.Get(r.Context(), scope)
	if err != nil {
		d.fail(w, r, "subscription", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, subscriptionJSON{
		PlanCode: sub.PlanCode, Status: string(sub.Status), CurrentPeriodEnd: rfc3339(sub.CurrentPeriodEnd),
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd, GraceUntil: rfc3339Ptr(sub.GraceUntil),
	})
}

type changeRequest struct {
	Scope    *scopeJSON `json:"scope"`
	PlanCode string     `json:"plan_code"`
}

type cancelRequest struct {
	Scope *scopeJSON `json:"scope"`
}

// accepted says the provider was asked. The subscription's status is the processor's alone (invariant 12), so a change is
// never reported as made: it is made when the webhook about it is processed.
func accepted(w http.ResponseWriter) {
	writeJSON(w, http.StatusAccepted, struct {
		Accepted bool `json:"accepted"`
	}{true})
}

func (d *Door) change(w http.ResponseWriter, r *http.Request) {
	var req changeRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "subscription.change", err)
		return
	}
	if strings.TrimSpace(req.PlanCode) == "" {
		d.fail(w, r, "subscription.change", required("plan_code"))
		return
	}
	scope, err := d.scopeOf(r, req.Scope)
	if err != nil {
		d.fail(w, r, "subscription.change", err)
		return
	}
	if err := d.cfg.Changes.Change(r.Context(), scope, req.PlanCode); err != nil {
		d.fail(w, r, "subscription.change", err)
		return
	}
	accepted(w)
}

func (d *Door) cancel(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "subscription.cancel", err)
		return
	}
	scope, err := d.scopeOf(r, req.Scope)
	if err != nil {
		d.fail(w, r, "subscription.cancel", err)
		return
	}
	if err := d.cfg.Changes.CancelAtPeriodEnd(r.Context(), scope); err != nil {
		d.fail(w, r, "subscription.cancel", err)
		return
	}
	accepted(w)
}

// ---------------------------------------------------------------- auto top-up --

type rechargeRequest struct {
	Scope            *scopeJSON `json:"scope"`
	Enabled          bool       `json:"enabled"`
	Pool             string     `json:"pool"`
	Threshold        int64      `json:"threshold"`
	PlanCode         string     `json:"plan_code"`
	Currency         string     `json:"currency"`
	Account          string     `json:"account"`
	PaymentMethodRef string     `json:"payment_method_ref"`
	MaxPerDay        int        `json:"max_per_day"`
}

type rechargeJSON struct {
	Enabled          bool   `json:"enabled"`
	Pool             string `json:"pool"`
	Threshold        int64  `json:"threshold"`
	PlanCode         string `json:"plan_code"`
	Currency         string `json:"currency"`
	Account          string `json:"account"`
	MaxPerDay        int    `json:"max_per_day"`
	HasPaymentMethod bool   `json:"has_payment_method"`
	DisabledReason   string `json:"disabled_reason,omitempty"`
}

func rechargeToJSON(a domain.AutoRecharge) rechargeJSON {
	return rechargeJSON{
		Enabled: a.Enabled, Pool: string(a.Pool), Threshold: a.Threshold, PlanCode: a.PlanCode, Currency: a.Currency, Account: a.Account,
		MaxPerDay: a.MaxPerDay, HasPaymentMethod: a.PaymentMethodRef != "", DisabledReason: a.DisabledReason,
	}
}

func (d *Door) getRecharge(w http.ResponseWriter, r *http.Request) {
	scope, err := d.scopeFromQuery(r)
	if err != nil {
		d.fail(w, r, "auto-recharge.get", err)
		return
	}
	d.writeRecharge(w, r, scope)
}

func (d *Door) writeRecharge(w http.ResponseWriter, r *http.Request, scope domain.Scope) {
	a, found, err := d.cfg.Recharges.Get(r.Context(), scope)
	if err != nil {
		d.fail(w, r, "auto-recharge.get", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, rechargeToJSON(a))
}

// putRecharge stores the standing instruction and nothing of the day's ledger: the request has no field for it, so editing an
// instruction cannot hand out more charges today. The saved method is the provider's reference, never card data.
func (d *Door) putRecharge(w http.ResponseWriter, r *http.Request) {
	var req rechargeRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "auto-recharge.put", err)
		return
	}
	scope, err := d.scopeOf(r, req.Scope)
	if err != nil {
		d.fail(w, r, "auto-recharge.put", err)
		return
	}
	a := domain.AutoRecharge{
		Scope: scope, Enabled: req.Enabled, Pool: domain.Pool(req.Pool), Threshold: req.Threshold, PlanCode: req.PlanCode, Currency: req.Currency,
		Account: req.Account, PaymentMethodRef: req.PaymentMethodRef, MaxPerDay: req.MaxPerDay,
	}
	if err := a.Validate(); err != nil {
		d.fail(w, r, "auto-recharge.put", err)
		return
	}
	if err := d.cfg.Recharges.Put(r.Context(), a); err != nil {
		d.fail(w, r, "auto-recharge.put", err)
		return
	}
	d.writeRecharge(w, r, scope)
}

// -------------------------------------------------------------------- payments --

type moneyJSON struct {
	MinorUnits int64  `json:"minor_units"`
	Currency   string `json:"currency"`
}

type paymentJSON struct {
	ID             string     `json:"id"`
	PlanCode       string     `json:"plan_code,omitempty"`
	Kind           string     `json:"kind"`
	Amount         moneyJSON  `json:"amount"`
	TaxAmount      *moneyJSON `json:"tax_amount,omitempty"`
	CreditsGranted int64      `json:"credits_granted"`
	CreditsRevoked int64      `json:"credits_revoked"`
	Pool           string     `json:"pool,omitempty"`
	RefundedMinor  int64      `json:"refunded_minor"`
	Status         string     `json:"status"`
	DisputeStatus  string     `json:"dispute_status,omitempty"`
	ReceiptURL     string     `json:"receipt_url,omitempty"`
	FailureReason  string     `json:"failure_reason,omitempty"`
	CreatedAt      string     `json:"created_at"`
}

func paymentToJSON(p domain.Payment) paymentJSON {
	out := paymentJSON{
		ID: p.ProviderPaymentID, PlanCode: p.PlanCode, Kind: p.Kind, Amount: moneyJSON{p.Amount.MinorUnits, p.Amount.Currency},
		CreditsGranted: int64(p.CreditsGranted), CreditsRevoked: int64(p.CreditsRevoked), Pool: string(p.Pool), RefundedMinor: p.RefundedMinor,
		Status: string(p.Status), DisputeStatus: p.DisputeStatus, ReceiptURL: p.ReceiptURL, FailureReason: p.FailureReason, CreatedAt: rfc3339(p.CreatedAt),
	}
	if p.TaxAmount != nil {
		out.TaxAmount = &moneyJSON{p.TaxAmount.MinorUnits, p.TaxAmount.Currency}
	}
	return out
}

// payments is the scope's history, newest first, a page at a time.
func (d *Door) payments(w http.ResponseWriter, r *http.Request) {
	scope, err := d.scopeFromQuery(r)
	if err != nil {
		d.fail(w, r, "payments", err)
		return
	}
	limit, err := limitOf(r)
	if err != nil {
		d.fail(w, r, "payments", err)
		return
	}
	list, next, err := d.cfg.Payments.List(r.Context(), scope, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		d.fail(w, r, "payments", err)
		return
	}
	out := struct {
		Data []paymentJSON `json:"data"`
		Next *string       `json:"next"`
	}{Data: make([]paymentJSON, 0, len(list))}
	for _, p := range list {
		out.Data = append(out.Data, paymentToJSON(p))
	}
	if next != "" {
		out.Next = &next
	}
	writeJSON(w, http.StatusOK, out)
}
