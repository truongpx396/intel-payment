package domain

import (
	"fmt"
	"time"
)

// Charge settles a completed unit of work on the hot tier. Built by the Meter from an Event and
// its Price; the Ledger derives the request fingerprint from the canonical charge.
type Charge struct {
	Scope      Scope
	Amount     Credits  // positive magnitude to debit
	IdemKey    string   // REQUIRED
	Reason     string   // operation_type: "query" | "ingest" | "agent_step" | …
	Resource   string   // selects eligible pools and the counters it increments
	Subjects   Subjects // increments subject counters, including a job budget
	Quantities []Quantity
	Ref        map[string]string

	// The event, carried so the intent is a complete usage record (invariant 8) and so the request
	// fingerprint covers what identifies the operation. None of these is a cost input to the ledger.
	RateKey         string
	RateCardVersion string
	CostMicros      int64
	OccurredAt      time.Time
}

// Validate checks a Charge structurally. The range check against MaxOperationAmount belongs to the
// component that owns the configuration.
func (c Charge) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if c.IdemKey == "" {
		return fmt.Errorf("%w: idem_key is required", ErrInvalid)
	}
	if c.Amount < 0 {
		return fmt.Errorf("%w: a charge amount is a positive magnitude, got %d", ErrInvalid, c.Amount)
	}
	if err := c.Subjects.Validate(); err != nil {
		return err
	}
	for _, q := range c.Quantities {
		if q.Amount < 0 {
			return fmt.Errorf("%w: quantity %q is negative", ErrInvalid, q.Unit)
		}
	}
	return nil
}

// Grant adds or removes credits in one pool (payments, refunds, chargebacks, promos, admin).
type Grant struct {
	Scope     Scope
	Pool      Pool       // "" = GeneralPool; a plan's allotment names its pool
	Amount    Credits    // positive to add; NEGATIVE for refund/chargeback — NegativeBalancePolicy applies
	IdemKey   string     // REQUIRED; unique per realm: one provider payment mints once, for one scope
	Reason    string     // "purchase" | "subscription_grant" | "signup_grant" | "promo" | "refund" | "chargeback" | "chargeback_reversal" | "admin_adjustment"
	ExpiresAt *time.Time // the lot's expiry under CreditExpiry=lots_fifo
	Ref       map[string]string
}

// Validate checks a Grant structurally.
func (g Grant) Validate() error {
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if err := g.Pool.Validate(); err != nil {
		return err
	}
	if g.IdemKey == "" {
		return fmt.Errorf("%w: idem_key is required", ErrInvalid)
	}
	if g.Reason == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	if g.Amount == 0 {
		return fmt.Errorf("%w: a grant of zero credits changes nothing", ErrInvalid)
	}
	if g.ExpiresAt != nil && g.Amount < 0 {
		return fmt.Errorf("%w: only a positive grant can expire", ErrInvalid)
	}
	return nil
}

// Transfer moves credits between two scopes of ONE realm under one IdemKey — the pool →
// allocation pattern: a parent buys once and its children draw allocations. It is NOT a Grant:
// nothing enters or leaves the realm.
//
// It is two-phase (hot-path-consistency.md §4) because the two scopes live in different shards
// and no script may touch both: phase 1 debits the source's HOT pool — refusing rather than
// overdrawing — and the writer then books allocation_out, credits the destination, and books
// allocation_in. Σ ledger + Σ in-transit is invariant at every step, and once phase 1 succeeds
// both sides apply; there is no outcome in which one side moved and the other never will.
type Transfer struct {
	From, To         Scope // MUST share a realm
	FromPool, ToPool Pool  // "" = GeneralPool
	Amount           Credits
	IdemKey          string // REQUIRED
	Reason           string // "allocation" | "reallocation" | "reclaim"
	// MaxDestBalance caps what the destination may HOLD (0 = uncapped), checked against its hot
	// balance at request time. It guards against an errant admin action handing one team the pool.
	MaxDestBalance Credits
	Ref            map[string]string
}

// Validate checks a Transfer structurally, and refuses a cross-realm one.
func (t Transfer) Validate() error {
	if err := t.From.Validate(); err != nil {
		return err
	}
	if err := t.To.Validate(); err != nil {
		return err
	}
	if t.From.Realm.Or() != t.To.Realm.Or() {
		return fmt.Errorf("%w: %s → %s", ErrCrossRealm, t.From.Realm.Or(), t.To.Realm.Or())
	}
	if err := t.FromPool.Validate(); err != nil {
		return err
	}
	if err := t.ToPool.Validate(); err != nil {
		return err
	}
	if t.IdemKey == "" {
		return fmt.Errorf("%w: idem_key is required", ErrInvalid)
	}
	if t.Amount <= 0 {
		return fmt.Errorf("%w: a transfer amount must be positive, got %d", ErrInvalid, t.Amount)
	}
	if t.MaxDestBalance < 0 {
		return fmt.Errorf("%w: max_dest_balance must be >= 0", ErrInvalid)
	}
	if t.From.Normalized() == t.To.Normalized() && t.FromPool.Or() == t.ToPool.Or() {
		return fmt.Errorf("%w: a transfer needs two different accounts", ErrInvalid)
	}
	return nil
}

// Receipt is the outcome of an idempotent apply.
type Receipt struct {
	IdemKey  string
	Applied  bool    // false = idempotent replay, already applied (same key, same fingerprint)
	Deferred bool    // true = accepted into the spend journal while the hot tier was unavailable
	Delta    Credits // the signed delta this call applied (0 on replay)
	Balance  Credits // resulting hot balance across pools (eventual; informational)
	Seq      int64   // the scope's sequence number for this operation (0 when deferred)

	// RateCardVersion is the card that priced a Record. It is on the wire receipt (Receipt.rate_card_version).
	RateCardVersion string
	// Writeoff is the shortfall a clamp_to_zero / block_and_flag policy floored on a negative
	// grant. The writer books it as its own ledger row.
	Writeoff Credits
}

// TransferReceipt reports phase 1. The destination is credited by the writer within milliseconds;
// GET /v1/transfers/{idem_key} reports the transfer "settled".
type TransferReceipt struct {
	IdemKey     string
	Applied     bool // false = idempotent replay
	Amount      Credits
	FromBalance Credits // the source's hot balance after the debit
	Status      string  // TransferInTransit | TransferSettled
}

// Transfer statuses.
const (
	TransferInTransit = "in_transit"
	TransferSettled   = "settled"
)
