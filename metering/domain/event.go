package domain

import (
	"fmt"
	"time"
)

// Unit is a host-extensible metered dimension. The kernel ships none as special.
type Unit string // "llm_input_token" | "storage_byte_day" | "api_call" | "seat_day" | "compute_ms" | …

// CreditUnit is the unit of a limit counted in credits rather than in a metered quantity.
const CreditUnit Unit = "credit"

// Quantity is an integer count of a Unit.
type Quantity struct {
	Unit   Unit
	Amount int64 // integer count of Unit, >= 0
}

// Event is one metered occurrence. A single event may carry several quantities.
type Event struct {
	Scope      Scope
	Resource   string     // host taxonomy: "llm.chat" | "storage" | … — selects limits and eligible pools
	RateKey    string     // pricing selector (model / tier / sku) — a LEGITIMATE cost input
	Quantities []Quantity // integer counts, priced by the Pricer over RateKey + Quantities
	Subjects   Subjects   // who/what within the scope; keys subject ceilings; never a cost input
	IdemKey    string     // REQUIRED, stable across retries of one logical operation (SC-002)
	// OccurredAt is REQUIRED and MUST be identical on every retry. An event older than the
	// usage dedup window is refused (ErrStaleEvent, 422) rather than risk a second charge once
	// its durable guard has expired (hot-path-consistency.md §6).
	OccurredAt time.Time
	Attributes map[string]string // trace_id, feature, provider — usage-log/audit ONLY, never a cost input
}

// Validate checks the parts of an Event that do not depend on configuration. Staleness is checked
// by the Meter, which owns the clock.
func (e Event) Validate() error {
	if err := e.Scope.Validate(); err != nil {
		return err
	}
	if e.IdemKey == "" {
		return fmt.Errorf("%w: idem_key is required", ErrInvalid)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("%w: occurred_at is required and must be identical on every retry", ErrInvalid)
	}
	if e.RateKey == "" {
		return fmt.Errorf("%w: rate_key is required", ErrInvalid)
	}
	if err := e.Subjects.Validate(); err != nil {
		return err
	}
	for _, q := range e.Quantities {
		if q.Unit == "" {
			return fmt.Errorf("%w: quantity unit is required", ErrInvalid)
		}
		if q.Amount < 0 {
			return fmt.Errorf("%w: quantity %q is negative", ErrInvalid, q.Unit)
		}
	}
	return nil
}

// Price is the output of pricing an Event.
type Price struct {
	Credits         Credits // positive magnitude to debit, rounded UP once per event
	CostMicros      int64   // upstream cost in USD micros — informational, for analytics only
	RateCardVersion string  // persisted on the ledger row: replay is only possible if the row names its card
}
