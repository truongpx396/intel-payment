package domain

import (
	"fmt"
	"math/bits"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// RevokeTotal is how many of a payment's granted credits a refund of `refunded` (cumulative, in minor units) has
// taken back in total, of a payment of `amount`:
//
//	revoke_total = min(granted, ceil(granted × refunded / amount))
//
// It is computed in 128 bits, so the product cannot wrap, and rounded UP — the one place a fraction of a credit
// is settled, and settled against the customer rather than silently in their favour (constitution I). Because the
// figure is cumulative, three partial refunds revoke exactly what one refund of the same total would: there is
// no rounding drift to accumulate (payments invariant 16, SC-017).
//
// A refund fiat cannot explain — more than was paid, negative, of a payment that cost nothing — is ErrInvalid:
// it fails loudly, and the inbox retries it into a page, instead of inventing a number.
func RevokeTotal(granted Credits, amount, refunded int64) (Credits, error) {
	switch {
	case granted < 0:
		return 0, fmt.Errorf("%w: credits granted %d is negative", md.ErrInvalid, granted)
	case amount < 0:
		return 0, fmt.Errorf("%w: payment amount %d is negative", md.ErrInvalid, amount)
	case refunded < 0:
		return 0, fmt.Errorf("%w: refunded amount %d is negative", md.ErrInvalid, refunded)
	case refunded > amount:
		return 0, fmt.Errorf("%w: refunded %d exceeds the %d that was paid", md.ErrInvalid, refunded, amount)
	}
	if granted == 0 || refunded == 0 {
		return 0, nil
	}
	// refunded > 0 and refunded <= amount, so amount > 0 and the quotient is at most granted: it fits, and the
	// divisor exceeds the high word (bits.Div64 would panic otherwise).
	hi, lo := bits.Mul64(uint64(granted), uint64(refunded))
	q, r := bits.Div64(hi, lo, uint64(amount))
	if r != 0 {
		q++
	}
	if q > uint64(granted) { //nolint:gosec // granted > 0 here, so the conversion is exact
		return granted, nil
	}
	return Credits(q), nil //nolint:gosec // q <= granted < 2^63, checked on the line above
}

// CapAtUnexpired limits a clawback to what expiry has not already taken: min(revoke, granted − expired). Under
// CreditExpiry=lots_fefo a payment's credits are a lot that may have lapsed unspent, and revoking those credits
// again would take them from the customer twice — out of whatever other purchase still holds credits
// (invariant 20). With expiry off there are no lots, expired is 0, and the cap is a no-op.
func CapAtUnexpired(revoke, granted, expired Credits) Credits {
	if expired < 0 {
		expired = 0
	}
	left := granted - expired
	if left < 0 {
		left = 0
	}
	if revoke > left {
		return left
	}
	return revoke
}

// RefundPlan is what to do about one refund report.
type RefundPlan struct {
	// Revoke is the credits to take back NOW, never negative. Zero means nothing: a replayed report, an older
	// one, or credits that expiry already took.
	Revoke Credits
	// RevokedTotal is the payment's cumulative clawback after this report.
	RevokedTotal Credits
	// RefundedMinor is the payment's cumulative refund after this report. It never decreases.
	RefundedMinor int64
	Status        PaymentStatus
}

// ApplyRefund reads a refund report — the CUMULATIVE amount the provider says has been refunded — against the
// payment and says what to revoke now. It changes nothing: a refund appends a negative grant and updates the
// payment's cumulative fields; the original amount and grant are immutable (invariant 8).
//
// expired is Σ lot.Expired of the payment's credits, read from the books, so a refund never claws back what
// expiry already took (invariant 20). Credits the customer SPENT are still revoked; what a refund past the
// spend does is NegativeBalancePolicy's decision, not this function's.
func (p Payment) ApplyRefund(cumulative Money, expired Credits) (RefundPlan, error) {
	switch {
	case expired < 0:
		return RefundPlan{}, fmt.Errorf("%w: expired credits %d is negative", md.ErrInvalid, expired)
	case cumulative.Currency != p.Amount.Currency:
		return RefundPlan{}, fmt.Errorf("%w: refund in %s for a payment in %s", md.ErrInvalid, cumulative.Currency, p.Amount.Currency)
	case cumulative.MinorUnits < 0 || cumulative.MinorUnits > p.Amount.MinorUnits:
		return RefundPlan{}, fmt.Errorf("%w: refunded %d of a %d payment", md.ErrInvalid, cumulative.MinorUnits, p.Amount.MinorUnits)
	}
	// Providers do not order deliveries: a report older than one already applied must not lower the total.
	refunded := max(cumulative.MinorUnits, p.RefundedMinor)
	total, err := RevokeTotal(p.CreditsGranted, p.Amount.MinorUnits, refunded)
	if err != nil {
		return RefundPlan{}, err
	}
	total = CapAtUnexpired(total, p.CreditsGranted, expired)
	now := max(total-p.CreditsRevoked, 0)

	status := p.Status
	switch p.Status {
	case Succeeded, PartiallyRefunded, Refunded:
		switch {
		case refunded > 0 && refunded == p.Amount.MinorUnits:
			status = Refunded
		case refunded > 0:
			status = PartiallyRefunded
		}
	}
	return RefundPlan{Revoke: now, RevokedTotal: p.CreditsRevoked + now, RefundedMinor: refunded, Status: status}, nil
}
