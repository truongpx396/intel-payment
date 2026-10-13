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
	// Granted is everything the books ever credited to the scope: the positive rows, less the writeoffs
	// (forgiven debt is not a grant). A transfer IN is a grant to its destination.
	Granted(ctx context.Context, s domain.Scope) (domain.Credits, error)
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

// ----------------------------------------------------------- credits snapshot --

// SnapshotQuery asks for a scope's credits snapshot. SubjectKind and SubjectID, both or neither, make it a
// member's: the scope-wide ceilings and that member's own, with that member's counters, and only the rows that
// member's spend produced. A member must not learn colleagues' usage from a credits screen.
type SnapshotQuery struct {
	Scope                  domain.Scope
	SubjectKind, SubjectID string
	LedgerAfter            string // the ledger page's cursor
	LedgerLimit            int    // the ledger page's size; 0 is DefaultLedgerPage
}

// CreditsSnapshot is what a credits panel draws, in one read (FR-036). Its shape is the credits-ui contract's
// CreditsSnapshot, so the wire needs no translation table. The usage_daily rollup (a burn rate, a series, a
// breakdown) is a later phase: until then those are absent, not invented.
type CreditsSnapshot struct {
	Scope      domain.Scope
	Balance    BalanceView
	Limits     []LimitView
	Ledger     []LedgerEntry
	LedgerNext string
}

// BalanceView is the hero: what is left, what was granted, and the parts.
type BalanceView struct {
	Remaining domain.Credits // Σ of Pools, exactly: the panel shows both, and they close
	Granted   domain.Credits
	// PrimaryLimitKey is the limit the hero mirrors: the first Balance limit, else the first limit.
	PrimaryLimitKey string
	Pools           []PoolView // in draw order
}

// PoolView is one credit pool's share. Label is the key.
type PoolView struct {
	Key       string
	Remaining domain.Credits
	AppliesTo string // the resources a restricted pool may pay for, comma-joined; empty = every resource
}

// LimitView is one ceiling as a person reads it. It is the kernel's Limit, pass-through.
type LimitView struct {
	Key       string // the limit's name; also its label
	Used      int64
	Cap       *int64 // nil = uncapped
	Window    string // none | day | hour | period | job — the view vocabulary, not the kernel's
	ResetsAt  *time.Time
	WarnAtPct int // from Limit.WarnAt, whole percent
	DenyCode  string
	ScopeHint string // "all members" | "you" — set only in a member's view
}

// CreditsReader builds the snapshot. The cap it shows is the cap Admit enforces: it sizes entitlement-sized
// limits through the same QuotaSource.
type CreditsReader interface {
	Credits(ctx context.Context, q SnapshotQuery) (CreditsSnapshot, error)
}
