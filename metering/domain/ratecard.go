package domain

import "fmt"

// RateCard is IMMUTABLE, versioned configuration data (rate_cards / rate_card_entries; the
// database refuses edits). It names the Pricer that interprets it.
type RateCard struct {
	Realm   Realm
	Version string
	Pricer  string // registry name: "table" (default) | a compiled pricer
	Entries []RateEntry
	Params  map[string]string // read only by a named pricer

	index map[string]map[Unit]RateEntry // built by NewRateCard; nil on a hand-built card
}

// RateEntry prices one (rate key, unit) as a RATIONAL: CreditsPerBlock credits for every
// BlockSize units. $0.15 per million tokens at 1 credit = 1 µ$ is {150000, 1000000} — exact,
// where an integer "credits per unit" cannot express any price below one credit per unit (D31).
type RateEntry struct {
	RateKey            string
	Unit               Unit
	CreditsPerBlock    int64 // >= 0
	BlockSize          int64 // > 0
	CostMicrosPerBlock int64 // informational upstream cost; 0 = unknown
}

// Validate checks one entry.
func (r RateEntry) Validate() error {
	switch {
	case r.RateKey == "":
		return fmt.Errorf("%w: rate entry needs a rate key", ErrInvalid)
	case r.Unit == "":
		return fmt.Errorf("%w: rate entry %q needs a unit", ErrInvalid, r.RateKey)
	case r.CreditsPerBlock < 0:
		return fmt.Errorf("%w: rate entry %q/%q has negative credits_per_block", ErrInvalid, r.RateKey, r.Unit)
	case r.BlockSize <= 0:
		return fmt.Errorf("%w: rate entry %q/%q needs a positive block_size", ErrInvalid, r.RateKey, r.Unit)
	case r.CostMicrosPerBlock < 0:
		return fmt.Errorf("%w: rate entry %q/%q has negative cost_micros_per_block", ErrInvalid, r.RateKey, r.Unit)
	}
	return nil
}

// NewRateCard validates a card and indexes it by (rate key, unit), so pricing an event is a map
// lookup rather than a scan of every entry. Stores return cards built here. A duplicate
// (rate key, unit) is refused: two prices for one thing is an ambiguity, not a choice.
func NewRateCard(realm Realm, version, pricer string, entries []RateEntry, params map[string]string) (RateCard, error) {
	if version == "" {
		return RateCard{}, fmt.Errorf("%w: rate card needs a version", ErrInvalid)
	}
	if pricer == "" {
		pricer = "table"
	}
	idx := make(map[string]map[Unit]RateEntry)
	for _, e := range entries {
		if err := e.Validate(); err != nil {
			return RateCard{}, err
		}
		if idx[e.RateKey] == nil {
			idx[e.RateKey] = make(map[Unit]RateEntry)
		}
		if _, dup := idx[e.RateKey][e.Unit]; dup {
			return RateCard{}, fmt.Errorf("%w: card %q prices %q/%q twice", ErrInvalid, version, e.RateKey, e.Unit)
		}
		idx[e.RateKey][e.Unit] = e
	}
	return RateCard{Realm: realm.Or(), Version: version, Pricer: pricer, Entries: entries, Params: params, index: idx}, nil
}

// Rates returns the entries for one rate key, keyed by unit; empty when the key is unknown. It uses
// the index when the card was built by NewRateCard and falls back to a scan otherwise, so a
// hand-built card in a test behaves identically, only slower.
func (c RateCard) Rates(rateKey string) map[Unit]RateEntry {
	if c.index != nil {
		return c.index[rateKey]
	}
	var out map[Unit]RateEntry
	for _, e := range c.Entries {
		if e.RateKey == rateKey {
			if out == nil {
				out = make(map[Unit]RateEntry)
			}
			out[e.Unit] = e
		}
	}
	return out
}
