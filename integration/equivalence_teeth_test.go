//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// A conformance suite is held to the rule it enforces: shown to FAIL against a deliberately broken
// implementation, or it has not been shown to test anything. These are broken transports — each a faithful
// client with one defect of the kinds a wire is prone to — and scripts/verify-equivalence-teeth.sh runs the
// suite against every one and requires it to be rejected, by name.
//
// Skipped in an ordinary run: a broken transport fails by design.

type brokenTransport struct {
	ports.Meter // the real client
	defect      string
}

var _ ports.Meter = brokenTransport{}

// flatten is the classic: an error serialised to its text. Every typed error stops being itself.
func (b brokenTransport) err(err error) error {
	if err != nil && b.defect == "flatten-errors" {
		return errors.New(err.Error())
	}
	return err
}

func (b brokenTransport) Admit(ctx context.Context, s domain.Scope, r domain.AdmitRequest) (domain.Admission, error) {
	adm, err := b.Meter.Admit(ctx, s, r)
	if err == nil && !adm.Allowed && b.defect == "refusal-as-error" {
		return domain.Admission{}, errors.New("denied") // a decision reported as a failure
	}
	return adm, b.err(err)
}

func (b brokenTransport) Record(ctx context.Context, e domain.Event) (domain.Receipt, error) {
	if b.defect == "default-time" && e.OccurredAt.IsZero() {
		e.OccurredAt = time.Unix(0, 0) // a missing time encoded as the epoch
	}
	rc, err := b.Meter.Record(ctx, e)
	if b.defect == "retry" && err == nil {
		rc, err = b.Meter.Record(ctx, e) // sent twice: the caller sees the replay of its own first success
	}
	return rc, b.err(err)
}

func (b brokenTransport) Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error) {
	rc, err := b.Meter.Grant(ctx, g)
	if b.defect == "retry" && err == nil {
		rc, err = b.Meter.Grant(ctx, g)
	}
	if b.defect == "drop-writeoff" {
		rc.Writeoff = 0 // a field the first draft of the wire left out
	}
	return rc, b.err(err)
}

func (b brokenTransport) Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	rc, err := b.Meter.Transfer(ctx, t)
	return rc, b.err(err)
}

func TestMeterEquivalenceBroken(t *testing.T) {
	t.Parallel()
	defect := os.Getenv("EQUIVALENCE_BROKEN")
	if defect == "" {
		t.Skip("run by scripts/verify-equivalence-teeth.sh: a broken transport is expected to fail the suite")
	}
	contracts.MeterEquivalence(t, func(t *testing.T, o contracts.MeterOptions) contracts.MeterHarness {
		s, h := equivalenceStack(t, o)
		client := serveOverTheWire(t, s, h)
		h.Meter = brokenTransport{Meter: client, defect: defect}
		h.Balance = client.Balance
		return h
	})
}
