package contracts

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// SubsFactory builds the subscriptions store under test, on a world that has the harness's accounts and plans.
type SubsFactory func(t testing.TB) ports.Subs

func subOf(scope md.Scope, id string, status domain.SubStatus, plan string) domain.Sub {
	return domain.Sub{Scope: scope.Normalized(), PlanCode: plan, Account: AccountID, ProviderSubscriptionID: id, Status: status,
		CurrentPeriodStart: us(-24 * time.Hour), CurrentPeriodEnd: us(29 * 24 * time.Hour)}
}

func apply(t testing.TB, s ports.Subs, sub domain.Sub, at time.Time) bool {
	t.Helper()
	ok, err := s.ApplyIfNewer(Ctx(), sub, at)
	if err != nil {
		t.Fatalf("ApplyIfNewer(%s %s@%s): %v", sub.ProviderSubscriptionID, sub.Status, at, err)
	}
	return ok
}

func getSub(t testing.TB, s ports.Subs, scope md.Scope) (domain.Sub, bool) {
	t.Helper()
	got, ok, err := s.Get(Ctx(), scope)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got, ok
}

// SubsCases are the subtests of SubsContract (payments invariants 12, 13, 15).
func SubsCases() []StoreCase[SubsFactory] {
	scope := func(n int) md.Scope {
		return md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: fmt.Sprintf("s%d", n)}
	}
	return []StoreCase[SubsFactory]{
		{"the first state applies, a replay does not, a newer one does, an older one does not", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(1)
			active := subOf(sc, "sub-1", domain.Active, ProCode)
			if !apply(t, s, active, us(time.Hour)) {
				t.Fatal("the first state was not applied")
			}
			if apply(t, s, active, us(time.Hour)) {
				t.Fatal("a redelivery was applied again")
			}
			canceled := subOf(sc, "sub-1", domain.Canceled, ProCode)
			if apply(t, s, subOf(sc, "sub-1", domain.PastDue, ProCode), us(30*time.Minute)) {
				t.Fatal("an older state was applied")
			}
			if !apply(t, s, canceled, us(2*time.Hour)) {
				t.Fatal("a newer state was not applied")
			}
			got, ok := getSub(t, s, sc)
			if !ok || got.Status != domain.Canceled || !got.LastEventAt.Equal(us(2*time.Hour)) {
				t.Fatalf("subscription = %+v", got)
			}
		}},
		{"a subscription round-trips every field", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(2)
			grace, lapsed := us(7*24*time.Hour), us(7*24*time.Hour+time.Minute)
			want := subOf(sc, "sub-2", domain.PastDue, TeamCode)
			want.CancelAtPeriodEnd, want.GraceUntil, want.GraceLapsedAt = true, &grace, &lapsed
			want.LastEventAt = us(time.Hour)
			apply(t, s, want, want.LastEventAt)
			got, ok := getSub(t, s, sc)
			if !ok || jsonOf(t, got) != jsonOf(t, want) {
				t.Fatalf("got  %s\nwant %s", jsonOf(t, got), jsonOf(t, want))
			}
			if _, ok := getSub(t, s, scope(99)); ok {
				t.Fatal("a scope with no subscription has one")
			}
		}},
		{"any order of events converges to the newest state (15)", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			type ev struct {
				sub domain.Sub
				at  time.Time
			}
			build := func(sc md.Scope) []ev {
				return []ev{
					{subOf(sc, "sub-p", domain.Incomplete, ProCode), us(0)},
					{subOf(sc, "sub-p", domain.Active, ProCode), us(time.Minute)},
					{subOf(sc, "sub-p", domain.Active, TeamCode), us(2 * time.Minute)},
					{subOf(sc, "sub-p", domain.Canceled, TeamCode), us(2 * time.Minute)}, // the same instant: the more final state wins
				}
			}
			rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a seeded shuffle of test events, not a secret
			for n := range 12 {
				sc := scope(100 + n)
				evs := build(sc)
				for i := range evs {
					evs[i].sub.ProviderSubscriptionID = fmt.Sprintf("sub-p%d", n)
				}
				rng.Shuffle(len(evs), func(i, j int) { evs[i], evs[j] = evs[j], evs[i] })
				for _, e := range evs {
					apply(t, s, e.sub, e.at)
				}
				got, _ := getSub(t, s, sc)
				if got.Status != domain.Canceled || got.PlanCode != TeamCode || !got.LastEventAt.Equal(us(2*time.Minute)) {
					t.Fatalf("order %d ended in %s/%s@%s — delivery order decided state", n, got.Status, got.PlanCode, got.LastEventAt)
				}
			}
		}},
		{"concurrent appliers converge on the newest state", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(3)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					st := domain.Active
					if i == 7 {
						st = domain.Canceled
					}
					if _, err := s.ApplyIfNewer(Ctx(), subOf(sc, "sub-3", st, ProCode), us(time.Duration(i)*time.Minute)); err != nil {
						t.Errorf("ApplyIfNewer: %v", err)
					}
				}()
			}
			wg.Wait()
			if got, _ := getSub(t, s, sc); got.Status != domain.Canceled || !got.LastEventAt.Equal(us(7*time.Minute)) {
				t.Fatalf("subscription = %+v: the newest state must win however the appliers interleave", got)
			}
		}},
		{"Get is the live subscription, else the newest the scope ever had", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(4)
			apply(t, s, subOf(sc, "sub-old", domain.Canceled, ProCode), us(time.Hour))
			if got, ok := getSub(t, s, sc); !ok || got.ProviderSubscriptionID != "sub-old" {
				t.Fatalf("only a canceled one exists: %+v %v", got, ok)
			}
			apply(t, s, subOf(sc, "sub-new", domain.Active, TeamCode), us(2*time.Hour))
			if got, _ := getSub(t, s, sc); got.ProviderSubscriptionID != "sub-new" {
				t.Fatalf("Get = %+v: the live subscription is the scope's", got)
			}
		}},
		{"a scope has one live subscription at a time", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(5)
			apply(t, s, subOf(sc, "sub-a", domain.Active, ProCode), us(time.Hour))
			if _, err := s.ApplyIfNewer(Ctx(), subOf(sc, "sub-b", domain.Active, TeamCode), us(2*time.Hour)); !errors.Is(err, md.ErrAlreadyExists) {
				t.Fatalf("a second live subscription: %v, want ErrAlreadyExists (the event is retried until the first one ends)", err)
			}
			apply(t, s, subOf(sc, "sub-a", domain.Canceled, ProCode), us(3*time.Hour))
			if !apply(t, s, subOf(sc, "sub-b", domain.Active, TeamCode), us(4*time.Hour)) {
				t.Fatal("the second subscription could not be applied once the first had ended")
			}
		}},
		{"a grace window is due once it has ended, only for past_due, and is announced once", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(6)
			grace := us(7 * 24 * time.Hour)
			pd := subOf(sc, "sub-6", domain.PastDue, ProCode)
			pd.GraceUntil = &grace
			apply(t, s, pd, us(time.Hour))
			active := subOf(scope(7), "sub-7", domain.Active, ProCode)
			stale := grace
			active.GraceUntil = &stale // a stale window on a recovered subscription is not a grace
			apply(t, s, active, us(time.Hour))

			if due, err := s.DueForGraceExpiry(Ctx(), grace.Add(-time.Nanosecond)); err != nil || len(due) != 0 {
				t.Fatalf("due a nanosecond early: %v, %v", due, err)
			}
			due, err := s.DueForGraceExpiry(Ctx(), grace)
			if err != nil || len(due) != 1 || due[0].ProviderSubscriptionID != "sub-6" {
				t.Fatalf("due at the instant: %+v, %v; want only the past_due one", due, err)
			}
			marked, err := s.MarkGraceLapsed(Ctx(), due[0], grace.Add(time.Second))
			if err != nil || !marked {
				t.Fatalf("MarkGraceLapsed = %v, %v", marked, err)
			}
			if again, _ := s.MarkGraceLapsed(Ctx(), due[0], grace.Add(time.Minute)); again {
				t.Fatal("the end of a grace was recorded twice")
			}
			if due, _ := s.DueForGraceExpiry(Ctx(), grace.Add(time.Hour)); len(due) != 0 {
				t.Fatalf("an announced grace is due again: %+v", due)
			}
			if got, _ := getSub(t, s, sc); got.GraceLapsedAt == nil || !got.GraceLapsedAt.Equal(grace.Add(time.Second)) {
				t.Fatalf("subscription = %+v", got)
			}
		}},
		{"a newer state clears a grace before it is announced", func(t testing.TB, mk SubsFactory) {
			s := mk(t)
			sc := scope(8)
			grace := us(7 * 24 * time.Hour)
			pd := subOf(sc, "sub-8", domain.PastDue, ProCode)
			pd.GraceUntil = &grace
			apply(t, s, pd, us(time.Hour))
			due, _ := s.DueForGraceExpiry(Ctx(), grace)
			if len(due) != 1 {
				t.Fatalf("due: %v", due)
			}
			apply(t, s, subOf(sc, "sub-8", domain.Active, ProCode), us(2*time.Hour)) // the customer fixed their card
			if marked, _ := s.MarkGraceLapsed(Ctx(), due[0], grace.Add(time.Second)); marked {
				t.Fatal("the end of a grace was announced for a subscription that recovered")
			}
			if due, _ := s.DueForGraceExpiry(Ctx(), grace.Add(time.Hour)); len(due) != 0 {
				t.Fatalf("a recovered subscription is due: %+v", due)
			}
		}},
	}
}

// SubsContract is the specification of the subscriptions store (payments invariants 12, 13, 15): state is applied only
// if newer by the domain's total order, whatever the order or interleaving of the appliers; a scope has one live
// subscription; and the end of a grace window is due once and recorded once.
func SubsContract(t *testing.T, mk SubsFactory) {
	t.Helper()
	runStoreCases(t, SubsCases(), mk)
}
