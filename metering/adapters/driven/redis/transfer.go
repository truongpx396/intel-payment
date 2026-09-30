package redis

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Transfer is phase 1 of a transfer (hot-path-consistency.md §4): an atomic check-and-debit of the
// SOURCE's hot pool that emits a transfer_out intent, or ErrInsufficient. The two scopes live in
// different shards, so no script may touch both; the writer completes the destination's side.
//
// MaxDestBalance is checked first, against the destination's hot balance. It is a guard against an
// errant admin action, not a race-free bound: concurrent credits to the destination can exceed it
// by their own amounts.
func (s *Store) Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	if err := t.Validate(); err != nil {
		return domain.TransferReceipt{}, err
	}
	if err := s.checkAmount(t.Amount); err != nil {
		return domain.TransferReceipt{}, err
	}
	t.From, t.To = t.From.Normalized(), t.To.Normalized()
	ctx, cancel := s.withTimeout(ctx, s.cfg.RecordTimeout)
	defer cancel()

	if t.MaxDestBalance > 0 {
		dest, err := s.Snapshot(ctx, t.To)
		if err != nil {
			return domain.TransferReceipt{}, err
		}
		// A cold destination holds nothing YET; the writer rehydrates it before crediting.
		held := dest.Pools[t.ToPool.Or()]
		if after, err := held.Add(t.Amount); err != nil || after > t.MaxDestBalance {
			return domain.TransferReceipt{}, fmt.Errorf("%w: %d + %d > %d", domain.ErrDestBalanceCap, held, t.Amount, t.MaxDestBalance)
		}
	}

	sh := s.shard(t.From)
	payload, err := domain.Payload{
		Reason: t.Reason, Ref: t.Ref, Counterparty: domain.PayloadScopeOf(t.To),
		FromPool: string(t.FromPool.Or()), ToPool: string(t.ToPool.Or()), Amount: int64(t.Amount),
	}.JSON()
	if err != nil {
		return domain.TransferReceipt{}, fmt.Errorf("redis: payload: %w", err)
	}
	keys := []string{metaKey(sh), acctKey(sh, t.From), idemKey(sh, t.From, idemTransfer, t.IdemKey), outboxKey(sh)}
	r, err := s.mutate(ctx, "ip_transfer_out", keys,
		string(t.FromPool.Or()), int64(t.Amount), t.Fingerprint(), s.cfg.HotIdemTTL.Milliseconds(), s.cfg.BusMaxLen,
		string(t.From.Realm), t.From.Kind, t.From.ID, t.IdemKey, payload)
	if err != nil {
		return domain.TransferReceipt{}, err
	}
	rc := domain.TransferReceipt{IdemKey: t.IdemKey, Amount: t.Amount}
	switch r.status() {
	case "APPLIED":
		total, err := r.num(2)
		if err != nil {
			return domain.TransferReceipt{}, err
		}
		rc.Applied, rc.FromBalance, rc.Status = true, domain.Credits(total), domain.TransferInTransit
	case "REPLAY":
		// Redis knows the request was accepted, not whether the writer has since settled it: the
		// status is left empty for the Meter to fill in from the books.
	case "INSUFFICIENT":
		bal, _ := r.num(1)
		return domain.TransferReceipt{}, fmt.Errorf("%w: pool %q holds %d, transfer needs %d", domain.ErrInsufficient, t.FromPool.Or(), bal, t.Amount)
	default:
		return domain.TransferReceipt{}, statusError(r.status())
	}
	return rc, nil
}
