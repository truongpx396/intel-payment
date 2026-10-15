package contracts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ----------------------------------------------------------- plan change --

// SubscriptionsFactory builds the subscription service under test over a harness's collaborators: its providers,
// accounts, catalogue, subscriptions and payments, the policy given, and the guard (nil is no guard). It is given
// no way to write subscription status.
type SubscriptionsFactory func(t testing.TB, h *Harness, policy domain.PlanChangePolicy, guard ports.PlanChangeGuard) ports.Subscriptions

// GuardFunc is a PlanChangeGuard, with a record of how it was asked.
type GuardFunc struct {
	Err   error
	mu    sync.Mutex
	calls [][2]string
}

// CheckChange implements ports.PlanChangeGuard.
func (g *GuardFunc) CheckChange(_ context.Context, _ domain.Scope, from, to domain.Plan) error {
	g.mu.Lock()
	g.calls = append(g.calls, [2]string{from.Code, to.Code})
	g.mu.Unlock()
	return g.Err
}

// Calls is every (from, to) the guard was asked about.
func (g *GuardFunc) Calls() [][2]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][2]string(nil), g.calls...)
}

// PlanChangeCase is one subtest of PlanChangeContract.
type PlanChangeCase struct {
	Name string
	Run  func(t testing.TB, mk SubscriptionsFactory)
}

// GivenSubscription puts the scope on the Pro plan, active, as the processor would have left it, with the invoice
// that fixed its currency.
func (h *Harness) GivenSubscription() domain.Sub {
	s := domain.Sub{Scope: h.Scope, PlanCode: ProCode, Account: AccountID, ProviderSubscriptionID: "sub-1", Status: domain.Active,
		CurrentPeriodStart: Start.Add(-24 * time.Hour), CurrentPeriodEnd: Start.Add(29 * 24 * time.Hour), LastEventAt: Start}
	h.Subs.Put(s)
	if err := h.Payments.Upsert(Ctx(), domain.Payment{Scope: h.Scope, Account: AccountID, ProviderPaymentID: "in-1", PlanCode: ProCode, Kind: "subscription_invoice",
		Amount: domain.Money{MinorUnits: 2000, Currency: "USD"}, CreditsGranted: ProCredits, Pool: "plan", Status: domain.Succeeded, IdemKey: "in-1", CreatedAt: Start}); err != nil {
		panic(err)
	}
	h.Log.Reset()
	return s
}

// PlanChangeCases are the subtests of PlanChangeContract (invariant 12; FR-029).
func PlanChangeCases() []PlanChangeCase {
	return []PlanChangeCase{
		{"a plan change follows the policy, and the guard decides whether it can be made now (FR-029)", func(t testing.TB, mk SubscriptionsFactory) {
			below := fmt.Errorf("%w: 12 seats in use, the plan allows 5", domain.ErrBelowUsage)
			outage := errors.New("the entitlement service is unavailable")
			for _, tc := range []struct {
				name     string
				to       string
				price    string
				policy   domain.PlanChangePolicy
				guardErr error
				wantCall string // the provider's call, "" for none
				wantAsk  bool   // the guard is consulted
				wantErr  error
			}{
				{"an upgrade, at the period's end", TeamCode, TeamPriceID, domain.AtPeriodEnd, nil, "sub-1>price_team/at_period_end", false, nil},
				{"an upgrade now, prorated", TeamCode, TeamPriceID, domain.ImmediateProrate, nil, "sub-1>price_team/immediate_prorate", true, nil},
				{"an upgrade now, not prorated", TeamCode, TeamPriceID, domain.ImmediateNoProrate, nil, "sub-1>price_team/immediate_no_prorate", true, nil},
				{"a downgrade, at the period's end, though usage is over the new plan", LiteCode, LitePriceID, domain.AtPeriodEnd, below, "sub-1>price_lite/at_period_end", false, nil},
				{"a downgrade now, prorated, within usage", LiteCode, LitePriceID, domain.ImmediateProrate, nil, "sub-1>price_lite/immediate_prorate", true, nil},
				{"a downgrade now, not prorated, within usage", LiteCode, LitePriceID, domain.ImmediateNoProrate, nil, "sub-1>price_lite/immediate_no_prorate", true, nil},
				{"a downgrade now, prorated, that would take a quota below usage", LiteCode, LitePriceID, domain.ImmediateProrate, below, "", true, domain.ErrBelowUsage},
				{"a downgrade now, not prorated, that would take a quota below usage", LiteCode, LitePriceID, domain.ImmediateNoProrate, below, "", true, domain.ErrBelowUsage},
				{"a change now when the guard cannot answer", LiteCode, LitePriceID, domain.ImmediateProrate, outage, "", true, outage},
			} {
				h := NewHarness()
				h.GivenSubscription()
				guard := &GuardFunc{Err: tc.guardErr}
				s := mk(t, h, tc.policy, guard)
				err := s.Change(Ctx(), h.Scope, tc.to)
				if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Errorf("%s: Change = %v, want %v", tc.name, err, tc.wantErr)
				}
				calls := h.Provider.Changes()
				switch {
				case tc.wantCall == "" && len(calls) != 0:
					t.Errorf("%s: the provider was asked for %v though the change cannot be made", tc.name, calls)
				case tc.wantCall != "" && (len(calls) != 1 || calls[0] != tc.wantCall):
					t.Errorf("%s: the provider was asked for %v, want [%s]", tc.name, calls, tc.wantCall)
				}
				if asked := len(guard.Calls()) > 0; asked != tc.wantAsk {
					t.Errorf("%s: guard consulted = %v, want %v", tc.name, asked, tc.wantAsk)
				}
				if tc.wantAsk && len(guard.Calls()) == 1 && guard.Calls()[0] != [2]string{ProCode, tc.to} {
					t.Errorf("%s: the guard was asked about %v, want pro → %s", tc.name, guard.Calls()[0], tc.to)
				}
			}
		}},
		{"a change no one can make is refused before the provider hears of it", func(t testing.TB, mk SubscriptionsFactory) {
			h := NewHarness()
			h.GivenSubscription()
			s := mk(t, h, domain.ImmediateProrate, nil)
			for name, tc := range map[string]struct {
				plan string
				want error
			}{
				"the plan it is already on": {ProCode, md.ErrInvalid},
				"a one-time pack":           {PackCode, md.ErrInvalid},
				"a plan nobody sells":       {"platinum", domain.ErrUnknownPrice},
			} {
				if err := s.Change(Ctx(), h.Scope, tc.plan); !errors.Is(err, tc.want) {
					t.Errorf("%s: Change = %v, want %v", name, err, tc.want)
				}
			}
			nobody := md.Scope{Realm: md.DefaultRealm, Kind: "workspace", ID: "no-sub"}
			if err := s.Change(Ctx(), nobody, TeamCode); !errors.Is(err, md.ErrNotFound) {
				t.Errorf("a scope with no subscription: Change = %v, want ErrNotFound", err)
			}
			canceled := h.GivenSubscription()
			canceled.Status = domain.Canceled
			canceled.LastEventAt = Start.Add(time.Hour)
			h.Subs.Put(canceled)
			if err := s.Change(Ctx(), h.Scope, TeamCode); !errors.Is(err, md.ErrNotFound) {
				t.Errorf("a canceled subscription: Change = %v, want ErrNotFound — there is nothing live to change", err)
			}
			if len(h.Provider.Changes()) != 0 {
				t.Fatalf("the provider was asked for %v", h.Provider.Changes())
			}
		}},
		{"changing or canceling writes no status: the webhook does (12)", func(t testing.TB, mk SubscriptionsFactory) {
			h := NewHarness()
			before := h.GivenSubscription()
			s := mk(t, h, domain.ImmediateProrate, nil)
			if err := s.Change(Ctx(), h.Scope, TeamCode); err != nil {
				t.Fatal(err)
			}
			if err := s.CancelAtPeriodEnd(Ctx(), h.Scope); err != nil {
				t.Fatal(err)
			}
			if got := h.Subs.Row("sub-1"); got != before {
				t.Fatalf("the subscription changed without a webhook: %+v → %+v", before, got)
			}
			if touched := h.Log.Touched("subs.ApplyIfNewer", "subs.MarkGraceLapsed"); len(touched) != 0 {
				t.Fatalf("a request wrote subscription state: %v", touched)
			}
			if c := h.Provider.Cancels(); len(c) != 1 || c[0] != "sub-1" {
				t.Fatalf("the provider was asked to cancel %v, want [sub-1]", c)
			}
		}},
		{"canceling needs a live subscription", func(t testing.TB, mk SubscriptionsFactory) {
			h := NewHarness()
			s := mk(t, h, domain.AtPeriodEnd, nil)
			if err := s.CancelAtPeriodEnd(Ctx(), h.Scope); !errors.Is(err, md.ErrNotFound) {
				t.Fatalf("CancelAtPeriodEnd = %v, want ErrNotFound", err)
			}
			if len(h.Provider.Cancels()) != 0 {
				t.Fatal("the provider was asked to cancel nothing")
			}
		}},
	}
}

// PlanChangeContract is the specification of plan changes and cancellations (invariant 12, FR-029): the policy
// decides when a change takes effect, an immediate one that would take a quota below usage is refused, and neither
// writes subscription status — they call the provider and wait for the webhook.
func PlanChangeContract(t *testing.T, mk SubscriptionsFactory) {
	t.Helper()
	for _, c := range PlanChangeCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}
