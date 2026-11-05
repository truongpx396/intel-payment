package app_test

import (
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// driftDeps are the processor's collaborators over a harness, reading the books `page` subscriptions at a time.
func driftDeps(h *contracts.Harness, page int) app.ProcessorDeps {
	return app.ProcessorDeps{
		Inbox: h.Inbox, Accounts: h.Accounts, Providers: h.Providers(), Customers: h.Customers, Catalog: h.Catalog,
		Payments: h.Payments, Subs: h.Subs, Meter: h.Meter, Lots: h.Lots, Clock: h.Clock, Metrics: h.Metrics, Bus: h.Bus, Recharges: h.Recharges,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Grace: contracts.GraceWindow, DriftPage: page,
	}
}

var driftUnderTest contracts.DriftFactory = func(t testing.TB, h *contracts.Harness, page int) ports.SubscriptionDrift {
	t.Helper()
	p, err := app.NewProcessor(driftDeps(h, page))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProcessorDrift(t *testing.T) {
	t.Parallel()
	contracts.ProcessorDrift(t, driftUnderTest)
}

// The drift suite is shown to reject a store whose listing reaches another account, one that lists what has ended, one that stops after a
// page, and one that lets arrival order decide state.
func TestProcessorDriftRejectsEveryBrokenCollaboratorByName(t *testing.T) {
	t.Parallel()
	broken := []struct {
		name     string
		breakIt  func(h *contracts.Harness)
		caughtBy string
	}{
		{"a store whose listing reaches another account", func(h *contracts.Harness) { h.Subs.ListsEveryAccount = true },
			"another account's subscriptions are not this account's to sweep"},
		{"a store whose listing includes what has ended", func(h *contracts.Harness) { h.Subs.ListsEnded = true },
			"only live subscriptions are swept: one that has ended is never asked about"},
		{"a store that answers only the first page", func(h *contracts.Harness) { h.Subs.ListsOnePage = true },
			"more live subscriptions than a page are all swept"},
		{"a store that applies whatever arrives last", func(h *contracts.Harness) { h.Subs.NoOrder = true },
			"a lagging replica's older answer never overwrites a newer state"},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.DriftCases() {
				var broke sync.Map
				mk := func(t testing.TB, h *contracts.Harness, page int) ports.SubscriptionDrift {
					if _, already := broke.LoadOrStore(h, true); !already {
						b.breakIt(h)
					}
					return driftUnderTest(t, h, page)
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

func TestProcessorDriftPassesEveryCaseOnTheProcessorUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.DriftCases() {
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, driftUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

func TestEveryDriftCaseHasAUniqueName(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.DriftCases() {
		if seen[c.Name] {
			t.Errorf("two drift cases are called %q", c.Name)
		}
		seen[c.Name] = true
	}
}

// A page size that is not a number of subscriptions is a mistake to refuse, not a sweep that reads nothing.
func TestAPageOfSubscriptionsCannotBeNegative(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	if _, err := app.NewProcessor(driftDeps(h, -1)); err == nil {
		t.Fatal("a negative page was accepted")
	}
	if _, err := app.NewProcessor(driftDeps(h, 0)); err != nil {
		t.Fatalf("the default page was refused: %v", err)
	}
}
