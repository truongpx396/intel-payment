package contracts

import (
	"fmt"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// ProcessorCase is one subtest of a processor suite. The cases are exported so each suite can be shown to have
// teeth: run a case against a deliberately broken processor with Probe and require that it is rejected.
type ProcessorCase struct {
	Name string
	Run  func(t testing.TB, mk ProcessorFactory)
}

// The metric names the processor suites check, as literals: billing/ports holds them equal to the contract.
const (
	metricProcessed    = "billing_webhook_processed_total"
	metricIgnored      = "billing_webhook_ignored_total"
	metricFailed       = "billing_webhook_failed_total"
	metricDead         = "billing_webhook_dead_total"
	metricForeignRealm = "billing_webhook_foreign_realm_total"
	metricGrantApplied = "billing_grant_applied_total"
	metricInboxAge     = "billing_webhook_inbox_age_seconds"
)

func fake(extra ...string) mports.Labels {
	l := mports.Labels{"provider": "fake"}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i]] = extra[i+1]
	}
	return l
}

// runCases runs a suite's cases as subtests.
func runCases(t *testing.T, cases []ProcessorCase, mk ProcessorFactory) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}

// row finds the inbox row of an event, failing the case if the event is not there.
func row(t testing.TB, h *Harness, eventID string) InboxRow {
	t.Helper()
	r, ok := h.Inbox.Row(eventID)
	if !ok {
		t.Fatalf("event %s is not in the inbox", eventID)
	}
	return r
}

func wantRow(t testing.TB, h *Harness, eventID, status string) InboxRow {
	t.Helper()
	r := row(t, h, eventID)
	if r.Status != status {
		t.Fatalf("event %s is %s (%s), want %s", eventID, r.Status, r.Err, status)
	}
	return r
}

// GrantCases are the subtests of the grants suite (stage 2: payment_succeeded; invariants 2, 3, 4, 5, 19).
func GrantCases() []ProcessorCase {
	return []ProcessorCase{
		{"a crash after the acknowledgement still grants, exactly once", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Meter.FailNextGrant(1) // the first attempt dies mid-dispatch

			h.Tick(t, p)
			if h.Meter.Grants("pay-1") != 0 {
				t.Fatal("the grant failed and yet counts as granted")
			}
			r := wantRow(t, h, "evt_1", RowFailed)
			if r.Err == "" {
				t.Fatal("a failed attempt must keep its reason")
			}

			h.Rounds(t, p, 1, time.Hour) // an hour: past any backoff
			wantRow(t, h, "evt_1", RowProcessed)
			if n := h.Meter.Grants("pay-1"); n != 1 {
				t.Fatalf("the inbox must re-drive the event to exactly one grant, got %d (invariant 5)", n)
			}
			h.Rounds(t, p, 2, time.Hour)
			if h.Meter.Grants("pay-1") != 1 || h.Meter.GrantCalls() != 2 {
				t.Fatalf("a processed event ran again: %d grants in %d calls", h.Meter.Grants("pay-1"), h.Meter.GrantCalls())
			}
			pay := h.Payments.Row("pay-1")
			if pay.Status != domain.Succeeded || pay.CreditsGranted != PackCredits || pay.IdemKey != "pay-1" || pay.Scope != h.Scope {
				t.Fatalf("payment row = %+v", pay)
			}
		}},
		{"a processor killed mid-event leaves a lease that expires, and the next one finishes the event", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			// A worker leased the event and died without acknowledging it.
			if leased, err := h.Inbox.Lease(Ctx(), 10, 2*time.Minute); err != nil || len(leased) != 1 {
				t.Fatalf("setup: leased %d, %v", len(leased), err)
			}
			h.Tick(t, p)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("a processor took an event another one holds a lease on")
			}
			h.Rounds(t, p, 1, time.Hour)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 {
				t.Fatalf("grants = %d, want 1", h.Meter.Grants("pay-1"))
			}
		}},
		{"a grant already made before the crash is a replay, not a second grant", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			// The previous run granted and died before it could write the payment or acknowledge the event.
			if _, err := h.Meter.Grant(Ctx(), md.Grant{Scope: h.Scope, Pool: md.GeneralPool, Amount: PackCredits, IdemKey: "pay-1", Reason: "purchase"}); err != nil {
				t.Fatal(err)
			}
			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 || h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
				t.Fatalf("the re-run granted again: %d grants, balance %d", h.Meter.Grants("pay-1"), h.Meter.Balance(h.Scope, md.GeneralPool))
			}
			if h.Payments.Row("pay-1").Status != domain.Succeeded {
				t.Fatal("the re-run did not finish the payment row")
			}
		}},
		{"concurrent processors grant once", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-2"))
			h.Inbox.SyncLeases(8) // all eight reach the inbox together: whoever may take the event must be decided by the lease
			done := make(chan struct{})
			for range 8 {
				p := mk(t, h)
				go func() {
					defer func() { done <- struct{}{} }()
					if err := p.Tick(Ctx()); err != nil {
						t.Errorf("Tick: %v", err)
					}
				}()
			}
			for range 8 {
				<-done
			}
			if h.Meter.Grants("pay-2") != 1 {
				t.Fatalf("leases must keep processors off one event: %d grants", h.Meter.Grants("pay-2"))
			}
			if h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
				t.Fatalf("balance = %d, want one pack", h.Meter.Balance(h.Scope, md.GeneralPool))
			}
		}},
		{"metadata cannot redirect a grant to another scope", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentSucceeded("evt_1", "pay-3")
			fe.Metadata = map[string]string{"scope_id": "victim", "scope_kind": "workspace", "realm": "other"}
			h.Persisted(fe)
			h.Tick(t, p)
			g := h.Meter.LastGrant()
			if g.Scope != h.Scope {
				t.Fatalf("the grant went to %+v: scope comes from the verified customer, never from metadata (invariant 4)", g.Scope)
			}
		}},
		{"the customer decides the scope", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentSucceeded("evt_1", "pay-1")
			fe.Customer = "cus_2"
			h.Persisted(fe)
			h.Tick(t, p)
			if g := h.Meter.LastGrant(); g.Scope != h.Scope2 {
				t.Fatalf("customer cus_2 is %+v, the grant went to %+v", h.Scope2, g.Scope)
			}
		}},
		{"credits come from the catalogue's allotment and pool, never from the amount paid", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			discounted := h.PaymentSucceeded("evt_1", "pay-1")
			discounted.Amount = 500 // a coupon
			renewal := h.SubscriptionPaymentSucceeded("evt_2", "in-1", "sub-1")
			h.Persisted(discounted)
			h.Persisted(renewal)
			h.Rounds(t, p, 2, time.Minute) // one customer's events are handled one at a time, in the provider's order

			pack, pro := h.Payments.Row("pay-1"), h.Payments.Row("in-1")
			if pack.CreditsGranted != PackCredits || pack.Pool != md.GeneralPool || pack.Amount.MinorUnits != 500 {
				t.Fatalf("pack = %+v: the allotment is the catalogue's; the amount recorded is what the provider reported", pack)
			}
			if pro.CreditsGranted != ProCredits || pro.Pool != "plan" || pro.Kind != "subscription_invoice" || pro.PlanCode != ProCode {
				t.Fatalf("pro = %+v", pro)
			}
			grants := map[string]md.Grant{}
			for _, g := range h.Meter.Applied() {
				grants[g.IdemKey] = g
			}
			if g := grants["pay-1"]; g.Amount != PackCredits || g.Reason != "purchase" || g.Pool != md.GeneralPool {
				t.Fatalf("pack grant = %+v", g)
			}
			if g := grants["in-1"]; g.Amount != ProCredits || g.Reason != "subscription_grant" || g.Pool != "plan" {
				t.Fatalf("pro grant = %+v", g)
			}
		}},
		{"the grant names the payment it belongs to", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Tick(t, p)
			g := h.Meter.LastGrant()
			if g.Ref["payment"] != "pay-1" || g.Ref["account"] != AccountID || g.Ref["event"] != "evt_1" {
				t.Fatalf("ref = %v: the books must be traceable to the provider's payment", g.Ref)
			}
			if got := h.Metrics.Counter(metricGrantApplied, mports.Labels{"realm": "default", "reason": "purchase"}); got != 1 {
				t.Fatalf("%s = %d, want 1", metricGrantApplied, got)
			}
		}},
		{"a plan with no allotment mints nothing", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Catalog.Add(domain.Plan{Realm: md.DefaultRealm, Code: "enterprise", Name: "Negotiated", Kind: domain.OneTime, Pool: md.GeneralPool,
				Active: true, Prices: map[string]domain.Money{"USD": {MinorUnits: 90000, Currency: "USD"}}}, AccountID, "USD", "price_custom")
			fe := h.PaymentSucceeded("evt_1", "pay-9")
			fe.Price, fe.Amount = "price_custom", 90000
			h.Persisted(fe)
			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("a negotiated plan has no allotment: credits are granted by hand")
			}
			if pay := h.Payments.Row("pay-9"); pay.Status != domain.Succeeded || pay.CreditsGranted != 0 {
				t.Fatalf("the payment is still recorded: %+v", pay)
			}
		}},
		{"a price whose plan is another realm's fails loudly instead of granting into the customer's", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			credits := PackCredits
			h.Catalog.Add(domain.Plan{Realm: "other", Code: "pack", Name: "Pack of another product", Kind: domain.OneTime, CreditAllotment: &credits,
				Pool: md.GeneralPool, Active: true, Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}}}, AccountID, "USD", "price_elsewhere")
			fe := h.PaymentSucceeded("evt_1", "pay-1")
			fe.Price = "price_elsewhere" // cus_1 is in the default realm
			h.Persisted(fe)
			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowFailed)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("credits were granted to a customer for a plan of another realm: money taken for something the customer's realm does not sell")
			}
		}},
		{"an event for a realm the account does not serve is ignored and counted (19)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentSucceeded("evt_1", "pay-6")
			fe.Customer = "cus_foreign"
			h.Persisted(fe)
			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowIgnored)
			if h.Meter.GrantCalls() != 0 || h.Payments.Count() != 0 {
				t.Fatalf("an account reached a realm it does not serve: %d grant calls, %d payments", h.Meter.GrantCalls(), h.Payments.Count())
			}
			if got := h.Metrics.Counter(metricForeignRealm, fake()); got != 1 {
				t.Fatalf("%s = %d, want 1: a cross-realm resolution is a security event", metricForeignRealm, got)
			}
		}},
		{"an unknown customer is ignored, not an error", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentSucceeded("evt_1", "pay-1")
			fe.Customer = "cus_from_another_environment"
			h.Persisted(fe)
			h.Tick(t, p)
			r := wantRow(t, h, "evt_1", RowIgnored)
			if r.Attempts != 1 || h.Meter.GrantCalls() != 0 {
				t.Fatalf("attempts %d, grant calls %d", r.Attempts, h.Meter.GrantCalls())
			}
			if got := h.Metrics.Counter(metricIgnored, fake("reason", "unknown_customer")); got != 1 {
				t.Fatalf("%s{unknown_customer} = %d, want 1", metricIgnored, got)
			}
			if h.Metrics.Counter(metricFailed, fake()) != 0 {
				t.Fatal("normal traffic from a shared provider account must not look like a failure")
			}
		}},
		{"a payment for a price the catalogue does not sell grants nothing and falls back to no default plan", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentSucceeded("evt_1", "pay-1")
			fe.Price = "price_mystery"
			h.Persisted(fe)
			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowIgnored)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("credits were granted for a price the deployment does not sell (FR-021)")
			}
			if got := h.Metrics.Counter(metricIgnored, fake("reason", "unknown_price")); got != 1 {
				t.Fatalf("%s{unknown_price} = %d, want 1", metricIgnored, got)
			}
		}},
		{"one payment under two events grants once", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			// invoice.paid and checkout.session.completed can both arrive for one purchase: the grant is keyed on the
			// payment, never on the event.
			h.Persisted(h.PaymentSucceeded("evt_a", "pay-1"))
			h.Persisted(h.PaymentSucceeded("evt_b", "pay-1"))
			h.Rounds(t, p, 2, time.Minute)
			wantRow(t, h, "evt_a", RowProcessed)
			wantRow(t, h, "evt_b", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 || h.Payments.Count() != 1 || h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
				t.Fatalf("grants %d, payments %d, balance %d", h.Meter.Grants("pay-1"), h.Payments.Count(), h.Meter.Balance(h.Scope, md.GeneralPool))
			}
		}},
		{"a payment without an amount fails loudly instead of granting", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			fe := h.PaymentSucceeded("evt_1", "pay-1")
			fe.Amount, fe.Currency = 0, ""
			h.Persisted(fe)
			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowFailed)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("a payment of unknown amount was granted")
			}
		}},
		{"an unsubscribed type is marked ignored", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(FakeEvent{ID: "evt_u", Type: "customer.created", Created: h.Now(), Customer: "cus_1"})
			h.Tick(t, p)
			wantRow(t, h, "evt_u", RowIgnored)
			if got := h.Metrics.Counter(metricIgnored, fake("reason", "unsubscribed_type")); got != 1 {
				t.Fatalf("%s{unsubscribed_type} = %d, want 1", metricIgnored, got)
			}
		}},
		{"an event that keeps failing is retried on a backoff, then dead and paged, and never touched again", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Meter.FailNextGrant(1000)

			h.Tick(t, p)
			r := wantRow(t, h, "evt_1", RowFailed)
			if !r.NextAttemptAt.After(h.Clock.Now()) {
				t.Fatal("a failed attempt must wait before the next: a hot loop on a failing dependency makes it worse")
			}
			calls := h.Meter.GrantCalls()
			h.Tick(t, p) // no time has passed
			if h.Meter.GrantCalls() != calls {
				t.Fatal("an event was retried before its backoff elapsed")
			}

			h.Rounds(t, p, 6, time.Hour)
			wantRow(t, h, "evt_1", RowDead)
			if got := h.Metrics.Counter(metricDead, fake()); got != 1 {
				t.Fatalf("%s = %d, want exactly 1: a dead event pages once", metricDead, got)
			}
			if got := h.Metrics.Counter(metricFailed, fake()); got != 5 {
				t.Fatalf("%s = %d, want 5, one per attempt", metricFailed, got)
			}
			calls = h.Meter.GrantCalls()
			h.Rounds(t, p, 3, time.Hour)
			if h.Meter.GrantCalls() != calls {
				t.Fatal("a dead event was processed again: it waits for a human")
			}
		}},
		{"the age of the oldest unprocessed event is published", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.PaymentSucceeded("evt_1", "pay-1"))
			h.Clock.Advance(10 * time.Minute)
			h.Meter.FailNextGrant(1)
			h.Tick(t, p)
			if age, ok := h.Metrics.GaugeValue(metricInboxAge, nil); !ok || age != 600 {
				t.Fatalf("%s = %d (set %v), want 600: a payment someone made that has waited ten minutes", metricInboxAge, age, ok)
			}
			h.Rounds(t, p, 1, time.Hour)
			if age, _ := h.Metrics.GaugeValue(metricInboxAge, nil); age != 0 {
				t.Fatalf("%s = %d once drained, want 0", metricInboxAge, age)
			}
		}},
	}
}

// metricUnderpaid is a payment that settled for less than its plan costs.
const metricUnderpaid = "billing_payment_underpaid_total"

// UnderpaymentCases are the subtests of the underpayment suite, under the default policy. A plan's credits are minted from the
// catalogue and never from the amount paid (GrantCases): a provider-side coupon that takes a pack from $10.00 to $5.00 still
// grants the whole pack. What no catalogue can honour is a payment of NOTHING — a trial invoice, a coupon of 100% — which would
// mint a plan's whole allotment for no money at all. That is held: nothing is granted, it is announced, and the event is retried
// and ends dead and paging, as a charge the processor cannot fulfil does. A discount above nothing is granted as before, and
// counted, so that revenue given away is a figure and not a silence (constitution I, IV and XII).
func UnderpaymentCases() []ProcessorCase {
	paid := func(h *Harness, minor int64) FakeEvent {
		ev := h.PaymentSucceeded("evt_1", "pay-1")
		ev.Amount = minor
		return ev
	}
	return []ProcessorCase{
		{"a payment of exactly nothing for a plan that costs money grants nothing, is retried, and in the end is dead and pages", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			ev := paid(h, 0)
			ev.ZeroPaid = true // a trial, or an invoice a coupon took to zero
			h.Persisted(ev)

			h.Tick(t, p)
			if n := h.Meter.Grants("pay-1"); n != 0 {
				t.Fatalf("a payment of 0 against a price of 1000 granted the pack (%d grants, %d credits): credits were minted for no money", n, h.Meter.Balance(h.Scope, md.GeneralPool))
			}
			if r := wantRow(t, h, "evt_1", RowFailed); r.Err == "" {
				t.Fatal("a held payment must keep its reason: an operator reads it to decide")
			}
			if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", "held")); got != 1 {
				t.Fatalf("%s{outcome=held} = %d, want 1: an underpayment is announced the moment it is seen", metricUnderpaid, got)
			}

			h.Rounds(t, p, 12, time.Hour) // past every backoff and the attempt cap
			wantRow(t, h, "evt_1", RowDead)
			if h.Meter.Grants("pay-1") != 0 {
				t.Fatal("a held payment was granted on a retry")
			}
			if got := h.Metrics.Counter(metricDead, fake()); got != 1 {
				t.Fatalf("%s = %d, want 1: a payment no one has fulfilled pages", metricDead, got)
			}
		}},
		{"a discounted payment above nothing is still granted in full, and is counted as an underpayment", func(t testing.TB, mk ProcessorFactory) {
			for i, minor := range []int64{500, 1} { // a coupon of half, and one cent
				h := NewHarness()
				p := mk(t, h)
				id := fmt.Sprintf("pay-%d", i)
				ev := h.PaymentSucceeded(fmt.Sprintf("evt_%d", i), id)
				ev.Amount = minor
				h.Persisted(ev)

				h.Tick(t, p)
				wantRow(t, h, ev.ID, RowProcessed)
				if h.Meter.Grants(id) != 1 || h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
					t.Fatalf("a payment of %d for a price of 1000: %d grants, balance %d, want one grant of %d (credits never come from the amount paid)",
						minor, h.Meter.Grants(id), h.Meter.Balance(h.Scope, md.GeneralPool), PackCredits)
				}
				if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", "granted")); got != 1 {
					t.Fatalf("a payment of %d for a price of 1000 was granted without being counted: %s{outcome=granted} = %d, want 1", minor, metricUnderpaid, got)
				}
				if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", "held")); got != 0 {
					t.Fatalf("a discounted payment above nothing was held: %s{outcome=held} = %d", metricUnderpaid, got)
				}
			}
		}},
		{"a payment of the price, or of more because tax was added, is granted in full and raises nothing", func(t testing.TB, mk ProcessorFactory) {
			for i, minor := range []int64{1000, 1080} { // the price, and the price plus 8% tax: a gross amount is never short of the price
				h := NewHarness()
				p := mk(t, h)
				id := fmt.Sprintf("pay-%d", i)
				ev := h.PaymentSucceeded(fmt.Sprintf("evt_%d", i), id)
				ev.Amount = minor
				h.Persisted(ev)

				h.Tick(t, p)
				wantRow(t, h, ev.ID, RowProcessed)
				if h.Meter.Grants(id) != 1 || h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
					t.Fatalf("a payment of %d for a price of 1000: %d grants, balance %d, want one grant of %d", minor, h.Meter.Grants(id), h.Meter.Balance(h.Scope, md.GeneralPool), PackCredits)
				}
				for _, outcome := range []string{"held", "granted"} {
					if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", outcome)); got != 0 {
						t.Fatalf("a full payment counted as an underpayment (%s=%s: %d)", metricUnderpaid, outcome, got)
					}
				}
			}
		}},
		{"a payment in a currency the plan has no price in is not compared with anything", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			ev := paid(h, 0)
			ev.ZeroPaid = true
			ev.Currency = "EUR" // the pack is priced in USD only: there is nothing to hold the amount against
			h.Persisted(ev)

			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 {
				t.Fatalf("a payment with no price to compare against was held: %d grants", h.Meter.Grants("pay-1"))
			}
			for _, outcome := range []string{"held", "granted"} {
				if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", outcome)); got != 0 {
					t.Fatalf("a payment with no price to compare against was counted as an underpayment (%s=%s: %d)", metricUnderpaid, outcome, got)
				}
			}
		}},
	}
}

// ProcessorUnderpayment is the specification of what a payment buys when it settled for less than the plan costs.
func ProcessorUnderpayment(t *testing.T, mk ProcessorFactory) {
	t.Helper()
	runCases(t, UnderpaymentCases(), mk)
}

// PolicyFactory builds the processor under test over a harness with an underpayment policy: "hold_zero" (the default),
// "hold" or "grant".
type PolicyFactory func(t testing.TB, h *Harness, policy string) ports.WebhookProcessor

// PolicyCase is one subtest of the underpayment-policy suite.
type PolicyCase struct {
	Name string
	Run  func(t testing.TB, mk PolicyFactory)
}

// UnderpaymentPolicyCases are the subtests of the underpayment-policy suite. The default holds a payment of nothing and honours
// a discount (UnderpaymentCases); a deployment that never runs a coupon asks for "hold" and has every payment below the price
// held, and one that uses a provider's trial as free credits asks for "grant" and has nothing held — each counted either way.
func UnderpaymentPolicyCases() []PolicyCase {
	paid := func(h *Harness, minor int64, zero bool) FakeEvent {
		ev := h.PaymentSucceeded("evt_1", "pay-1")
		ev.Amount, ev.ZeroPaid = minor, zero
		return ev
	}
	return []PolicyCase{
		{"under hold, a discount is held like a payment of nothing, and a price put right grants it once", func(t testing.TB, mk PolicyFactory) {
			h := NewHarness()
			p := mk(t, h, "hold")
			h.Persisted(paid(h, 500, false)) // the provider charges $5.00 for a pack the catalogue says costs $10.00

			h.Tick(t, p)
			if n := h.Meter.Grants("pay-1"); n != 0 {
				t.Fatalf("a payment of 500 against a price of 1000 was granted under hold (%d grants)", n)
			}
			wantRow(t, h, "evt_1", RowFailed)
			if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", "held")); got != 1 {
				t.Fatalf("%s{outcome=held} = %d, want 1", metricUnderpaid, got)
			}

			w := WorldPlans()[0] // an operator corrects the catalogue to what the provider charges
			w.Plan.Prices = map[string]domain.Money{"USD": {MinorUnits: 500, Currency: "USD"}}
			h.Catalog.Add(w.Plan, w.Account, w.Currency, w.ProviderPriceID)

			h.Rounds(t, p, 1, time.Hour)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 || h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
				t.Fatalf("the corrected payment was not granted once: %d grants, balance %d", h.Meter.Grants("pay-1"), h.Meter.Balance(h.Scope, md.GeneralPool))
			}
		}},
		{"under hold, a payment of the price is granted and raises nothing", func(t testing.TB, mk PolicyFactory) {
			h := NewHarness()
			p := mk(t, h, "hold")
			h.Persisted(paid(h, 1000, false))

			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 {
				t.Fatalf("a payment of the price was held under hold: %d grants", h.Meter.Grants("pay-1"))
			}
			for _, outcome := range []string{"held", "granted"} {
				if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", outcome)); got != 0 {
					t.Fatalf("a full payment counted as an underpayment (%s=%s: %d)", metricUnderpaid, outcome, got)
				}
			}
		}},
		{"under grant, a payment of nothing is granted in full and counted, never held", func(t testing.TB, mk PolicyFactory) {
			h := NewHarness()
			p := mk(t, h, "grant")
			h.Persisted(paid(h, 0, true)) // a provider trial, used on purpose as free credits

			h.Tick(t, p)
			wantRow(t, h, "evt_1", RowProcessed)
			if h.Meter.Grants("pay-1") != 1 || h.Meter.Balance(h.Scope, md.GeneralPool) != PackCredits {
				t.Fatalf("a payment of nothing was not granted under grant: %d grants, balance %d", h.Meter.Grants("pay-1"), h.Meter.Balance(h.Scope, md.GeneralPool))
			}
			if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", "granted")); got != 1 {
				t.Fatalf("a payment of nothing granted under grant was not counted: %s{outcome=granted} = %d, want 1", metricUnderpaid, got)
			}
			if got := h.Metrics.Counter(metricUnderpaid, fake("outcome", "held")); got != 0 {
				t.Fatalf("a payment was held under grant: %s{outcome=held} = %d", metricUnderpaid, got)
			}
		}},
	}
}

// ProcessorUnderpaymentPolicies is the specification of the three underpayment policies.
func ProcessorUnderpaymentPolicies(t *testing.T, mk PolicyFactory) {
	t.Helper()
	for _, c := range UnderpaymentPolicyCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, mk)
		})
	}
}

// ProcessorGrants is the specification of fulfilment from the inbox (invariants 2, 3, 4, 5, 19; FR-019…022,
// SC-016): a crash at any point after the acknowledgement delays a grant and never loses or duplicates it,
// credits are created only through Meter.Grant from the verified object plus the deployment's own records, and
// traffic that is not this deployment's is ignored rather than guessed at.
func ProcessorGrants(t *testing.T, mk ProcessorFactory) {
	t.Helper()
	runCases(t, GrantCases(), mk)
}
