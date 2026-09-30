package contracts

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// ConsistencyHarness is a WriterHarness whose hot tier can be sent back in time.
type ConsistencyHarness struct {
	WriterHarness
	// Checkpoint remembers the hot tier's durable state as of now.
	Checkpoint func(t *testing.T)
	// Rollback loses everything the hot tier did after the Checkpoint, balance changes and their
	// intents together — as a restart from a truncated AOF does, or a failover to a replica that had
	// not yet received them. The tier is serving again, on a node with a new replication id, when it
	// returns.
	Rollback func(t *testing.T)
}

// ConsistencyContract is the specification of what the two stores guarantee TOGETHER when the hot
// tier loses history (hot-path-consistency.md §5, §9; SC-002, SC-008): no operation is charged twice,
// nothing the books hold is lost, the shard is frozen and rebuilt from the books, and what the
// rollback took with it costs exactly the loss window and nothing else.
//
// A rollback removes a charge's balance change AND its intent together, so the two stores stay
// consistent; reconcile could never see the loss, because nothing remembers it. What CAN go wrong is
// the other direction — the books hold intents the hot tier no longer has — and that is what these
// subtests attack.
func ConsistencyContract(t *testing.T, newHarness func(t *testing.T) ConsistencyHarness) {
	t.Helper()
	ctx := context.Background()
	var seq atomic.Int64
	ws := func() domain.Scope {
		return domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d-%d", time.Now().UnixNano(), seq.Add(1))}
	}
	sub := func(name string, f func(t *testing.T)) { t.Run(name, func(t *testing.T) { t.Parallel(); f(t) }) }

	hot := func(t *testing.T, h ConsistencyHarness, s domain.Scope) domain.Credits {
		t.Helper()
		b, err := h.Ledger.Balance(ctx, s)
		mustNoErr(t, err)
		var sum domain.Credits
		for _, v := range b {
			sum += v
		}
		return sum
	}
	charge := func(s domain.Scope, key string) domain.Charge {
		return domain.Charge{Scope: s, Amount: 100, IdemKey: key, Reason: "query", Resource: "llm.chat", RateKey: "gpt",
			OccurredAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	}
	debit := func(t *testing.T, h ConsistencyHarness, s domain.Scope, key string) domain.Receipt {
		t.Helper()
		r, err := h.Ledger.Debit(ctx, charge(s, key))
		mustNoErr(t, err)
		return r
	}
	openSuspense := func(t *testing.T, h ConsistencyHarness, s domain.Scope) (n int) {
		t.Helper()
		for _, e := range h.Suspense(t, s) {
			if e.Status == ports.SuspenseStatusOpen {
				n++
			}
		}
		return
	}
	// settled asserts the end state every scenario must reach: the books hold each operation once,
	// the hot tier equals them, and nothing is left open.
	settled := func(t *testing.T, h ConsistencyHarness, s domain.Scope, wantBalance domain.Credits, wantRows int) {
		t.Helper()
		if got := h.Booked(t, s, domain.GeneralPool); got != wantBalance {
			t.Fatalf("booked %d, want %d: an operation was booked twice or not at all", got, wantBalance)
		}
		if got := hot(t, h, s); got != wantBalance {
			t.Fatalf("hot %d, want %d", got, wantBalance)
		}
		if got := len(h.Rows(t, s)); got != wantRows {
			t.Fatalf("ledger rows %d, want %d", got, wantRows)
		}
		if n := openSuspense(t, h, s); n != 0 {
			t.Fatalf("%d suspense entries still open", n)
		}
		if run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull); err != nil || run.Drifted != 0 || run.Regressions != 0 {
			t.Fatalf("reconcile after recovery: %+v %v", run, err)
		}
	}

	sub("a rollback of intents the books already hold: nothing is charged twice, the shard is rebuilt, retries converge", func(t *testing.T) {
		h, s := newHarness(t), ws()
		if _, err := h.Ledger.Grant(ctx, domain.Grant{Scope: s, Amount: 10_000, IdemKey: "g1", Reason: "purchase"}); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 5; i++ {
			debit(t, h, s, fmt.Sprintf("u%d", i))
		}
		h.Drain(t) // establishes the node the writer trusts
		h.Checkpoint(t)
		for i := 6; i <= 10; i++ {
			debit(t, h, s, fmt.Sprintf("u%d", i))
		}
		h.Drain(t) // the books now hold seq 1..11
		gen := h.ShardGen(t, s)

		h.Rollback(t)
		if h.HotSeq(t, s) != 6 {
			t.Fatalf("the rollback left the tier at seq %d, want 6", h.HotSeq(t, s))
		}

		// The writer notices — by the node's replication id, before any sequence number shows it — and
		// freezes, drains, bumps and rebuilds from the books.
		h.Drain(t)
		if g := h.ShardGen(t, s); g != gen+1 {
			t.Fatalf("shard generation %d → %d: the shard was not rebuilt", gen, g)
		}
		if got := hot(t, h, s); got != 9000 {
			t.Fatalf("rebuilt from the books: hot %d, want 9000", got)
		}
		if next := debit(t, h, s, "fresh"); next.Seq != 12 {
			t.Fatalf("the sequence resumes above every number the books hold: %d, want 12", next.Seq)
		}

		// Every producer whose acknowledgement it lost retries. u1..u5 still have their hot guards; the
		// guards for u6..u10 went with the rollback, so the hot tier charges them AGAIN — and the books,
		// which remember them for 72 h, must catch every one.
		var replays int
		for i := 1; i <= 10; i++ {
			if r := debit(t, h, s, fmt.Sprintf("u%d", i)); !r.Applied {
				replays++
			}
		}
		if replays != 5 {
			t.Fatalf("the surviving hot guards answered %d retries, want 5", replays)
		}
		h.Drain(t)
		settled(t, h, s, 8900, 12) // 10_000 − 10×100 − "fresh"; the grant, ten charges and "fresh"
		if h.Metric(ports.MetricLateReplay) != 5 {
			t.Fatalf("five duplicates were suspended and reversed, got %d", h.Metric(ports.MetricLateReplay))
		}
		if h.LedgerSum(t, "t1") != 8900 {
			t.Fatalf("SC-002: Σ ledger = %d", h.LedgerSum(t, "t1"))
		}
	})

	sub("a rollback of intents the writer never saw costs exactly the loss window, and a retry heals it", func(t *testing.T) {
		h, s := newHarness(t), ws()
		if _, err := h.Ledger.Grant(ctx, domain.Grant{Scope: s, Amount: 10_000, IdemKey: "g1", Reason: "purchase"}); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 3; i++ {
			debit(t, h, s, fmt.Sprintf("u%d", i))
		}
		h.Drain(t)
		h.Checkpoint(t)
		debit(t, h, s, "u4") // acknowledged to the producer, in neither the books nor — after this — Redis
		debit(t, h, s, "u5")
		h.Rollback(t)

		h.Drain(t)
		// Hot and books agree, both without u4 and u5: the loss is bounded and CONSISTENT. That it is
		// invisible to reconcile is exactly why the loss window is what the design states.
		if hot(t, h, s) != 9700 || h.Booked(t, s, domain.GeneralPool) != 9700 {
			t.Fatalf("hot %d booked %d, both 9700", hot(t, h, s), h.Booked(t, s, domain.GeneralPool))
		}
		// The producers retry, as they must when an acknowledgement is not trusted: applied once.
		for _, k := range []string{"u4", "u5"} {
			if r := debit(t, h, s, k); !r.Applied {
				t.Fatalf("%s was lost with the rollback: its retry must apply, %+v", k, r)
			}
		}
		h.Drain(t)
		settled(t, h, s, 9500, 6)
	})

	sub("producers that keep charging before the writer notices are booked once, and the rebuild discards their hot-only effects", func(t *testing.T) {
		h, s := newHarness(t), ws()
		if _, err := h.Ledger.Grant(ctx, domain.Grant{Scope: s, Amount: 10_000, IdemKey: "g1", Reason: "purchase"}); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 3; i++ {
			debit(t, h, s, fmt.Sprintf("u%d", i))
		}
		h.Drain(t)
		h.Checkpoint(t)
		for i := 4; i <= 8; i++ {
			debit(t, h, s, fmt.Sprintf("u%d", i))
		}
		h.Drain(t) // booked: seq 1..9
		h.Rollback(t)

		// Before the writer looks: a retry (same key, and — the tier being rewound — the same sequence
		// number it had) and brand-new work that reuses a number the books already spent.
		debit(t, h, s, "u4")
		debit(t, h, s, "new1")
		debit(t, h, s, "new2")

		h.Drain(t)
		// u4 was already booked: a redelivery, not a second charge. new1 and new2 reused numbers the
		// books had spent, and must be booked as the real operations they are.
		settled(t, h, s, 10_000-8*100-2*100, 1+8+2)
	})

	sub("a transfer caught between its two legs is completed by the redrive, and the realm is conserved throughout", func(t *testing.T) {
		h := newHarness(t)
		from, to := ws(), ws()
		if _, err := h.Ledger.Grant(ctx, domain.Grant{Scope: from, Amount: 5000, IdemKey: "g1", Reason: "purchase"}); err != nil {
			t.Fatal(err)
		}
		h.Drain(t)
		h.Checkpoint(t)
		before := h.LedgerSum(t, "t1") + h.InTransit(t, "t1")

		if _, err := h.Ledger.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc", Reason: "allocation"}); err != nil {
			t.Fatal(err)
		}
		// One pass over the source's shard: the out leg is booked (and the destination credited hot),
		// but the destination's own intent has not been booked.
		if _, err := h.Writer.Drain(ctx, h.ShardOf(from)); err != nil {
			t.Fatal(err)
		}
		if h.InTransit(t, "t1") != 2000 {
			t.Fatalf("in transit %d", h.InTransit(t, "t1"))
		}
		h.Rollback(t) // the hot tier forgets both legs

		h.Drain(t)
		if got := h.LedgerSum(t, "t1") + h.InTransit(t, "t1"); got != before {
			t.Fatalf("Σ ledger + Σ in-transit must survive the rollback: %d → %d", before, got)
		}
		if hot(t, h, from) != 3000 || hot(t, h, to) != 0 {
			t.Fatalf("rebuilt from the books: from %d, to %d — the credit is still in transit", hot(t, h, from), hot(t, h, to))
		}

		h.Clock.Advance(time.Minute)
		if n := h.Redrive(t, 30*time.Second); n != 1 {
			t.Fatalf("redriven %d", n)
		}
		h.Drain(t)
		if h.InTransit(t, "t1") != 0 || hot(t, h, to) != 2000 || h.Booked(t, to, domain.GeneralPool) != 2000 || h.Booked(t, from, domain.GeneralPool) != 3000 {
			t.Fatalf("to: hot %d booked %d; from booked %d; in transit %d",
				hot(t, h, to), h.Booked(t, to, domain.GeneralPool), h.Booked(t, from, domain.GeneralPool), h.InTransit(t, "t1"))
		}
		if h.LedgerSum(t, "t1") != before {
			t.Fatalf("the realm is conserved: %d → %d", before, h.LedgerSum(t, "t1"))
		}
	})
}
