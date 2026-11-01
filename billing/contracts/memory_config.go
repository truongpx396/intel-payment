package contracts

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ConfigWriterDefects are the deliberate faults the contract is shown to catch: each switches on one way a writer could be
// kinder than a database that holds money.
type ConfigWriterDefects struct {
	OverwriteOnCreate  bool // creating a plan that exists replaces it
	ReplaceMapping     bool // a plan's mapping to a provider's price is silently replaced
	ShareProviderPrice bool // one provider price may sell two plans
	RepointCustomer    bool // a scope's provider customer is overwritten
	ChangeProvider     bool // an existing account's provider is rewritten
	SkipValidation     bool // a plan or an account that is not valid is stored
	LoseAnUpdate       bool // creation is not atomic: two creations of one code can both win
}

// MemConfigWriter is ports.ConfigWriter over maps, for the suites that stand on it. It is held to the same contract as the
// PostgreSQL writer, so a rule it is kinder about is a suite that passes for the wrong reason.
type MemConfigWriter struct {
	Defects ConfigWriterDefects

	mu        sync.Mutex
	plans     map[string]*ports.PlanConfig // realm/code
	accounts  map[string]domain.ProviderAccount
	customers map[string]domain.Customer // realm/kind/id/account
	changes   []ports.ConfigChange
}

var _ ports.ConfigWriter = (*MemConfigWriter)(nil)

// NewMemConfigWriter returns a writer over a world that has these provider accounts.
func NewMemConfigWriter(accounts ...domain.ProviderAccount) *MemConfigWriter {
	w := &MemConfigWriter{plans: map[string]*ports.PlanConfig{}, accounts: map[string]domain.ProviderAccount{}, customers: map[string]domain.Customer{}}
	for _, a := range accounts {
		w.accounts[a.ID] = cloneAccount(a)
	}
	return w
}

// Changes are the attributions of the writes made so far, in order.
func (w *MemConfigWriter) Changes() []ports.ConfigChange {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.changes)
}

func planKey(realm md.Realm, code string) string { return string(realm.Or()) + "/" + code }

func cloneAccount(a domain.ProviderAccount) domain.ProviderAccount {
	a.Realms = slices.Clone(a.Realms)
	return a
}

func clonePlan(p domain.Plan) domain.Plan {
	p.Realm = p.Realm.Or()
	p.Prices = maps.Clone(p.Prices)
	if p.Prices == nil {
		p.Prices = map[string]domain.Money{}
	}
	if p.CreditAllotment != nil {
		v := *p.CreditAllotment
		p.CreditAllotment = &v
	}
	if p.Interval != nil {
		v := *p.Interval
		p.Interval = &v
	}
	p.Entitlements = slices.Clone(p.Entitlements)
	return p
}

func cloneConfig(pc *ports.PlanConfig) ports.PlanConfig {
	return ports.PlanConfig{Plan: clonePlan(pc.Plan), ProviderPrices: slices.Clone(pc.ProviderPrices)}
}

func (w *MemConfigWriter) record(ch ports.ConfigChange) { w.changes = append(w.changes, ch) }

// Plans implements ports.ConfigWriter.
func (w *MemConfigWriter) Plans(_ context.Context, realm md.Realm) ([]ports.PlanConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []ports.PlanConfig
	for _, pc := range w.plans {
		if pc.Plan.Realm.Or() == realm.Or() {
			out = append(out, cloneConfig(pc))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Plan, out[j].Plan
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.Code < b.Code
	})
	return out, nil
}

// Plan implements ports.ConfigWriter.
func (w *MemConfigWriter) Plan(_ context.Context, realm md.Realm, code string) (ports.PlanConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pc, ok := w.plans[planKey(realm, code)]
	if !ok {
		return ports.PlanConfig{}, fmt.Errorf("%w: plan %s in realm %s", md.ErrNotFound, code, realm.Or())
	}
	return cloneConfig(pc), nil
}

// CreatePlan implements ports.ConfigWriter.
func (w *MemConfigWriter) CreatePlan(_ context.Context, ch ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error) {
	if !w.Defects.SkipValidation {
		if err := p.Validate(); err != nil {
			return ports.PlanConfig{}, err
		}
	}
	if w.Defects.LoseAnUpdate {
		// Check, then (after the lock is released) write: two creations of one code can both pass the check.
		w.mu.Lock()
		_, exists := w.plans[planKey(p.Realm, p.Code)]
		w.mu.Unlock()
		if exists && !w.Defects.OverwriteOnCreate {
			return ports.PlanConfig{}, fmt.Errorf("%w: plan %s", md.ErrAlreadyExists, p.Code)
		}
		time.Sleep(20 * time.Millisecond) // long enough for the others to pass the same check
		w.mu.Lock()
		defer w.mu.Unlock()
		w.plans[planKey(p.Realm, p.Code)] = &ports.PlanConfig{Plan: clonePlan(p)}
		w.record(ch)
		return cloneConfig(w.plans[planKey(p.Realm, p.Code)]), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	key := planKey(p.Realm, p.Code)
	if _, exists := w.plans[key]; exists && !w.Defects.OverwriteOnCreate {
		return ports.PlanConfig{}, fmt.Errorf("%w: plan %s in realm %s", md.ErrAlreadyExists, p.Code, p.Realm.Or())
	}
	w.plans[key] = &ports.PlanConfig{Plan: clonePlan(p)}
	w.record(ch)
	return cloneConfig(w.plans[key]), nil
}

// PatchPlan implements ports.ConfigWriter.
func (w *MemConfigWriter) PatchPlan(_ context.Context, ch ports.ConfigChange, realm md.Realm, code string, patch ports.PlanPatch) (ports.PlanConfig, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pc, ok := w.plans[planKey(realm, code)]
	if !ok {
		return ports.PlanConfig{}, fmt.Errorf("%w: plan %s in realm %s", md.ErrNotFound, code, realm.Or())
	}
	next := clonePlan(pc.Plan)
	if patch.Name != nil {
		next.Name = *patch.Name
	}
	if patch.Active != nil {
		next.Active = *patch.Active
	}
	if patch.SortOrder != nil {
		next.SortOrder = *patch.SortOrder
	}
	for cur, minor := range patch.Prices {
		if minor == nil {
			delete(next.Prices, cur)
			continue
		}
		next.Prices[cur] = domain.Money{MinorUnits: *minor, Currency: cur}
	}
	if err := next.Validate(); err != nil {
		return ports.PlanConfig{}, err
	}
	pc.Plan = next
	w.record(ch)
	return cloneConfig(pc), nil
}

// PutProviderPrice implements ports.ConfigWriter.
func (w *MemConfigWriter) PutProviderPrice(_ context.Context, ch ports.ConfigChange, realm md.Realm, code string, pp domain.ProviderPrice) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pc, ok := w.plans[planKey(realm, code)]
	if !ok {
		return false, fmt.Errorf("%w: plan %s in realm %s", md.ErrNotFound, code, realm.Or())
	}
	if err := pp.Validate(); err != nil {
		return false, err
	}
	if _, known := w.accounts[pp.Account]; !known {
		return false, fmt.Errorf("%w: there is no provider account %s", md.ErrInvalid, pp.Account)
	}
	if _, priced := pc.Plan.Prices[pp.Currency]; !priced {
		return false, fmt.Errorf("%w: plan %s has no price in %s, so a mapping for it would never sell", md.ErrInvalid, code, pp.Currency)
	}
	for i, have := range pc.ProviderPrices {
		if have.Account == pp.Account && have.Currency == pp.Currency {
			if have.ProviderPriceID == pp.ProviderPriceID {
				return false, nil
			}
			if !w.Defects.ReplaceMapping {
				return false, fmt.Errorf("%w: plan %s is already sold through %s in %s as %s", md.ErrAlreadyExists, code, pp.Account, pp.Currency, have.ProviderPriceID)
			}
			pc.ProviderPrices[i] = pp
			w.record(ch)
			return false, nil
		}
	}
	if !w.Defects.ShareProviderPrice {
		for _, other := range w.plans {
			for _, have := range other.ProviderPrices {
				if have.Account == pp.Account && have.ProviderPriceID == pp.ProviderPriceID {
					return false, fmt.Errorf("%w: %s is already the provider price of plan %s", md.ErrAlreadyExists, pp.ProviderPriceID, other.Plan.Code)
				}
			}
		}
	}
	pc.ProviderPrices = append(pc.ProviderPrices, pp)
	sort.Slice(pc.ProviderPrices, func(i, j int) bool {
		a, b := pc.ProviderPrices[i], pc.ProviderPrices[j]
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		return a.Currency < b.Currency
	})
	w.record(ch)
	return true, nil
}

// Accounts implements ports.ConfigWriter.
func (w *MemConfigWriter) Accounts(context.Context) ([]domain.ProviderAccount, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]domain.ProviderAccount, 0, len(w.accounts))
	for _, a := range w.accounts {
		out = append(out, cloneAccount(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Account implements ports.ConfigWriter.
func (w *MemConfigWriter) Account(_ context.Context, id string) (domain.ProviderAccount, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.accounts[id]
	return cloneAccount(a), ok, nil
}

// PutAccount implements ports.ConfigWriter.
func (w *MemConfigWriter) PutAccount(_ context.Context, ch ports.ConfigChange, a domain.ProviderAccount) (bool, error) {
	if !w.Defects.SkipValidation {
		if err := a.Validate(); err != nil {
			return false, err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	have, exists := w.accounts[a.ID]
	if exists && have.Provider != a.Provider && !w.Defects.ChangeProvider {
		return false, fmt.Errorf("%w: account %s belongs to provider %s, whose webhooks arrive at a route that names it", md.ErrInvalid, a.ID, have.Provider)
	}
	w.accounts[a.ID] = cloneAccount(a)
	w.record(ch)
	return !exists, nil
}

func linkKey(s md.Scope, account string) string {
	return string(s.Realm.Or()) + "/" + s.Kind + "/" + s.ID + "/" + account
}

// Customer implements ports.ConfigWriter.
func (w *MemConfigWriter) Customer(_ context.Context, s md.Scope, account string) (domain.Customer, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.customers[linkKey(s, account)]
	return c, ok, nil
}

// PutCustomer implements ports.ConfigWriter.
func (w *MemConfigWriter) PutCustomer(_ context.Context, ch ports.ConfigChange, c domain.Customer) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	c.Scope = c.Scope.Normalized()
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, known := w.accounts[c.Account]; !known {
		return false, fmt.Errorf("%w: there is no provider account %s", md.ErrInvalid, c.Account)
	}
	key := linkKey(c.Scope, c.Account)
	have, exists := w.customers[key]
	switch {
	case exists && have.ProviderCustomerID != c.ProviderCustomerID && !w.Defects.RepointCustomer:
		return false, fmt.Errorf("%w: %s is already linked to provider customer %s", md.ErrAlreadyExists, c.Scope.Tag(), have.ProviderCustomerID)
	case !exists || have.ProviderCustomerID != c.ProviderCustomerID:
		for k, other := range w.customers {
			if k != key && other.Account == c.Account && other.ProviderCustomerID == c.ProviderCustomerID {
				return false, fmt.Errorf("%w: provider customer %s is already linked to %s", md.ErrAlreadyExists, c.ProviderCustomerID, other.Scope.Tag())
			}
		}
	}
	w.customers[key] = c
	w.record(ch)
	return !exists, nil
}
