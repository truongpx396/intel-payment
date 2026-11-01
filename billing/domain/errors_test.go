package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/truongpx396/intel-payment/billing/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R099e: billing reuses the engine's errors as they are, and a layer that speaks for billing recognises them by billing's
// names. They must be the SAME values: an error the engine returned, wrapped any depth, is found by either name — a door that
// asked for a copy would answer 500 for what is really the caller's mistake.
func TestTheEnginesErrorsAreBillingsToo(t *testing.T) {
	t.Parallel()
	for name, pair := range map[string][2]error{
		"invalid":        {domain.ErrInvalid, md.ErrInvalid},
		"not found":      {domain.ErrNotFound, md.ErrNotFound},
		"already exists": {domain.ErrAlreadyExists, md.ErrAlreadyExists},
		"idem conflict":  {domain.ErrIdemConflict, md.ErrIdemConflict},
	} {
		wrapped := fmt.Errorf("a payment: %w", fmt.Errorf("deeper: %w", pair[1]))
		if !errors.Is(wrapped, pair[0]) {
			t.Errorf("%s: the engine's error is not found by billing's name", name)
		}
	}
	if errors.Is(domain.ErrInvalid, domain.ErrNotFound) {
		t.Error("two different errors are the same error")
	}
}
