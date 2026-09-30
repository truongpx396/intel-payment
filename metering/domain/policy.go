package domain

import "fmt"

// Every policy is a closed set: an unknown value is refused at Validate rather than guessed at,
// because a typo in a money policy that silently falls back to a default is a billing incident.

// SettlementDurability selects the durability/latency trade for Record.
type SettlementDurability string

const (
	SettlementOutbox  SettlementDurability = "outbox"  // one Redis Function, no durable wait (default)
	SettlementJournal SettlementDurability = "journal" // Postgres first, then the hot function: no under-bill
)

// HotAckWait narrows outbox mode's loss window by waiting for the hot write to be durable before
// acknowledging (hot-path-consistency.md §5). Ignored under SettlementJournal.
type HotAckWait string

const (
	HotAckNone       HotAckWait = "none"        // default
	HotAckAOFLocal   HotAckWait = "aof_local"   // WAITAOF 1 0: survives a crash
	HotAckAOFReplica HotAckWait = "aof_replica" // WAITAOF 0 1: survives losing the primary
)

// LedgerGranularity decides how usage is booked. `event` writes one ledger row per event and pool;
// `rollup` writes one row per (scope, pool, resource, rate key, card version) per drain batch and
// archives per-event detail through the UsageArchive port — the path past a single Postgres's
// per-event write budget (plan.md § Scale envelope).
type LedgerGranularity string

const (
	GranularityEvent  LedgerGranularity = "event" // default
	GranularityRollup LedgerGranularity = "rollup"
)

// NegativeBalancePolicy decides what happens when a refund/chargeback exceeds the credits still
// held in a pool — not an edge case but the normal outcome of refunding consumed usage.
type NegativeBalancePolicy string

const (
	AllowDebt    NegativeBalancePolicy = "allow_debt"     // record the negative; keep serving; the host collects
	ClampToZero  NegativeBalancePolicy = "clamp_to_zero"  // floor it AND book a `writeoff` row (default)
	BlockAndFlag NegativeBalancePolicy = "block_and_flag" // floor it, book the writeoff, refuse spend until cleared
)

// CreditExpiry decides whether granted credits have a lifetime.
type CreditExpiry string

const (
	ExpiryOff      CreditExpiry = "off"       // default — a grant never expires
	ExpiryLotsFIFO CreditExpiry = "lots_fifo" // credit_lots per pool; oldest lot consumed and expired first
)

// AdmitFailPolicy is what Admit does when the hot store is unreachable or the shard is frozen.
// Record never drops usage either way: it defers to the journal (invariant 16).
type AdmitFailPolicy string

const (
	FailClosed AdmitFailPolicy = "fail_closed" // refuse (default)
	FailOpen   AdmitFailPolicy = "fail_open"   // serve; usage is journaled and metered on recovery
)

func oneOf[T ~string](name string, v T, allowed ...T) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%w: %s must be one of %v, got %q", ErrInvalid, name, allowed, string(v))
}

func (v SettlementDurability) Validate() error {
	return oneOf("Settlement", v, SettlementOutbox, SettlementJournal)
}
func (v HotAckWait) Validate() error {
	return oneOf("HotAckWait", v, HotAckNone, HotAckAOFLocal, HotAckAOFReplica)
}
func (v LedgerGranularity) Validate() error {
	return oneOf("LedgerGranularity", v, GranularityEvent, GranularityRollup)
}
func (v NegativeBalancePolicy) Validate() error {
	return oneOf("NegativeBalance", v, AllowDebt, ClampToZero, BlockAndFlag)
}
func (v CreditExpiry) Validate() error {
	return oneOf("CreditExpiry", v, ExpiryOff, ExpiryLotsFIFO)
}
func (v AdmitFailPolicy) Validate() error {
	return oneOf("AdmitFail", v, FailClosed, FailOpen)
}
