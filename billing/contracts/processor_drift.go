package contracts

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// metricSubdrift is the counter the sweep keeps, as a literal: billing/ports holds the catalogue equal to the contract.
const metricSubdrift = "billing_subdrift_total"

// swept is how many subscriptions the sweep counted with an outcome: repaired, unchanged, skipped or failed.
func swept(h *Harness, outcome string) int64 {
	return h.Metrics.Counter(metricSubdrift, mports.Labels{"outcome": outcome})
}

// DriftFactory builds the subscription drift sweep over a harness's collaborators. page is how many subscriptions it may read from the
// books at a time: a suite that needs to see the sweep read past one page says so here, and the sweep under test must honour it.
type DriftFactory func(t testing.TB, h *Harness, page int) ports.SubscriptionDrift

// DriftCase is one subtest of the drift suite. Exported so the suite can be shown to have teeth: run a case against a deliberately broken
// store with Probe and require that it is rejected.
type DriftCase struct {
	Name string
	Run  func(t testing.TB, mk DriftFactory)
}

// held puts a subscription in the books as it stood at `at`, and says what the provider holds now. It returns the provider's object.
func held(h *Harness, scope md.Scope, account, id string, local domain.SubStatus, localAt time.Time, remote domain.SubStatus, remoteAt time.Time) domain.Sub {
	mk := func(status domain.SubStatus, at time.Time) domain.Sub {
		return domain.Sub{Scope: scope, PlanCode: ProCode, Account: account, ProviderSubscriptionID: id, Status: status,
			CurrentPeriodStart: at.Add(-24 * time.Hour), CurrentPeriodEnd: at.Add(29 * 24 * time.Hour)}
	}
	l := mk(local, localAt)
	l.LastEventAt = localAt
	h.Subs.Put(l)
	r := mk(remote, remoteAt)
	h.Provider.SetSubscription(id, r, remoteAt)
	return r
}

// fetches is how many times the sweep asked the provider about a subscription.
func fetches(h *Harness) int { return count(h.Log.Calls(), "provider.FetchSubscription") }

// announcements is every change billing told the entitlement layer and the customer about, for a scope.
func announcements(h *Harness, scope md.Scope) int {
	return len(entitlementReasons(h, scope)) + len(paymentEvents(h, scope))
}

// DriftCases are the subtests of ProcessorDrift (R110a): the sweep is the backstop for a webhook that never arrived.
func DriftCases() []DriftCase {
	t1 := Start.Add(time.Hour)
	return []DriftCase{
		{"a webhook that never arrived is repaired: a subscription the provider cancelled is cancelled here, announced once", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope, AccountID, "sub-1", domain.Active, Start, domain.Canceled, t1)
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			got := h.Subs.Row("sub-1")
			if got.Status != domain.Canceled || !got.LastEventAt.Equal(t1) {
				t.Fatalf("the books say %+v after the sweep; the provider cancelled it at %v", got, t1)
			}
			if rep.Checked != 1 || rep.Repaired != 1 || rep.Unchanged != 0 || rep.Failed != 0 {
				t.Fatalf("report = %+v, want one checked and one repaired", rep)
			}
			if n := count(entitlementReasons(h, h.Scope), "subscription"); n != 1 {
				t.Fatalf("the entitlement layer was told %d times, want once", n)
			}
			if n := count(paymentEvents(h, h.Scope), "canceled"); n != 1 {
				t.Fatalf("the customer was told of the cancellation %d times, want once", n)
			}
			if n := swept(h, "repaired"); n != 1 {
				t.Fatalf("billing_subdrift_total{outcome=repaired} = %d, want 1: a repair is a webhook that was missed, and the number says how often", n)
			}
			rep, err = d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Repaired != 0 || announcements(h, h.Scope) != 2 {
				t.Fatalf("a second sweep repaired %d and made %d announcements in all: what was repaired is not repaired again", rep.Repaired, announcements(h, h.Scope))
			}
		}},
		{"a subscription the provider and the books agree on is not written, announced or called repaired", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			want := held(h, h.Scope, AccountID, "sub-1", domain.Active, Start, domain.Active, Start)
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Checked != 1 || rep.Unchanged != 1 || rep.Repaired != 0 {
				t.Fatalf("report = %+v, want one checked and left alone", rep)
			}
			if got := h.Subs.Row("sub-1"); got.Status != want.Status || !got.LastEventAt.Equal(Start) {
				t.Fatalf("the books changed: %+v", got)
			}
			if n := announcements(h, h.Scope); n != 0 {
				t.Fatalf("%d announcements for a state nothing changed", n)
			}
			if swept(h, "unchanged") != 1 || swept(h, "repaired") != 0 {
				t.Fatalf("unchanged = %d, repaired = %d, want 1 and 0", swept(h, "unchanged"), swept(h, "repaired"))
			}
		}},
		{"a lagging replica's older answer never overwrites a newer state", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope, AccountID, "sub-1", domain.Active, t1, domain.Paused, Start)
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if got := h.Subs.Row("sub-1"); got.Status != domain.Active || !got.LastEventAt.Equal(t1) {
				t.Fatalf("an older answer replaced a newer state: %+v", got)
			}
			if rep.Repaired != 0 || rep.Unchanged != 1 || announcements(h, h.Scope) != 0 {
				t.Fatalf("report = %+v with %d announcements, want it left alone and silent", rep, announcements(h, h.Scope))
			}
		}},
		{"only live subscriptions are swept: one that has ended is never asked about", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope, AccountID, "sub-1", domain.Canceled, t1, domain.Active, Start)
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Checked != 0 || fetches(h) != 0 {
				t.Fatalf("report = %+v after %d provider calls: a subscription that ended was fetched", rep, fetches(h))
			}
			if got := h.Subs.Row("sub-1"); got.Status != domain.Canceled {
				t.Fatalf("an ended subscription was brought back: %+v", got)
			}
		}},
		{"another account's subscriptions are not this account's to sweep", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope, OtherAccountID, "sub-1", domain.Active, Start, domain.Canceled, t1)
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Checked != 0 || fetches(h) != 0 {
				t.Fatalf("report = %+v after %d provider calls: another account's subscription was fetched", rep, fetches(h))
			}
			if got := h.Subs.Row("sub-1"); got.Status != domain.Active {
				t.Fatalf("another account's subscription was changed: %+v", got)
			}
		}},
		{"a subscription on a plan the realm does not sell is skipped and written nowhere", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			r := held(h, h.Scope, AccountID, "sub-1", domain.Active, Start, domain.Canceled, t1)
			r.PlanCode = ""
			h.Provider.SetSubscription("sub-1", r, t1)
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Skipped != 1 || rep.Repaired != 0 {
				t.Fatalf("report = %+v, want it skipped", rep)
			}
			if swept(h, "skipped") != 1 {
				t.Fatalf("skipped = %d, want 1", swept(h, "skipped"))
			}
			if got := h.Subs.Row("sub-1"); got.Status != domain.Active || announcements(h, h.Scope) != 0 {
				t.Fatalf("a subscription the catalogue does not sell was written or announced: %+v", got)
			}
		}},
		{"a provider that cannot answer for one subscription does not stop the others, and the failure is reported", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope2, AccountID, "sub-2", domain.Active, Start, domain.Canceled, t1)
			h.Subs.Put(domain.Sub{Scope: h.Scope, PlanCode: ProCode, Account: AccountID, ProviderSubscriptionID: "sub-1", Status: domain.Active, LastEventAt: Start})
			rep, err := d.SweepSubscriptions(Ctx(), AccountID) // the provider has no object for sub-1
			if err == nil || !strings.Contains(err.Error(), "sub-1") {
				t.Fatalf("err = %v, want the subscription the provider could not answer for to be named", err)
			}
			if rep.Checked != 2 || rep.Failed != 1 || rep.Repaired != 1 {
				t.Fatalf("report = %+v, want two checked, one failed and one repaired", rep)
			}
			if got := h.Subs.Row("sub-2"); got.Status != domain.Canceled {
				t.Fatalf("one failure stopped the sweep: %+v", got)
			}
			if swept(h, "failed") != 1 || swept(h, "repaired") != 1 {
				t.Fatalf("failed = %d, repaired = %d, want 1 and 1", swept(h, "failed"), swept(h, "repaired"))
			}
		}},
		{"more live subscriptions than a page are all swept", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 2)
			for i := range 5 {
				sc := h.Scope
				sc.ID = fmt.Sprintf("p%d", i)
				held(h, sc, AccountID, fmt.Sprintf("sub-p%d", i), domain.Active, Start, domain.Canceled, t1)
			}
			rep, err := d.SweepSubscriptions(Ctx(), AccountID)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Checked != 5 || rep.Repaired != 5 {
				t.Fatalf("report = %+v, want all five of them, two to a page", rep)
			}
			for i := range 5 {
				if got := h.Subs.Row(fmt.Sprintf("sub-p%d", i)); got.Status != domain.Canceled {
					t.Fatalf("sub-p%d was left behind: %+v", i, got)
				}
			}
		}},
		{"two sweeps at once repair once", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope, AccountID, "sub-1", domain.Active, Start, domain.Canceled, t1)
			var wg sync.WaitGroup
			var mu sync.Mutex
			var repaired int
			var errs []error
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rep, err := d.SweepSubscriptions(Ctx(), AccountID)
					mu.Lock()
					defer mu.Unlock()
					repaired += rep.Repaired
					if err != nil {
						errs = append(errs, err)
					}
				}()
			}
			wg.Wait()
			if err := errors.Join(errs...); err != nil {
				t.Fatal(err)
			}
			if repaired != 1 || announcements(h, h.Scope) != 2 {
				t.Fatalf("eight sweeps repaired it %d times and made %d announcements, want once and two (one for the entitlement layer, one for the customer)", repaired, announcements(h, h.Scope))
			}
		}},
		{"an account that does not exist, or is switched off, is refused before the provider is asked anything", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			held(h, h.Scope, InactiveID, "sub-1", domain.Active, Start, domain.Canceled, t1)
			for _, id := range []string{"no-such-account", InactiveID} {
				if _, err := d.SweepSubscriptions(Ctx(), id); !errors.Is(err, domain.ErrUnknownAccount) {
					t.Fatalf("a sweep of %q: err = %v, want %v", id, err, domain.ErrUnknownAccount)
				}
			}
			if fetches(h) != 0 || h.Subs.Row("sub-1").Status != domain.Active {
				t.Fatalf("a refused sweep still fetched (%d) or wrote: %+v", fetches(h), h.Subs.Row("sub-1"))
			}
		}},
		{"a sweep of everything visits each account with a live subscription and no switched-off one", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			sc3 := h.Scope
			sc3.ID = "w3"
			held(h, h.Scope, AccountID, "sub-a", domain.Active, Start, domain.Canceled, t1)
			held(h, h.Scope2, OtherAccountID, "sub-b", domain.Active, Start, domain.Canceled, t1)
			held(h, sc3, InactiveID, "sub-c", domain.Active, Start, domain.Canceled, t1)
			rep, err := d.SweepAllSubscriptions(Ctx())
			if err != nil {
				t.Fatal(err)
			}
			if rep.Accounts != 2 || rep.Checked != 2 || rep.Repaired != 2 {
				t.Fatalf("report = %+v, want two accounts, two subscriptions, two repaired", rep)
			}
			if h.Subs.Row("sub-a").Status != domain.Canceled || h.Subs.Row("sub-b").Status != domain.Canceled {
				t.Fatal("a live account was left unswept")
			}
			if got := h.Subs.Row("sub-c"); got.Status != domain.Active || fetches(h) != 2 {
				t.Fatalf("a switched-off account was swept (%d provider calls): %+v", fetches(h), got)
			}
		}},
		{"a sweep with nothing live is idle: no provider call, no write", func(t testing.TB, mk DriftFactory) {
			h := NewHarness()
			d := mk(t, h, 100)
			rep, err := d.SweepAllSubscriptions(Ctx())
			if err != nil {
				t.Fatal(err)
			}
			if rep != (ports.DriftReport{}) || fetches(h) != 0 {
				t.Fatalf("report = %+v after %d provider calls, want nothing at all", rep, fetches(h))
			}
		}},
	}
}

// ProcessorDrift is the specification of the subscription drift sweep (R110a, FR-025): the backstop for a webhook that never arrived.
// It asks the provider for every live subscription's current object and applies it through the one door that writes status, so a state that
// has not changed is not written, announced or counted, an older answer never wins, one failure does not stop the rest, and two sweeps at
// once repair once.
func ProcessorDrift(t *testing.T, mk DriftFactory) {
	t.Helper()
	for _, c := range DriftCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}
