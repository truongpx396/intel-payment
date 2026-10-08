package domain_test

import (
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Every policy enum accepts exactly its documented values. The empty string and a near miss are
// refused: a mistyped policy silently falling through to a default is how money moves the wrong way.
func TestEveryPolicyEnumAcceptsItsValuesAndNothingElse(t *testing.T) {
	t.Parallel()
	type validator interface{ Validate() error }
	cases := map[string]struct {
		valid   []validator
		invalid []validator
	}{
		"Settlement": {
			[]validator{domain.SettlementOutbox, domain.SettlementJournal},
			[]validator{domain.SettlementDurability(""), domain.SettlementDurability("Outbox")}},
		"HotAckWait": {
			[]validator{domain.HotAckNone, domain.HotAckAOFLocal, domain.HotAckAOFReplica},
			[]validator{domain.HotAckWait(""), domain.HotAckWait("aof")}},
		"LedgerGranularity": {
			[]validator{domain.GranularityEvent, domain.GranularityRollup},
			[]validator{domain.LedgerGranularity(""), domain.LedgerGranularity("events")}},
		"NegativeBalance": {
			[]validator{domain.AllowDebt, domain.ClampToZero, domain.BlockAndFlag},
			[]validator{domain.NegativeBalancePolicy(""), domain.NegativeBalancePolicy("clamp")}},
		// The literals are the contract with operators (PAYMENT_CREDIT_EXPIRY), so they are pinned as text.
		// `lots_fifo` named a draw order the engine no longer uses; refusing it makes an operator who set
		// it read the new name, instead of a mode quietly changing its meaning under them.
		"CreditExpiry": {
			[]validator{domain.ExpiryOff, domain.CreditExpiry("lots_fefo")},
			[]validator{domain.CreditExpiry(""), domain.CreditExpiry("lots"), domain.CreditExpiry("lots_fifo")}},
		"AdmitFail": {
			[]validator{domain.FailClosed, domain.FailOpen},
			[]validator{domain.AdmitFailPolicy(""), domain.AdmitFailPolicy("closed")}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, v := range c.valid {
				if err := v.Validate(); err != nil {
					t.Fatalf("%v is a documented value: %v", v, err)
				}
			}
			for _, v := range c.invalid {
				if err := v.Validate(); !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("%q must be refused as invalid, got %v", v, err)
				}
			}
		})
	}
}
