package wire_test

import (
	"errors"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/wire"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// namesOf is the one hand-kept list: the exported domain errors by variable name. Go cannot reflect a
// package variable from its value, so the scan above reads the names from the source and this maps
// them to values. A new Err* fails the scan until it is added here AND to the table.
var namesOf = map[string]error{
	"ErrInvalid":             domain.ErrInvalid,
	"ErrNotFound":            domain.ErrNotFound,
	"ErrAlreadyExists":       domain.ErrAlreadyExists,
	"ErrIdemConflict":        domain.ErrIdemConflict,
	"ErrStaleEvent":          domain.ErrStaleEvent,
	"ErrMissingSubject":      domain.ErrMissingSubject,
	"ErrUnpriceable":         domain.ErrUnpriceable,
	"ErrAmountOutOfRange":    domain.ErrAmountOutOfRange,
	"ErrInsufficient":        domain.ErrInsufficient,
	"ErrDestBalanceCap":      domain.ErrDestBalanceCap,
	"ErrShardCountMismatch":  domain.ErrShardCountMismatch,
	"ErrCrossRealm":          domain.ErrCrossRealm,
	"ErrHotStoreUnavailable": domain.ErrHotStoreUnavailable,
	"ErrQuotaUnavailable":    domain.ErrQuotaUnavailable,
	"ErrQuotaNotGranted":     domain.ErrQuotaNotGranted,
	"ErrUncountableLimit":    domain.ErrUncountableLimit,
	"ErrColdScope":           domain.ErrColdScope,
	"ErrColdShard":           domain.ErrColdShard,
	"ErrShardFrozen":         domain.ErrShardFrozen,
	"ErrBackpressure":        domain.ErrBackpressure,
}

// mappedSentinels is the set of names that have a row of their own in the table: classifying the error
// finds a row whose sentinel IS that error, not a fallback and not a neighbour's row.
func mappedSentinels() map[string]bool {
	out := map[string]bool{}
	for name, err := range namesOf {
		if c, ok := wire.Classify(err); ok && errors.Is(c.Err, err) {
			out[name] = true
		}
	}
	return out
}
