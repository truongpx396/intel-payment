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

// Accounts serves provider accounts, which are configuration data: a row names the credentials by REFERENCE, never
// by value, and the realms it may touch.
type Accounts struct{ pool *pgxpool.Pool }

var _ ports.Accounts = (*Accounts)(nil)

// NewAccounts returns the accounts of a database.
func NewAccounts(pool *pgxpool.Pool) *Accounts { return &Accounts{pool: pool} }

const accountColumns = `id, provider, realms, mode, secret_ref, webhook_secret_ref, active`

func scanAccount(row pgx.Row) (domain.ProviderAccount, error) {
	var a domain.ProviderAccount
	var provider string
	var realms []string
	if err := row.Scan(&a.ID, &provider, &realms, &a.Mode, &a.SecretRef, &a.WebhookSecretRef, &a.Active); err != nil {
		return domain.ProviderAccount{}, err
	}
	a.Provider = domain.Provider(provider)
	for _, r := range realms {
		a.Realms = append(a.Realms, domain.Realm(r))
	}
	return a, nil
}

// Account implements ports.Accounts.
func (a *Accounts) Account(ctx context.Context, id string) (domain.ProviderAccount, bool, error) {
	acct, err := scanAccount(a.pool.QueryRow(ctx, `SELECT `+accountColumns+` FROM provider_accounts WHERE id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.ProviderAccount{}, false, nil
	case err != nil:
		return domain.ProviderAccount{}, false, fmt.Errorf("postgres: provider account %s: %w", id, mapErr(err))
	}
	return acct, true, nil
}

// ForRealm implements ports.Accounts.
func (a *Accounts) ForRealm(ctx context.Context, realm domain.Realm) ([]domain.ProviderAccount, error) {
	rows, err := a.pool.Query(ctx, `SELECT `+accountColumns+` FROM provider_accounts WHERE active AND $1 = ANY(realms) ORDER BY id`, string(realm.Or()))
	if err != nil {
		return nil, fmt.Errorf("postgres: provider accounts of realm %s: %w", realm.Or(), err)
	}
	defer rows.Close()
	var out []domain.ProviderAccount
	for rows.Next() {
		acct, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: provider accounts: %w", err)
		}
		out = append(out, acct)
	}
	return out, rows.Err()
}
