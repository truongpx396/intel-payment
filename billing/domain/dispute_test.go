package domain_test

import (
	"math"
	"testing"

	domain "github.com/truongpx396/intel-payment/billing/domain"
)

func TestADisputeKeyNamesTheDisputeAndThePhase(t *testing.T) {
	t.Parallel()
	if got := domain.DisputeKey("dp_1", domain.DisputeWithdrawn); got != "dispute:dp_1:withdrawn" {
		t.Fatalf("key = %q", got)
	}
	if got := domain.DisputeKey("dp_1", domain.DisputeReinstated); got != "dispute:dp_1:reinstated" {
		t.Fatalf("key = %q", got)
	}
	seen := map[string]string{}
	for _, id := range []string{"dp_1", "dp_2", "dp_1:withdrawn", "dp_1:reinstated", ""} {
		for _, ph := range []domain.DisputePhase{domain.DisputeWithdrawn, domain.DisputeReinstated} {
			k := domain.DisputeKey(id, ph)
			if prev, dup := seen[k]; dup {
				t.Fatalf("key %q is both (%s) and (%s, %s): two phases would share one idempotency guard", k, prev, id, ph)
			}
			seen[k] = id + "/" + string(ph)
		}
	}
}

func TestFundsWithdrawnClawsBackWhatIsLeftOfTheGrant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		revoked domain.Credits
		expired domain.Credits
		want    domain.Credits // the (negative) grant
	}{
		{"the whole grant", 0, 0, -10_000_000},
		{"after a partial refund took part", 3_330_000, 0, -6_670_000},
		{"what expiry already took is not taken again", 0, 4_000_000, -6_000_000},
		{"refund and expiry together", 3_330_000, 2_000_000, -4_670_000},
		{"nothing left to take", 10_000_000, 0, 0},
		{"it all lapsed", 0, 10_000_000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := payment(10_000_000, 1000)
			p.CreditsRevoked = c.revoked
			plan, err := p.WithdrawFunds("dp_1", c.expired)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Delta != c.want {
				t.Fatalf("grant = %d, want %d", plan.Delta, c.want)
			}
			if c.want != 0 {
				if plan.IdemKey != "dispute:dp_1:withdrawn" || plan.Reason != "chargeback" {
					t.Fatalf("key/reason = %q/%q", plan.IdemKey, plan.Reason)
				}
				if plan.Payment.Status != domain.ChargedBack || plan.Payment.DisputeID != "dp_1" {
					t.Fatalf("payment = %+v", plan.Payment)
				}
				if plan.Payment.CreditsRevoked != c.revoked-c.want {
					t.Fatalf("revoked after = %d, want %d", plan.Payment.CreditsRevoked, c.revoked-c.want)
				}
			}
		})
	}
}

func TestAWonDisputeReturnsExactlyWhatTheChargebackTook(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		revoked domain.Credits
		expired domain.Credits
	}{
		{"untouched", 0, 0},
		{"after a partial refund", 3_330_000, 0},
		{"after expiry", 0, 4_000_000},
		{"after both", 3_330_000, 1_000_000},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := payment(10_000_000, 1000)
			p.CreditsRevoked = c.revoked
			if c.revoked > 0 {
				p.RefundedMinor, p.Status = 333, domain.PartiallyRefunded
			}
			taken, err := p.WithdrawFunds("dp_1", c.expired)
			if err != nil {
				t.Fatal(err)
			}
			given, err := taken.Payment.ReinstateFunds("dp_1")
			if err != nil {
				t.Fatal(err)
			}
			if taken.Delta+given.Delta != 0 {
				t.Fatalf("withdrawn %d then reinstated %d: net %d — a won dispute must give back exactly what was taken", taken.Delta, given.Delta, taken.Delta+given.Delta)
			}
			if given.IdemKey != "dispute:dp_1:reinstated" || given.Reason != "chargeback_reversal" {
				t.Fatalf("key/reason = %q/%q", given.IdemKey, given.Reason)
			}
			after := given.Payment
			if after.CreditsRevoked != c.revoked || after.CreditsGranted != 10_000_000 {
				t.Fatalf("the payment's credits must be back where they were: revoked %d, want %d", after.CreditsRevoked, c.revoked)
			}
			if after.DisputeStatus != "won" {
				t.Fatalf("dispute status = %q, want won", after.DisputeStatus)
			}
			wantStatus := domain.Succeeded
			if c.revoked > 0 {
				wantStatus = domain.PartiallyRefunded
			}
			if after.Status != wantStatus {
				t.Fatalf("status = %s, want %s — a won dispute restores what the payment was", after.Status, wantStatus)
			}
		})
	}
}

func FuzzAWonDisputeNetsToZero(f *testing.F) {
	f.Add(uint64(10_000_000), uint64(3_330_000), uint64(0))
	f.Add(uint64(math.MaxInt64), uint64(1), uint64(1))
	f.Add(uint64(5), uint64(5), uint64(0))
	f.Fuzz(func(t *testing.T, g, r, e uint64) {
		granted := domain.Credits(g >> 1)
		revoked := domain.Credits(r>>1) % (granted + 1)
		expired := domain.Credits(e>>1) % (granted - revoked + 1)
		p := payment(granted, 1000)
		p.CreditsRevoked = revoked
		taken, err := p.WithdrawFunds("dp", expired)
		if err != nil {
			t.Fatal(err)
		}
		if taken.Delta > 0 || taken.Delta < -granted {
			t.Fatalf("a chargeback grant of %d is outside [−granted, 0]", taken.Delta)
		}
		given, err := taken.Payment.ReinstateFunds("dp")
		if err != nil {
			t.Fatal(err)
		}
		if taken.Delta+given.Delta != 0 || given.Payment.CreditsRevoked != revoked {
			t.Fatalf("net %d, revoked %d→%d", taken.Delta+given.Delta, revoked, given.Payment.CreditsRevoked)
		}
	})
}

func TestDisputeEventsAreReplaySafe(t *testing.T) {
	t.Parallel()
	taken, err := payment(10_000_000, 1000).WithdrawFunds("dp_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := taken.Payment.WithdrawFunds("dp_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Delta != 0 || again.Payment != taken.Payment {
		t.Fatalf("a replayed withdrawal must change nothing and grant nothing: %+v", again)
	}
	given, _ := taken.Payment.ReinstateFunds("dp_1")
	again2, err := given.Payment.ReinstateFunds("dp_1")
	if err != nil {
		t.Fatal(err)
	}
	if again2.Delta != 0 || again2.Payment != given.Payment {
		t.Fatalf("a replayed reinstatement must change nothing and grant nothing: %+v", again2)
	}
}

func TestAReinstatementWithNothingWithdrawnGrantsNothing(t *testing.T) {
	t.Parallel()
	plan, err := payment(10_000_000, 1000).ReinstateFunds("dp_1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Delta != 0 {
		t.Fatalf("granted %d back for a withdrawal that was never applied — credits from nothing", plan.Delta)
	}
	if plan.Payment.DisputeID != "dp_1" || plan.Payment.DisputeStatus != "won" {
		t.Fatalf("the outcome is still recorded: %+v", plan.Payment)
	}
}

func TestEventsOfOneDisputeConvergeInEitherOrder(t *testing.T) {
	t.Parallel()
	// Withdrawn then reinstated, and reinstated then withdrawn, leave the customer with the credits.
	p := payment(10_000_000, 1000)
	w, _ := p.WithdrawFunds("dp_1", 0)
	r1, _ := w.Payment.ReinstateFunds("dp_1")

	r, _ := p.ReinstateFunds("dp_1")
	w2, err := r.Payment.WithdrawFunds("dp_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if w.Delta+r1.Delta != 0 || r.Delta+w2.Delta != 0 {
		t.Fatalf("in order %d+%d, reversed %d+%d: a won dispute must not take credits in either order", w.Delta, r1.Delta, r.Delta, w2.Delta)
	}
	if r1.Payment.CreditsRevoked != 0 || w2.Payment.CreditsRevoked != 0 {
		t.Fatalf("revoked after: %d / %d", r1.Payment.CreditsRevoked, w2.Payment.CreditsRevoked)
	}
}

func TestALostDisputeTakesNothingFurther(t *testing.T) {
	t.Parallel()
	taken, _ := payment(10_000_000, 1000).WithdrawFunds("dp_1", 0)
	lost, err := taken.Payment.CloseDispute("dp_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if lost.Delta != 0 || lost.Payment.DisputeStatus != "lost" || lost.Payment.Status != domain.ChargedBack {
		t.Fatalf("a lost dispute stays charged back and grants nothing: %+v", lost)
	}
	// A withdrawal that arrives after the verdict still takes the credits.
	late, err := payment(10_000_000, 1000).CloseDispute("dp_2", false)
	if err != nil {
		t.Fatal(err)
	}
	w, err := late.Payment.WithdrawFunds("dp_2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if w.Delta != -10_000_000 || w.Payment.DisputeStatus != "lost" {
		t.Fatalf("a lost dispute's late withdrawal must still claw back, and keep the verdict: %+v", w)
	}
}

func TestClosingAsWonIsTheReinstatement(t *testing.T) {
	t.Parallel()
	taken, _ := payment(10_000_000, 1000).WithdrawFunds("dp_1", 0)
	won, err := taken.Payment.CloseDispute("dp_1", true)
	if err != nil {
		t.Fatal(err)
	}
	if won.Delta != 10_000_000 || won.IdemKey != "dispute:dp_1:reinstated" || won.Payment.DisputeStatus != "won" {
		t.Fatalf("dispute_closed{won} must reverse the chargeback under the reinstated key: %+v", won)
	}
}

func TestOpeningADisputeMovesNoCredits(t *testing.T) {
	t.Parallel()
	opened := payment(10_000_000, 1000).OpenDispute("dp_1")
	if opened.Status != domain.Disputed || opened.DisputeID != "dp_1" || opened.DisputeStatus != "open" {
		t.Fatalf("opened = %+v", opened)
	}
	if opened.CreditsRevoked != 0 {
		t.Fatalf("funds have not moved: revoked %d", opened.CreditsRevoked)
	}
	// An opening that arrives after the money already moved must not un-charge the payment back.
	taken, _ := payment(10_000_000, 1000).WithdrawFunds("dp_1", 0)
	late := taken.Payment.OpenDispute("dp_1")
	if late.Status != domain.ChargedBack || late.CreditsRevoked != 10_000_000 {
		t.Fatalf("a late dispute_opened regressed the payment: %+v", late)
	}
}
