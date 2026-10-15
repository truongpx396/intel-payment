package contracts

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mcontracts "github.com/truongpx396/intel-payment/metering/contracts"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// StoreCase is one subtest of a store contract. F builds the store under test.
type StoreCase[F any] struct {
	Name string
	Run  func(t testing.TB, mk F)
}

func runStoreCases[F any](t *testing.T, cases []StoreCase[F], mk F) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}

// jsonOf canonicalizes a value for comparison: times in UTC, pointers followed. A store that rounds a time to the
// microsecond its database keeps would fail a comparison of values built with nanoseconds, so test values are built
// to the microsecond.
func jsonOf(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// us is an instant in the harness's day, to the microsecond.
func us(d time.Duration) time.Time { return Start.Add(d).Truncate(time.Microsecond).UTC() }

// InboxFactory builds the inbox under test on the clock given, with the attempt cap given.
type InboxFactory func(t testing.TB, clock *mcontracts.FakeClock, maxAttempts int) ports.Inbox

// fullEvent is an event with every fact an EventObject can carry, so a store that drops one is caught.
func fullEvent(id string, created time.Time, customer string) domain.Event {
	amount := domain.Money{MinorUnits: 1000, Currency: "USD"}
	refunded := domain.Money{MinorUnits: 500, Currency: "USD"}
	tax := domain.Money{MinorUnits: 81, Currency: "USD"}
	start, end := created.Add(-24*time.Hour), created.Add(29*24*time.Hour)
	return domain.Event{Provider: ProviderName, Account: AccountID, ProviderEventID: id, CreatedAt: created, Type: domain.PaymentRefunded,
		RawHash: strings.Repeat("ab", 32), Object: domain.EventObject{
			ProviderCustomerID: customer, ProviderPaymentID: "pay-" + id, ProviderSubscriptionID: "sub-" + id, ProviderPriceID: PackPriceID,
			ProviderRefundID: "re-" + id, DisputeID: "dp-" + id, Amount: &amount, AmountRefunded: &refunded, TaxAmount: &tax, Status: "won",
			PeriodStart: &start, PeriodEnd: &end, CancelAtPeriodEnd: true, ReceiptURL: "https://pay.fake.test/receipt/" + id,
			FailureReason: "card_declined", PaymentMethodRef: "pm_" + id}}
}

func leaseAll(t testing.TB, in ports.Inbox, n int, ttl time.Duration) []ports.LeasedEvent {
	t.Helper()
	got, err := in.Lease(Ctx(), n, ttl)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	return got
}

func ids(es []ports.LeasedEvent) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Event.ProviderEventID)
	}
	return out
}

func persist(t testing.TB, in ports.Inbox, e domain.Event) bool {
	t.Helper()
	inserted, err := in.Persist(Ctx(), e)
	if err != nil {
		t.Fatalf("Persist(%s): %v", e.ProviderEventID, err)
	}
	return inserted
}

// InboxCases are the subtests of InboxContract (payments invariants 5, 6, 14; D34).
func InboxCases() []StoreCase[InboxFactory] {
	const ttl = 2 * time.Minute
	return []StoreCase[InboxFactory]{
		{"persisting is idempotent on the account and the provider's event id, and keeps every fact", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			e := fullEvent("evt_1", us(time.Minute), "cus_1")
			if !persist(t, in, e) || persist(t, in, e) {
				t.Fatal("the first Persist must insert and the second must be a no-op that still succeeds (a replay answers 200)")
			}
			other := e
			other.Account = OtherAccountID
			if !persist(t, in, other) {
				t.Fatal("the same provider event id under another account is another event")
			}
			got := leaseAll(t, in, 10, ttl)
			if len(got) != 2 {
				t.Fatalf("leased %d events, want 2", len(got))
			}
			for _, le := range got {
				want := e
				want.Account = le.Event.Account
				if jsonOf(t, le.Event) != jsonOf(t, want) {
					t.Fatalf("the event did not survive the inbox:\n got  %s\n want %s", jsonOf(t, le.Event), jsonOf(t, want))
				}
				if le.ID == "" || le.Attempts != 1 {
					t.Fatalf("lease = %+v", le)
				}
			}
		}},
		{"an event verification could not have produced is refused and not stored", func(t testing.TB, mk InboxFactory) {
			in := mk(t, mcontracts.NewFakeClock(Start), 5)
			bad := fullEvent("evt_1", us(0), "cus_1")
			bad.RawHash = ""
			if _, err := in.Persist(Ctx(), bad); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("Persist = %v, want ErrInvalid", err)
			}
			if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
				t.Fatalf("a refused event was stored: %v", ids(got))
			}
		}},
		{"leases come oldest provider timestamp first and are exclusive until they expire", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			persist(t, in, fullEvent("c", us(3*time.Second), "cus_3"))
			persist(t, in, fullEvent("a", us(1*time.Second), "cus_1"))
			persist(t, in, fullEvent("b", us(2*time.Second), "cus_2"))

			first := leaseAll(t, in, 2, ttl)
			if got := ids(first); len(got) != 2 || got[0] != "a" || got[1] != "b" {
				t.Fatalf("leased %v, want the two oldest by the provider's clock: [a b]", got)
			}
			if got := ids(leaseAll(t, in, 10, ttl)); len(got) != 1 || got[0] != "c" {
				t.Fatalf("a second processor leased %v: events a and b are held, only c is free", got)
			}
			if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
				t.Fatalf("every event is held, and yet %v was leased", ids(got))
			}
			clock.Advance(ttl + time.Second)
			again := leaseAll(t, in, 10, ttl)
			if got := ids(again); len(got) != 3 || got[0] != "a" || got[2] != "c" {
				t.Fatalf("after the leases expired, leased %v, want [a b c] (a worker that died leaves its events to the next)", got)
			}
			for _, le := range again {
				if le.Attempts != 2 {
					t.Fatalf("event %s attempts = %d, want 2", le.Event.ProviderEventID, le.Attempts)
				}
			}
		}},
		{"a customer's events are handed out one at a time, in the provider's order", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			persist(t, in, fullEvent("e1", us(1*time.Second), "cus_1"))
			persist(t, in, fullEvent("e2", us(2*time.Second), "cus_1"))
			persist(t, in, fullEvent("e3", us(3*time.Second), "cus_2"))
			first := leaseAll(t, in, 10, ttl)
			if got := ids(first); len(got) != 2 || got[0] != "e1" || got[1] != "e3" {
				t.Fatalf("leased %v, want [e1 e3]: e2 waits for its customer's e1", got)
			}
			if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
				t.Fatalf("e2 was leased while e1 is still being worked: %v", ids(got))
			}
			if err := in.MarkProcessed(Ctx(), first[0].ID, "processed"); err != nil {
				t.Fatal(err)
			}
			if got := ids(leaseAll(t, in, 10, ttl)); len(got) != 1 || got[0] != "e2" {
				t.Fatalf("after e1 finished, leased %v, want [e2]", got)
			}
		}},
		{"events that name no customer do not exclude one another", func(t testing.TB, mk InboxFactory) {
			in := mk(t, mcontracts.NewFakeClock(Start), 5)
			persist(t, in, fullEvent("a", us(1*time.Second), ""))
			persist(t, in, fullEvent("b", us(2*time.Second), ""))
			if got := leaseAll(t, in, 10, ttl); len(got) != 2 {
				t.Fatalf("leased %v, want both", ids(got))
			}
		}},
		{"a failed event waits for its retry time, and is dead past the attempt cap and never leased again", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 3)
			persist(t, in, fullEvent("a", us(time.Second), "cus_1"))
			for attempt := 1; attempt <= 3; attempt++ {
				got := leaseAll(t, in, 10, ttl)
				if len(got) != 1 || got[0].Attempts != attempt {
					t.Fatalf("attempt %d: leased %+v", attempt, got)
				}
				dead, err := in.MarkFailed(Ctx(), got[0].ID, errors.New("meter is down"), clock.Now().Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if dead != (attempt == 3) {
					t.Fatalf("attempt %d: dead = %v", attempt, dead)
				}
				if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
					t.Fatalf("attempt %d: the event was leased again before its retry time", attempt)
				}
				clock.Advance(time.Minute)
			}
			clock.Advance(24 * time.Hour)
			if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
				t.Fatalf("a dead event was leased again: %v — it waits for a human", ids(got))
			}
			if _, ok, _ := in.Oldest(Ctx()); ok {
				t.Fatal("a dead event still counts as waiting to be fulfilled: it would pin the age gauge forever")
			}
		}},
		{"processed and ignored events are never leased again, and an unknown event is not found", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			persist(t, in, fullEvent("a", us(1*time.Second), "cus_1"))
			persist(t, in, fullEvent("b", us(2*time.Second), "cus_2"))
			got := leaseAll(t, in, 10, ttl)
			if err := in.MarkProcessed(Ctx(), got[0].ID, "processed"); err != nil {
				t.Fatal(err)
			}
			if err := in.MarkProcessed(Ctx(), got[1].ID, "ignored"); err != nil {
				t.Fatal(err)
			}
			clock.Advance(24 * time.Hour)
			if again := leaseAll(t, in, 10, ttl); len(again) != 0 {
				t.Fatalf("finished events were leased again: %v", ids(again))
			}
			if err := in.MarkProcessed(Ctx(), "00000000-0000-0000-0000-000000000000", "processed"); !errors.Is(err, md.ErrNotFound) {
				t.Fatalf("MarkProcessed of an unknown event = %v, want ErrNotFound", err)
			}
		}},
		{"concurrent processors are never handed the same event", func(t testing.TB, mk InboxFactory) {
			in := mk(t, mcontracts.NewFakeClock(Start), 5)
			const total = 40
			for i := range total {
				persist(t, in, fullEvent("e"+string(rune('A'+i/26))+string(rune('a'+i%26)), us(time.Duration(i)*time.Millisecond), "cus_"+string(rune('A'+i/26))+string(rune('a'+i%26))))
			}
			var mu sync.Mutex
			seen := map[string]int{}
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						got, err := in.Lease(Ctx(), 3, time.Hour)
						if err != nil {
							t.Errorf("Lease: %v", err)
							return
						}
						if len(got) == 0 {
							return
						}
						mu.Lock()
						for _, le := range got {
							seen[le.ID]++
						}
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if len(seen) != total {
				t.Fatalf("%d events were leased, want %d", len(seen), total)
			}
			for id, n := range seen {
				if n != 1 {
					t.Fatalf("event %s was leased %d times: two processors would have granted the same payment", id, n)
				}
			}
		}},
		{"a parked delivery keeps its raw body and headers byte for byte, once per delivery", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			body := []byte("{\"id\":\"evt_1\",\n \"amount\":1000, \"naïve\":\"\\u0000\"}\x00\xff")
			hdr := http.Header{"X-Fake-Signature": {"t=1,v1=abc"}, "Content-Type": {"application/json"}}
			for range 2 {
				if err := in.Park(Ctx(), ProviderName, AccountID, "parked:abc", body, hdr); err != nil {
					t.Fatal(err)
				}
			}
			if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
				t.Fatalf("an unverified delivery was leased as an event: %v", ids(got))
			}
			parked, err := in.LeaseParked(Ctx(), 10, ttl)
			if err != nil || len(parked) != 1 {
				t.Fatalf("LeaseParked = %d, %v: one delivery, parked twice", len(parked), err)
			}
			p := parked[0]
			if string(p.Body) != string(body) || p.Header.Get("X-Fake-Signature") != "t=1,v1=abc" || p.Header.Get("Content-Type") != "application/json" ||
				p.Provider != ProviderName || p.Account != AccountID || p.Attempts != 1 {
				t.Fatalf("parked = %+v: the body must be exactly what arrived or it can never be verified", p)
			}
			if again, _ := in.LeaseParked(Ctx(), 10, ttl); len(again) != 0 {
				t.Fatal("a held parked delivery was leased again")
			}
		}},
		{"a parked delivery that verifies becomes an ordinary event and loses its body", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			if err := in.Park(Ctx(), ProviderName, AccountID, "parked:abc", []byte("raw"), http.Header{"A": {"b"}}); err != nil {
				t.Fatal(err)
			}
			parked, _ := in.LeaseParked(Ctx(), 10, ttl)
			e := fullEvent("evt_1", us(time.Second), "cus_1")
			inserted, err := in.Promote(Ctx(), parked[0].ID, e)
			if err != nil || !inserted {
				t.Fatalf("Promote = %v, %v", inserted, err)
			}
			clock.Advance(24 * time.Hour)
			if again, _ := in.LeaseParked(Ctx(), 10, ttl); len(again) != 0 {
				t.Fatal("a promoted delivery is still parked")
			}
			got := leaseAll(t, in, 10, ttl)
			if len(got) != 1 || jsonOf(t, got[0].Event) != jsonOf(t, e) || got[0].Attempts != 1 {
				t.Fatalf("leased %+v, want the promoted event as a fresh one", got)
			}
			if persist(t, in, e) {
				t.Fatal("the promoted event can be persisted again: the replay guard must cover it")
			}
		}},
		{"promoting a delivery whose event already arrived normally keeps one event", func(t testing.TB, mk InboxFactory) {
			in := mk(t, mcontracts.NewFakeClock(Start), 5)
			e := fullEvent("evt_1", us(time.Second), "cus_1")
			if err := in.Park(Ctx(), ProviderName, AccountID, "parked:abc", []byte("raw"), nil); err != nil {
				t.Fatal(err)
			}
			persist(t, in, e) // the provider redelivered it, and verification was up this time
			parked, _ := in.LeaseParked(Ctx(), 10, ttl)
			inserted, err := in.Promote(Ctx(), parked[0].ID, e)
			if err != nil || inserted {
				t.Fatalf("Promote = %v, %v; want (false, nil): it is the same event", inserted, err)
			}
			if got := leaseAll(t, in, 10, ttl); len(got) != 1 {
				t.Fatalf("%d events, want 1", len(got))
			}
			if again, _ := in.LeaseParked(Ctx(), 10, ttl); len(again) != 0 {
				t.Fatal("the parked copy is still waiting")
			}
		}},
		{"a parked delivery can back off, and a rejected one is never tried again", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			if err := in.Park(Ctx(), ProviderName, AccountID, "parked:abc", []byte("raw"), nil); err != nil {
				t.Fatal(err)
			}
			p1, _ := in.LeaseParked(Ctx(), 10, ttl)
			if err := in.RetryParked(Ctx(), p1[0].ID, clock.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if got, _ := in.LeaseParked(Ctx(), 10, ttl); len(got) != 0 {
				t.Fatal("a parked delivery was retried before its time")
			}
			clock.Advance(time.Minute)
			p2, _ := in.LeaseParked(Ctx(), 10, ttl)
			if len(p2) != 1 || p2[0].Attempts != 2 {
				t.Fatalf("after backing off: %+v", p2)
			}
			if err := in.Reject(Ctx(), p2[0].ID, "invalid_signature"); err != nil {
				t.Fatal(err)
			}
			clock.Advance(24 * time.Hour)
			if got, _ := in.LeaseParked(Ctx(), 10, ttl); len(got) != 0 {
				t.Fatal("a rejected delivery was tried again")
			}
			if got := leaseAll(t, in, 10, ttl); len(got) != 0 {
				t.Fatal("a rejected delivery became an event")
			}
		}},
		{"the age of the oldest unfulfilled event is known, parked deliveries included", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			if _, ok, err := in.Oldest(Ctx()); ok || err != nil {
				t.Fatalf("Oldest of an empty inbox = %v, %v", ok, err)
			}
			persist(t, in, fullEvent("a", us(time.Second), "cus_1"))
			clock.Advance(5 * time.Minute)
			persist(t, in, fullEvent("b", us(2*time.Second), "cus_2"))
			at, ok, _ := in.Oldest(Ctx())
			if !ok || !at.Equal(Start) {
				t.Fatalf("Oldest = %v, %v; want the first receipt, %v", at, ok, Start)
			}
			got := leaseAll(t, in, 10, ttl)
			for _, le := range got {
				if le.Event.ProviderEventID == "a" {
					_ = in.MarkProcessed(Ctx(), le.ID, "processed")
				}
			}
			at, ok, _ = in.Oldest(Ctx())
			if !ok || !at.Equal(Start.Add(5*time.Minute)) {
				t.Fatalf("Oldest = %v, %v; want the second receipt", at, ok)
			}
			for _, le := range got {
				if le.Event.ProviderEventID == "b" {
					_ = in.MarkProcessed(Ctx(), le.ID, "processed")
				}
			}
			if _, ok, _ := in.Oldest(Ctx()); ok {
				t.Fatal("a drained inbox has an oldest event")
			}
			clock.Advance(time.Minute)
			if err := in.Park(Ctx(), ProviderName, AccountID, "parked:abc", []byte("raw"), nil); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := in.Oldest(Ctx()); !ok {
				t.Fatal("a payment parked for want of verification is unfulfilled, and must show in the age")
			}
		}},
		{"processed events past retention are purged and nothing else", func(t testing.TB, mk InboxFactory) {
			clock := mcontracts.NewFakeClock(Start)
			in := mk(t, clock, 5)
			persist(t, in, fullEvent("old", us(time.Second), "cus_1"))
			persist(t, in, fullEvent("stuck", us(2*time.Second), "cus_2"))
			got := leaseAll(t, in, 10, 24*time.Hour)
			for _, le := range got {
				if le.Event.ProviderEventID == "old" {
					_ = in.MarkProcessed(Ctx(), le.ID, "processed")
				}
			}
			clock.Advance(48 * time.Hour)
			persist(t, in, fullEvent("new", us(49*time.Hour), "cus_3"))
			for _, le := range leaseAll(t, in, 10, ttl) {
				if le.Event.ProviderEventID == "new" {
					_ = in.MarkProcessed(Ctx(), le.ID, "processed")
				}
			}
			n, err := in.Purge(Ctx(), Start.Add(24*time.Hour))
			if err != nil || n != 1 {
				t.Fatalf("Purge = %d, %v; want exactly the old processed event", n, err)
			}
			if !persist(t, in, fullEvent("old", us(time.Second), "cus_1")) {
				t.Fatal("a purged event can be persisted again (the permanent grant guard, not the inbox, stops a second grant)")
			}
			clock.Advance(time.Hour)
			leased := ids(leaseAll(t, in, 10, ttl))
			sort.Strings(leased)
			if len(leased) == 0 || leased[0] != "old" {
				t.Fatalf("leased %v", leased)
			}
			if _, ok, _ := in.Oldest(Ctx()); !ok {
				t.Fatal("an unprocessed event was purged or lost")
			}
		}},
	}
}

// InboxContract is the specification of the webhook inbox (payments invariants 5, 6, 14; D34): persisting is
// idempotent and lossless, a lease is exclusive per event and per customer and expires, a failure backs off and dies
// at a cap, parked deliveries keep their raw body until they verify, and retention purges only what is finished.
func InboxContract(t *testing.T, mk InboxFactory) {
	t.Helper()
	runStoreCases(t, InboxCases(), mk)
}
