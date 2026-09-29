package app

import (
	"context"
	"errors"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Grant mints or removes credits in one pool (FR-015). PRIVILEGED: separated at the RPC, the route,
// the authz check and the audit trail (invariant 13) — this method trusts its caller was.
//
// It is NOT journaled when the hot tier is unavailable: a grant originates from a durable source (a
// verified webhook in the inbox, an audited admin action) that retries, and answering "accepted"
// for a mint that has not happened would let a caller believe credits exist that do not.
func (m *meter) Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error) {
	if err := g.Validate(); err != nil {
		return domain.Receipt{}, err
	}
	g.Scope = g.Scope.Normalized()
	var rc domain.Receipt
	err := m.reh.heal(ctx, g.Scope, func() (e error) { rc, e = m.d.Balance.Grant(ctx, g); return })
	if errors.Is(err, domain.ErrIdemConflict) {
		m.met.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": string(g.Scope.Realm), "op": "grant"})
	}
	return rc, err
}
