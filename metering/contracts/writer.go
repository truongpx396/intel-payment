package contracts

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// WriterOptions configure one writer under test.
type WriterOptions struct {
	LedgerOptions
	Rollup      bool           // LedgerGranularity=rollup (needs an archive)
	Lots        bool           // CreditExpiry=lots_fifo
	Tolerance   domain.Credits // ReconcileTolerance
	MaxAttempts int            // deliveries before an intent is parked; 0 = the default
	// Dedicated asks for a hot tier no other test shares. A behaviour that bumps a shard's
	// generation, freezes it or rewinds its sequence would otherwise land on a neighbour.
	Dedicated bool
}

// WriterHarness is a writer wired to a real hot tier and real books, plus the levers a suite needs
// to reach BEHIND the API: to expire a hot guard, to rewind a sequence, to lose an intent, to make
// the hot tier refuse the writer's own follow-up mutations.
type WriterHarness struct {
	LedgerHarness // Ledger, Clock, Drain, Booked, HasRow, InTransit, LedgerSum — the writer is wired
	Writer        ports.LedgerWriter
	// Second is another writer over the same books and the same stream, for the race between two
	// owners of one shard. nil skips the subtests that need it.
	Second func(t *testing.T) ports.LedgerWriter

	// Sweeps, run once.
	Expire  func(t *testing.T) int
	Redrive func(t *testing.T, olderThan time.Duration) int
	Reissue func(t *testing.T, minAge time.Duration) int

	// The hot side, behind the API.
	ShardOf     func(s domain.Scope) domain.Shard
	HotSeq      func(t *testing.T, s domain.Scope) int64
	SetHotSeq   func(t *testing.T, s domain.Scope, seq int64)
	TamperHot   func(t *testing.T, s domain.Scope, p domain.Pool, delta domain.Credits)
	ForgetGuard func(t *testing.T, s domain.Scope, usageKey string) // the hot guard's TTL passing
	// EffectsDown makes the hot tier refuse the writer's own follow-up mutations (the second leg of a
	// transfer, a correction, an expiry) while the producers' calls keep working.
	EffectsDown func(t *testing.T, down bool)

	// The stream, behind the API.
	Intents    func(t *testing.T, s domain.Scope) []domain.Intent // every intent the hot tier issued, in order
	Inject     func(t *testing.T, in domain.Intent)               // an intent as it would appear in the stream
	InjectRaw  func(t *testing.T, s domain.Scope, fields map[string]string)
	DropIntent func(t *testing.T, s domain.Scope, seq int64) // lose an entry, as a truncated AOF does

	// The books.
	Watermark func(t *testing.T, s domain.Scope) ports.Watermark
	Suspense  func(t *testing.T, s domain.Scope) []ports.Suspense
	Rows      func(t *testing.T, s domain.Scope) []ports.LedgerRow
	ShardGen  func(t *testing.T, s domain.Scope) int64
	Dead      func(t *testing.T, s domain.Scope) int // parked intents
	Archived  func(t *testing.T, s domain.Scope) int // events in the usage archive
	Metric    func(name string) int64
	Gauge     func(name string) int64
	Published func(subjectPrefix string) int // messages the writer put on the bus
}

func (h WriterHarness) need(t *testing.T, name string, ok bool) {
	t.Helper()
	if !ok {
		t.Skipf("the harness does not provide %s", name)
	}
}

// LedgerWriterContract is the specification of the durable writer: it books the hot tier's intents
// once, in order, and — because the two stores never share a transaction — is where they are made to
// agree (invariants 1, 2, 12, 14; FR-007, FR-009, FR-010, FR-016; hot-path-consistency.md §3–§8).
// It runs against real Redis and real Postgres; the suite never sees an adapter type.
func LedgerWriterContract(t *testing.T, newHarness func(t *testing.T, o WriterOptions) WriterHarness) {
	t.Helper()
	ctx := context.Background()
	var seq atomic.Int64
	ws := func() domain.Scope {
		return domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d-%d", time.Now().UnixNano(), seq.Add(1))}
	}
	sub := func(name string, f func(t *testing.T)) { t.Run(name, func(t *testing.T) { t.Parallel(); f(t) }) }

	hot := func(t *testing.T, h WriterHarness, s domain.Scope) domain.Credits {
		t.Helper()
		b, err := h.Ledger.Balance(ctx, s)
		mustNoErr(t, err)
		var sum domain.Credits
		for _, v := range b {
			sum += v
		}
		return sum
	}
	grant := func(t *testing.T, h WriterHarness, s domain.Scope, pool domain.Pool, n domain.Credits, key string) domain.Receipt {
		t.Helper()
		r, err := h.Ledger.Grant(ctx, domain.Grant{Scope: s, Pool: pool, Amount: n, IdemKey: key, Reason: "purchase"})
		mustNoErr(t, err)
		return r
	}
	charge := func(s domain.Scope, n domain.Credits, key string) domain.Charge {
		return domain.Charge{Scope: s, Amount: n, IdemKey: key, Reason: "query", Resource: "llm.chat", RateKey: "gpt",
			Quantities: []domain.Quantity{{Unit: "in", Amount: int64(n)}}, OccurredAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	}
	debit := func(t *testing.T, h WriterHarness, c domain.Charge) domain.Receipt {
		t.Helper()
		r, err := h.Ledger.Debit(ctx, c)
		mustNoErr(t, err)
		return r
	}
	drainShard := func(t *testing.T, h WriterHarness, s domain.Scope) int {
		t.Helper()
		n, err := h.Writer.Drain(ctx, h.ShardOf(s))
		mustNoErr(t, err)
		return n
	}
	// agree asserts invariant 12: the hot balance equals booked + open suspense in every pool.
	agree := func(t *testing.T, h WriterHarness, s domain.Scope) {
		t.Helper()
		open := map[domain.Pool]domain.Credits{}
		for _, e := range h.Suspense(t, s) {
			if e.Status == ports.SuspenseStatusOpen {
				open[e.Pool] += e.Delta
			}
		}
		b, err := h.Ledger.Balance(ctx, s)
		mustNoErr(t, err)
		pools := map[domain.Pool]bool{}
		for p := range b {
			pools[p] = true
		}
		for p := range open {
			pools[p] = true
		}
		for p := range pools {
			if want := h.Booked(t, s, p) + open[p]; b[p] != want {
				t.Fatalf("pool %q: hot %d != booked %d + open suspense %d", p, b[p], h.Booked(t, s, p), open[p])
			}
		}
	}
	openSuspense := func(t *testing.T, h WriterHarness, s domain.Scope) (n int) {
		t.Helper()
		for _, e := range h.Suspense(t, s) {
			if e.Status == ports.SuspenseStatusOpen {
				n++
			}
		}
		return
	}

	// ---------------------------------------------------------------- booking --

	sub("books every intent once, in sequence order, and hot == booked", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 10_000, "g1")
		for i := 0; i < 5; i++ {
			debit(t, h, charge(s, 100, fmt.Sprintf("u%d", i)))
		}
		grant(t, h, s, "", 50, "g2")
		hotSeq := h.HotSeq(t, s)
		if hotSeq != 7 {
			t.Fatalf("seven mutations, hot seq %d", hotSeq)
		}

		h.Drain(t)
		if wm := h.Watermark(t, s); wm.AppliedSeq != 7 || !wm.Exists {
			t.Fatalf("the watermark follows the hot sequence: %+v", wm)
		}
		rows := h.Rows(t, s)
		if len(rows) != 7 {
			t.Fatalf("one ledger row per intent at event granularity, got %d", len(rows))
		}
		for i, r := range rows {
			if r.SeqFrom != int64(i+1) || r.SeqTo != int64(i+1) {
				t.Fatalf("row %d books seq %d..%d: booked out of order", i, r.SeqFrom, r.SeqTo)
			}
		}
		if h.Booked(t, s, domain.GeneralPool) != 9550 || hot(t, h, s) != 9550 {
			t.Fatalf("booked %d hot %d", h.Booked(t, s, domain.GeneralPool), hot(t, h, s))
		}
		agree(t, h, s)
	})

	sub("a drained batch is booked in ONE call however many intents it holds", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1_000_000, "g1")
		for i := 0; i < 60; i++ {
			debit(t, h, charge(s, 10, fmt.Sprintf("u%d", i)))
		}
		if n := drainShard(t, h, s); n != 61 {
			t.Fatalf("one Drain books the whole backlog (≤ DrainBatch): got %d", n)
		}
		if n := drainShard(t, h, s); n != 0 {
			t.Fatalf("nothing is booked twice: %d", n)
		}
		agree(t, h, s)
	})

	sub("a redelivery — a crash between COMMIT and XACK — books nothing and opens no suspense", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		debit(t, h, charge(s, 100, "u1"))
		h.Drain(t)
		before, sumBefore := len(h.Rows(t, s)), h.LedgerSum(t, "t1")

		for _, in := range h.Intents(t, s) {
			h.Inject(t, in) // the very same (gen, seq, key): delivered again
		}
		h.Drain(t)
		if len(h.Rows(t, s)) != before || h.LedgerSum(t, "t1") != sumBefore {
			t.Fatal("a redelivery booked a second time")
		}
		if openSuspense(t, h, s) != 0 || len(h.Suspense(t, s)) != 0 {
			t.Fatal("a redelivery is not a duplicate: it must not open suspense")
		}
		if wm := h.Watermark(t, s); wm.AppliedSeq != 2 {
			t.Fatalf("the watermark did not move: %+v", wm)
		}
		agree(t, h, s)
	})

	sub("a gap is waited out, never skipped: later intents book only once the missing one arrives", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		for i := 1; i <= 4; i++ {
			debit(t, h, charge(s, 10, fmt.Sprintf("u%d", i)))
		}
		lost := h.Intents(t, s)[2] // seq 3
		h.DropIntent(t, s, lost.Seq)

		drainShard(t, h, s)
		drainShard(t, h, s)
		if wm := h.Watermark(t, s); wm.AppliedSeq != 2 {
			t.Fatalf("seq 3 is missing: nothing behind it may book, applied_seq = %d", wm.AppliedSeq)
		}
		if got := len(h.Rows(t, s)); got != 2 {
			t.Fatalf("ledger rows %d, want 2 (the grant and the first charge)", got)
		}
		if h.Metric == nil || h.Gauge == nil {
			t.Skip("no metrics to observe the gap")
		}
		if g := h.Gauge(ports.MetricOutboxDepth); g < 2 {
			t.Fatalf("the two intents behind the gap are still pending (outbox depth %d)", g)
		}

		h.Inject(t, lost) // it arrives late
		h.Drain(t)
		if wm := h.Watermark(t, s); wm.AppliedSeq != 5 {
			t.Fatalf("the gap filled, everything books: %+v", wm)
		}
		if got := len(h.Rows(t, s)); got != 5 {
			t.Fatalf("ledger rows %d, want 5", got)
		}
		agree(t, h, s)
	})

	sub("a sequence REGRESSION is booked, never lost, never lowers the watermark, and rebuilds the shard", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{Dedicated: true}), ws()
		grant(t, h, s, "", 1000, "g1")
		for i := 1; i <= 3; i++ {
			debit(t, h, charge(s, 10, fmt.Sprintf("u%d", i)))
		}
		h.Drain(t) // applied_seq 4, hot 970
		gen := h.ShardGen(t, s)

		// The hot tier comes back having lost its last two mutations: its next number is reused.
		h.SetHotSeq(t, s, 2)
		r := debit(t, h, charge(s, 25, "after-loss"))
		if r.Seq != 3 {
			t.Fatalf("the rewound tier reissued seq %d, want 3", r.Seq)
		}
		h.Drain(t)

		if !h.HasRow(t, s, "query", -25) {
			t.Fatal("the charge issued under the reused number was not booked: money lost")
		}
		if wm := h.Watermark(t, s); wm.AppliedSeq < 4 {
			t.Fatalf("the watermark moved backwards: %+v", wm)
		}
		if g := h.ShardGen(t, s); g != gen+1 {
			t.Fatalf("a regression freezes and rebuilds the shard: generation %d → %d", gen, g)
		}
		if h.Metric(ports.MetricHotRegression) != 1 {
			t.Fatal("the regression is observable")
		}
		if got := hot(t, h, s); got != h.Booked(t, s, domain.GeneralPool) {
			t.Fatalf("after the rebuild hot %d != booked %d", got, h.Booked(t, s, domain.GeneralPool))
		}
		// And the tier serves again, resuming above every number the books hold.
		next := debit(t, h, charge(s, 1, "resumed"))
		if next.Seq <= h.Watermark(t, s).AppliedSeq {
			t.Fatalf("a sequence number was reissued after the rebuild: %d <= %d", next.Seq, h.Watermark(t, s).AppliedSeq)
		}
	})

	sub("two writers racing on one shard book each intent exactly once", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		h.need(t, "a second writer", h.Second != nil)
		other := h.Second(t)
		grant(t, h, s, "", 1_000_000, "g1")
		for i := 0; i < 200; i++ {
			debit(t, h, charge(s, 3, fmt.Sprintf("u%d", i)))
		}
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for _, w := range []ports.LedgerWriter{h.Writer, other, h.Writer, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 4; i++ {
					if _, err := w.Drain(ctx, h.ShardOf(s)); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		h.Drain(t)
		if got := len(h.Rows(t, s)); got != 201 {
			t.Fatalf("ledger rows %d, want 201: an intent was booked twice or not at all", got)
		}
		agree(t, h, s)
	})

	// ------------------------------------------------------------- suspense --

	for _, c := range []struct {
		name   string
		second domain.Credits
		reason string
		metric string
	}{
		{"a replay that outlived its hot guard is a DUPLICATE: suspended, reversed on the hot side, never booked twice", 100, ports.SuspenseDuplicate, ports.MetricLateReplay},
		{"the same key for a DIFFERENT request is a CONFLICT: suspended, reversed, and counted", 250, ports.SuspenseIdemConflict, ports.MetricIdemConflict},
	} {
		sub(c.name, func(t *testing.T) {
			h, s := newHarness(t, WriterOptions{}), ws()
			grant(t, h, s, "", 1000, "g1")
			debit(t, h, charge(s, 100, "evt"))
			h.Drain(t)
			rowsBefore := len(h.Rows(t, s))

			h.ForgetGuard(t, s, "evt") // the hot guard's TTL passes; the durable one (72 h) remains
			r := debit(t, h, charge(s, c.second, "evt"))
			if !r.Applied {
				t.Fatalf("with the hot guard gone the hot tier charges again — the books must catch it: %+v", r)
			}
			if hot(t, h, s) != 1000-100-c.second {
				t.Fatalf("hot %d", hot(t, h, s))
			}

			// One pass books the replay as suspense and issues the correction; the next books the
			// correction, which closes the entry.
			if n := drainShard(t, h, s); n != 1 {
				t.Fatalf("booked %d", n)
			}
			open := h.Suspense(t, s)
			if len(open) != 1 || open[0].Reason != c.reason || open[0].Delta != -c.second {
				t.Fatalf("suspense: %+v", open)
			}
			h.Drain(t)
			if got := h.Suspense(t, s); len(got) != 1 || got[0].Status != ports.SuspenseStatusClosed {
				t.Fatalf("the correction closes the entry: %+v", got)
			}
			if len(h.Rows(t, s)) != rowsBefore {
				t.Fatal("the replay reached the ledger")
			}
			if hot(t, h, s) != 900 || h.Booked(t, s, domain.GeneralPool) != 900 {
				t.Fatalf("hot %d booked %d: the hot side was not reversed", hot(t, h, s), h.Booked(t, s, domain.GeneralPool))
			}
			if h.Metric(c.metric) != 1 {
				t.Fatalf("%s = %d", c.metric, h.Metric(c.metric))
			}
			agree(t, h, s)
		})
	}

	sub("a correction the hot tier could not take is re-issued by the sweep, exactly once", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		h.need(t, "EffectsDown and Reissue", h.EffectsDown != nil && h.Reissue != nil)
		grant(t, h, s, "", 1000, "g1")
		debit(t, h, charge(s, 100, "evt"))
		h.Drain(t)
		h.ForgetGuard(t, s, "evt")
		debit(t, h, charge(s, 100, "evt"))

		h.EffectsDown(t, true)
		h.Drain(t)
		if openSuspense(t, h, s) != 1 || hot(t, h, s) != 800 {
			t.Fatal("with the hot tier refusing the writer, the suspense stays open and the hot side unreversed")
		}
		agree(t, h, s) // hot == booked + open suspense holds all the while

		h.EffectsDown(t, false)
		h.Clock.Advance(time.Minute) // the sweep only touches entries older than its grace period
		if n := h.Reissue(t, 30*time.Second); n != 1 {
			t.Fatalf("reissued %d", n)
		}
		h.Drain(t)
		if openSuspense(t, h, s) != 0 || hot(t, h, s) != 900 {
			t.Fatalf("hot %d, open %d", hot(t, h, s), openSuspense(t, h, s))
		}
		if n := h.Reissue(t, 30*time.Second); n != 0 {
			t.Fatalf("a second sweep found %d entries to reissue", n)
		}
		agree(t, h, s)
	})

	sub("an intent that cannot be booked is parked whole after its retry budget, not dropped, and the scope moves on", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{MaxAttempts: 3}), ws()
		grant(t, h, s, "", 1000, "g1")
		h.Drain(t)
		// A transfer_out that names no destination can never be booked.
		bad := domain.Intent{Op: domain.OpTransferOut, Scope: s, Gen: h.Watermark(t, s).Gen, Seq: 2, IdemKey: "bad", Fingerprint: "fp",
			Draws: []domain.PoolDelta{{Pool: domain.GeneralPool, Delta: -100}}}
		h.Inject(t, bad)
		for i := 0; i < 6 && h.Dead(t, s) == 0; i++ {
			_, _ = h.Writer.Drain(ctx, h.ShardOf(s)) // the failures are the point
		}
		if h.Dead(t, s) != 1 {
			t.Fatal("the intent must be parked after MaxAttempts")
		}
		if wm := h.Watermark(t, s); wm.AppliedSeq != 2 {
			t.Fatalf("the watermark advances past a parked intent so the scope is not held hostage: %+v", wm)
		}
		var poison int
		for _, e := range h.Suspense(t, s) {
			if e.Reason == ports.SuspensePoison && e.Delta == -100 {
				poison++
			}
		}
		if poison != 1 {
			t.Fatalf("the hot movement the intent carried is kept as poison suspense: %+v", h.Suspense(t, s))
		}
	})

	sub("an entry that cannot even be parsed is parked whole", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		h.InjectRaw(t, s, map[string]string{"op": "usage", "seq": "not-a-number"})
		h.Drain(t)
		if h.Dead(t, s) != 1 {
			t.Fatalf("malformed entries are kept for an operator (FR-043): %d", h.Dead(t, s))
		}
		if wm := h.Watermark(t, s); wm.AppliedSeq != 1 {
			t.Fatalf("the good intent still booked: %+v", wm)
		}
	})

	// -------------------------------------------------------------- reconcile --

	sub("reconcile at equal sequence numbers finds nothing in a healthy scope", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		debit(t, h, charge(s, 100, "u1"))
		h.Drain(t)
		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if run.Checked != 1 || run.InSync != 1 || run.Drifted != 0 || len(run.Findings) != 0 {
			t.Fatalf("%+v", run)
		}
	})

	sub("intents in flight are DEFERRED, not drift, and nothing is healed", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		h.Drain(t)
		debit(t, h, charge(s, 100, "in-flight")) // the hot tier is ahead of the books
		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if run.Deferred != 1 || run.Drifted != 0 || run.AbsDrift != 0 {
			t.Fatalf("lag is not drift: %+v", run)
		}
		if hot(t, h, s) != 900 {
			t.Fatal("a deferred scope must be left alone")
		}
	})

	sub("real drift is healed on the HOT side by compare-and-set, alarmed, and never books a row", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		h.Drain(t)
		sum, rows := h.LedgerSum(t, "t1"), len(h.Rows(t, s))

		h.TamperHot(t, s, domain.GeneralPool, 50) // hot 1050, books 1000, both at seq 1
		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if run.Drifted != 1 || run.AbsDrift != 50 || len(run.Findings) != 1 {
			t.Fatalf("%+v", run)
		}
		f := run.Findings[0]
		if f.Outcome != domain.OutcomeDrift || f.Drift != 50 || f.Hot != 1050 || f.Expected != 1000 || !f.Healed || !f.Alarmed {
			t.Fatalf("%+v", f)
		}
		if hot(t, h, s) != 1000 {
			t.Fatalf("the hot side is brought to booked + open suspense: %d", hot(t, h, s))
		}
		if h.LedgerSum(t, "t1") != sum || len(h.Rows(t, s)) != rows {
			t.Fatal("reconcile booked a ledger row: the books change only through intents (FR-009)")
		}
		if run, _ = h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull); run.Drifted != 0 {
			t.Fatalf("healed: %+v", run)
		}
	})

	sub("drift is reported per scope AND pool: +100 and −100 never net to zero", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{LedgerOptions: LedgerOptions{Pools: []domain.PoolDef{{Name: "promo", Priority: 10}}}}), ws()
		grant(t, h, s, "", 1000, "g1")
		grant(t, h, s, "promo", 500, "g2")
		h.Drain(t)
		h.TamperHot(t, s, domain.GeneralPool, 100)
		h.TamperHot(t, s, "promo", -100)
		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if run.AbsDrift != 200 || len(run.Findings) != 2 {
			t.Fatalf("Σ|drift| = %d over %d findings: %+v", run.AbsDrift, len(run.Findings), run)
		}
	})

	sub("drift within the tolerance is healed but does not page", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{Tolerance: 100}), ws()
		grant(t, h, s, "", 1000, "g1")
		h.Drain(t)
		h.TamperHot(t, s, domain.GeneralPool, 60)
		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if len(run.Findings) != 1 || !run.Findings[0].Healed || run.Findings[0].Alarmed {
			t.Fatalf("%+v", run)
		}
	})

	sub("reconcile compares against booked + OPEN suspense, so a suspended replay is not drift", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		h.need(t, "EffectsDown", h.EffectsDown != nil)
		grant(t, h, s, "", 1000, "g1")
		debit(t, h, charge(s, 100, "evt"))
		h.Drain(t)
		h.ForgetGuard(t, s, "evt")
		debit(t, h, charge(s, 100, "evt"))
		h.EffectsDown(t, true)
		h.Drain(t) // suspense open, hot still 800
		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if run.Drifted != 0 || run.InSync != 1 {
			t.Fatalf("hot 800 == booked 900 + open suspense −100: %+v", run)
		}
	})

	sub("a hot sequence BEHIND the watermark is a regression: found, alarmed, and the shard rebuilt", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{Dedicated: true}), ws()
		grant(t, h, s, "", 1000, "g1")
		debit(t, h, charge(s, 100, "u1"))
		debit(t, h, charge(s, 100, "u2"))
		h.Drain(t)
		gen := h.ShardGen(t, s)
		h.SetHotSeq(t, s, 1)

		run, err := h.Writer.Reconcile(ctx, h.ShardOf(s), domain.ReconcileFull)
		mustNoErr(t, err)
		if run.Regressions != 1 || len(run.Findings) != 1 || run.Findings[0].Outcome != domain.OutcomeRegression || !run.Findings[0].Alarmed {
			t.Fatalf("%+v", run)
		}
		if g := h.ShardGen(t, s); g != gen+1 {
			t.Fatalf("the shard is rebuilt: generation %d → %d", gen, g)
		}
		if got := hot(t, h, s); got != 800 {
			t.Fatalf("hot resumes from the books: %d", got)
		}
		if r := debit(t, h, charge(s, 1, "next")); r.Seq != 4 {
			t.Fatalf("the sequence resumes at applied_seq: %d", r.Seq)
		}
	})

	// ------------------------------------------------------------ the money --

	sub("block_and_flag: the writeoff is booked and the block is durable", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{LedgerOptions: LedgerOptions{Policy: domain.BlockAndFlag}}), ws()
		grant(t, h, s, "", 10, "g1")
		r, err := h.Ledger.Grant(ctx, domain.Grant{Scope: s, Amount: -100, IdemKey: "cb", Reason: "chargeback"})
		mustNoErr(t, err)
		if r.Writeoff != 90 {
			t.Fatalf("%+v", r)
		}
		h.Drain(t)
		if !h.HasRow(t, s, "chargeback", -100) || !h.HasRow(t, s, "writeoff", 90) {
			t.Fatal("the request and its writeoff are booked as two rows")
		}
		if !h.Watermark(t, s).Blocked {
			t.Fatal("the block outlives the hot tier: it is on the watermark, and a rebuild restores it")
		}
		if h.Booked(t, s, domain.GeneralPool) != 0 || hot(t, h, s) != 0 {
			t.Fatal("booked and hot both floor at zero")
		}
		agree(t, h, s)
	})

	sub("a usage split across pools is one event: quantities and cost ride the first row only", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{LedgerOptions: LedgerOptions{Pools: []domain.PoolDef{{Name: "promo", Priority: 10}}}}), ws()
		grant(t, h, s, "promo", 30, "g1")
		grant(t, h, s, "", 1000, "g2")
		c := charge(s, 100, "split")
		c.CostMicros = 500
		debit(t, h, c)
		h.Drain(t)
		var withQty, rows int
		var cost int64
		for _, r := range h.Rows(t, s) {
			if r.IdemKey != "split" {
				continue
			}
			rows++
			if len(r.Quantities) > 0 {
				withQty++
			}
			cost += r.CostMicros
		}
		if rows != 2 || withQty != 1 || cost != 500 {
			t.Fatalf("rows %d, carrying quantities %d, cost %d", rows, withQty, cost)
		}
		if h.Booked(t, s, "promo") != 0 || h.Booked(t, s, domain.GeneralPool) != 930 {
			t.Fatalf("promo %d general %d", h.Booked(t, s, "promo"), h.Booked(t, s, domain.GeneralPool))
		}
		agree(t, h, s)
	})

	sub("rollup: one row per batch, every event archived whole, and a redelivery adds nothing", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{Rollup: true}), ws()
		h.need(t, "an archive", h.Archived != nil)
		grant(t, h, s, "", 10_000, "g1")
		for i := 0; i < 5; i++ {
			debit(t, h, charge(s, 100, fmt.Sprintf("u%d", i)))
		}
		h.Drain(t)
		var usage []ports.LedgerRow
		for _, r := range h.Rows(t, s) {
			if r.OperationType == "query" {
				usage = append(usage, r)
			}
		}
		if len(usage) != 1 || usage[0].EventCount != 5 || usage[0].Delta != -500 || usage[0].SeqFrom != 2 || usage[0].SeqTo != 6 {
			t.Fatalf("five events collapse to one row covering seq 2..6: %+v", usage)
		}
		if h.Archived(t, s) != 5 {
			t.Fatalf("every event stays in the archive, re-priceable: %d", h.Archived(t, s))
		}
		for _, in := range h.Intents(t, s) {
			h.Inject(t, in)
		}
		h.Drain(t)
		if h.Archived(t, s) != 5 || h.Booked(t, s, domain.GeneralPool) != 9500 {
			t.Fatalf("a redelivery changed something: archived %d booked %d", h.Archived(t, s), h.Booked(t, s, domain.GeneralPool))
		}
		agree(t, h, s)
	})

	// ------------------------------------------------------------- transfers --

	sub("a transfer whose second leg could not land is re-driven, and settles once", func(t *testing.T) {
		h := newHarness(t, WriterOptions{})
		h.need(t, "EffectsDown and Redrive", h.EffectsDown != nil && h.Redrive != nil)
		from, to := ws(), ws()
		grant(t, h, from, "", 5000, "g1")
		h.Drain(t)
		before := h.LedgerSum(t, "t1") + h.InTransit(t, "t1")

		h.EffectsDown(t, true)
		_, err := h.Ledger.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc", Reason: "allocation"})
		mustNoErr(t, err)
		h.Drain(t)
		if h.InTransit(t, "t1") != 2000 || h.LedgerSum(t, "t1")+h.InTransit(t, "t1") != before {
			t.Fatalf("Σ ledger + Σ in-transit is invariant with the second leg stuck: %d → %d", before, h.LedgerSum(t, "t1")+h.InTransit(t, "t1"))
		}

		h.EffectsDown(t, false)
		h.Clock.Advance(time.Minute) // only a transfer stuck longer than the redrive delay is touched
		if n := h.Redrive(t, 30*time.Second); n != 1 {
			t.Fatalf("redriven %d", n)
		}
		h.Drain(t)
		if h.InTransit(t, "t1") != 0 || h.Booked(t, to, domain.GeneralPool) != 2000 || h.Booked(t, from, domain.GeneralPool) != 3000 {
			t.Fatal("both legs are booked and the transfer is settled")
		}
		if hot(t, h, to) != 2000 || hot(t, h, from) != 3000 {
			t.Fatalf("hot: to %d from %d", hot(t, h, to), hot(t, h, from))
		}
		if n := h.Redrive(t, 30*time.Second); n != 0 {
			t.Fatalf("a settled transfer is not re-driven: %d", n)
		}
		h.Drain(t)
		if hot(t, h, to) != 2000 {
			t.Fatal("credited twice")
		}
		if h.LedgerSum(t, "t1") != before {
			t.Fatalf("realm conserved: %d → %d", before, h.LedgerSum(t, "t1"))
		}
	})

	// -------------------------------------------------- lots and expiry --

	sub("expiry: the exact remainder is booked, and the hot side's upper bound is trued up through suspense", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{Lots: true}), ws()
		h.need(t, "Expire", h.Expire != nil)
		soon := h.Clock.Now().Add(time.Hour)
		_, err := h.Ledger.Grant(ctx, domain.Grant{Scope: s, Amount: 1000, IdemKey: "lot-a", Reason: "promo", ExpiresAt: &soon})
		mustNoErr(t, err)
		grant(t, h, s, "", 5000, "lot-b") // never expires
		h.Drain(t)

		// 300 are spent from lot A (the oldest expiry first) and are still in flight when the expiry
		// tick runs: the books, at the watermark, still show lot A whole.
		debit(t, h, charge(s, 300, "spend"))
		h.Clock.Advance(2 * time.Hour)
		if n := h.Expire(t); n != 1 {
			t.Fatalf("expired %d lots", n)
		}
		if hot(t, h, s) != 6000-300-1000 {
			t.Fatalf("the hot tier pre-applies the watermark's remainder, an upper bound: %d", hot(t, h, s))
		}

		h.Drain(t) // books the spend, then the expiry at the EXACT remainder, then the correction
		if !h.HasRow(t, s, "expiry", -700) {
			t.Fatal("the ledger holds the exact remainder of the lot, 700")
		}
		var trueup int
		for _, e := range h.Suspense(t, s) {
			if e.Reason == ports.SuspenseExpiryTrueup && e.Delta == -300 {
				trueup++
				if e.Status != ports.SuspenseStatusClosed {
					t.Fatalf("the true-up is corrected and closed: %+v", e)
				}
			}
		}
		if trueup != 1 {
			t.Fatalf("the 300 the hot side over-expired are a true-up: %+v", h.Suspense(t, s))
		}
		if hot(t, h, s) != 5000 || h.Booked(t, s, domain.GeneralPool) != 5000 {
			t.Fatalf("hot %d booked %d, both 6000 − 300 − 700", hot(t, h, s), h.Booked(t, s, domain.GeneralPool))
		}
		if n := h.Expire(t); n != 0 {
			t.Fatalf("an expired lot is not expired again: %d", n)
		}
	})

	// ------------------------------------------------------------------ audit --

	sub("the deep audit agrees with the ledger and advances the checkpoints", func(t *testing.T) {
		h, s := newHarness(t, WriterOptions{}), ws()
		grant(t, h, s, "", 1000, "g1")
		for i := 0; i < 3; i++ {
			debit(t, h, charge(s, 10, fmt.Sprintf("u%d", i)))
		}
		h.Drain(t)
		rep, err := h.Writer.Audit(ctx, h.ShardOf(s))
		mustNoErr(t, err)
		if rep.Checked != 1 || len(rep.Findings) != 0 || rep.Mismatches != 0 {
			t.Fatalf("%+v", rep)
		}
	})
}
