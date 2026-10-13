package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	domain "github.com/truongpx396/intel-payment/billing/domain"
	mdomain "github.com/truongpx396/intel-payment/metering/domain"
)

func account() domain.ProviderAccount {
	return domain.ProviderAccount{
		ID: "stripe-live", Provider: "stripe", Realms: []domain.Realm{"acme", "default"}, Mode: "live",
		SecretRef: "env:STRIPE_SECRET_KEY", WebhookSecretRef: "env:STRIPE_WEBHOOK_SECRET", Active: true,
	}
}

func TestAnAccountServesOnlyTheRealmsItNames(t *testing.T) {
	t.Parallel()
	a := account()
	for realm, want := range map[domain.Realm]bool{"acme": true, "default": true, "": true /* the default realm */, "other": false, "ACME": false} {
		if got := a.Serves(realm); got != want {
			t.Errorf("Serves(%q) = %v, want %v (invariant 19)", realm, got, want)
		}
	}
	a.Realms = []domain.Realm{"acme"}
	if a.Serves("") {
		t.Error("an account that does not name the default realm must not serve an unnamed one")
	}
}

func TestAnInactiveAccountServesNothing(t *testing.T) {
	t.Parallel()
	a := account()
	a.Active = false
	if a.Serves("acme") {
		t.Fatal("an inactive account is a 404 at the route; it must not resolve a customer either")
	}
}

func TestAnAccountIsRefusedUnlessItIsReferencesAndNarrowRealms(t *testing.T) {
	t.Parallel()
	if err := account().Validate(); err != nil {
		t.Fatalf("a good account refused: %v", err)
	}
	cases := map[string]func(*domain.ProviderAccount){
		"no id":                                  func(a *domain.ProviderAccount) { a.ID = "" },
		"an id the route cannot carry":           func(a *domain.ProviderAccount) { a.ID = "stripe/live" },
		"an upper-case id":                       func(a *domain.ProviderAccount) { a.ID = "Stripe" },
		"no provider":                            func(a *domain.ProviderAccount) { a.Provider = "" },
		"no realms":                              func(a *domain.ProviderAccount) { a.Realms = nil },
		"a malformed realm":                      func(a *domain.ProviderAccount) { a.Realms = []domain.Realm{"Not A Realm"} },
		"a mode that is neither":                 func(a *domain.ProviderAccount) { a.Mode = "sandbox" },
		"no secret reference":                    func(a *domain.ProviderAccount) { a.SecretRef = "" },
		"a raw secret where a reference belongs": func(a *domain.ProviderAccount) { a.SecretRef = "sk_live_51Habcdef" },
		"a raw webhook secret":                   func(a *domain.ProviderAccount) { a.WebhookSecretRef = "whsec_abcdef" },
		"an empty reference":                     func(a *domain.ProviderAccount) { a.WebhookSecretRef = "env:" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := account()
			mutate(&a)
			if err := a.Validate(); !errors.Is(err, mdomain.ErrInvalid) {
				t.Fatalf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
}

func verified() domain.Event {
	amount := usd(1000)
	return domain.Event{
		Provider: "stripe", Account: "stripe-live", ProviderEventID: "evt_1",
		CreatedAt: t0, Type: domain.PaymentSucceeded, RawHash: strings.Repeat("ab", 32),
		Object: domain.EventObject{ProviderCustomerID: "cus_1", ProviderPaymentID: "pi_1", Amount: &amount},
	}
}

func TestAnEventThatVerificationCouldNotHaveProducedIsRefused(t *testing.T) {
	t.Parallel()
	if err := verified().Validate(); err != nil {
		t.Fatalf("a good event refused: %v", err)
	}
	cases := map[string]func(*domain.Event){
		"no provider":                func(e *domain.Event) { e.Provider = "" },
		"no account":                 func(e *domain.Event) { e.Account = "" },
		"no event id":                func(e *domain.Event) { e.ProviderEventID = "" },
		"no provider clock":          func(e *domain.Event) { e.CreatedAt = time.Time{} },
		"no body hash":               func(e *domain.Event) { e.RawHash = "" },
		"a hash that is not SHA-256": func(e *domain.Event) { e.RawHash = "abc" },
		"an unknown type":            func(e *domain.Event) { e.Type = "payment_teleported" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := verified()
			mutate(&e)
			if err := e.Validate(); !errors.Is(err, mdomain.ErrInvalid) {
				t.Fatalf("Validate = %v, want ErrInvalid — a defect that fiat then trusts", err)
			}
		})
	}
}

func TestIgnoredIsAFirstClassTypeNotAnError(t *testing.T) {
	t.Parallel()
	e := verified()
	e.Type = domain.Ignored
	e.Object = domain.EventObject{}
	if err := e.Validate(); err != nil {
		t.Fatalf("an unsubscribed provider type is normal traffic, not an error: %v", err)
	}
	for _, ty := range []domain.EventType{domain.PaymentSucceeded, domain.PaymentFailed, domain.PaymentRefunded,
		domain.SubscriptionUpdated, domain.SubscriptionDeleted, domain.DisputeOpened, domain.DisputeFundsWithdrawn,
		domain.DisputeFundsReinstated, domain.DisputeClosed, domain.Ignored} {
		if !ty.Valid() {
			t.Errorf("%q must be a valid type", ty)
		}
	}
	if domain.EventType("").Valid() || domain.EventType("nonsense").Valid() {
		t.Error("an empty or unknown type is not valid")
	}
}
