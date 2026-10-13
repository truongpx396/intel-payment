package domain_test

import (
	"errors"
	"math"
	"math/big"
	"sort"
	"testing"

	domain "github.com/truongpx396/intel-payment/billing/domain"
	mdomain "github.com/truongpx396/intel-payment/metering/domain"
)

// referenceRevoke is min(granted, ceil(granted × refunded / amount)) in arbitrary precision: the independent
// computation the production code (128-bit integer arithmetic) is held to (payments invariant 16, SC-017).
func referenceRevoke(granted, amount, refunded int64) int64 {
	if amount == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(granted), big.NewInt(refunded))
	q, r := new(big.Int).QuoRem(n, big.NewInt(amount), new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if q.Cmp(big.NewInt(granted)) > 0 {
		q = big.NewInt(granted)
	}
	return q.Int64()
}

func usd(minor int64) domain.Money { return domain.Money{MinorUnits: minor, Currency: "USD"} }

// payment is a succeeded payment of `amount` cents that granted `granted` credits.
func payment(granted domain.Credits, amount int64) domain.Payment {
	return domain.Payment{
		Scope:             domain.Scope{Realm: "default", Kind: "workspace", ID: "w1"},
		Account:           "stripe-live",
		ProviderPaymentID: "pay-1",
		Kind:              "one_time",
		Amount:            usd(amount),
		CreditsGranted:    granted,
		Pool:              mdomain.GeneralPool,
		Status:            domain.Succeeded,
		IdemKey:           "pay-1",
	}
}

func TestRevokeTotalIsTheCumulativeFormulaRoundedUp(t *testing.T) {
	t.Parallel()
	// The contract's worked example: $10.00 bought 10,000,000 credits.
	cases := []struct {
		name             string
		granted          domain.Credits
		amount, refunded int64
		want             domain.Credits
	}{
		{"nothing refunded", 10_000_000, 1000, 0, 0},
		{"a third", 10_000_000, 1000, 333, 3_330_000},
		{"two thirds", 10_000_000, 1000, 666, 6_660_000},
		{"all of it", 10_000_000, 1000, 1000, 10_000_000},
		{"a fraction rounds UP, never under-revokes", 10, 3, 1, 4},
		{"one cent of a large grant", 1, 1000, 1, 1},
		{"a free payment refunds nothing", 5_000, 0, 0, 0},
		{"nothing was granted", 0, 1000, 1000, 0},
		{"the largest grant in the largest payment", math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{"half of the largest grant, rounded up exactly", math.MaxInt64, 2, 1, 1 << 62},
		{"one unit short of all", math.MaxInt64, math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64 - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.RevokeTotal(c.granted, c.amount, c.refunded)
			if err != nil || got != c.want {
				t.Fatalf("RevokeTotal(%d, %d, %d) = %d, %v; want %d", c.granted, c.amount, c.refunded, got, err, c.want)
			}
		})
	}
}

func TestRevokeTotalRefusesNumbersThatCannotBeRight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		granted          domain.Credits
		amount, refunded int64
	}{
		{"refunded more than was paid", 100, 1000, 1001},
		{"a negative refund", 100, 1000, -1},
		{"a negative amount", 100, -1000, 0},
		{"credits granted below zero", -1, 1000, 500},
		{"a refund of a free payment", 100, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.RevokeTotal(c.granted, c.amount, c.refunded)
			if !errors.Is(err, mdomain.ErrInvalid) {
				t.Fatalf("RevokeTotal(%d, %d, %d) = %d, %v; want ErrInvalid — a refund that fiat cannot explain fails loudly", c.granted, c.amount, c.refunded, got, err)
			}
			if got != 0 {
				t.Fatalf("a refused computation must return zero, got %d", got)
			}
		})
	}
}

func FuzzRevokeTotalAgainstBigArithmetic(f *testing.F) {
	f.Add(uint64(10_000_000), uint64(1000), uint64(333))
	f.Add(uint64(math.MaxInt64), uint64(math.MaxInt64), uint64(math.MaxInt64))
	f.Add(uint64(math.MaxInt64), uint64(2), uint64(1))
	f.Add(uint64(1), uint64(1000), uint64(1))
	f.Add(uint64(0), uint64(7), uint64(7))
	f.Fuzz(func(t *testing.T, g, a, r uint64) {
		granted := int64(g >> 1)
		amount := int64(a>>1) | 1 // > 0
		refunded := int64(r>>1) % (amount + 1)
		if amount == math.MaxInt64 { // amount+1 would wrap above; keep refunded in range
			refunded = int64(r >> 1)
		}
		got, err := domain.RevokeTotal(domain.Credits(granted), amount, refunded)
		if err != nil {
			t.Fatalf("RevokeTotal(%d, %d, %d) refused a valid refund: %v", granted, amount, refunded, err)
		}
		if want := referenceRevoke(granted, amount, refunded); int64(got) != want {
			t.Fatalf("RevokeTotal(%d, %d, %d) = %d, want %d (math/big)", granted, amount, refunded, got, want)
		}
		if got < 0 || int64(got) > granted {
			t.Fatalf("RevokeTotal(%d, %d, %d) = %d — outside [0, granted]", granted, amount, refunded, got)
		}
	})
}

// A refund report names the CUMULATIVE amount; applying it returns what to take back now.
func TestSeveralPartialRefundsRevokeWhatOneRefundOfTheTotalWould(t *testing.T) {
	t.Parallel()
	p := payment(10_000_000, 1000)
	var revokedNow domain.Credits
	for _, cumulative := range []int64{333, 666, 666 /* a replayed report */} {
		plan, err := p.ApplyRefund(usd(cumulative), 0)
		if err != nil {
			t.Fatalf("ApplyRefund(%d): %v", cumulative, err)
		}
		revokedNow += plan.Revoke
		p.RefundedMinor, p.CreditsRevoked, p.Status = plan.RefundedMinor, plan.RevokedTotal, plan.Status
	}
	if revokedNow != 6_660_000 || p.CreditsRevoked != 6_660_000 {
		t.Fatalf("three reports revoked %d (cumulative field %d), want 6,660,000 — what one refund of 666 revokes", revokedNow, p.CreditsRevoked)
	}
	if p.Amount.MinorUnits != 1000 || p.CreditsGranted != 10_000_000 {
		t.Fatalf("a refund must append, never mutate, the original payment: %+v", p)
	}
	if p.Status != domain.PartiallyRefunded {
		t.Fatalf("status = %s, want partially_refunded", p.Status)
	}
}

func FuzzAnyPartitionOfARefundRevokesTheSameAsOneRefund(f *testing.F) {
	f.Add(uint64(10_000_000), uint64(1000), uint64(333), uint64(666), uint64(1000))
	f.Add(uint64(7), uint64(3), uint64(1), uint64(1), uint64(2))
	f.Add(uint64(math.MaxInt64), uint64(math.MaxInt64), uint64(1), uint64(2), uint64(math.MaxInt64))
	f.Fuzz(func(t *testing.T, g, a, r1, r2, r3 uint64) {
		granted := domain.Credits(g >> 1)
		amount := int64(a>>1) | 1
		cum := []int64{int64(r1>>1) % amount, int64(r2>>1) % amount, int64(r3>>1) % amount}
		if amount == 1 {
			cum = []int64{0, 0, 1}
		}
		sort.Slice(cum, func(i, j int) bool { return cum[i] < cum[j] })
		p := payment(granted, amount)
		var total domain.Credits
		for _, c := range cum {
			plan, err := p.ApplyRefund(usd(c), 0)
			if err != nil {
				t.Fatalf("ApplyRefund(%d of %d): %v", c, amount, err)
			}
			if plan.Revoke < 0 {
				t.Fatalf("a refund took back a negative amount: %d", plan.Revoke)
			}
			total += plan.Revoke
			p.RefundedMinor, p.CreditsRevoked, p.Status = plan.RefundedMinor, plan.RevokedTotal, plan.Status
		}
		last := cum[len(cum)-1]
		want, err := domain.RevokeTotal(granted, amount, last)
		if err != nil {
			t.Fatal(err)
		}
		if total != want || p.CreditsRevoked != want {
			t.Fatalf("partitioned refunds %v of %d (granted %d) revoked %d (field %d); one refund of %d revokes %d", cum, amount, granted, total, p.CreditsRevoked, last, want)
		}
		if p.CreditsRevoked > granted {
			t.Fatalf("revoked %d of %d granted — more than was ever granted", p.CreditsRevoked, granted)
		}
	})
}

func TestARefundReportThatRegressesRevokesNothingAndNeverLowersTheTotal(t *testing.T) {
	t.Parallel()
	p := payment(10_000_000, 1000)
	p.RefundedMinor, p.CreditsRevoked, p.Status = 666, 6_660_000, domain.PartiallyRefunded
	// Providers do not order deliveries: the 333 report can arrive after the 666 one.
	plan, err := p.ApplyRefund(usd(333), 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revoke != 0 || plan.RefundedMinor != 666 || plan.RevokedTotal != 6_660_000 || plan.Status != domain.PartiallyRefunded {
		t.Fatalf("an older cumulative report must change nothing: %+v", plan)
	}
}

func TestRefundStatusFollowsTheCumulativeAmount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		status     domain.PaymentStatus
		cumulative int64
		want       domain.PaymentStatus
	}{
		{"part of it", domain.Succeeded, 1, domain.PartiallyRefunded},
		{"all of it", domain.Succeeded, 1000, domain.Refunded},
		{"all of it, after part of it", domain.PartiallyRefunded, 1000, domain.Refunded},
		{"a refund while a chargeback stands does not rewrite the chargeback", domain.ChargedBack, 500, domain.ChargedBack},
		{"nor a dispute still open", domain.Disputed, 500, domain.Disputed},
		{"a zero report on a fresh payment", domain.Succeeded, 0, domain.Succeeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := payment(1000, 1000)
			p.Status = c.status
			plan, err := p.ApplyRefund(usd(c.cumulative), 0)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != c.want {
				t.Fatalf("status = %s, want %s", plan.Status, c.want)
			}
		})
	}
}

func TestApplyRefundRefusesAReportFiatCannotExplain(t *testing.T) {
	t.Parallel()
	p := payment(1000, 1000)
	cases := map[string]domain.Money{
		"more than was paid": usd(1001),
		"another currency":   {MinorUnits: 10, Currency: "EUR"},
		"negative":           usd(-1),
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if plan, err := p.ApplyRefund(m, 0); !errors.Is(err, mdomain.ErrInvalid) {
				t.Fatalf("ApplyRefund(%+v) = %+v, %v; want ErrInvalid", m, plan, err)
			}
		})
	}
	if _, err := p.ApplyRefund(usd(10), -1); !errors.Is(err, mdomain.ErrInvalid) {
		t.Fatalf("a negative expired figure must be refused, got %v", err)
	}
}

// ---- invariant 20: a refund never claws back what expiry already took ----

func TestACappedClawbackNeverExceedsWhatExpiryLeft(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                           string
		revoke, granted, expired, want domain.Credits
	}{
		{"no lots: the cap is a no-op", 6_000_000, 10_000_000, 0, 6_000_000},
		{"a lot that lapsed unspent: nothing left to take", 10_000_000, 10_000_000, 10_000_000, 0},
		{"a lot that lapsed partly: only the rest", 10_000_000, 10_000_000, 4_000_000, 6_000_000},
		{"a refund smaller than the rest is untouched", 5_000_000, 10_000_000, 4_000_000, 5_000_000},
		{"exactly the rest", 6_000_000, 10_000_000, 4_000_000, 6_000_000},
		{"one over the rest", 6_000_001, 10_000_000, 4_000_000, 6_000_000},
		{"more expired than granted cannot go below zero", 10, 10, 12, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.CapAtUnexpired(c.revoke, c.granted, c.expired); got != c.want {
				t.Fatalf("CapAtUnexpired(%d, %d, %d) = %d, want %d", c.revoke, c.granted, c.expired, got, c.want)
			}
		})
	}
}

func TestARefundDoesNotRevokeCreditsThatLapsedUnspent(t *testing.T) {
	t.Parallel()
	p := payment(10_000_000, 1000)
	plan, err := p.ApplyRefund(usd(1000), 10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revoke != 0 || plan.RevokedTotal != 0 {
		t.Fatalf("revoked %d of credits the customer had already lost to expiry — taken twice (invariant 20)", plan.Revoke)
	}
	if plan.Status != domain.Refunded || plan.RefundedMinor != 1000 {
		t.Fatalf("the fiat side of the refund is still recorded: %+v", plan)
	}
}

func TestARefundAfterPartialExpiryRevokesOnlyTheRest(t *testing.T) {
	t.Parallel()
	p := payment(10_000_000, 1000)
	plan, err := p.ApplyRefund(usd(1000), 4_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revoke != 6_000_000 || plan.RevokedTotal != 6_000_000 {
		t.Fatalf("revoke = %d (total %d), want 6,000,000: granted 10M less 4M that expired", plan.Revoke, plan.RevokedTotal)
	}
}

func TestExpiryThatFollowsAClawbackIsNotTakenTwice(t *testing.T) {
	t.Parallel()
	// Half was refunded first (5M revoked); the lot's remaining 5M then lapsed. A full refund now has nothing left.
	p := payment(10_000_000, 1000)
	p.RefundedMinor, p.CreditsRevoked, p.Status = 500, 5_000_000, domain.PartiallyRefunded
	plan, err := p.ApplyRefund(usd(1000), 5_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revoke != 0 || plan.RevokedTotal != 5_000_000 {
		t.Fatalf("revoke = %d (total %d), want 0 (total 5,000,000)", plan.Revoke, plan.RevokedTotal)
	}
}

func FuzzACappedRefundNeverRevokesMoreThanWasLeftOrGranted(f *testing.F) {
	f.Add(uint64(10_000_000), uint64(1000), uint64(1000), uint64(4_000_000))
	f.Add(uint64(10), uint64(3), uint64(1), uint64(9))
	f.Fuzz(func(t *testing.T, g, a, r, e uint64) {
		granted := domain.Credits(g >> 1)
		amount := int64(a>>1) | 1
		refunded := int64(r>>1) % amount
		expired := domain.Credits(e>>1) % (granted + 1)
		p := payment(granted, amount)
		plan, err := p.ApplyRefund(usd(refunded), expired)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Revoke < 0 || plan.Revoke > granted-expired {
			t.Fatalf("revoke %d outside [0, granted−expired = %d]", plan.Revoke, granted-expired)
		}
		uncapped, _ := domain.RevokeTotal(granted, amount, refunded)
		if expired == 0 && plan.Revoke != uncapped {
			t.Fatalf("with no lots the cap must be a no-op: %d vs %d", plan.Revoke, uncapped)
		}
	})
}
