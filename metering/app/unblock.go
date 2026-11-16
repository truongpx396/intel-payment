package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// UnblockResult says where the block was held. Each is independent of the other: an unblock that finds the books already clear and the hot
// account still blocked is the second half of one that was interrupted, and reports exactly that.
type UnblockResult struct {
	Books bool // the books held the block and no longer do
	Hot   bool // the live hot account held the block and no longer does
}

// ErrUnblockRaced is an unblock that could not settle: every look found the hot tier at a sequence the books had not reached, or a block
// installed again after it was cleared. The books are clear and the hot account may not be; it is safe to ask again.
var ErrUnblockRaced = errors.New("the writer had not booked what the hot tier issued, or the block was installed again")

// unblockAttempts bounds how often an unblock drains and looks again. A shard that never settles — a gap that is not filling, a hot tier
// written to faster than it is booked — fails rather than loops, and says so.
const unblockAttempts = 4

// Unblock clears block_and_flag for one scope: the durable flag in the books, then the one the admission gate reads in the hot account
// (R112a). It is an operator's act and is the only way the block is ever cleared; a top-up does not.
//
// The danger is a write-off the hot tier has issued and the writer has not yet booked. Booking it sets the block in the books, after this has
// cleared it, and the two would then disagree about whether the account may spend — with nothing in reconcile to say so, which compares
// balances and not the flag, until the next rehydrate put the block back. Draining first does not prevent that: a drain books one batch and
// returns, and a write-off can arrive after any drain. So the hot account is cleared only at the sequence the books were at (ip_unblock is a
// compare-and-set on it, as ip_heal is): equal means nothing issued is waiting to be booked, and not equal is a race, which drains and looks
// again. A block that is back after the clear — a rehydrate that read the books before they were cleared and installed it after the hot
// account was found cold — is found by reading the account once more, and cleared again.
//
// The books are cleared before the hot account, so that a crash between the two leaves the books clear and the hot account holding a block
// nothing else would remove — and asking again finishes it, because the books then say there is nothing to clear and the hot account says
// there is. In the other order a crash would leave the hot account open and the books blocked, and the next rehydrate would put the block
// back. The caller owns the shard (the worker's lease), as for a recovery.
func (w *Writer) Unblock(ctx context.Context, s domain.Scope) (UnblockResult, error) {
	if err := s.Validate(); err != nil {
		return UnblockResult{}, err
	}
	s = s.Normalized()
	shard := s.Shard(w.cfg.Shards)
	var res UnblockResult
	for range unblockAttempts {
		if _, err := w.drain(ctx, shard, false, 0); err != nil {
			return res, fmt.Errorf("unblock %s: drain shard %d: %w", s.Tag(), shard, err)
		}
		books, err := w.d.Books.ClearBlocked(ctx, s)
		if err != nil {
			return res, fmt.Errorf("unblock %s: clear the books: %w", s.Tag(), err)
		}
		res.Books = res.Books || books.Cleared
		hot, err := w.d.Balance.ClearBlocked(ctx, s, books.AppliedSeq)
		if err != nil {
			return res, fmt.Errorf("unblock %s: clear the hot account (the books are clear; ask again): %w", s.Tag(), err)
		}
		if hot == ports.HotUnblockRaced {
			continue // the writer had not caught up with the hot tier: let it, and look again
		}
		res.Hot = res.Hot || hot == ports.HotUnblockCleared
		acct, err := w.d.Balance.Snapshot(ctx, s)
		if err != nil {
			return res, fmt.Errorf("unblock %s: read the hot account afterwards: %w", s.Tag(), err)
		}
		if !acct.Cold && acct.Blocked {
			continue // the block is back: installed after we looked, by a rebuild that read the books before they were cleared
		}
		return res, nil
	}
	return res, fmt.Errorf("unblock %s: %w (the books are clear; ask again)", s.Tag(), ErrUnblockRaced)
}
