package ports

import (
	"context"

	"github.com/truongpx396/intel-payment/billing/domain"
)

// TaxStrategy names WHO computes tax, so the answer is never "nobody". The engine does not compute tax; there is
// no compute-it-here mode.
type TaxStrategy interface {
	Mode() domain.TaxMode
	// Annotate runs before a checkout is created and may add what the provider needs to compute tax (or what an
	// external service computed). Its failure fails the checkout closed.
	Annotate(ctx context.Context, in *CheckoutInput) error
}
