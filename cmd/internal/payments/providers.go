package payments

import (
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driven/paypal"
	"github.com/truongpx396/intel-payment/billing/adapters/driven/polar"
	"github.com/truongpx396/intel-payment/billing/adapters/driven/stripe"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// ProviderDeps is what every provider adapter is built from.
type ProviderDeps struct {
	Secrets ports.SecretResolver
	Catalog ports.Catalog
	Clock   mports.Clock
	// Tolerance is how far a signature's timestamp may be from when the delivery arrived (0: each adapter's own default).
	Tolerance time.Duration
}

// Providers is the registry of every provider adapter the repository has, which is what a route's {provider} names. A
// provider with no row here is answered 404 unknown_account in production while every test of it passes, so the adapters
// directory is held equal to this list by a test. Each adapter refuses a missing dependency or a negative tolerance itself.
func Providers(d ProviderDeps) (ports.Providers, error) {
	st, err := stripe.New(stripe.Config{Secrets: d.Secrets, Catalog: d.Catalog, Clock: d.Clock, Tolerance: d.Tolerance})
	if err != nil {
		return nil, err
	}
	po, err := polar.New(polar.Config{Secrets: d.Secrets, Catalog: d.Catalog, Clock: d.Clock, Tolerance: d.Tolerance})
	if err != nil {
		return nil, err
	}
	pp, err := paypal.New(paypal.Config{Secrets: d.Secrets, Catalog: d.Catalog, Clock: d.Clock, Tolerance: d.Tolerance})
	if err != nil {
		return nil, err
	}
	return ports.Providers{st.Name(): st, po.Name(): po, pp.Name(): pp}, nil
}
