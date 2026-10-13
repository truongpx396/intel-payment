package contracts_test

import (
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
)

// The suite is itself held to the rule it enforces: it passes an honest provider, and it rejects each deliberately
// broken one — by name. A suite that cannot fail has not been shown to test anything.

func TestProviderContractHoldsOnTheHonestFakeProvider(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	contracts.ProviderContract(t, h.Provider, h.Fixtures())
}

func TestProviderContractRejectsEveryBrokenProviderByName(t *testing.T) {
	t.Parallel()
	const (
		tampered  = "rejects a tampered body"
		missing   = "rejects a missing signature"
		stale     = "rejects a stale signature"
		otherAcct = "rejects a body signed for another account"
		normal    = "verifies and normalizes with the provider's timestamp and a body hash"
		refund    = "refund amounts are normalized as cumulative"
		ignored   = "unsubscribed types are Ignored, not errors"
	)
	broken := []struct {
		defect   contracts.FakeDefect
		caughtBy []string // every one of these subtests must reject it
	}{
		{contracts.DefectSkipVerification, []string{tampered, missing, stale, otherAcct}},
		{contracts.DefectIgnoreTimestamp, []string{stale}},
		{contracts.DefectSharedSecret, []string{otherAcct}},
		{contracts.DefectNoRawHash, []string{normal}},
		{contracts.DefectPerEventRefund, []string{refund}},
		{contracts.DefectNoIgnore, []string{ignored}},
		{contracts.DefectPartialOnError, []string{tampered, missing}},
	}
	for _, b := range broken {
		t.Run(string(b.defect), func(t *testing.T) {
			t.Parallel()
			h := contracts.NewHarness()
			p, fx := h.Provider.WithDefect(b.defect), h.Fixtures()
			var rejected []string
			for _, c := range contracts.ProviderCases() {
				if failed, _ := contracts.Probe(10*time.Second, func(t testing.TB) { c.Run(t, p, fx) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the suite PASSED a provider with defect %q on %q (rejected only by %q)", b.defect, want, rejected)
				}
			}
		})
	}
}

func TestEveryProviderCaseHasAUniqueNameAndTheControlPassesThemAll(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	seen := map[string]bool{}
	for _, c := range contracts.ProviderCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(10*time.Second, func(t testing.TB) { c.Run(t, h.Provider, h.Fixtures()) }); failed {
			t.Errorf("the honest provider fails %q: %v", c.Name, said)
		}
	}
}

func TestProbeReportsWhatAFailingCaseSaid(t *testing.T) {
	t.Parallel()
	if failed, said := contracts.Probe(time.Second, func(t testing.TB) { t.Fatalf("boom %d", 7) }); !failed || len(said) != 1 || said[0] != "boom 7" {
		t.Fatalf("a Fatalf must fail the probe and be reported: %v %v", failed, said)
	}
	if failed, _ := contracts.Probe(time.Second, func(t testing.TB) { t.Errorf("one"); t.Errorf("two") }); !failed {
		t.Fatal("an Errorf must fail the probe")
	}
	if failed, _ := contracts.Probe(time.Second, func(t testing.TB) { panic("nil map") }); !failed {
		t.Fatal("a panic is a failure, not a pass")
	}
	if failed, _ := contracts.Probe(50*time.Millisecond, func(t testing.TB) { time.Sleep(300 * time.Millisecond) }); !failed {
		t.Fatal("a case that never finishes is rejected, not waited for")
	}
	if failed, _ := contracts.Probe(time.Second, func(t testing.TB) { t.Helper(); t.Cleanup(func() {}) }); failed {
		t.Fatal("a quiet case passes")
	}
}
