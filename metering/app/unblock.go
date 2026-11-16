package app

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// UnblockResult says where the block was held. Each is independent of the other: an unblock that finds the books already clear and the hot
// account still blocked is the second half of one that was interrupted, and reports exactly that.
type UnblockResult struct {
	Books bool // the books held the block and no longer do
	Hot   bool // the live hot account held the block and no longer does
}

// Unblock clears block_and_flag for one scope: the durable flag in the books, then the one the admission gate reads in the hot account
// (R112a). It is an operator's act and is the only way the block is ever cleared; a top-up does not.
//
// The shard is drained first. A write-off the hot tier has issued and the writer has not yet booked is going to set the block in the
// books when it is booked, after this has cleared it, and the two would then disagree about whether the account is blocked. With the
// writer caught up, the block the books hold is the block the hot tier holds.
//
// The books are cleared before the hot account, so that a crash between the two leaves the books clear and the hot account holding a
// block nothing else would remove — and asking again finishes it, because the books then say there is nothing to clear and the hot account
// says there is. In the other order a crash would leave the hot account open and the books blocked, and the next rehydrate would put the
// block back. The caller owns the shard (the worker's lease), as for a recovery.
func (w *Writer) Unblock(ctx context.Context, s domain.Scope) (UnblockResult, error) {
	if err := s.Validate(); err != nil {
		return UnblockResult{}, err
	}
	s = s.Normalized()
	shard := s.Shard(w.cfg.Shards)
	if _, err := w.drain(ctx, shard, false, 0); err != nil {
		return UnblockResult{}, fmt.Errorf("unblock %s: drain shard %d: %w", s.Tag(), shard, err)
	}
	books, err := w.d.Books.ClearBlocked(ctx, s)
	if err != nil {
		return UnblockResult{}, fmt.Errorf("unblock %s: clear the books: %w", s.Tag(), err)
	}
	hot, err := w.d.Balance.ClearBlocked(ctx, s)
	if err != nil {
		return UnblockResult{Books: books}, fmt.Errorf("unblock %s: clear the hot account (the books are clear; ask again): %w", s.Tag(), err)
	}
	return UnblockResult{Books: books, Hot: hot}, nil
}
