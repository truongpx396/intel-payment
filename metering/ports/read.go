package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// The reads a transport offers beside the Meter: the books, as a caller sees them. They never write.

// LedgerQuery asks for one scope's booked rows, newest first.
type LedgerQuery struct {
	Scope domain.Scope
	// SubjectKind and SubjectID, both or neither, restrict the read to the rows one member's spend produced
	// (a member-scoped read). Rows attributed to nobody — a purchase, a grant — are not that member's.
	SubjectKind, SubjectID string
	// After is the opaque cursor of the previous page. Empty starts at the newest row.
	After string
	// Limit is the page size: 1..MaxLedgerPage, and 0 means DefaultLedgerPage.
	Limit int
}

// Page sizes of a ledger read.
const (
	DefaultLedgerPage = 50
	MaxLedgerPage     = 200
)

// LedgerEntry is a booked row with its identity.
type LedgerEntry struct {
	ID        string
	CreatedAt time.Time
	LedgerRow
}

// LedgerPage is one page. Next is empty on the last.
type LedgerPage struct {
	Rows []LedgerEntry
	Next string
}

// LedgerReader reads the account of record. A page is a keyset, so every row appears exactly once across
// pages however many are booked between them. A bad cursor, limit, scope or half a member is ErrInvalid.
type LedgerReader interface {
	LedgerRows(ctx context.Context, q LedgerQuery) (LedgerPage, error)
}

// TransferReader reads a transfer's status from the books. found=false means no writer has booked its first
// leg yet — a transfer is accepted in the hot tier first.
type TransferReader interface {
	Transfer(ctx context.Context, realm domain.Realm, idemKey string) (TransferRecord, bool, error)
}

// UsageReader reads how much of each window limit a scope has used: what the credits panel draws. It is the
// read Admit makes of the counters, without the verdict, and it changes nothing. The answer is keyed by limit
// name; a balance has no counter and is not in it. A limit that needs a member the caller did not name is
// ErrMissingSubject, and a scope that was never built is ErrColdScope.
type UsageReader interface {
	Usage(ctx context.Context, s domain.Scope, subjects domain.Subjects, limits []domain.Limit) (map[string]int64, error)
}
