package stripe

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Defaults of the adapter.
const (
	DefaultBaseURL   = "https://api.stripe.com"
	DefaultTolerance = 5 * time.Minute
	// APIVersion is the Stripe API version the shapes in this package follow; a webhook endpoint is pinned to it.
	APIVersion = "2024-06-20"
	maxBody    = 1 << 20
)

// Config wires the adapter.
type Config struct {
	// Secrets resolves the references a provider account holds. Required.
	Secrets ports.SecretResolver
	// Catalog maps a Stripe price id back to a plan, so a subscription fetched from Stripe carries the plan code the
	// engine knows it by. Required.
	Catalog ports.Catalog
	// Clock is the adapter's "now" for the signature tolerance. Required.
	Clock mports.Clock
	// HTTP is the client for API calls (default: one with a 30 s timeout).
	HTTP *http.Client
	// BaseURL is Stripe's API (default DefaultBaseURL); a test points it at a fake.
	BaseURL string
	// Tolerance is how far a signature's timestamp may be from now, in either direction (default DefaultTolerance).
	Tolerance time.Duration
}

// Provider is the Stripe adapter.
type Provider struct {
	cfg  Config
	base *url.URL
}

var _ ports.PaymentProvider = (*Provider)(nil)

// New validates the configuration and returns the adapter.
func New(cfg Config) (*Provider, error) {
	var errs []error
	if cfg.Secrets == nil {
		errs = append(errs, errors.New("a SecretResolver is required: the adapter holds no secret of its own"))
	}
	if cfg.Catalog == nil {
		errs = append(errs, errors.New("a Catalog is required to map Stripe prices to plans"))
	}
	if cfg.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: the core reads no wall clock"))
	}
	if cfg.Tolerance < 0 {
		errs = append(errs, errors.New("the signature tolerance cannot be negative"))
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		errs = append(errs, fmt.Errorf("the base URL %q is not an http(s) URL", cfg.BaseURL))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("stripe: %w", err)
	}
	if cfg.Tolerance == 0 {
		cfg.Tolerance = DefaultTolerance
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Provider{cfg: cfg, base: base}, nil
}

// Name implements ports.PaymentProvider.
func (p *Provider) Name() domain.Provider { return "stripe" }

// apiURL is a path under the base URL.
func (p *Provider) apiURL(path string) string {
	return strings.TrimRight(p.base.String(), "/") + path
}
