package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// UsageStore is the usage_daily rollup (FR-039): derived from the booked ledger, never a billing input, and recomputed rather than added to.
type UsageStore interface {
	// Rollup recomputes the days from `from` through `to` in loc from the booked usage rows of the ledger (priced rows: the ones that carry a
	// rate key — grants, expiries, transfers and write-offs are not spend), and makes usage_daily say exactly
	// that for those days: a row whose totals changed is rewritten, a row nothing now supports is deleted, and a row that is already right is
	// not touched. It returns how many rows it wrote or deleted, so a window nothing changed in returns 0 and has written nothing. Because it
	// is a pure function of the ledger, running it twice, or concurrently, or after a run that was killed midway, leaves the same rows. Days
	// outside the window are never read or written.
	Rollup(ctx context.Context, from, to time.Time, loc *time.Location) (changed int, err error)
	// Breakdown is a scope's spend by dimension over a range of days, one member's or — when the query says so by name — everyone's.
	Breakdown(ctx context.Context, q domain.BreakdownQuery) ([]domain.BreakdownRow, error)
}
