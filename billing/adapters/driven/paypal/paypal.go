package paypal

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Defaults of the adapter.
const (
	// ProductionURL is where an account in "live" mode is served; SandboxURL is where one in "test" mode is.
	ProductionURL = "https://api-m.paypal.com"
	SandboxURL    = "https://api-m.sandbox.paypal.com"
	// DefaultTolerance is how far a delivery's transmission time may be from when it arrived, in either direction.
	DefaultTolerance = 5 * time.Minute
	// tokenMargin is how long before a token expires it is replaced rather than risked.
	tokenMargin = time.Minute
	maxBody     = 1 << 20
)

// Config wires the adapter.
type Config struct {
	// Secrets resolves the references a provider account holds. Required.
	Secrets ports.SecretResolver
	// Catalog maps a PayPal plan id back to a plan, so a subscription fetched from PayPal carries the plan code the
	// engine knows it by. Required.
	Catalog ports.Catalog
	// Clock is the adapter's "now" for the transmission tolerance and for when a token expires. Required.
	Clock mports.Clock
	// HTTP is the client for API calls (default: one with a 30 s timeout).
	HTTP *http.Client
	// BaseURL, when set, is used for every account; a test points it at a fake. When empty an account is served by
	// the host of its mode: "live" by ProductionURL and "test" by SandboxURL, and any other mode is refused.
	BaseURL string
	// Tolerance is how far a delivery's transmission time may be from its arrival, in either direction (default
	// DefaultTolerance).
	Tolerance time.Duration
}

// Provider is the PayPal adapter.
type Provider struct {
	cfg      Config
	override string

	mu     sync.Mutex
	tokens map[string]token // by account and client id
}

type token struct {
	value   string
	expires time.Time
}

var _ ports.PaymentProvider = (*Provider)(nil)

// New validates the configuration and returns the adapter.
func New(cfg Config) (*Provider, error) {
	var errs []error
	if cfg.Secrets == nil {
		errs = append(errs, errors.New("a SecretResolver is required: the adapter holds no secret of its own"))
	}
	if cfg.Catalog == nil {
		errs = append(errs, errors.New("a Catalog is required to map PayPal plans to plans"))
	}
	if cfg.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: the core reads no wall clock"))
	}
	if cfg.Tolerance < 0 {
		errs = append(errs, errors.New("the transmission tolerance cannot be negative"))
	}
	if cfg.BaseURL != "" {
		if u, err := url.Parse(cfg.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("the base URL %q is not an http(s) URL", cfg.BaseURL))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("paypal: %w", err)
	}
	if cfg.Tolerance == 0 {
		cfg.Tolerance = DefaultTolerance
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Provider{cfg: cfg, override: strings.TrimRight(cfg.BaseURL, "/"), tokens: map[string]token{}}, nil
}

// Name implements ports.PaymentProvider.
func (p *Provider) Name() domain.Provider { return "paypal" }

// apiURL is a path on the host that serves the account. A live account is never sent to the sandbox, nor a test one to
// production, and a mode that is neither is refused before anything is sent.
func (p *Provider) apiURL(acct domain.ProviderAccount, path string) (string, error) {
	if p.override != "" {
		return p.override + path, nil
	}
	switch acct.Mode {
	case "live":
		return ProductionURL + path, nil
	case "test":
		return SandboxURL + path, nil
	}
	return "", fmt.Errorf("%w: provider account %s has mode %q, which is neither live nor test", md.ErrInvalid, acct.ID, acct.Mode)
}
