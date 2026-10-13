package app_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// unbuiltProcessor is the processor as it is before it exists: Tick does nothing, so every case fails on what it
// claims — nothing was granted, nothing was processed — and not on a compile error. The one variable below that
// binds it is the only edit each green makes to this file's wiring.
type unbuiltProcessor struct{}

func (unbuiltProcessor) Tick(context.Context) error { return nil }

// processorUnderTest builds the processor over a harness's collaborators.
var processorUnderTest contracts.ProcessorFactory = func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
	return unbuiltProcessor{}
}

func TestProcessorGrants(t *testing.T) {
	t.Parallel()
	contracts.ProcessorGrants(t, processorUnderTest)
}

// The grants suite is shown to reject a processor whose collaborators are broken in the ways that lose or
// duplicate money, each by a subtest that names the invariant.
func TestProcessorGrantsRejectsEveryBrokenCollaboratorByName(t *testing.T) {
	t.Parallel()
	const (
		crash      = "a crash after the acknowledgement still grants, exactly once"
		replay     = "a grant already made before the crash is a replay, not a second grant"
		concurrent = "concurrent processors grant once"
		noDefault  = "a payment for a price the catalogue does not sell grants nothing and falls back to no default plan"
	)
	mystery := domain.Plan{Code: "free", Kind: domain.OneTime, Active: true}
	broken := []struct {
		name     string
		break_   func(h *contracts.Harness)
		caughtBy string
	}{
		{"an inbox that acknowledges an event when it leases it", func(h *contracts.Harness) { h.Inbox.AckOnLease = true }, crash},
		{"a meter with no idempotency guard", func(h *contracts.Harness) { h.Meter.NoIdempotency = true }, replay},
		{"a lease that excludes nobody, over a meter with no guard", func(h *contracts.Harness) {
			h.Inbox.NoExclusion, h.Meter.NoIdempotency = true, true
		}, concurrent},
		{"a catalogue that answers an unknown price with a default plan", func(h *contracts.Harness) { h.Catalog.FallbackPlan = &mystery }, noDefault},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.GrantCases() {
				mk := func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
					b.break_(h)
					return processorUnderTest(t, h)
				}
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, mk) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			if !slices.Contains(rejected, b.caughtBy) {
				t.Fatalf("the suite PASSED %s on %q (rejected only by %q)", b.name, b.caughtBy, rejected)
			}
		})
	}
}

func TestProcessorGrantsPassEveryCaseOnTheProcessorUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.GrantCases() {
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, processorUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}
