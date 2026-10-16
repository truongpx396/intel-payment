package domain

import (
	"fmt"
	"net/mail"
	"path"
	"regexp"
	"strings"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

var (
	planCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	envNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// IsCurrencyCode reports whether s is an upper-case ISO 4217 code, which is how every amount here names its currency.
func IsCurrencyCode(s string) bool { return currencyPattern.MatchString(s) }

const (
	maxPlanName     = 120
	maxProviderID   = 255 // a provider's own identifier: Stripe's are well under this
	maxFileRefBytes = 4096
)

// Validate refuses a plan that is not one thing: its code is the only identifier a client ever sees and is carried in a URL, a
// subscription renews only if it has an interval and a pack only if it has none, and an amount is whole minor units in a
// currency. A plan that is wrongly shaped is refused — it is never repaired into one that sells something nobody meant to sell.
func (p Plan) Validate() error {
	if err := p.Realm.Validate(); err != nil {
		return err
	}
	switch {
	case !planCodePattern.MatchString(p.Code):
		return fmt.Errorf("%w: plan code %q must match %s", md.ErrInvalid, p.Code, planCodePattern)
	case strings.TrimSpace(p.Name) == "" || len(p.Name) > maxPlanName:
		return fmt.Errorf("%w: plan %s needs a name of at most %d characters", md.ErrInvalid, p.Code, maxPlanName)
	case p.CreditAllotment != nil && *p.CreditAllotment < 0:
		return fmt.Errorf("%w: plan %s grants %d credits: an allotment is not negative", md.ErrInvalid, p.Code, *p.CreditAllotment)
	}
	if err := p.Pool.Validate(); err != nil {
		return fmt.Errorf("plan %s: %w", p.Code, err)
	}
	switch p.Kind {
	case OneTime:
		if p.Interval != nil {
			return fmt.Errorf("%w: plan %s is a one-time pack and has a renewal interval", md.ErrInvalid, p.Code)
		}
	case Subscription:
		if p.Interval == nil || (*p.Interval != Monthly && *p.Interval != Yearly) {
			return fmt.Errorf("%w: plan %s is a subscription and renews every month or every year", md.ErrInvalid, p.Code)
		}
	default:
		return fmt.Errorf("%w: plan %s is %q, which is neither one_time nor subscription", md.ErrInvalid, p.Code, p.Kind)
	}
	for cur, m := range p.Prices {
		switch {
		case !currencyPattern.MatchString(cur):
			return fmt.Errorf("%w: plan %s is priced in %q, which is not an upper-case ISO 4217 code", md.ErrInvalid, p.Code, cur)
		case m.Currency != cur:
			return fmt.Errorf("%w: plan %s files a %q price under %s", md.ErrInvalid, p.Code, m.Currency, cur)
		case m.MinorUnits < 0:
			return fmt.Errorf("%w: plan %s costs %d %s: a price is not negative", md.ErrInvalid, p.Code, m.MinorUnits, cur)
		}
	}
	return nil
}

// Validate refuses a mapping that could not be addressed on the admin route or does not name the provider's price.
func (pp ProviderPrice) Validate() error {
	switch {
	case !accountIDPattern.MatchString(pp.Account):
		return fmt.Errorf("%w: provider account %q must match %s", md.ErrInvalid, pp.Account, accountIDPattern)
	case !currencyPattern.MatchString(pp.Currency):
		return fmt.Errorf("%w: currency %q is not an upper-case ISO 4217 code", md.ErrInvalid, pp.Currency)
	case !providerID(pp.ProviderPriceID):
		return fmt.Errorf("%w: the provider's price id must be 1 to %d characters with no whitespace", md.ErrInvalid, maxProviderID)
	}
	return nil
}

// providerID is an identifier a provider issued: visible characters only, so it cannot smuggle a line break into a log.
func providerID(s string) bool {
	if s == "" || len(s) > maxProviderID {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// Validate refuses a customer that names no scope, no account or no provider customer. The customer row is where a payment
// lands: it decides which scope's books a verified event is booked to (invariant 4).
func (c Customer) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	switch {
	case !accountIDPattern.MatchString(c.Account):
		return fmt.Errorf("%w: provider account %q must match %s", md.ErrInvalid, c.Account, accountIDPattern)
	case !providerID(c.ProviderCustomerID):
		return fmt.Errorf("%w: the provider's customer id must be 1 to %d characters with no whitespace", md.ErrInvalid, maxProviderID)
	}
	if c.BillingEmail != "" {
		if a, err := mail.ParseAddress(c.BillingEmail); err != nil || a.Address != c.BillingEmail {
			return fmt.Errorf("%w: billing email %q is not an address", md.ErrInvalid, c.BillingEmail)
		}
	}
	return nil
}

// ValidateRefs refuses an account whose secret references use a scheme the deployment cannot resolve — a webhook answered
// 500 for ever — or are not shaped as the scheme requires. schemes are the ones the deployment resolves (env, file); with
// none, nothing is acceptable.
func (a ProviderAccount) ValidateRefs(schemes ...string) error {
	for name, ref := range map[string]string{"secret_ref": a.SecretRef, "webhook_secret_ref": a.WebhookSecretRef} {
		if err := validRef(ref, schemes); err != nil {
			return fmt.Errorf("%w: provider account %s %s: %w", md.ErrInvalid, a.ID, name, err)
		}
	}
	return nil
}

func validRef(ref string, schemes []string) error {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return fmt.Errorf("want scheme:name")
	}
	found := false
	for _, s := range schemes {
		found = found || s == scheme
	}
	if !found {
		return fmt.Errorf("the scheme %q is not one this deployment resolves (%s)", scheme, strings.Join(schemes, ", "))
	}
	switch scheme {
	case "env":
		if !envNamePattern.MatchString(rest) {
			return fmt.Errorf("%q is not an environment variable name", rest)
		}
	case "file":
		switch {
		case !strings.HasPrefix(rest, "/"):
			return fmt.Errorf("a file reference is an absolute path")
		case len(rest) > maxFileRefBytes || strings.ContainsRune(rest, 0):
			return fmt.Errorf("a file reference is a path of at most %d bytes with no NUL", maxFileRefBytes)
		case path.Clean(rest) != rest:
			return fmt.Errorf("a file reference is a clean path: no .., no //, no trailing /")
		}
	}
	return nil
}
