package domain

import (
	"fmt"
	"regexp"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

var (
	accountIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	providerPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	secretRefPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*:.+$`)
)

// Serves reports whether the account may touch the realm: a verified event whose customer resolves to a realm
// the account does not serve is ignored and counted as a security event (invariant 19). An inactive account
// serves nothing. An unnamed realm is the default realm.
func (a ProviderAccount) Serves(realm Realm) bool {
	if !a.Active {
		return false
	}
	want := realm.Or()
	for _, r := range a.Realms {
		if r.Or() == want {
			return true
		}
	}
	return false
}

// Validate refuses an account that could not be addressed on the webhook route, that names no realm, or whose
// secrets are not references: a row that stored a provider key would put it in every backup and every audit
// row (invariant 10), so a value with no scheme — `sk_live_…`, `whsec_…` — is refused outright.
func (a ProviderAccount) Validate() error {
	switch {
	case !accountIDPattern.MatchString(a.ID):
		return fmt.Errorf("%w: provider account id %q must match %s", md.ErrInvalid, a.ID, accountIDPattern)
	case !providerPattern.MatchString(string(a.Provider)):
		return fmt.Errorf("%w: provider %q must match %s", md.ErrInvalid, a.Provider, providerPattern)
	case len(a.Realms) == 0:
		return fmt.Errorf("%w: provider account %s serves no realm", md.ErrInvalid, a.ID)
	case a.Mode != "live" && a.Mode != "test":
		return fmt.Errorf("%w: provider account %s mode %q is neither live nor test", md.ErrInvalid, a.ID, a.Mode)
	case !secretRefPattern.MatchString(a.SecretRef):
		return fmt.Errorf("%w: provider account %s secret_ref is not a reference (want scheme:name, e.g. env:STRIPE_SECRET_KEY)", md.ErrInvalid, a.ID)
	case !secretRefPattern.MatchString(a.WebhookSecretRef):
		return fmt.Errorf("%w: provider account %s webhook_secret_ref is not a reference (want scheme:name)", md.ErrInvalid, a.ID)
	}
	for _, r := range a.Realms {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}
