package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// AdminDeps are what the admin use case needs.
type AdminDeps struct {
	Writer ports.ConfigWriter
	// Providers are the providers this deployment has an adapter for. An account of any other is refused: its webhooks would be
	// answered 404 for ever.
	Providers []domain.Provider
	// SecretSchemes are the secret-reference schemes this deployment resolves (env, file). An account naming any other is
	// refused: its deliveries would be answered 500 for ever.
	SecretSchemes []string
}

// Admin is the rules above the configuration writer (FR-049): every change names a person and a reason, an account is only
// accepted if the deployment can speak to it, and nothing is linked to an account that does not name the realm. The writer
// keeps the data honest; this keeps the operator honest.
type Admin struct {
	d         AdminDeps
	providers map[domain.Provider]bool
}

var _ ports.Admin = (*Admin)(nil)

// NewAdmin validates its dependencies and returns the admin use case.
func NewAdmin(d AdminDeps) (*Admin, error) {
	var errs []error
	if d.Writer == nil {
		errs = append(errs, errors.New("a configuration writer is required"))
	}
	if len(d.Providers) == 0 {
		errs = append(errs, errors.New("at least one provider is required: without one every account would be refused"))
	}
	if len(d.SecretSchemes) == 0 {
		errs = append(errs, errors.New("at least one secret scheme is required: without one every account would be refused"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("billing admin: %w", err)
	}
	a := &Admin{d: d, providers: map[domain.Provider]bool{}}
	for _, p := range d.Providers {
		a.providers[p] = true
	}
	return a, nil
}

// attributed refuses a change that names no person or no reason: the database's audit row is how anyone later answers "who
// changed the price, and why".
func attributed(ch ports.ConfigChange) error {
	switch {
	case strings.TrimSpace(ch.Actor) == "":
		return fmt.Errorf("%w: a change names the person making it (actor)", md.ErrInvalid)
	case strings.TrimSpace(ch.Reason) == "":
		return fmt.Errorf("%w: a change names why (reason)", md.ErrInvalid)
	}
	return nil
}

// Plans implements ports.Admin.
func (a *Admin) Plans(ctx context.Context, realm domain.Realm) ([]ports.PlanConfig, error) {
	return a.d.Writer.Plans(ctx, realm)
}

// Plan implements ports.Admin.
func (a *Admin) Plan(ctx context.Context, realm domain.Realm, code string) (ports.PlanConfig, error) {
	return a.d.Writer.Plan(ctx, realm, code)
}

// CreatePlan implements ports.Admin.
func (a *Admin) CreatePlan(ctx context.Context, ch ports.ConfigChange, p domain.Plan) (ports.PlanConfig, error) {
	if err := attributed(ch); err != nil {
		return ports.PlanConfig{}, err
	}
	if err := p.Validate(); err != nil {
		return ports.PlanConfig{}, err
	}
	return a.d.Writer.CreatePlan(ctx, ch, p)
}

// PatchPlan implements ports.Admin.
func (a *Admin) PatchPlan(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, code string, patch ports.PlanPatch) (ports.PlanConfig, error) {
	if err := attributed(ch); err != nil {
		return ports.PlanConfig{}, err
	}
	switch {
	case patch.Empty():
		return ports.PlanConfig{}, fmt.Errorf("%w: the patch changes nothing", md.ErrInvalid)
	case patch.Name != nil && strings.TrimSpace(*patch.Name) == "":
		return ports.PlanConfig{}, fmt.Errorf("%w: a plan needs a name", md.ErrInvalid)
	}
	for cur, minor := range patch.Prices {
		if !domain.IsCurrencyCode(cur) {
			return ports.PlanConfig{}, fmt.Errorf("%w: %q is not an upper-case ISO 4217 code", md.ErrInvalid, cur)
		}
		if minor != nil && *minor < 0 {
			return ports.PlanConfig{}, fmt.Errorf("%w: a price in %s is not negative", md.ErrInvalid, cur)
		}
	}
	return a.d.Writer.PatchPlan(ctx, ch, realm, code, patch)
}

// serves reports whether the account names the realm — active or not: an account may be wired before it is switched on.
func serves(acct domain.ProviderAccount, realm domain.Realm) bool {
	for _, r := range acct.Realms {
		if r.Or() == realm.Or() {
			return true
		}
	}
	return false
}

// accountFor finds an account and requires that it names the realm: a payment of an account is only booked to a realm it
// serves, so nothing is linked through one that does not.
func (a *Admin) accountFor(ctx context.Context, id string, realm domain.Realm) error {
	acct, ok, err := a.d.Writer.Account(ctx, id)
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("%w: there is no provider account %s", md.ErrInvalid, id)
	case !serves(acct, realm):
		return fmt.Errorf("%w: provider account %s does not serve realm %s", md.ErrInvalid, id, realm.Or())
	}
	return nil
}

// PutProviderPrice implements ports.Admin.
func (a *Admin) PutProviderPrice(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, code string, pp domain.ProviderPrice) (bool, error) {
	if err := attributed(ch); err != nil {
		return false, err
	}
	if err := pp.Validate(); err != nil {
		return false, err
	}
	if err := a.accountFor(ctx, pp.Account, realm); err != nil {
		return false, err
	}
	return a.d.Writer.PutProviderPrice(ctx, ch, realm, code, pp)
}

// Accounts implements ports.Admin.
func (a *Admin) Accounts(ctx context.Context) ([]domain.ProviderAccount, error) {
	return a.d.Writer.Accounts(ctx)
}

// Account implements ports.Admin.
func (a *Admin) Account(ctx context.Context, id string) (domain.ProviderAccount, error) {
	acct, ok, err := a.d.Writer.Account(ctx, id)
	if err != nil {
		return domain.ProviderAccount{}, err
	}
	if !ok {
		return domain.ProviderAccount{}, fmt.Errorf("%w: provider account %s", md.ErrNotFound, id)
	}
	return acct, nil
}

// PutAccount implements ports.Admin.
func (a *Admin) PutAccount(ctx context.Context, ch ports.ConfigChange, acct domain.ProviderAccount) (bool, error) {
	if err := attributed(ch); err != nil {
		return false, err
	}
	if err := acct.Validate(); err != nil {
		return false, err
	}
	if err := acct.ValidateRefs(a.d.SecretSchemes...); err != nil {
		return false, err
	}
	if !a.providers[acct.Provider] {
		return false, fmt.Errorf("%w: this deployment has no adapter for provider %q, so the account's webhooks would be answered 404 for ever", md.ErrInvalid, acct.Provider)
	}
	return a.d.Writer.PutAccount(ctx, ch, acct)
}

// Customer implements ports.Admin.
func (a *Admin) Customer(ctx context.Context, s domain.Scope, account string) (domain.Customer, error) {
	c, ok, err := a.d.Writer.Customer(ctx, s.Normalized(), account)
	if err != nil {
		return domain.Customer{}, err
	}
	if !ok {
		return domain.Customer{}, fmt.Errorf("%w: %s has no customer on %s", md.ErrNotFound, s.Tag(), account)
	}
	return c, nil
}

// PutCustomer implements ports.Admin.
func (a *Admin) PutCustomer(ctx context.Context, ch ports.ConfigChange, c domain.Customer) (bool, error) {
	if err := attributed(ch); err != nil {
		return false, err
	}
	if err := c.Validate(); err != nil {
		return false, err
	}
	c.Scope = c.Scope.Normalized()
	if err := a.accountFor(ctx, c.Account, c.Scope.Realm); err != nil {
		return false, err
	}
	return a.d.Writer.PutCustomer(ctx, ch, c)
}
