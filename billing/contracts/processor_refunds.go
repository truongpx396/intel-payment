package contracts

import (
	"fmt"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// negativeGrants is the clawbacks that took effect, in order.
func negativeGrants(h *Harness) []md.Grant {
	var out []md.Grant
	for _, g := range h.Meter.Applied() {
		if g.Amount < 0 {
			out = append(out, g)
		}
	}
	return out
}

func sumNegative(h *Harness) md.Credits {
	var n md.Credits
	for _, g := range negativeGrants(h) {
		n += g.Amount
	}
	return n
}

// paymentEvents is what billing announced for the scope's payments, by event name.
func paymentEvents(h *Harness, scope md.Scope) []string {
	var out []string
	for _, m := range h.Bus.On("billing.payment." + scope.SubjectToken()) {
		if e, ok := m["event"].(string); ok {
			out = append(out, e)
		}
	}
	return out
}

func contains(xs []string, want string) bool { return count(xs, want) > 0 }

func count(xs []string, want string) int {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n
}

// RefundCases are the subtests of the refunds half of ProcessorRefunds (invariants 8, 16, 20; FR-023, SC-017).
func RefundCases() []ProcessorCase {
	const (
		paid    = md.Credits(10_000_000) // $10.00 bought 10,000,000 credits
		perCent = md.Credits(10_000)
	)
	return []ProcessorCase{
		{"partial refunds revoke exactly, and a replayed report revokes nothing (16)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-4", 1000, paid)
			h.Persisted(h.Refund("evt_r1", "pay-4", "re_1", 333))
			h.Clock.Advance(time.Second)
			h.Persisted(h.Refund("evt_r2", "pay-4", "re_2", 666)) // cumulative
			h.Clock.Advance(time.Second)
			h.Persisted(h.Refund("evt_r3", "pay-4", "re_2", 666)) // a replayed report
			h.Rounds(t, p, 3, time.Minute)

			for _, id := range []string{"evt_r1", "evt_r2", "evt_r3"} {
				wantRow(t, h, id, RowProcessed)
			}
			pay := h.Payments.Row("pay-4")
			if pay.CreditsRevoked != 666*perCent || pay.RefundedMinor != 666 || pay.Status != domain.PartiallyRefunded {
				t.Fatalf("revoked %d of 10,000,000 (refunded %d, %s), want exactly 6,660,000 — what one refund of 666 revokes", pay.CreditsRevoked, pay.RefundedMinor, pay.Status)
			}
			if got := sumNegative(h); got != -666*perCent {
				t.Fatalf("the books were charged back %d, want 6,660,000", got)
			}
			if n := len(negativeGrants(h)); n != 2 {
				t.Fatalf("%d clawbacks for two distinct refunds, want 2: a replay must not claw back again", n)
			}
			if h.Meter.NetGranted("pay-4") != paid-666*perCent {
				t.Fatalf("net granted for the payment = %d", h.Meter.NetGranted("pay-4"))
			}
			if n := count(paymentEvents(h, h.Scope), "refunded"); n != 2 {
				t.Fatalf("the refunds were announced %d times, want 2: a replayed report changes nothing, so it announces nothing (%v)", n, paymentEvents(h, h.Scope))
			}
		}},
		{"a refund naming another customer's payment is refused, not applied to it", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-4", 1000, paid) // cus_1's
			fe := h.Refund("evt_r1", "pay-4", "re_1", 500)
			fe.Customer = "cus_2" // another customer of the same account names it
			h.Persisted(fe)
			h.Tick(t, p)
			wantRow(t, h, "evt_r1", RowFailed)
			if len(negativeGrants(h)) != 0 || h.Payments.Row("pay-4").CreditsRevoked != 0 {
				t.Fatal("credits were taken from one scope on the word of an event about another")
			}
		}},
		{"any partition of a total into reports revokes what one refund of the total does", func(t testing.TB, mk ProcessorFactory) {
			// ceil(10,000,000 × 777 / 1000) = 7,770,000, however the cumulative reports leading to 777 arrive.
			for _, partition := range [][]int64{{777}, {1, 777}, {100, 300, 777}, {333, 666, 777}, {776, 777}, {7, 70, 700, 777}} {
				h := NewHarness()
				p := mk(t, h)
				h.GivenSucceededPayment("pay-p", 1000, paid)
				for i, cum := range partition {
					h.Clock.Advance(time.Second)
					h.Persisted(h.Refund(fmt.Sprintf("evt_%d", i), "pay-p", fmt.Sprintf("re_%d", i), cum))
				}
				h.Rounds(t, p, len(partition)+1, time.Minute)
				if got := h.Payments.Row("pay-p").CreditsRevoked; got != 777*perCent {
					t.Fatalf("reports %v revoked %d, want 7,770,000", partition, got)
				}
				if got := sumNegative(h); got != -777*perCent {
					t.Fatalf("reports %v charged the books back %d, want 7,770,000", partition, got)
				}
			}
		}},
		{"a refund appends and never mutates (8), and names the payment it reverses", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			before := h.GivenSucceededPayment("pay-4", 1000, paid)
			h.Persisted(h.Refund("evt_r1", "pay-4", "re_1", 250))
			h.Tick(t, p)

			after := h.Payments.Row("pay-4")
			if after.Amount != before.Amount || after.CreditsGranted != before.CreditsGranted || after.IdemKey != before.IdemKey || after.Scope != before.Scope {
				t.Fatalf("the original payment was mutated: %+v → %+v", before, after)
			}
			grants := h.Meter.Applied()
			if len(grants) != 2 || grants[0].Amount != paid || grants[0].IdemKey != "pay-4" {
				t.Fatalf("the purchase grant must stay as it was, with the refund beside it: %+v", grants)
			}
			g := grants[1]
			if g.Amount != -250*perCent || g.IdemKey != "refund:re_1" || g.Reason != "refund" || g.SourceKey != "pay-4" || g.Pool != md.GeneralPool || g.Scope != h.Scope {
				t.Fatalf("clawback = %+v: a refund is a negative grant keyed on the provider's refund, naming the payment it reverses (source_key)", g)
			}
		}},
		{"a refund never claws back what expiry already took (20)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-7", 1000, paid)
			h.GivenLapsed("pay-7", paid, paid) // its lot expired unspent
			h.Persisted(h.Refund("evt_r1", "pay-7", "re_1", 1000))
			h.Tick(t, p)

			wantRow(t, h, "evt_r1", RowProcessed)
			pay := h.Payments.Row("pay-7")
			if pay.CreditsRevoked != 0 || len(negativeGrants(h)) != 0 {
				t.Fatalf("revoked %d credits the customer had already lost to expiry (%d clawbacks): taken twice, out of whatever other purchase still holds credits", pay.CreditsRevoked, len(negativeGrants(h)))
			}
			if pay.Status != domain.Refunded || pay.RefundedMinor != 1000 {
				t.Fatalf("the fiat side of the refund is still recorded: %+v", pay)
			}
		}},
		{"a refund after partial expiry revokes only the rest, across reports", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-8", 1000, paid)
			h.GivenLapsed("pay-8", paid, 4_000_000) // 4,000,000 lapsed; 6,000,000 can still be taken
			h.Persisted(h.Refund("evt_r1", "pay-8", "re_1", 500))
			h.Clock.Advance(time.Second)
			h.Persisted(h.Refund("evt_r2", "pay-8", "re_2", 1000))
			h.Rounds(t, p, 2, time.Minute)

			if got := sumNegative(h); got != -6_000_000 {
				t.Fatalf("clawed back %d, want 6,000,000: granted 10M less the 4M expiry took", got)
			}
			if h.Payments.Row("pay-8").CreditsRevoked != 6_000_000 {
				t.Fatalf("revoked = %d", h.Payments.Row("pay-8").CreditsRevoked)
			}
			for _, g := range negativeGrants(h) {
				if g.SourceKey != "pay-8" {
					t.Fatalf("a clawback must name the payment it reverses: %+v", g)
				}
			}
		}},
		{"a report older than one already applied revokes nothing and lowers nothing", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-4", 1000, paid)
			h.Persisted(h.Refund("evt_r2", "pay-4", "re_2", 666))
			h.Clock.Advance(time.Second)
			h.Persisted(h.Refund("evt_r1", "pay-4", "re_1", 333)) // the provider delivered it late
			h.Rounds(t, p, 2, time.Minute)
			pay := h.Payments.Row("pay-4")
			if pay.RefundedMinor != 666 || pay.CreditsRevoked != 666*perCent || len(negativeGrants(h)) != 1 {
				t.Fatalf("a late, lower cumulative report changed the books: refunded %d, revoked %d, %d clawbacks", pay.RefundedMinor, pay.CreditsRevoked, len(negativeGrants(h)))
			}
		}},
		{"a refund that arrives before its payment is retried until the payment is recorded", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.Refund("evt_r1", "pay-9", "re_1", 500))
			h.Tick(t, p)
			wantRow(t, h, "evt_r1", RowFailed)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("credits were revoked for a payment this deployment has not recorded")
			}
			h.Clock.Advance(-time.Hour) // the payment event was created before the refund
			h.Persisted(h.PaymentSucceeded("evt_s", "pay-9"))
			h.Clock.Advance(time.Hour)
			h.Rounds(t, p, 3, time.Hour)
			wantRow(t, h, "evt_s", RowProcessed)
			wantRow(t, h, "evt_r1", RowProcessed)
			if got := h.Payments.Row("pay-9").CreditsRevoked; got != 5_000_000 {
				t.Fatalf("revoked %d after the payment was recorded, want 5,000,000 (half of a $10.00 pack)", got)
			}
			if h.Meter.NetGranted("pay-9") != 5_000_000 {
				t.Fatalf("net granted = %d", h.Meter.NetGranted("pay-9"))
			}
		}},
		{"a refund that fiat cannot explain fails loudly and revokes nothing", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-4", 1000, paid)
			h.Persisted(h.Refund("evt_ok", "pay-4", "re_1", 500)) // a refund fiat can explain is applied…
			h.Clock.Advance(time.Second)
			h.Persisted(h.Refund("evt_bad", "pay-4", "re_2", 1001)) // …and one for more than was paid is not
			h.Rounds(t, p, 7, time.Hour)
			wantRow(t, h, "evt_ok", RowProcessed)
			wantRow(t, h, "evt_bad", RowDead)
			if got := h.Payments.Row("pay-4").CreditsRevoked; got != 5_000_000 || sumNegative(h) != -5_000_000 {
				t.Fatalf("revoked %d, clawed back %d: only the refund fiat can explain may take credits back", got, sumNegative(h))
			}
			if got := h.Metrics.Counter(metricDead, fake()); got != 1 {
				t.Fatalf("%s = %d: an impossible refund must page", metricDead, got)
			}
		}},
		{"a clawback whose commit failed is not taken twice on the retry", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-4", 1000, paid)
			h.Persisted(h.Refund("evt_r1", "pay-4", "re_1", 500))
			h.Payments.FailCommit(1) // the grant is made, then the payment row cannot be written

			h.Tick(t, p)
			wantRow(t, h, "evt_r1", RowFailed)
			if h.Payments.Row("pay-4").CreditsRevoked != 0 {
				t.Fatal("the payment row moved though its commit failed")
			}
			h.Rounds(t, p, 1, time.Hour)
			wantRow(t, h, "evt_r1", RowProcessed)
			if got := sumNegative(h); got != -5_000_000 {
				t.Fatalf("clawed back %d in all, want 5,000,000 once: the retry recomputes the same clawback under the same key", got)
			}
			if h.Payments.Row("pay-4").CreditsRevoked != 5_000_000 {
				t.Fatalf("revoked = %d", h.Payments.Row("pay-4").CreditsRevoked)
			}
		}},
	}
}

// DisputeCases are the subtests of the disputes half of ProcessorRefunds (invariants 17, 20; FR-023, SC-017).
func DisputeCases() []ProcessorCase {
	const paid = md.Credits(10_000_000)
	return []ProcessorCase{
		{"a won dispute reverses its chargeback, each phase keyed on the dispute id (17)", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-5", 1000, paid)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Clock.Advance(time.Second)
			h.Persisted(h.Dispute("evt_d2", domain.DisputeFundsReinstated, "pay-5", "dp_1"))
			h.Rounds(t, p, 2, time.Minute)

			if net := h.Meter.NetGranted("pay-5"); net != paid {
				t.Fatalf("net after a won dispute = %d, want 10,000,000", net)
			}
			if h.Meter.Grants("dispute:dp_1:withdrawn") != 1 || h.Meter.Grants("dispute:dp_1:reinstated") != 1 {
				t.Fatalf("each phase is one grant under its own key")
			}
			grants := h.Meter.Applied()
			w, r := grants[1], grants[2]
			if w.Amount != -paid || w.Reason != "chargeback" || w.SourceKey != "pay-5" || r.Amount != paid || r.Reason != "chargeback_reversal" {
				t.Fatalf("withdrawal %+v, reinstatement %+v", w, r)
			}
			pay := h.Payments.Row("pay-5")
			if pay.Status != domain.Succeeded || pay.DisputeStatus != "won" || pay.CreditsRevoked != 0 || pay.DisputeID != "dp_1" {
				t.Fatalf("payment = %+v: a won dispute restores what the payment was", pay)
			}
			if ev := paymentEvents(h, h.Scope); !contains(ev, "charged_back") || !contains(ev, "succeeded") {
				t.Fatalf("billing.payment.<tag> announced %v", ev)
			}
		}},
		{"a replayed dispute event changes nothing", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-5", 1000, paid)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Clock.Advance(time.Second)
			h.Persisted(h.Dispute("evt_d2", domain.DisputeFundsWithdrawn, "pay-5", "dp_1")) // the provider sent it again
			h.Rounds(t, p, 2, time.Minute)
			if n := len(negativeGrants(h)); n != 1 || h.Meter.NetGranted("pay-5") != 0 {
				t.Fatalf("%d chargebacks, net %d: one chargeback, once", n, h.Meter.NetGranted("pay-5"))
			}
			if n := count(paymentEvents(h, h.Scope), "charged_back"); n != 1 {
				t.Fatalf("the chargeback was announced %d times, want 1: a replay changes nothing, so it announces nothing", n)
			}
		}},
		{"a chargeback takes what a refund and expiry left", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			pay := h.GivenSucceededPayment("pay-5", 1000, paid)
			pay.RefundedMinor, pay.CreditsRevoked, pay.Status = 333, 3_330_000, domain.PartiallyRefunded
			if err := h.Payments.Upsert(Ctx(), pay); err != nil {
				t.Fatal(err)
			}
			h.GivenLapsed("pay-5", paid, 2_000_000)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Tick(t, p)
			if got := sumNegative(h); got != -4_670_000 {
				t.Fatalf("chargeback took %d, want 4,670,000: 10M less 3.33M refunded less 2M expired", got)
			}
			// …and a win returns exactly that, to the status the refunds alone give.
			h.Clock.Advance(time.Second)
			h.Persisted(h.Dispute("evt_d2", domain.DisputeFundsReinstated, "pay-5", "dp_1"))
			h.Rounds(t, p, 1, time.Minute)
			got := h.Payments.Row("pay-5")
			if got.CreditsRevoked != 3_330_000 || got.Status != domain.PartiallyRefunded || got.DisputeStatus != "won" {
				t.Fatalf("payment after the win = %+v", got)
			}
			// The fixture put the earlier refund on the payment row, not in the fake meter, so the chargeback and its
			// reversal are all the meter has seen of this payment besides the purchase: they cancel.
			if sumNegative(h) != -4_670_000 || h.Meter.NetGranted("pay-5") != paid {
				t.Fatalf("net after the win = %d, want the purchase back: the reversal returns exactly the 4,670,000 taken", h.Meter.NetGranted("pay-5"))
			}
		}},
		{"opening a dispute moves no credits", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-5", 1000, paid)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeOpened, "pay-5", "dp_1"))
			h.Tick(t, p)
			wantRow(t, h, "evt_d1", RowProcessed)
			pay := h.Payments.Row("pay-5")
			if pay.Status != domain.Disputed || pay.DisputeStatus != "open" || pay.DisputeID != "dp_1" || pay.CreditsRevoked != 0 {
				t.Fatalf("payment = %+v", pay)
			}
			if len(negativeGrants(h)) != 0 || h.Meter.NetGranted("pay-5") != paid {
				t.Fatal("funds have not moved: neither have the credits")
			}
			if !contains(paymentEvents(h, h.Scope), "disputed") {
				t.Fatalf("announced %v", paymentEvents(h, h.Scope))
			}
		}},
		{"a lost dispute keeps the credits taken", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-5", 1000, paid)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Clock.Advance(time.Second)
			h.Persisted(h.DisputeClosed("evt_d2", "pay-5", "dp_1", "lost"))
			h.Rounds(t, p, 2, time.Minute)
			pay := h.Payments.Row("pay-5")
			if pay.Status != domain.ChargedBack || pay.DisputeStatus != "lost" || h.Meter.NetGranted("pay-5") != 0 {
				t.Fatalf("payment = %+v, net %d", pay, h.Meter.NetGranted("pay-5"))
			}
		}},
		{"closing a dispute as won is the reinstatement", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-5", 1000, paid)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Clock.Advance(time.Second)
			h.Persisted(h.DisputeClosed("evt_d2", "pay-5", "dp_1", "won"))
			h.Rounds(t, p, 2, time.Minute)
			if h.Meter.NetGranted("pay-5") != paid || h.Meter.Grants("dispute:dp_1:reinstated") != 1 {
				t.Fatalf("net %d: dispute_closed{won} must reverse the chargeback under the reinstated key", h.Meter.NetGranted("pay-5"))
			}
		}},
		{"dispute events delivered out of order converge", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.GivenSucceededPayment("pay-5", 1000, paid)
			h.Persisted(h.Dispute("evt_d2", domain.DisputeFundsReinstated, "pay-5", "dp_1")) // the win is processed first
			h.Clock.Advance(time.Second)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Rounds(t, p, 2, time.Minute)
			wantRow(t, h, "evt_d1", RowProcessed)
			wantRow(t, h, "evt_d2", RowProcessed)
			if h.Meter.NetGranted("pay-5") != paid || len(negativeGrants(h)) != 0 {
				t.Fatalf("net %d, %d chargebacks: a won dispute must not take credits in either order", h.Meter.NetGranted("pay-5"), len(negativeGrants(h)))
			}
			if pay := h.Payments.Row("pay-5"); pay.DisputeStatus != "won" || pay.Status != domain.Succeeded {
				t.Fatalf("payment = %+v: the verdict is recorded even when it arrives first", pay)
			}
		}},
		{"a dispute for a payment not yet recorded is retried, not dropped", func(t testing.TB, mk ProcessorFactory) {
			h := NewHarness()
			p := mk(t, h)
			h.Persisted(h.Dispute("evt_d1", domain.DisputeFundsWithdrawn, "pay-5", "dp_1"))
			h.Tick(t, p)
			wantRow(t, h, "evt_d1", RowFailed)
			if h.Meter.GrantCalls() != 0 {
				t.Fatal("credits were taken for a payment this deployment has not recorded")
			}
			h.GivenSucceededPayment("pay-5", 1000, paid) // the payment is recorded after all
			h.Rounds(t, p, 1, time.Hour)
			wantRow(t, h, "evt_d1", RowProcessed)
			if h.Meter.NetGranted("pay-5") != 0 {
				t.Fatalf("net %d: the chargeback must be applied once the payment is known", h.Meter.NetGranted("pay-5"))
			}
		}},
	}
}

// ProcessorRefunds is the specification of refunds and disputes (invariants 8, 16, 17, 20; FR-016a, FR-023,
// SC-017): a refund appends and never mutates, partial refunds revoke exactly and several equal one, a refund
// never claws back what expiry already took, and a chargeback is reversible when the dispute is won.
func ProcessorRefunds(t *testing.T, mk ProcessorFactory) {
	t.Helper()
	runCases(t, append(RefundCases(), DisputeCases()...), mk)
}
