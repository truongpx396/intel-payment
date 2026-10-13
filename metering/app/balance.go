package app

import (
	"context"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Balance reads a scope's hot balance per pool — what GetBalance and GET /v1/balance serve. It reads
// what Admit reads, and heals the same way: a cold account is rebuilt from the books and read again, so a
// fresh Redis or a bumped shard generation is not reported to a caller as an error. An outage is an
// error and never a zero.
func (m *meter) Balance(ctx context.Context, s domain.Scope) (map[domain.Pool]domain.Credits, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	s = s.Normalized()
	var pools map[domain.Pool]domain.Credits
	err := m.reh.heal(ctx, s, func() (e error) { pools, e = m.d.Balance.Balance(ctx, s); return })
	if err != nil {
		return nil, err
	}
	return pools, nil
}

// NewService is New for a host that serves the engine to others: the Meter and the read beside it, one
// value. A transport (grpcserver, the REST handler) takes both.
func NewService(cfg metering.Config, d Deps) (ports.Service, error) {
	m, err := build(cfg, d)
	if err != nil {
		return nil, err
	}
	return m, nil
}

var _ ports.Service = (*meter)(nil)
