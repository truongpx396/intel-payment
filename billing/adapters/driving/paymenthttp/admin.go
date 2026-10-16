package paymenthttp

import (
	"net/http"
	"strings"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// The admin routes: payment configuration is data (constitution VIII), and this is its supported interface (FR-049). Every
// write names a person (`actor`) and a reason, is refused without both, and is attributed in the database's own audit row,
// which also records the credential it came through. The realm is the credential's, named with ?realm= when it spans several.

// attribute is who and why, from a body's fields, with the credential the request came through. Both are required.
func (d *Door) attribute(r *http.Request, actor, reason string) (ports.ConfigChange, error) {
	switch {
	case strings.TrimSpace(actor) == "":
		return ports.ConfigChange{}, required("actor")
	case strings.TrimSpace(reason) == "":
		return ports.ConfigChange{}, required("reason")
	}
	return ports.ConfigChange{Actor: actor, Reason: reason, Via: d.cfg.Caller.Via(r)}, nil
}

// adminRealm is the realm an admin call addresses: the credential's, or ?realm= when it spans several.
func (d *Door) adminRealm(r *http.Request) (domain.Realm, error) {
	s, err := d.cfg.Caller.Resolve(r, domain.Scope{Realm: domain.Realm(r.URL.Query().Get("realm"))})
	return s.Realm, err
}

// covers reports whether the credential covers every one of these realms (ErrRealmMismatch otherwise): an admin administers
// the realms it holds, so it cannot hand an account a realm it lacks.
func (d *Door) covers(r *http.Request, realms ...domain.Realm) error {
	for _, realm := range realms {
		if _, err := d.cfg.Caller.Resolve(r, domain.Scope{Realm: realm.Or()}); err != nil {
			return err
		}
	}
	return nil
}

// covered reports whether the credential covers any of these realms: what it may see of an account.
func (d *Door) covered(r *http.Request, realms []domain.Realm) bool {
	for _, realm := range realms {
		if _, err := d.cfg.Caller.Resolve(r, domain.Scope{Realm: realm.Or()}); err == nil {
			return true
		}
	}
	return false
}

func status(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

// ----------------------------------------------------------------------- plans --

type providerPriceJSON struct {
	Account         string `json:"account"`
	Currency        string `json:"currency"`
	ProviderPriceID string `json:"provider_price_id"`
}

type planAdminJSON struct {
	Code            string              `json:"code"`
	Name            string              `json:"name"`
	Kind            string              `json:"kind"`
	CreditAllotment *int64              `json:"credit_allotment"`
	Pool            string              `json:"pool"`
	Interval        *string             `json:"interval,omitempty"`
	Active          bool                `json:"active"`
	SortOrder       int                 `json:"sort_order"`
	Prices          map[string]int64    `json:"prices"`
	ProviderPrices  []providerPriceJSON `json:"provider_prices"`
}

func planAdminToJSON(pc ports.PlanConfig) planAdminJSON {
	p := planToJSON(pc.Plan)
	out := planAdminJSON{
		Code: p.Code, Name: p.Name, Kind: p.Kind, CreditAllotment: p.CreditAllotment, Pool: string(pc.Plan.Pool.Or()), Interval: p.Interval,
		Active: pc.Plan.Active, SortOrder: p.SortOrder, Prices: p.Prices, ProviderPrices: make([]providerPriceJSON, 0, len(pc.ProviderPrices)),
	}
	for _, pp := range pc.ProviderPrices {
		out.ProviderPrices = append(out.ProviderPrices, providerPriceJSON{pp.Account, pp.Currency, pp.ProviderPriceID})
	}
	return out
}

func (d *Door) listPlans(w http.ResponseWriter, r *http.Request) {
	realm, err := d.adminRealm(r)
	if err != nil {
		d.fail(w, r, "admin.plans", err)
		return
	}
	plans, err := d.cfg.Admin.Plans(r.Context(), realm)
	if err != nil {
		d.fail(w, r, "admin.plans", err)
		return
	}
	out := struct {
		Data []planAdminJSON `json:"data"`
	}{Data: make([]planAdminJSON, 0, len(plans))}
	for _, pc := range plans {
		out.Data = append(out.Data, planAdminToJSON(pc))
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Door) getPlan(w http.ResponseWriter, r *http.Request) {
	realm, err := d.adminRealm(r)
	if err != nil {
		d.fail(w, r, "admin.plan", err)
		return
	}
	pc, err := d.cfg.Admin.Plan(r.Context(), realm, r.PathValue("code"))
	if err != nil {
		d.fail(w, r, "admin.plan", err)
		return
	}
	writeJSON(w, http.StatusOK, planAdminToJSON(pc))
}

type createPlanRequest struct {
	Actor           string           `json:"actor"`
	Reason          string           `json:"reason"`
	Code            string           `json:"code"`
	Name            string           `json:"name"`
	Kind            string           `json:"kind"`
	CreditAllotment *int64           `json:"credit_allotment"`
	Pool            string           `json:"pool"`
	Interval        *string          `json:"interval"`
	Active          *bool            `json:"active"`
	SortOrder       int              `json:"sort_order"`
	Prices          map[string]int64 `json:"prices"`
}

func (d *Door) createPlan(w http.ResponseWriter, r *http.Request) {
	var req createPlanRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "admin.plan.create", err)
		return
	}
	ch, err := d.attribute(r, req.Actor, req.Reason)
	if err != nil {
		d.fail(w, r, "admin.plan.create", err)
		return
	}
	realm, err := d.adminRealm(r)
	if err != nil {
		d.fail(w, r, "admin.plan.create", err)
		return
	}
	p := domain.Plan{
		Realm: realm, Code: req.Code, Name: req.Name, Kind: domain.PlanKind(req.Kind), Pool: domain.Pool(req.Pool), Active: req.Active == nil || *req.Active,
		SortOrder: req.SortOrder, Prices: make(map[string]domain.Money, len(req.Prices)),
	}
	if req.CreditAllotment != nil {
		c := domain.Credits(*req.CreditAllotment)
		p.CreditAllotment = &c
	}
	if req.Interval != nil {
		i := domain.Interval(*req.Interval)
		p.Interval = &i
	}
	for cur, minor := range req.Prices {
		p.Prices[cur] = domain.Money{MinorUnits: minor, Currency: cur}
	}
	pc, err := d.cfg.Admin.CreatePlan(r.Context(), ch, p)
	if err != nil {
		d.fail(w, r, "admin.plan.create", err)
		return
	}
	writeJSON(w, http.StatusCreated, planAdminToJSON(pc))
}

type patchPlanRequest struct {
	Actor     string            `json:"actor"`
	Reason    string            `json:"reason"`
	Name      *string           `json:"name"`
	Active    *bool             `json:"active"`
	SortOrder *int              `json:"sort_order"`
	Prices    map[string]*int64 `json:"prices"`
}

func (d *Door) patchPlan(w http.ResponseWriter, r *http.Request) {
	var req patchPlanRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "admin.plan.patch", err)
		return
	}
	ch, err := d.attribute(r, req.Actor, req.Reason)
	if err != nil {
		d.fail(w, r, "admin.plan.patch", err)
		return
	}
	realm, err := d.adminRealm(r)
	if err != nil {
		d.fail(w, r, "admin.plan.patch", err)
		return
	}
	pc, err := d.cfg.Admin.PatchPlan(r.Context(), ch, realm, r.PathValue("code"), ports.PlanPatch{Name: req.Name, Active: req.Active, SortOrder: req.SortOrder, Prices: req.Prices})
	if err != nil {
		d.fail(w, r, "admin.plan.patch", err)
		return
	}
	writeJSON(w, http.StatusOK, planAdminToJSON(pc))
}

type putProviderPriceRequest struct {
	Actor           string `json:"actor"`
	Reason          string `json:"reason"`
	ProviderPriceID string `json:"provider_price_id"`
}

// putProviderPrice maps a plan's price to the provider's. The account and the currency are the path's: they are the mapping's
// identity, and a body that said them as well could disagree.
func (d *Door) putProviderPrice(w http.ResponseWriter, r *http.Request) {
	var req putProviderPriceRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "admin.provider-price", err)
		return
	}
	ch, err := d.attribute(r, req.Actor, req.Reason)
	if err != nil {
		d.fail(w, r, "admin.provider-price", err)
		return
	}
	realm, err := d.adminRealm(r)
	if err != nil {
		d.fail(w, r, "admin.provider-price", err)
		return
	}
	pp := domain.ProviderPrice{Account: r.PathValue("account"), Currency: r.PathValue("currency"), ProviderPriceID: req.ProviderPriceID}
	created, err := d.cfg.Admin.PutProviderPrice(r.Context(), ch, realm, r.PathValue("code"), pp)
	if err != nil {
		d.fail(w, r, "admin.provider-price", err)
		return
	}
	writeJSON(w, status(created), providerPriceJSON{pp.Account, pp.Currency, pp.ProviderPriceID})
}

// -------------------------------------------------------------------- accounts --

type accountJSON struct {
	ID               string   `json:"id"`
	Provider         string   `json:"provider"`
	Realms           []string `json:"realms"`
	Mode             string   `json:"mode"`
	SecretRef        string   `json:"secret_ref"`
	WebhookSecretRef string   `json:"webhook_secret_ref"`
	Active           bool     `json:"active"`
}

func accountToJSON(a domain.ProviderAccount) accountJSON {
	out := accountJSON{ID: a.ID, Provider: string(a.Provider), Realms: make([]string, 0, len(a.Realms)), Mode: a.Mode,
		SecretRef: a.SecretRef, WebhookSecretRef: a.WebhookSecretRef, Active: a.Active}
	for _, r := range a.Realms {
		out.Realms = append(out.Realms, string(r))
	}
	return out
}

func (d *Door) listAccounts(w http.ResponseWriter, r *http.Request) {
	all, err := d.cfg.Admin.Accounts(r.Context())
	if err != nil {
		d.fail(w, r, "admin.accounts", err)
		return
	}
	out := struct {
		Data []accountJSON `json:"data"`
	}{Data: []accountJSON{}}
	for _, a := range all {
		if d.covered(r, a.Realms) {
			out.Data = append(out.Data, accountToJSON(a))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Door) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := d.cfg.Admin.Account(r.Context(), r.PathValue("id"))
	if err == nil && !d.covered(r, a.Realms) {
		// Not found, not forbidden: whether another realm's account exists is not for this credential to probe.
		err = domain.ErrNotFound
	}
	if err != nil {
		d.fail(w, r, "admin.account", err)
		return
	}
	writeJSON(w, http.StatusOK, accountToJSON(a))
}

type putAccountRequest struct {
	Actor            string   `json:"actor"`
	Reason           string   `json:"reason"`
	Provider         string   `json:"provider"`
	Realms           []string `json:"realms"`
	Mode             string   `json:"mode"`
	SecretRef        string   `json:"secret_ref"`
	WebhookSecretRef string   `json:"webhook_secret_ref"`
	Active           *bool    `json:"active"`
}

// putAccount replaces an account whole. A credential administers the realms it covers: the realms asked for must all be its
// own, and so must those the account serves now, or it could take over — or hand away — an account that serves a realm it
// does not hold.
func (d *Door) putAccount(w http.ResponseWriter, r *http.Request) {
	var req putAccountRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "admin.account.put", err)
		return
	}
	ch, err := d.attribute(r, req.Actor, req.Reason)
	if err != nil {
		d.fail(w, r, "admin.account.put", err)
		return
	}
	if req.Active == nil {
		d.fail(w, r, "admin.account.put", required("active"))
		return
	}
	a := domain.ProviderAccount{
		ID: r.PathValue("id"), Provider: domain.Provider(req.Provider), Mode: req.Mode, SecretRef: req.SecretRef, WebhookSecretRef: req.WebhookSecretRef, Active: *req.Active,
	}
	for _, realm := range req.Realms {
		a.Realms = append(a.Realms, domain.Realm(realm))
	}
	if err := d.covers(r, a.Realms...); err != nil {
		d.fail(w, r, "admin.account.put", err)
		return
	}
	switch have, err := d.cfg.Admin.Account(r.Context(), a.ID); {
	case err == nil:
		if err := d.covers(r, have.Realms...); err != nil {
			d.fail(w, r, "admin.account.put", err)
			return
		}
	case !isNotFound(err):
		d.fail(w, r, "admin.account.put", err)
		return
	}
	created, err := d.cfg.Admin.PutAccount(r.Context(), ch, a)
	if err != nil {
		d.fail(w, r, "admin.account.put", err)
		return
	}
	writeJSON(w, status(created), accountToJSON(a))
}

// ------------------------------------------------------------------- customers --

type customerJSON struct {
	Scope              scopeJSON `json:"scope"`
	Account            string    `json:"account"`
	ProviderCustomerID string    `json:"provider_customer_id"`
	BillingEmail       string    `json:"billing_email,omitempty"`
}

func customerToJSON(c domain.Customer) customerJSON {
	return customerJSON{Scope: scopeJSON{Kind: c.Scope.Kind, ID: c.Scope.ID, Realm: string(c.Scope.Realm.Or())}, Account: c.Account,
		ProviderCustomerID: c.ProviderCustomerID, BillingEmail: c.BillingEmail}
}

func (d *Door) getCustomer(w http.ResponseWriter, r *http.Request) {
	scope, err := d.scopeFromQuery(r)
	if err != nil {
		d.fail(w, r, "admin.customer", err)
		return
	}
	account := r.URL.Query().Get("account")
	if account == "" {
		d.fail(w, r, "admin.customer", required("account"))
		return
	}
	c, err := d.cfg.Admin.Customer(r.Context(), scope, account)
	if err != nil {
		d.fail(w, r, "admin.customer", err)
		return
	}
	writeJSON(w, http.StatusOK, customerToJSON(c))
}

type putCustomerRequest struct {
	Actor              string     `json:"actor"`
	Reason             string     `json:"reason"`
	Scope              *scopeJSON `json:"scope"`
	Account            string     `json:"account"`
	ProviderCustomerID string     `json:"provider_customer_id"`
	BillingEmail       string     `json:"billing_email"`
}

// putCustomer links a scope to a provider customer that already exists: the way a customer of an earlier billing system is
// brought across. It never re-points a link that exists — see ports.ConfigStore.PutCustomer.
func (d *Door) putCustomer(w http.ResponseWriter, r *http.Request) {
	var req putCustomerRequest
	if err := d.decodeBody(w, r, &req); err != nil {
		d.fail(w, r, "admin.customer.put", err)
		return
	}
	ch, err := d.attribute(r, req.Actor, req.Reason)
	if err != nil {
		d.fail(w, r, "admin.customer.put", err)
		return
	}
	scope, err := d.scopeOf(r, req.Scope)
	if err != nil {
		d.fail(w, r, "admin.customer.put", err)
		return
	}
	c := domain.Customer{Scope: scope, Account: req.Account, ProviderCustomerID: req.ProviderCustomerID, BillingEmail: req.BillingEmail}
	created, err := d.cfg.Admin.PutCustomer(r.Context(), ch, c)
	if err != nil {
		d.fail(w, r, "admin.customer.put", err)
		return
	}
	writeJSON(w, status(created), customerToJSON(c))
}
