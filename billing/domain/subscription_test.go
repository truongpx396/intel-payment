package domain_test

import (
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	domain "github.com/truongpx396/intel-payment/billing/domain"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func sub(status domain.SubStatus, plan string) domain.Sub {
	return domain.Sub{
		Scope:                  domain.Scope{Realm: "default", Kind: "workspace", ID: "w1"},
		PlanCode:               plan,
		Account:                "stripe-live",
		ProviderSubscriptionID: "sub-1",
		Status:                 status,
		CurrentPeriodStart:     t0,
		CurrentPeriodEnd:       t0.AddDate(0, 1, 0),
	}
}

type stateEvent struct {
	sub domain.Sub
	at  time.Time
}

// fold applies the events in the given order to an empty store.
func fold(events []stateEvent) domain.Sub {
	var cur domain.Sub
	for _, e := range events {
		cur, _ = cur.Apply(e.sub, e.at)
	}
	return cur
}

// permutations of 0..n-1 (Heap's algorithm).
func permutations(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	var out [][]int
	var gen func(k int)
	gen = func(k int) {
		if k == 1 {
			out = append(out, append([]int(nil), idx...))
			return
		}
		gen(k - 1)
		for i := 0; i < k-1; i++ {
			if k%2 == 0 {
				idx[i], idx[k-1] = idx[k-1], idx[i]
			} else {
				idx[0], idx[k-1] = idx[k-1], idx[0]
			}
			gen(k - 1)
		}
	}
	gen(n)
	return out
}

func TestAnyOrderOfSubscriptionEventsConvergesToTheNewestState(t *testing.T) {
	t.Parallel()
	events := []stateEvent{
		{sub(domain.Incomplete, "pro"), t0},
		{sub(domain.Active, "pro"), t0.Add(1 * time.Minute)},
		{sub(domain.PastDue, "pro"), t0.Add(2 * time.Minute)},
		{sub(domain.Active, "team"), t0.Add(3 * time.Minute)},
		{sub(domain.Canceled, "team"), t0.Add(4 * time.Minute)},
	}
	perms := permutations(len(events))
	if len(perms) != 120 {
		t.Fatalf("generated %d permutations of 5, want 120", len(perms))
	}
	for _, p := range perms {
		var ordered []stateEvent
		for _, i := range p {
			ordered = append(ordered, events[i])
		}
		got := fold(ordered)
		if got.Status != domain.Canceled || got.PlanCode != "team" || !got.LastEventAt.Equal(t0.Add(4*time.Minute)) {
			t.Fatalf("order %v ended in %s/%s at %s — delivery order decided state (invariant 15)", p, got.Status, got.PlanCode, got.LastEventAt)
		}
	}
}

func TestAnOlderEventNeverOverwritesANewerState(t *testing.T) {
	t.Parallel()
	canceled, applied := domain.Sub{}.Apply(sub(domain.Canceled, "pro"), t0.Add(time.Hour))
	if !applied {
		t.Fatal("the first event must apply to an empty store")
	}
	got, applied := canceled.Apply(sub(domain.Active, "pro"), t0)
	if applied || got.Status != domain.Canceled || !got.LastEventAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("a late, older 'active' overwrote the newer cancel: applied=%v %+v", applied, got)
	}
}

func TestApplyReportsWhetherTheStateChanged(t *testing.T) {
	t.Parallel()
	first, applied := domain.Sub{}.Apply(sub(domain.Active, "pro"), t0)
	if !applied || !first.LastEventAt.Equal(t0) {
		t.Fatalf("first apply: %v %+v", applied, first)
	}
	// The same event again is a redelivery: nothing is applied, so nothing is announced twice.
	again, applied := first.Apply(sub(domain.Active, "pro"), t0)
	if applied || again != first {
		t.Fatalf("a redelivery was applied: %v", applied)
	}
	// A newer event with the same state still advances the clock.
	newer, applied := first.Apply(sub(domain.Active, "pro"), t0.Add(time.Second))
	if !applied || !newer.LastEventAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("a newer event must apply: %v %+v", applied, newer)
	}
}

func TestEventsWithTheSameSecondConvergeTooAndTheTerminalStateWins(t *testing.T) {
	t.Parallel()
	// Stripe stamps events to the second: 'updated' and 'deleted' can share one.
	events := []stateEvent{
		{sub(domain.Active, "pro"), t0},
		{sub(domain.PastDue, "pro"), t0},
		{sub(domain.Canceled, "pro"), t0},
	}
	for _, p := range permutations(len(events)) {
		var ordered []stateEvent
		for _, i := range p {
			ordered = append(ordered, events[i])
		}
		if got := fold(ordered); got.Status != domain.Canceled {
			t.Fatalf("order %v ended %s: with equal timestamps the more final state must win, or order decides", p, got.Status)
		}
	}
}

func TestADifferentSubscriptionIsNotThisOnesToOverwrite(t *testing.T) {
	t.Parallel()
	cur, _ := domain.Sub{}.Apply(sub(domain.Active, "pro"), t0)
	other := sub(domain.Canceled, "pro")
	other.ProviderSubscriptionID = "sub-2"
	if got, applied := cur.Apply(other, t0.Add(time.Hour)); applied || got != cur {
		t.Fatalf("state of sub-2 was written over sub-1: %v %+v", applied, got)
	}
}

func FuzzSubscriptionStateIsIndependentOfDeliveryOrder(f *testing.F) {
	f.Add(uint64(1), uint8(5))
	f.Add(uint64(99), uint8(8))
	statuses := []domain.SubStatus{domain.Trialing, domain.Active, domain.PastDue, domain.Paused, domain.Canceled, domain.Incomplete}
	plans := []string{"free", "pro", "team"}
	f.Fuzz(func(t *testing.T, seed uint64, n uint8) {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		var events []stateEvent
		for i := 0; i < int(n%12)+1; i++ {
			// A few distinct instants so ties are common.
			at := t0.Add(time.Duration(rng.IntN(4)) * time.Second)
			events = append(events, stateEvent{sub(statuses[rng.IntN(len(statuses))], plans[rng.IntN(len(plans))]), at})
		}
		want := fold(events)
		for k := 0; k < 6; k++ {
			shuffled := append([]stateEvent(nil), events...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if got := fold(shuffled); got != want {
				t.Fatalf("the same %d events in another order ended differently:\n got  %+v\n want %+v", len(events), got, want)
			}
		}
		// Independent reference: the newest instant wins, and at that instant the highest status rank,
		// then the plan name — computed here by sorting, not by folding.
		rank := map[domain.SubStatus]int{domain.Incomplete: 0, domain.Trialing: 1, domain.Active: 2, domain.PastDue: 3, domain.Paused: 4, domain.Canceled: 5}
		sorted := append([]stateEvent(nil), events...)
		sort.SliceStable(sorted, func(i, j int) bool {
			a, b := sorted[i], sorted[j]
			if !a.at.Equal(b.at) {
				return a.at.Before(b.at)
			}
			if rank[a.sub.Status] != rank[b.sub.Status] {
				return rank[a.sub.Status] < rank[b.sub.Status]
			}
			return a.sub.PlanCode < b.sub.PlanCode
		})
		win := sorted[len(sorted)-1]
		if want.Status != win.sub.Status || want.PlanCode != win.sub.PlanCode || !want.LastEventAt.Equal(win.at) {
			t.Fatalf("converged to %s/%s@%s, the newest event is %s/%s@%s", want.Status, want.PlanCode, want.LastEventAt, win.sub.Status, win.sub.PlanCode, win.at)
		}
	})
}

// ---- grace (invariant 13) ----

func pastDue(graceUntil *time.Time) domain.Sub {
	s := sub(domain.PastDue, "pro")
	s.GraceUntil = graceUntil
	return s
}

func TestGraceHoldsUntilTheInstantItIsReached(t *testing.T) {
	t.Parallel()
	end := t0.Add(7 * 24 * time.Hour)
	s := pastDue(&end)
	for _, c := range []struct {
		name string
		now  time.Time
		held bool
		due  bool
	}{
		{"a long way inside", t0, true, false},
		{"one nanosecond before", end.Add(-time.Nanosecond), true, false},
		{"the instant it is reached", end, false, true},
		{"one nanosecond after", end.Add(time.Nanosecond), false, true},
		{"long after", end.Add(30 * 24 * time.Hour), false, true},
	} {
		if got := s.HoldsEntitlements(c.now); got != c.held {
			t.Errorf("%s: HoldsEntitlements = %v, want %v", c.name, got, c.held)
		}
		if got := s.GraceDue(c.now); got != c.due {
			t.Errorf("%s: GraceDue = %v, want %v", c.name, got, c.due)
		}
	}
}

func TestOnlyAPastDueSubscriptionHasAGraceWindow(t *testing.T) {
	t.Parallel()
	end := t0.Add(24 * time.Hour)
	for _, c := range []struct {
		status  domain.SubStatus
		grace   *time.Time
		held    bool // inside the window
		dueLate bool // an hour after the window closed
	}{
		{domain.Active, nil, true, false},
		{domain.Trialing, nil, true, false},
		{domain.Active, &end, true, false},  // a stale grace on a recovered subscription changes nothing
		{domain.PastDue, nil, false, false}, // no window was set: nothing to hold on to, and nothing to expire
		{domain.PastDue, &end, true, true},
		{domain.Canceled, &end, false, false},
		{domain.Paused, &end, false, false},
		{domain.Incomplete, nil, false, false},
	} {
		s := sub(c.status, "pro")
		s.GraceUntil = c.grace
		if got := s.HoldsEntitlements(t0); got != c.held {
			t.Errorf("%s grace=%v: HoldsEntitlements = %v, want %v", c.status, c.grace != nil, got, c.held)
		}
		if got := s.GraceDue(end.Add(time.Hour)); got != c.dueLate {
			t.Errorf("%s grace=%v: GraceDue = %v, want %v", c.status, c.grace != nil, got, c.dueLate)
		}
	}
}

func TestAGraceThatWasClosedIsNotDueAgain(t *testing.T) {
	t.Parallel()
	end := t0.Add(time.Hour)
	lapsed := end.Add(time.Minute)
	s := pastDue(&end)
	s.GraceLapsedAt = &lapsed
	if s.GraceDue(end.Add(time.Hour)) {
		t.Fatal("a grace expiry already announced would be announced every sweep")
	}
	if s.HoldsEntitlements(end.Add(time.Hour)) {
		t.Fatal("closing the grace must not restore entitlements")
	}
}

func TestOnlyLiveStatusesCountAsLive(t *testing.T) {
	t.Parallel()
	for status, live := range map[domain.SubStatus]bool{
		domain.Trialing: true, domain.Active: true, domain.PastDue: true, domain.Paused: true,
		domain.Canceled: false, domain.Incomplete: false,
	} {
		if got := sub(status, "pro").Live(); got != live {
			t.Errorf("%s.Live() = %v, want %v (the schema's one-live-subscription-per-scope index)", status, got, live)
		}
	}
}
