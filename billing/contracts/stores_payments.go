package contracts

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// PaymentsFactory builds the payments store under test, on a world that has the harness's provider accounts and plans.
type PaymentsFactory func(t testing.TB) ports.Payments

// fullPayment is a payment with every field set, so a store that drops one is caught.
func fullPayment(scope md.Scope, id string, created time.Time) domain.Payment {
	tax := domain.Money{MinorUnits: 81, Currency: "USD"}
	return domain.Payment{Scope: scope.Normalized(), Account: AccountID, ProviderPaymentID: id, PlanCode: PackCode, Kind: "one_time",
		Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, TaxAmount: &tax, CreditsGranted: PackCredits, Pool: md.GeneralPool,
		RefundedMinor: 333, CreditsRevoked: 3_330_000, Status: domain.PartiallyRefunded, DisputeID: "dp_1", DisputeStatus: "open",
		DisputeClawed: 1_000_000, ReceiptURL: "https://pay.fake.test/receipt/" + id, FailureReason: "card_declined", IdemKey: id, CreatedAt: created}
}

func getPayment(t testing.TB, p ports.Payments, account, id string) (domain.Payment, bool) {
	t.Helper()
	got, ok, err := p.Get(Ctx(), account, id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return got, ok
}

// PaymentsCases are the subtests of PaymentsContract (invariants 8, 16, 17).
func PaymentsCases() []StoreCase[PaymentsFactory] {
	scopeA := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w1"}
	scopeB := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "w2"}
	return []StoreCase[PaymentsFactory]{
		{"a payment round-trips every field", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			want := fullPayment(scopeA, "pay-1", us(time.Minute))
			if err := p.Upsert(Ctx(), want); err != nil {
				t.Fatal(err)
			}
			got, ok := getPayment(t, p, AccountID, "pay-1")
			if !ok || jsonOf(t, got) != jsonOf(t, want) {
				t.Fatalf("got  %s\nwant %s", jsonOf(t, got), jsonOf(t, want))
			}
			if _, ok := getPayment(t, p, AccountID, "pay-unknown"); ok {
				t.Fatal("an unknown payment was found")
			}
		}},
		{"a payment with the same provider id under another account is another payment", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			a := fullPayment(scopeA, "pay-1", us(time.Minute))
			b := a
			b.Account, b.RefundedMinor, b.CreditsRevoked = OtherAccountID, 0, 0
			if err := p.Upsert(Ctx(), a); err != nil {
				t.Fatal(err)
			}
			if err := p.Upsert(Ctx(), b); err != nil {
				t.Fatal(err)
			}
			ga, _ := getPayment(t, p, AccountID, "pay-1")
			gb, _ := getPayment(t, p, OtherAccountID, "pay-1")
			if ga.RefundedMinor != 333 || gb.RefundedMinor != 0 {
				t.Fatalf("account A %+v, account B %+v", ga, gb)
			}
		}},
		{"Upsert replaces the payment it names and never makes a second", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			first := fullPayment(scopeA, "pay-1", us(time.Minute))
			if err := p.Upsert(Ctx(), first); err != nil {
				t.Fatal(err)
			}
			second := first
			second.Status, second.RefundedMinor, second.CreditsRevoked = domain.Refunded, 1000, PackCredits
			if err := p.Upsert(Ctx(), second); err != nil {
				t.Fatal(err)
			}
			got, _ := getPayment(t, p, AccountID, "pay-1")
			if got.Status != domain.Refunded || got.RefundedMinor != 1000 || !got.CreatedAt.Equal(first.CreatedAt) {
				t.Fatalf("payment = %+v", got)
			}
			if page, _, _ := p.List(Ctx(), scopeA, "", 10); len(page) != 1 {
				t.Fatalf("%d payments after two Upserts of one", len(page))
			}
		}},
		{"Update gives the function what is held and stores what it returns", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			base := fullPayment(scopeA, "pay-1", us(time.Minute))
			_, err := p.Update(Ctx(), AccountID, "pay-1", func(cur domain.Payment, found bool) (domain.Payment, error) {
				if found || cur.ProviderPaymentID != "" {
					t.Errorf("a payment that does not exist: found=%v cur=%+v", found, cur)
				}
				return base, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			out, err := p.Update(Ctx(), AccountID, "pay-1", func(cur domain.Payment, found bool) (domain.Payment, error) {
				if !found || jsonOf(t, cur) != jsonOf(t, base) {
					t.Errorf("found=%v cur=%s", found, jsonOf(t, cur))
				}
				cur.Status, cur.RefundedMinor = domain.Refunded, 1000
				return cur, nil
			})
			if err != nil || out.Status != domain.Refunded {
				t.Fatalf("Update = %+v, %v", out, err)
			}
			if got, _ := getPayment(t, p, AccountID, "pay-1"); got.Status != domain.Refunded || got.RefundedMinor != 1000 {
				t.Fatalf("stored = %+v", got)
			}
		}},
		{"concurrent updates of one payment are serialized and lose none", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			const n = 16
			var created atomic.Int32
			var wg sync.WaitGroup
			for range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := p.Update(Ctx(), AccountID, "pay-1", func(cur domain.Payment, found bool) (domain.Payment, error) {
						if !found {
							created.Add(1)
							cur = fullPayment(scopeA, "pay-1", us(time.Minute))
							cur.RefundedMinor, cur.CreditsRevoked, cur.DisputeClawed, cur.Status = 0, 0, 0, domain.Succeeded
						}
						time.Sleep(time.Millisecond) // hold the lock long enough for the others to arrive
						cur.RefundedMinor++
						return cur, nil
					})
					if err != nil {
						t.Errorf("Update: %v", err)
					}
				}()
			}
			wg.Wait()
			got, _ := getPayment(t, p, AccountID, "pay-1")
			if got.RefundedMinor != n || created.Load() != 1 {
				t.Fatalf("refunded %d after %d updates, created %d times: two reports for one payment must not read the same cumulative figures",
					got.RefundedMinor, n, created.Load())
			}
		}},
		{"an error from the function stores nothing", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			boom := errors.New("the meter is down")
			if _, err := p.Update(Ctx(), AccountID, "pay-1", func(domain.Payment, bool) (domain.Payment, error) {
				return fullPayment(scopeA, "pay-1", us(time.Minute)), boom
			}); !errors.Is(err, boom) {
				t.Fatalf("Update = %v, want the function's error", err)
			}
			if _, ok := getPayment(t, p, AccountID, "pay-1"); ok {
				t.Fatal("a failed update stored its payment")
			}
		}},
		{"a payment that breaks its own invariants is refused", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			good := fullPayment(scopeA, "pay-1", us(time.Minute))
			if err := p.Upsert(Ctx(), good); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func(*domain.Payment){
				"refunded more than was paid":        func(x *domain.Payment) { x.RefundedMinor = x.Amount.MinorUnits + 1 },
				"revoked more than was granted":      func(x *domain.Payment) { x.CreditsRevoked = x.CreditsGranted + 1 },
				"a chargeback larger than the grant": func(x *domain.Payment) { x.DisputeClawed = x.CreditsGranted + 1 },
				"a negative amount":                  func(x *domain.Payment) { x.Amount.MinorUnits = -1 },
				"a negative refund":                  func(x *domain.Payment) { x.RefundedMinor = -1 },
			} {
				bad := good
				mutate(&bad)
				if err := p.Upsert(Ctx(), bad); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("%s: Upsert = %v, want ErrInvalid", name, err)
				}
				if _, err := p.Update(Ctx(), AccountID, "pay-1", func(domain.Payment, bool) (domain.Payment, error) { return bad, nil }); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("%s: Update = %v, want ErrInvalid", name, err)
				}
			}
			if got, _ := getPayment(t, p, AccountID, "pay-1"); jsonOf(t, got) != jsonOf(t, good) {
				t.Fatalf("a refused write changed the payment: %+v", got)
			}
		}},
		{"listing is newest first, a page at a time, each payment once, and only the scope's", func(t testing.TB, mk PaymentsFactory) {
			p := mk(t)
			for i := range 7 {
				pay := fullPayment(scopeA, fmt.Sprintf("a-%d", i), us(time.Duration(i/2)*time.Minute)) // pairs share a timestamp
				if err := p.Upsert(Ctx(), pay); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 2 {
				if err := p.Upsert(Ctx(), fullPayment(scopeB, fmt.Sprintf("b-%d", i), us(time.Hour))); err != nil {
					t.Fatal(err)
				}
			}
			seen := map[string]bool{}
			var order []time.Time
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > 10 {
					t.Fatal("paging does not end")
				}
				page, next, err := p.List(Ctx(), scopeA, cursor, 3)
				if err != nil {
					t.Fatal(err)
				}
				for _, x := range page {
					if seen[x.ProviderPaymentID] {
						t.Fatalf("%s appeared on two pages", x.ProviderPaymentID)
					}
					seen[x.ProviderPaymentID] = true
					order = append(order, x.CreatedAt)
					if x.Scope != scopeA {
						t.Fatalf("scope B's payment %s leaked into scope A's list", x.ProviderPaymentID)
					}
				}
				if next == "" {
					break
				}
				cursor = next
			}
			if len(seen) != 7 {
				t.Fatalf("%d of 7 payments listed", len(seen))
			}
			for i := 1; i < len(order); i++ {
				if order[i].After(order[i-1]) {
					t.Fatalf("not newest first: %v", order)
				}
			}
			if _, _, err := p.List(Ctx(), scopeA, "not-a-cursor", 3); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("a bad cursor: %v, want ErrInvalid", err)
			}
			if _, _, err := p.List(Ctx(), scopeA, "", -1); !errors.Is(err, md.ErrInvalid) {
				t.Fatalf("a negative limit: %v, want ErrInvalid", err)
			}
			if page, _, err := p.List(Ctx(), scopeA, "", 0); err != nil || len(page) != 7 {
				t.Fatalf("a zero limit is the default page: got %d, %v", len(page), err)
			}
		}},
	}
}

// PaymentsContract is the specification of the payments store (invariants 8, 16, 17): a payment round-trips whole,
// is keyed by account and provider id, is changed only by an Update that holds an exclusive lock on it (so two
// refund reports cannot read the same cumulative figures), refuses what breaks its invariants, and is listed newest
// first without gaps or repeats.
func PaymentsContract(t *testing.T, mk PaymentsFactory) {
	t.Helper()
	runStoreCases(t, PaymentsCases(), mk)
}
