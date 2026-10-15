package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// Customers maps a provider's customer to a scope — the only sanctioned way a webhook learns whom it is about. Two
// unique constraints keep it honest: a scope has one customer per account, and a provider customer belongs to one
// scope, so a mapping cannot be re-pointed and a payment cannot land in the wrong books.
type Customers struct{ pool *pgxpool.Pool }

var _ ports.Customers = (*Customers)(nil)

// NewCustomers returns the customers of a database.
func NewCustomers(pool *pgxpool.Pool) *Customers { return &Customers{pool: pool} }

// Get implements ports.Customers.
func (c *Customers) Get(ctx context.Context, s domain.Scope, account string) (domain.Customer, bool, error) {
	cust := domain.Customer{Scope: s.Normalized(), Account: account}
	var email *string
	err := c.pool.QueryRow(ctx, `SELECT provider_customer_id, billing_email FROM billing_customers
	                             WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND provider_account = $4`,
		realmOf(s), s.Kind, s.ID, account).Scan(&cust.ProviderCustomerID, &email)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Customer{}, false, nil
	case err != nil:
		return domain.Customer{}, false, fmt.Errorf("postgres: customer of %s: %w", s.Tag(), mapErr(err))
	}
	cust.BillingEmail = orEmpty(email)
	return cust, true, nil
}

// Upsert implements ports.Customers. The provider customer of a scope is immutable once mapped — replacing it would
// orphan the webhooks of the old one — so a conflicting row keeps its id and takes only the new billing email. A
// provider customer already mapped to another scope is domain-level ErrAlreadyExists.
func (c *Customers) Upsert(ctx context.Context, cust domain.Customer) error {
	if err := cust.Scope.Validate(); err != nil {
		return err
	}
	_, err := c.pool.Exec(ctx, `INSERT INTO billing_customers (realm, scope_kind, scope_id, provider_account, provider_customer_id, billing_email)
	                            VALUES ($1,$2,$3,$4,$5,$6)
	                            ON CONFLICT (realm, scope_kind, scope_id, provider_account)
	                            DO UPDATE SET billing_email = EXCLUDED.billing_email, updated_at = now()`,
		realmOf(cust.Scope), cust.Scope.Kind, cust.Scope.ID, cust.Account, cust.ProviderCustomerID, nilIfEmpty(cust.BillingEmail))
	if err != nil {
		return fmt.Errorf("postgres: map the customer of %s: %w", cust.Scope.Tag(), mapErr(err))
	}
	return nil
}

// ResolveScope implements ports.Customers.
func (c *Customers) ResolveScope(ctx context.Context, account, providerCustomerID string) (domain.Scope, bool, error) {
	var s domain.Scope
	var realm string
	err := c.pool.QueryRow(ctx, `SELECT realm, scope_kind, scope_id FROM billing_customers WHERE provider_account = $1 AND provider_customer_id = $2`,
		account, providerCustomerID).Scan(&realm, &s.Kind, &s.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Scope{}, false, nil
	case err != nil:
		return domain.Scope{}, false, fmt.Errorf("postgres: resolve customer %s: %w", providerCustomerID, mapErr(err))
	}
	s.Realm = domain.Realm(realm)
	return s, true, nil
}
