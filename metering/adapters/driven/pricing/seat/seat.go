// Package seat is the reference pricer for seat-based metering: it validates the unit vocabulary,
// then delegates to the table pricer. Mid-period proration of a subscription is the provider's,
// under PlanChangePolicy — not this pricer's.
package seat

import (
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// SeatDay is one seat held for one day.
const SeatDay domain.Unit = "seat_day"

// Name is the registry name a rate card selects this pricer by.
const Name = "seat"

// Pricer is the seat pricer.
type Pricer = pricing.Vocabulary

// New returns the pricer, bounded by limit (0 = the default 2^50).
func New(limit domain.Credits) Pricer {
	return pricing.NewVocabulary(Name, table.New(limit), SeatDay)
}
