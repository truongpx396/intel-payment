package ports

import (
	"context"

	"github.com/truongpx396/intel-payment/billing/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// ConfigChange is who changed configuration and why, as the engine's admin API records it: the database writes the audit row
// from it, in the same transaction as the change.
type ConfigChange = mports.ConfigChange

// PlanConfig is a plan as the admin sees it, with the provider prices that sell it. A client sees neither the inactive plans nor a
// provider's price id; the admin sees both.
type PlanConfig struct {
	Plan           domain.Plan
	ProviderPrices []domain.ProviderPrice // by account, then currency
}

// PlanPatch is what may change about a plan once it exists. What a plan GRANTS — its kind, allotment, pool and interval —
// is fixed when it is created: a customer who paid for 2000 credits must not be granted another quantity because a plan was
// edited in between. To change what is sold, sell a new plan and take the old one off sale (Active=false); a payment for a
// plan taken off sale is still recognised.
type PlanPatch struct {
	Name      *string
	Active    *bool
	SortOrder *int
	// Prices sets the price in a currency (minor units); a nil entry removes the currency's price.
	Prices map[string]*int64
}

// Empty reports whether the patch changes nothing.
func (p PlanPatch) Empty() bool {
	return p.Name == nil && p.Active == nil && p.SortOrder == nil && len(p.Prices) == 0
}

// ConfigStore is the part of the payment configuration both sides share.
type ConfigStore interface {
	// Plans are the realm's plans, active or not, by sort order and then code.
	Plans(ctx context.Context, realm domain.Realm) ([]PlanConfig, error)
	// Plan is one plan; md.ErrNotFound when the realm has none by that code.
	Plan(ctx context.Context, realm domain.Realm, code string) (PlanConfig, error)
	// CreatePlan stores a new plan. A code the realm already has is md.ErrAlreadyExists and changes nothing; a plan that is not
	// valid is md.ErrInvalid and is stored nowhere.
	CreatePlan(ctx context.Context, ch ConfigChange, p domain.Plan) (PlanConfig, error)
	// PatchPlan changes what a PlanPatch may. md.ErrNotFound for a plan the realm lacks.
	PatchPlan(ctx context.Context, ch ConfigChange, realm domain.Realm, code string, patch PlanPatch) (PlanConfig, error)
	// PutProviderPrice maps a plan's price in a currency to the provider's price through an account. It reports whether the
	// mapping was created; an identical one is a no-op. A mapping is NEVER replaced (md.ErrAlreadyExists): a subscriber still
	// renews at the old price, and a payment for a price nobody maps is ignored. A provider's price belongs to one plan
	// (md.ErrAlreadyExists). md.ErrNotFound for a plan the realm lacks; md.ErrInvalid for a currency the plan is not priced in
	// or an account that does not exist.
	PutProviderPrice(ctx context.Context, ch ConfigChange, realm domain.Realm, code string, pp domain.ProviderPrice) (created bool, err error)

	// Accounts are every provider account, inactive ones included.
	Accounts(ctx context.Context) ([]domain.ProviderAccount, error)
	// Account is one account, and false when there is none.
	Account(ctx context.Context, id string) (domain.ProviderAccount, bool, error)
	// PutAccount creates or replaces an account and reports which. An account's provider never changes (md.ErrInvalid): its
	// webhooks arrive at a route that names it.
	PutAccount(ctx context.Context, ch ConfigChange, a domain.ProviderAccount) (created bool, err error)

	// Customer is the scope's provider customer on an account, and false when it has none.
	Customer(ctx context.Context, s domain.Scope, account string) (domain.Customer, bool, error)
	// PutCustomer links a scope to an existing provider customer and reports whether the link was created. The provider
	// customer of a scope is immutable once linked, and a provider customer belongs to one scope (md.ErrAlreadyExists in both
	// cases): re-pointing either would send a payment into the wrong books. The email may move.
	PutCustomer(ctx context.Context, ch ConfigChange, c domain.Customer) (created bool, err error)
}

// ConfigWriter is the driven side: the database, which tells its own audit trigger who made the change and why.
type ConfigWriter interface{ ConfigStore }

// Admin is the driving side the admin API calls: the ConfigWriter behind the rules. Every write names a person and a reason,
// an account is accepted only if the deployment can speak to its provider and read its secrets, and nothing is linked to an
// account that does not name the realm.
type Admin interface {
	Plans(ctx context.Context, realm domain.Realm) ([]PlanConfig, error)
	Plan(ctx context.Context, realm domain.Realm, code string) (PlanConfig, error)
	CreatePlan(ctx context.Context, ch ConfigChange, p domain.Plan) (PlanConfig, error)
	PatchPlan(ctx context.Context, ch ConfigChange, realm domain.Realm, code string, patch PlanPatch) (PlanConfig, error)
	PutProviderPrice(ctx context.Context, ch ConfigChange, realm domain.Realm, code string, pp domain.ProviderPrice) (created bool, err error)
	Accounts(ctx context.Context) ([]domain.ProviderAccount, error)
	// Account is md.ErrNotFound when there is none.
	Account(ctx context.Context, id string) (domain.ProviderAccount, error)
	PutAccount(ctx context.Context, ch ConfigChange, a domain.ProviderAccount) (created bool, err error)
	// Customer is md.ErrNotFound when the scope has none.
	Customer(ctx context.Context, s domain.Scope, account string) (domain.Customer, error)
	PutCustomer(ctx context.Context, ch ConfigChange, c domain.Customer) (created bool, err error)
}
