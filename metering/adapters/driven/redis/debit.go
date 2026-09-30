package redis

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Debit settles usage in ONE function call in the scope's slot: it draws the eligible pools in
// priority order (the last absorbs overshoot), increments the window counters, advances seq,
// writes the idempotency guard and XADDs the intent — atomically (FR-008).
func (s *Store) Debit(ctx context.Context, c domain.Charge) (domain.Receipt, error) {
	if err := c.Validate(); err != nil {
		return domain.Receipt{}, err
	}
	if err := s.checkAmount(c.Amount); err != nil {
		return domain.Receipt{}, err
	}
	c.Scope = c.Scope.Normalized()
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()

	sh := s.shard(c.Scope)
	pools, err := s.eligible(ctx, c.Scope.Realm, c.Resource)
	if err != nil {
		return domain.Receipt{}, err
	}
	limits, err := s.lims.Limits(ctx, c.Scope.Realm)
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("redis: limits: %w", err)
	}
	incs, err := planIncrements(sh, limits, c, s.clock.Now())
	if err != nil {
		return domain.Receipt{}, err
	}
	payload, err := domain.UsagePayload(c).JSON()
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("redis: payload: %w", err)
	}

	keys := []string{metaKey(sh), acctKey(sh, c.Scope), idemKey(sh, c.Scope, idemUsage, c.IdemKey), outboxKey(sh)}
	args := []any{
		int64(c.Amount), c.Fingerprint(), s.cfg.HotIdemTTL.Milliseconds(), s.cfg.BusMaxLen,
		poolNames(pools), string(c.Scope.Realm), c.Scope.Kind, c.Scope.ID, c.IdemKey, payload,
		s.lowWater(ctx, c.Scope, pools),
	}
	for _, in := range incs {
		keys = append(keys, in.key)
		args = append(args, in.by, in.ttl.Milliseconds())
	}

	r, err := s.mutate(ctx, "ip_debit", keys, args...)
	if err != nil {
		return domain.Receipt{}, err
	}
	rc := domain.Receipt{IdemKey: c.IdemKey, RateCardVersion: c.RateCardVersion}
	switch r.status() {
	case "APPLIED":
		if rc.Seq, err = r.num(1); err != nil {
			return domain.Receipt{}, err
		}
		total, err := r.num(2)
		if err != nil {
			return domain.Receipt{}, err
		}
		rc.Applied, rc.Delta, rc.Balance = true, -c.Amount, domain.Credits(total)
	case "REPLAY":
		if rc.Seq, err = r.num(1); err != nil {
			return domain.Receipt{}, err
		}
	default:
		return domain.Receipt{}, statusError(r.status())
	}
	return rc, nil
}
