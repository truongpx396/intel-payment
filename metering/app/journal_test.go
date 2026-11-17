package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func journalEntry(id, key string, attempts int) ports.JournalEntry {
	c := domain.Charge{Scope: ws, Amount: 50, IdemKey: key, Resource: "llm.chat", RateKey: "gpt", RateCardVersion: "v1", OccurredAt: epoch}
	return ports.JournalEntry{ID: id, Charge: c, Fingerprint: c.Fingerprint(), Attempts: attempts}
}

func replayer(t *testing.T, r *rig) *app.JournalReplayer {
	t.Helper()
	jr, err := app.NewJournalReplayer(r.cfg, r.deps)
	if err != nil {
		t.Fatal(err)
	}
	return jr
}

func TestReplayAppliesEntriesThroughTheHotFunction(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.journal.due = []ports.JournalEntry{journalEntry("a", "k1", 0), journalEntry("b", "k2", 0), journalEntry("c", "k3", 0)}
	r.journal.pending = 3
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		if c.IdemKey == "k2" { // the hot tier already has it: a REPLAY
			return domain.Receipt{IdemKey: c.IdemKey, Applied: false, Seq: 4}, nil
		}
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Seq: 5}, nil
	}
	res, err := replayer(t, r).Replay(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed != 3 || res.Applied != 2 || res.Replayed != 1 || res.Retried != 0 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	if len(r.journal.retries) != 0 {
		t.Fatal("applied entries are not rescheduled: the writer settles them when it books the intent")
	}
	if r.met.gauges[ports.MetricJournalPending] != 3 {
		t.Fatalf("the backlog is observable: %v", r.met.gauges)
	}
	// The replayed charge is the SAME request the journal stored.
	if got := r.bal.debited[0]; got.Fingerprint() != journalEntry("a", "k1", 0).Charge.Fingerprint() {
		t.Fatal("a replay must not re-price")
	}
}

// The backlog gauge is what MeteringDeferredUsageNotDraining pages on, for fifteen minutes. The writer settles a
// journal entry when it books the intent, which is AFTER Replay applied it, so the count Replay reads straight after
// its last batch is still above zero. Nothing is due on the next tick — so unless the gauge is refreshed then too, it
// stays at that last value for good, and the page for "deferred usage is not draining" never clears after an outage in
// which everything drained. (Found by the Phase 7 chaos run: the gauge read 12 over a journal that held nothing.)
func TestTheBacklogGaugeFallsToZeroOnceTheWriterHasSettledTheLastEntriesAndNothingIsDueAnyMore(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.journal.due = []ports.JournalEntry{journalEntry("a", "k1", 0), journalEntry("b", "k2", 0)}
	r.journal.pending = 2
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Seq: 5}, nil
	}
	jr := replayer(t, r)
	if _, err := jr.Replay(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := r.met.gauges[ports.MetricJournalPending]; got != 2 {
		t.Fatalf("setup: both entries were applied but not yet settled, so the backlog is 2, got %d", got)
	}

	// The writer books both intents and settles both entries. The next tick finds nothing due.
	r.journal.due, r.journal.pending = nil, 0
	res, err := jr.Replay(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claimed != 0 {
		t.Fatalf("setup: nothing is due, yet %d entries were claimed", res.Claimed)
	}
	if got := r.met.gauges[ports.MetricJournalPending]; got != 0 {
		t.Fatalf("the journal is empty but metering_journal_pending still says %d: the page for undrained deferred usage would never clear", got)
	}
}

// Two replays at once — the worker's own loop and a `payment-worker tick journal` run by hand — share the set of realms
// the replayer has reported. An unguarded map there is not a wrong number but a crash ("concurrent map writes").
func TestReplaysRunningAtOnceDoNotRaceOnTheRealmsTheReplayerHasReported(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.journal.due = []ports.JournalEntry{journalEntry("a", "k1", 0)}
	r.journal.pending = 1
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Seq: 5}, nil
	}
	jr := replayer(t, r)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := jr.Replay(context.Background(), 10); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestReplayBacksOffWhileTheHotTierIsDownAndNeverDropsAnEntry(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.journal.due = []ports.JournalEntry{journalEntry("a", "k1", 0), journalEntry("b", "k2", 3), journalEntry("c", "k3", 40)}
	r.bal.debit = func(domain.Charge) (domain.Receipt, error) { return domain.Receipt{}, domain.ErrShardFrozen }
	res, err := replayer(t, r).Replay(context.Background(), 10)
	if err != nil || res.Retried != 3 || res.Applied != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.journal.retries) != 3 {
		t.Fatalf("every entry is rescheduled, none dropped: %d", len(r.journal.retries))
	}
	want := []time.Duration{2 * time.Second, 16 * time.Second, 5 * time.Minute} // 1s<<1, 1s<<4, capped
	for i, rt := range r.journal.retries {
		if got := rt.next.Sub(epoch); got != want[i] {
			t.Errorf("entry %d backs off %v, want %v", i, got, want[i])
		}
	}
	if r.journal.retries[2].attempts != 41 {
		t.Fatalf("attempts keep counting: %d", r.journal.retries[2].attempts)
	}
}

func TestReplayCountsWhatWillNotFixItself(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.journal.due = []ports.JournalEntry{journalEntry("a", "k1", 0), journalEntry("b", "k2", 0)}
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		if c.IdemKey == "k1" {
			return domain.Receipt{}, domain.ErrIdemConflict
		}
		return domain.Receipt{}, errors.New("hot function said no")
	}
	res, _ := replayer(t, r).Replay(context.Background(), 10)
	if res.Failed != 2 || len(r.journal.retries) != 2 {
		t.Fatalf("it stays in the journal, backed off, and is counted for a human: %+v", res)
	}
	if r.met.count("metering_idem_conflict_total{usage}") != 1 {
		t.Fatal("a conflict is a producer bug and is counted")
	}
}

func TestReplayRehydratesAColdScope(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{})
	r.journal.due = []ports.JournalEntry{journalEntry("a", "k1", 0)}
	n := 0
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		n++
		if n == 1 {
			return domain.Receipt{}, domain.ErrColdScope
		}
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Seq: 1}, nil
	}
	res, _ := replayer(t, r).Replay(context.Background(), 10)
	if res.Applied != 1 || len(r.bal.rehydOn) != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestNewValidatesEverythingTheMeterNeeds(t *testing.T) {
	t.Parallel()
	good := func() (metering.Config, app.Deps) {
		r := newRig(t, rigOpt{})
		return r.cfg, r.deps
	}
	cases := map[string]func(*metering.Config, *app.Deps){
		"no balance store":       func(_ *metering.Config, d *app.Deps) { d.Balance = nil },
		"no books":               func(_ *metering.Config, d *app.Deps) { d.Books = nil },
		"no bus":                 func(_ *metering.Config, d *app.Deps) { d.Bus = nil },
		"no journal":             func(_ *metering.Config, d *app.Deps) { d.Journal = nil },
		"no table pricer":        func(_ *metering.Config, d *app.Deps) { d.Pricers = ports.PricerRegistry{} },
		"no limits":              func(_ *metering.Config, d *app.Deps) { d.Limits = nil },
		"no pools":               func(_ *metering.Config, d *app.Deps) { d.Pools = nil },
		"no rate cards":          func(_ *metering.Config, d *app.Deps) { d.RateCards = nil },
		"no clock":               func(_ *metering.Config, d *app.Deps) { d.Clock = nil },
		"rollup without archive": func(c *metering.Config, _ *app.Deps) { c.LedgerGranularity = metering.GranularityRollup },
		"invalid config":         func(c *metering.Config, _ *app.Deps) { c.Settlement = "yolo" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, d := good()
			mutate(&cfg, &d)
			if _, err := app.New(cfg, d); err == nil {
				t.Fatal("must refuse to build a Meter that would fail at the first request")
			}
			if _, err := app.NewJournalReplayer(cfg, d); err == nil {
				t.Fatal("the replayer needs the same dependencies")
			}
		})
	}
	cfg, d := good()
	if _, err := app.New(cfg, d); err != nil {
		t.Fatal(err)
	}
	if _, err := app.New(cfg, func() app.Deps { d.Metrics = nil; return d }()); err != nil {
		t.Fatalf("Metrics is optional (a no-op is the default): %v", err)
	}
}
