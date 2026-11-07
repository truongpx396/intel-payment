package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

// cursorless is a store that answers every page from the start, whatever it was asked to read after.
type cursorless struct{ ports.Subs }

func (c cursorless) LiveOf(ctx context.Context, account, _ string, limit int) ([]domain.Sub, error) {
	return c.Subs.LiveOf(ctx, account, "", limit)
}

// A store that does not move its cursor would hold a sweep on one page for ever, and the provider with it, a call per subscription per
// lap. The sweep ends with an error that says so.
func TestASweepOverAStoreThatDoesNotMoveItsCursorEndsInsteadOfLooping(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	for i, id := range []string{"sub-1", "sub-2", "sub-3"} {
		sc := h.Scope
		sc.ID = "c" + string(rune('a'+i))
		h.Subs.Put(domain.Sub{Scope: sc, PlanCode: contracts.ProCode, Account: contracts.AccountID, ProviderSubscriptionID: id, Status: domain.Active, LastEventAt: contracts.Start})
		h.GivenProviderSub(id, domain.Active, contracts.Start)
	}
	deps := driftDeps(h, 2)
	deps.Subs = cursorless{h.Subs}
	p, err := app.NewProcessor(deps)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.SweepSubscriptions(t.Context(), contracts.AccountID)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "the cursor did not move") {
			t.Fatalf("err = %v, want it to say the cursor did not move", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep is still reading the same page")
	}
}

// unreadable is a store whose books cannot be read.
type unreadable struct {
	ports.Subs
	err error
}

func (u unreadable) LiveOf(context.Context, string, string, int) ([]domain.Sub, error) {
	return nil, u.err
}
func (u unreadable) LiveAccounts(context.Context) ([]string, error) { return nil, u.err }

// A sweep that cannot read the books has swept nothing, and says so: an error is the difference between "nothing was drifting" and
// "nothing was looked at".
func TestASweepThatCannotReadTheBooksSaysSoAndAsksTheProviderNothing(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	h.GivenProviderSub("sub-1", domain.Canceled, contracts.Start)
	boom := errors.New("the database is down")
	deps := driftDeps(h, 2)
	deps.Subs = unreadable{h.Subs, boom}
	p, err := app.NewProcessor(deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.SweepSubscriptions(t.Context(), contracts.AccountID); !errors.Is(err, boom) || !strings.Contains(err.Error(), contracts.AccountID) {
		t.Fatalf("a sweep of an account: %v, want the failure, naming the account", err)
	}
	if _, err := p.SweepAllSubscriptions(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("a sweep of everything: %v, want the failure", err)
	}
	if n := count(h.Log.Calls(), "provider.FetchSubscription"); n != 0 {
		t.Fatalf("the provider was asked %d times about subscriptions nobody could read", n)
	}
}

func count(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}
