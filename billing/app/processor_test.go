package app_test

import (
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// processorUnderTest builds the processor over a harness's collaborators.
var processorUnderTest contracts.ProcessorFactory = func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
	t.Helper()
	p, err := app.NewProcessor(app.ProcessorDeps{
		Inbox: h.Inbox, Accounts: h.Accounts, Providers: h.Providers(), Customers: h.Customers, Catalog: h.Catalog,
		Payments: h.Payments, Subs: h.Subs, Meter: h.Meter, Lots: h.Lots, Clock: h.Clock, Metrics: h.Metrics, Bus: h.Bus, Recharges: h.Recharges,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Grace: contracts.GraceWindow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
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
		breakIt  func(h *contracts.Harness)
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
				var broken sync.Map // a case that builds several processors breaks its world once, before any of them runs
				mk := func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
					if _, already := broken.LoadOrStore(h, true); !already {
						b.breakIt(h)
					}
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

func TestProcessorRefundsAndDisputes(t *testing.T) {
	t.Parallel()
	contracts.ProcessorRefunds(t, processorUnderTest)
}

// The refunds suite is shown to reject collaborators broken in the ways that take credits twice or not at all.
func TestProcessorRefundsRejectsEveryBrokenCollaboratorByName(t *testing.T) {
	t.Parallel()
	const (
		expiry  = "a refund never claws back what expiry already took (20)"
		partial = "a refund after partial expiry revokes only the rest, across reports"
		commit  = "a clawback whose commit failed is not taken twice on the retry"
		chargeb = "a chargeback takes what a refund and expiry left"
	)
	broken := []struct {
		name     string
		breakIt  func(h *contracts.Harness)
		caughtBy []string
	}{
		{"a lot reader that cannot see what expired", func(h *contracts.Harness) { h.Lots.Blind = true }, []string{expiry, partial, chargeb}},
		{"a meter with no idempotency guard", func(h *contracts.Harness) { h.Meter.NoIdempotency = true }, []string{commit}},
	}
	cases := append(contracts.RefundCases(), contracts.DisputeCases()...)
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range cases {
				var broke sync.Map
				mk := func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
					if _, already := broke.LoadOrStore(h, true); !already {
						b.breakIt(h)
					}
					return processorUnderTest(t, h)
				}
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, mk) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the suite PASSED %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestProcessorRefundsPassEveryCaseOnTheProcessorUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range append(contracts.RefundCases(), contracts.DisputeCases()...) {
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, processorUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

func TestProcessorSubscriptions(t *testing.T) {
	t.Parallel()
	contracts.ProcessorSubscriptions(t, processorUnderTest)
}

// The subscription suite is shown to reject a store that lets delivery order decide state and one that forgets
// it announced the end of a grace.
func TestProcessorSubscriptionsRejectsEveryBrokenCollaboratorByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		breakIt  func(h *contracts.Harness)
		caughtBy string
	}{
		{"a store that applies whatever arrives last", func(h *contracts.Harness) { h.Subs.NoOrder = true },
			"a stale read of the provider never overwrites a newer state"},
		{"a store that forgets it announced the end of a grace", func(h *contracts.Harness) { h.Subs.NoLapseMark = true },
			"a failed payment holds entitlements until GraceUntil and then suspends (13)"},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.SubscriptionCases() {
				var broke sync.Map
				mk := func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
					if _, already := broke.LoadOrStore(h, true); !already {
						b.breakIt(h)
					}
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

func TestProcessorSubscriptionsPassEveryCaseOnTheProcessorUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.SubscriptionCases() {
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, processorUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

// Invariant 12 as a property of the code: subscription state has exactly one writer. Anything that calls
// Subs.ApplyIfNewer outside the processor is a second writer, and the one thing it would write is the status the
// provider has not yet confirmed.
func TestOnlyTheProcessorWritesSubscriptionStatus(t *testing.T) {
	t.Parallel()
	call := regexp.MustCompile(`\.ApplyIfNewer\(`)
	var writers []string
	for _, root := range []string{"../../billing", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/contracts/") {
				return err
			}
			src, err := os.ReadFile(path)
			if err == nil && call.Match(src) {
				writers = append(writers, filepath.ToSlash(path))
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"../../billing/app/processor.go"}; !slices.Equal(writers, want) {
		t.Fatalf("Subs.ApplyIfNewer is called from %v, want only %v (payments invariant 12)", writers, want)
	}
}

func TestProcessorAutoRecharge(t *testing.T) {
	t.Parallel()
	contracts.ProcessorAutoRecharge(t, processorUnderTest)
}

// The settlement of an auto top-up is shown to reject a store that settles carelessly: one that releases whichever slot a
// report names is caught by the late replay, and one that never switches an instruction off by the third failure.
func TestProcessorAutoRechargeRejectsEveryBrokenStoreByName(t *testing.T) {
	t.Parallel()
	const (
		lateReplay = "a success that arrives after its slot went stale does not release the slot that replaced it, and replaying it grants nothing more"
		thirdFail  = "the third failure switches the instruction off and tells the customer why"
		noPrice    = "an auto top-up's success needs no price: the provider's event about a charge made off-session names none, and its plan was fixed when it was charged"
	)
	broken := []struct {
		name     string
		breakIt  func(w *contracts.RechargeWorld)
		caughtBy string
	}{
		{"a store that releases whichever slot a report names", func(w *contracts.RechargeWorld) { w.Recharges.ReleasesAnySlot = true }, lateReplay},
		{"a store that never switches an instruction off", func(w *contracts.RechargeWorld) { w.Recharges.NeverDisables = true }, thirdFail},
		// The event names no price, so what the customer bought can only come from the payment the handler recorded when it charged.
		{"a payments store that does not remember which plan a payment was for", func(w *contracts.RechargeWorld) { w.Payments.HidesPlan = true }, noPrice},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.AutoRechargeSettlementCases() {
				mk := func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
					return processorUnderTest(t, h)
				}
				// The case builds its own world; the defect is applied to the store the processor is given.
				broke := func(t testing.TB, h *contracts.Harness) ports.WebhookProcessor {
					b.breakIt(&contracts.RechargeWorld{Harness: h})
					return mk(t, h)
				}
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, broke) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			if !slices.Contains(rejected, b.caughtBy) {
				t.Errorf("the suite PASSED a processor over %s on %q (rejected only by %q)", b.name, b.caughtBy, rejected)
			}
		})
	}
}
