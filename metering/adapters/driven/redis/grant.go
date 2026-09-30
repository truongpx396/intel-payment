package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Grant adds or removes credits in one pool. A negative grant applies NegativeBalancePolicy inside
// the function; a floored shortfall is reported as Receipt.Writeoff, which the writer books as its
// own ledger row so balance == SUM(ledger) still holds (invariant 12).
func (s *Store) Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error) {
	if err := g.Validate(); err != nil {
		return domain.Receipt{}, err
	}
	if err := s.checkAmount(g.Amount); err != nil {
		return domain.Receipt{}, err
	}
	g.Scope = g.Scope.Normalized()
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()

	sh := s.shard(g.Scope)
	p := domain.Payload{Reason: g.Reason, Ref: g.Ref}
	if g.ExpiresAt != nil {
		p.ExpiresAt = g.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	payload, err := p.JSON()
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("redis: payload: %w", err)
	}
	pool := g.Pool.Or()
	keys := []string{metaKey(sh), acctKey(sh, g.Scope), idemKey(sh, g.Scope, idemGrant, g.IdemKey), outboxKey(sh)}
	r, err := s.mutate(ctx, "ip_grant", keys,
		string(pool), int64(g.Amount), g.Fingerprint(), s.cfg.HotIdemTTL.Milliseconds(), s.cfg.BusMaxLen,
		string(s.cfg.NegativeBalance), string(g.Scope.Realm), g.Scope.Kind, g.Scope.ID, g.IdemKey, payload)
	if err != nil {
		return domain.Receipt{}, err
	}
	rc := domain.Receipt{IdemKey: g.IdemKey}
	switch r.status() {
	case "APPLIED":
		if rc.Seq, err = r.num(1); err != nil {
			return domain.Receipt{}, err
		}
		total, err := r.num(2)
		if err != nil {
			return domain.Receipt{}, err
		}
		wo, err := r.num(3)
		if err != nil {
			return domain.Receipt{}, err
		}
		rc.Applied, rc.Balance, rc.Writeoff = true, domain.Credits(total), domain.Credits(wo)
		// A clamp floors the pool: the delta the pool actually took is the request plus the shortfall.
		rc.Delta = g.Amount + rc.Writeoff
	case "REPLAY":
		if rc.Seq, err = r.num(1); err != nil {
			return domain.Receipt{}, err
		}
	default:
		return domain.Receipt{}, statusError(r.status())
	}
	return rc, nil
}

// ApplyDelta issues a writer hot mutation — a transfer's credit leg, a correction, an expiry — in
// the scope's slot. It is idempotent on its own key.
func (s *Store) ApplyDelta(ctx context.Context, d ports.HotDelta) (domain.Receipt, error) {
	switch d.Op {
	case domain.OpTransferIn, domain.OpCorrection, domain.OpExpiry:
	default:
		return domain.Receipt{}, fmt.Errorf("%w: ApplyDelta does not issue %q", domain.ErrInvalid, d.Op)
	}
	if err := d.Scope.Validate(); err != nil {
		return domain.Receipt{}, err
	}
	if d.IdemKey == "" {
		return domain.Receipt{}, fmt.Errorf("%w: idem_key is required", domain.ErrInvalid)
	}
	if err := s.checkAmount(d.Delta); err != nil {
		return domain.Receipt{}, err
	}
	d.Scope = d.Scope.Normalized()
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()

	sh := s.shard(d.Scope)
	payload, err := d.Payload.JSON()
	if err != nil {
		return domain.Receipt{}, fmt.Errorf("redis: payload: %w", err)
	}
	capArg := "0"
	if d.CapAtAvailable {
		capArg = "1"
	}
	pool := d.Pool.Or()
	keys := []string{metaKey(sh), acctKey(sh, d.Scope), idemKey(sh, d.Scope, idemClass(d.Op), d.IdemKey), outboxKey(sh)}
	r, err := s.mutate(ctx, "ip_apply_delta", keys,
		string(d.Op), string(pool), int64(d.Delta), domain.CorrectionFingerprint(d.Op, d.Scope, pool, d.Delta, d.IdemKey),
		s.cfg.HotIdemTTL.Milliseconds(), s.cfg.BusMaxLen,
		string(d.Scope.Realm), d.Scope.Kind, d.Scope.ID, d.IdemKey, payload, capArg)
	if err != nil {
		return domain.Receipt{}, err
	}
	rc := domain.Receipt{IdemKey: d.IdemKey}
	switch r.status() {
	case "APPLIED":
		if rc.Seq, err = r.num(1); err != nil {
			return domain.Receipt{}, err
		}
		total, err := r.num(2)
		if err != nil {
			return domain.Receipt{}, err
		}
		delta, err := r.num(3)
		if err != nil {
			return domain.Receipt{}, err
		}
		rc.Applied, rc.Balance, rc.Delta = true, domain.Credits(total), domain.Credits(delta)
	case "REPLAY":
		if rc.Seq, err = r.num(1); err != nil {
			return domain.Receipt{}, err
		}
	default:
		return domain.Receipt{}, statusError(r.status())
	}
	return rc, nil
}
