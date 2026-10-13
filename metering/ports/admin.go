package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Configuration is data (constitution VIII), and the admin API is its supported interface (FR-049): every
// write is validated, names a person and a reason, and is audited by the database in the same transaction.

// ConfigChange is who changed configuration and why. The database writes the audit row; this is what it
// writes into it. Via is the service credential the request came through, for the operator who asks which
// tool did it.
type ConfigChange struct {
	Actor, Reason, Via string
}

// CardPublication is a rate card to publish.
type CardPublication struct {
	Version string
	Pricer  string // "" = table
	Entries []domain.RateEntry
	// RetirePrevious retires the realm's active card in the same transaction. A realm has exactly one active
	// card (the database enforces it), so publishing without retiring one that exists is refused.
	RetirePrevious bool
}

// RateCardInfo is a published card with its lifetime.
type RateCardInfo struct {
	Card        domain.RateCard
	PublishedAt time.Time
	RetiredAt   *time.Time // nil = the active card
}

// LimitConfig is a configured ceiling. SortOrder, when nil, keeps a limit's place on an update and puts a new
// one last: the order is the order they are evaluated in, and the first exceeded is the one reported.
type LimitConfig struct {
	Limit     domain.Limit
	SortOrder *int
}

// PoolConfig is a configured credit pool.
type PoolConfig struct {
	Pool        domain.PoolDef
	Description string
}

// ConfigStore is the part of configuration both sides share: the reads and the writes that need no
// validation beyond what the rules already did.
type ConfigStore interface {
	RateCards(ctx context.Context, realm domain.Realm) ([]RateCardInfo, error)
	// PutLimit creates or replaces a limit; created says which. ErrInvalid for a shape the database refuses.
	PutLimit(ctx context.Context, ch ConfigChange, realm domain.Realm, l LimitConfig) (created bool, err error)
	// Limit and Pool read one by name; ErrNotFound when there is none.
	Limit(ctx context.Context, realm domain.Realm, name string) (LimitConfig, error)
	// DeleteLimit removes a limit; ErrNotFound when there is none.
	DeleteLimit(ctx context.Context, ch ConfigChange, realm domain.Realm, name string) error
	PutPool(ctx context.Context, ch ConfigChange, realm domain.Realm, p PoolConfig) (created bool, err error)
	Pool(ctx context.Context, realm domain.Realm, name string) (PoolConfig, error)
}

// ConfigWriter is the driven side: the database, which sets the actor and reason for the transaction so its
// own trigger writes an audit row that names them — and writes one, attributed to the role, for a change made
// directly in SQL.
type ConfigWriter interface {
	ConfigStore
	// PublishRateCard inserts a validated card, retiring the active one in the same transaction when asked.
	// ErrAlreadyExists when the version exists, or an active card does and was not retired.
	PublishRateCard(ctx context.Context, ch ConfigChange, realm domain.Realm, card domain.RateCard, retirePrevious bool) (RateCardInfo, error)
}

// Admin is the driving side the admin API calls: ConfigWriter behind the rules. A card is published only
// after a test pricing (a card that overflows, prices negative or names an unregistered pricer is refused),
// and every write names a person and a reason.
type Admin interface {
	ConfigStore
	PublishRateCard(ctx context.Context, ch ConfigChange, realm domain.Realm, p CardPublication) (RateCardInfo, error)
}
