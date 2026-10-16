package domain

import (
	"fmt"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// DisputePhase is a step of a dispute that moves credits.
type DisputePhase string

// The phases that move credits. Opening a dispute moves none: funds have not left yet.
const (
	DisputeWithdrawn  DisputePhase = "withdrawn"
	DisputeReinstated DisputePhase = "reinstated"
)

// The dispute statuses a payment records.
const (
	disputeOpen = "open"
	disputeWon  = "won"
	disputeLost = "lost"
)

// DisputeKey is the idempotency key of a dispute phase's grant: the dispute id and the phase, so the two phases
// of one dispute are two guards and a replay of either is one effect (invariant 17).
func DisputeKey(disputeID string, phase DisputePhase) string {
	return "dispute:" + disputeID + ":" + string(phase)
}

// DisputePlan is what a dispute event does. Delta is the signed credits for Meter.Grant under IdemKey and
// Reason; zero means there is nothing to grant and the call is skipped. Payment is the payment after the event.
type DisputePlan struct {
	Delta   Credits
	Payment Payment
	IdemKey string
	Reason  string
}

// settledStatus is what a payment is when nothing is disputed: its refunds decide.
func (p Payment) settledStatus() PaymentStatus {
	switch {
	case p.RefundedMinor > 0 && p.RefundedMinor == p.Amount.MinorUnits:
		return Refunded
	case p.RefundedMinor > 0:
		return PartiallyRefunded
	}
	return Succeeded
}

// OpenDispute records that a dispute exists. No credits move: funds have not been taken. An opening that
// arrives after the dispute already progressed (deliveries are unordered) is ignored, so it can never
// un-charge-back a payment.
func (p Payment) OpenDispute(id string) Payment {
	if p.DisputeID == id && p.DisputeStatus != "" {
		return p
	}
	p.DisputeID, p.DisputeStatus = id, disputeOpen
	switch p.Status {
	case Succeeded, PartiallyRefunded, Refunded:
		p.Status = Disputed
	}
	return p
}

// WithdrawFunds is the chargeback: the provider has taken the money, so the credits it bought go back —
// everything the payment granted that neither a refund nor expiry has already taken. expired is Σ lot.Expired,
// as for a refund (invariant 20). What it takes is remembered in DisputeClawed so a won dispute can give back
// exactly that (invariant 17).
//
// A replay changes nothing. So does a withdrawal that arrives after the dispute was already won: the
// reinstatement came first, and there is nothing to take back.
func (p Payment) WithdrawFunds(id string, expired Credits) (DisputePlan, error) {
	if expired < 0 {
		return DisputePlan{}, fmt.Errorf("%w: expired credits %d is negative", md.ErrInvalid, expired)
	}
	if p.DisputeID == id && (p.Status == ChargedBack || p.DisputeStatus == disputeWon) {
		return DisputePlan{Payment: p}, nil
	}
	claw := max(p.CreditsGranted-expired-p.CreditsRevoked, 0)
	p.DisputeID = id
	if p.DisputeStatus == "" {
		p.DisputeStatus = disputeOpen
	}
	p.Status = ChargedBack
	p.CreditsRevoked += claw
	p.DisputeClawed = claw
	return DisputePlan{Delta: -claw, Payment: p, IdemKey: DisputeKey(id, DisputeWithdrawn), Reason: "chargeback"}, nil
}

// ReinstateFunds is a won dispute: the chargeback reverses. It gives back exactly what the withdrawal took, and
// only if a withdrawal was applied for this dispute — a reinstatement that arrives first records the verdict
// and grants nothing, and the withdrawal that follows then takes nothing.
func (p Payment) ReinstateFunds(id string) (DisputePlan, error) {
	if p.DisputeID == id && p.DisputeStatus == disputeWon {
		return DisputePlan{Payment: p}, nil
	}
	var restore Credits
	if p.DisputeID == id && p.Status == ChargedBack {
		restore = p.DisputeClawed
	}
	p.DisputeID, p.DisputeStatus = id, disputeWon
	p.CreditsRevoked -= restore
	p.DisputeClawed = 0
	if p.Status == ChargedBack || p.Status == Disputed {
		p.Status = p.settledStatus()
	}
	return DisputePlan{Delta: restore, Payment: p, IdemKey: DisputeKey(id, DisputeReinstated), Reason: "chargeback_reversal"}, nil
}

// CloseDispute applies a verdict. Won is the reinstatement. Lost takes nothing further — the withdrawal already
// did, or will when it arrives — and keeps the payment charged back.
func (p Payment) CloseDispute(id string, won bool) (DisputePlan, error) {
	if won {
		return p.ReinstateFunds(id)
	}
	if p.DisputeID == id && p.DisputeStatus == disputeWon {
		return DisputePlan{Payment: p}, nil
	}
	p.DisputeID, p.DisputeStatus = id, disputeLost
	return DisputePlan{Payment: p}, nil
}
