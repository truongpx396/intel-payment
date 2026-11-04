package app

import (
	"context"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// quotaSource sizes a metering limit from the scope's entitlements: metering's QuotaSource, provided by this layer and wired in
// cmd/ (metering never imports it). It reads through the Entitler, so it answers from the resolution the Entitler enforces —
// precedence, grace, expiry — and from its cache and announcements: a plan change resizes the ceiling on the next Admit.
type quotaSource struct{ e ports.Entitler }

// NewQuotaSource adapts an Entitler to metering's QuotaSource. Over no Entitler it is no QuotaSource at all (nil), which the
// Meter reports as "no QuotaSource is wired" and answers by its AdmitFailPolicy, rather than a source that answers anything.
func NewQuotaSource(e ports.Entitler) mports.QuotaSource {
	if e == nil {
		return nil
	}
	return quotaSource{e: e}
}

// Quota returns the scope's quota for key: a size, -1 for unlimited (the Meter drops the ceiling), or domain.ErrQuotaNotGranted
// when nothing grants it (the limit keeps its own max). Any other error is returned untouched — an outage is not an absence and
// is never a size, so the Meter applies AdmitFailPolicy (metering invariant 17).
func (q quotaSource) Quota(ctx context.Context, s md.Scope, key string) (int64, error) {
	limit, ok, err := q.e.Quota(ctx, s, domain.Key(key))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, md.ErrQuotaNotGranted
	}
	return limit, nil
}
