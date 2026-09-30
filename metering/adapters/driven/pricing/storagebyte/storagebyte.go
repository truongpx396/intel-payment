// Package storagebyte is the reference pricer for object storage: it validates the unit
// vocabulary, then delegates to the table pricer. Tiers and free allowances depend on
// period-to-date volume, so they are a Rater concern (feature 002), not this pricer's.
package storagebyte

import (
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// The units this pricer meters.
const (
	StorageByteDay domain.Unit = "storage_byte_day"
	EgressByte     domain.Unit = "egress_byte"
)

// Name is the registry name a rate card selects this pricer by.
const Name = "storagebyte"

// Pricer is the storagebyte pricer.
type Pricer = pricing.Vocabulary

// New returns the pricer, bounded by limit (0 = the default 2^50).
func New(limit domain.Credits) Pricer {
	return pricing.NewVocabulary(Name, table.New(limit), StorageByteDay, EgressByte)
}
