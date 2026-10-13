package contracts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// MeterOptions configure one Meter under test. Each subtest builds its own harness, on a realm of its
// own, so the subtests run in parallel without sharing a balance.
type MeterOptions struct {
	Policy    domain.NegativeBalancePolicy // default ClampToZero
	AdmitFail domain.AdmitFailPolicy       // default FailClosed
	// Limits are ceilings configured for the realm besides the `scope_balance` Balance limit every
	// harness has.
	Limits []domain.Limit
}

// MeterHarness is a Meter under test — in-process, or behind a transport — and the levers a suite needs.
//
// The suite follows one rule that makes it a test of the transport and not of the engine: state is set
// up through Engine, and the behaviour under test goes through Meter. When Meter is the engine itself
// the two are the same value; when it is a client, a fault in the wire shows up on the call that
// claims to survive it, never in the setup.
type MeterHarness struct {
	// Meter is what the suite exercises: ports.Meter as a caller sees it.
	Meter ports.Meter
	// Balance is the read the transport offers next to Meter (GetBalance): the hot balance per pool.
	Balance func(ctx context.Context, s domain.Scope) (map[domain.Pool]domain.Credits, error)

	// Engine is the Meter the transport fronts, used for setup. EngineBalance is the oracle: the hot
	// balance per pool read from the hot tier directly.
	Engine        ports.Meter
	EngineBalance func(t *testing.T, s domain.Scope) map[domain.Pool]domain.Credits

	// Realm is the realm every scope of the suite lives in. OtherRealm is a second realm the same
	// caller may address, for the cross-realm transfer.
	Realm, OtherRealm domain.Realm
	// CardVersion is the version of the realm's active rate card.
	CardVersion string
	// Now is the harness's current instant; the suite stamps events relative to it.
	Now func() time.Time

	// Open prepares a scope's shard the way the worker does at start. nil = nothing to prepare.
	Open func(t *testing.T, s domain.Scope)
	// Outage cuts the hot store until restore is called. nil = unsupported; the subtests that need
	// it are skipped, by name.
	Outage func(t *testing.T) (restore func())
	// Replay runs the journal replay tick once, after the hot store is back. nil = unsupported.
	Replay func(t *testing.T)
}

// The realm every harness serves, so the suite's expectations are numbers and not formulas:
//   - a pool `promo` of priority 10 that applies to every resource; `general` is implicit;
//   - an active rate card, version CardVersion, pricing rate key `gpt` at 0.15 credits per `in` unit and
//     0.6 per `out` unit — so 1,200 in and 800 out is exactly 660 credits;
//   - a Balance limit `scope_balance` (deny code payment_required), then MeterOptions.Limits.
const (
	cardRateKey   = "gpt"
	standardPrice = domain.Credits(660) // 1,200 in × 0.15 + 800 out × 0.6
)

// MeterEquivalence is the specification of FR-044: an in-process Meter and a remote one are the same
// value to the code that calls them. It is the Meter-level behaviour of LedgerContract — idempotency,
// the gate, pool order, ceilings, transfers — plus what only a Meter can say: deferral during an outage,
// a refused call that changed nothing, and **error identity**. Every typed error in domain/errors.go
// that a Meter call can return must satisfy errors.Is after the round trip, not match by its text.
//
// It runs unchanged in two modes — newHarness builds the engine in-process, or the same engine
// behind a transport — and the in-process mode is the control: it shows the suite asserts nothing the
// engine does not already do.
func MeterEquivalence(t *testing.T, newHarness func(t *testing.T, o MeterOptions) MeterHarness) {
	t.Helper()
	ctx := context.Background()
	var seq atomic.Int64

	sub := func(name string, o MeterOptions, f func(t *testing.T, h MeterHarness)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f(t, newHarness(t, o))
		})
	}
	ws := func(t *testing.T, h MeterHarness) domain.Scope {
		t.Helper()
		s := domain.Scope{Realm: h.Realm, Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
		if h.Open != nil {
			h.Open(t, s)
		}
		return s
	}
	event := func(h MeterHarness, s domain.Scope, key string) domain.Event {
		return domain.Event{
			Scope: s, Resource: "llm.chat", RateKey: cardRateKey, IdemKey: key,
			OccurredAt: h.Now().Add(-time.Second).UTC().Truncate(time.Millisecond),
			Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}},
		}
	}
	seed := func(t *testing.T, h MeterHarness, s domain.Scope, pool domain.Pool, n domain.Credits) {
		t.Helper()
		r, err := h.Engine.Grant(ctx, domain.Grant{Scope: s, Pool: pool, Amount: n, IdemKey: fmt.Sprintf("seed-%s-%d", pool.Or(), seq.Add(1)), Reason: "purchase"})
		mustNoErr(t, err)
		if !r.Applied {
			t.Fatalf("seed grant did not apply: %+v", r)
		}
	}
	total := func(m map[domain.Pool]domain.Credits) domain.Credits {
		var sum domain.Credits
		for _, v := range m {
			sum += v
		}
		return sum
	}
	hot := func(t *testing.T, h MeterHarness, s domain.Scope) domain.Credits {
		t.Helper()
		return total(h.EngineBalance(t, s))
	}
	needOutage := func(t *testing.T, h MeterHarness) {
		t.Helper()
		if h.Outage == nil {
			t.Skip("needs a lever that cuts the hot store")
		}
	}
	// wantErr asserts identity, never text: errors.Is across the transport.
	wantErr := func(t *testing.T, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("want errors.Is(err, %v), got %v", want, err)
		}
	}
	samePools := func(t *testing.T, got, want map[domain.Pool]domain.Credits) {
		t.Helper()
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("pool %q: got %v, want %v", k, got, want)
			}
		}
		for k, v := range got {
			if _, ok := want[k]; !ok && v != 0 {
				t.Fatalf("unexpected pool %q = %d in %v, want %v", k, v, got, want)
			}
		}
	}

	// ----------------------------------------------------------------------------------- Grant --

	sub("a grant is idempotent: the replay applies nothing and reports the original sequence", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		g := domain.Grant{Scope: s, Amount: 1000, IdemKey: "pay-1", Reason: "purchase"}
		r1, err := h.Meter.Grant(ctx, g)
		mustNoErr(t, err)
		if !r1.Applied || r1.Delta != 1000 || r1.Seq < 1 || r1.IdemKey != "pay-1" {
			t.Fatalf("first grant should apply: %+v", r1)
		}
		r2, err := h.Meter.Grant(ctx, g)
		mustNoErr(t, err)
		if r2.Applied || r2.Delta != 0 || r2.Seq != r1.Seq {
			t.Fatalf("a replay applies nothing and reports the original sequence: %+v vs %+v", r2, r1)
		}
		if got := hot(t, h, s); got != 1000 {
			t.Fatalf("minted twice: balance = %d, want 1000", got)
		}
	})

	sub("a grant key reused for a different amount is a conflict, not a silent no-op", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		_, err := h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 1000, IdemKey: "pay-1", Reason: "purchase"})
		mustNoErr(t, err)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 999, IdemKey: "pay-1", Reason: "purchase"})
		wantErr(t, err, domain.ErrIdemConflict)
		if got := hot(t, h, s); got != 1000 {
			t.Fatalf("a refused grant changes nothing: balance = %d", got)
		}
	})

	sub("a grant delivered concurrently mints exactly once", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		const n = 16
		var applied atomic.Int64
		errs := make(chan error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 500, IdemKey: "race", Reason: "purchase"})
				if err != nil {
					errs <- err
					return
				}
				if r.Applied {
					applied.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a concurrent delivery of one key is a replay, never an error: %v", err)
		}
		if applied.Load() != 1 || hot(t, h, s) != 500 {
			t.Fatalf("exactly-once: %d applied, balance %d", applied.Load(), hot(t, h, s))
		}
	})

	sub("a negative grant that overdraws is floored and the shortfall comes back as a writeoff", MeterOptions{Policy: domain.ClampToZero}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 10)
		r, err := h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: -100, IdemKey: "cb-1", Reason: "chargeback"})
		mustNoErr(t, err)
		if !r.Applied || r.Delta != -10 || r.Writeoff != 90 {
			t.Fatalf("clamp_to_zero floors at zero and reports the 90 it forgave: %+v", r)
		}
		if got := hot(t, h, s); got != 0 {
			t.Fatalf("balance = %d, want 0", got)
		}
	})

	sub("a grant during an outage is an error and leaves nothing behind", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		needOutage(t, h)
		s := ws(t, h)
		restore := h.Outage(t)
		_, err := h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 700, IdemKey: "pay-out", Reason: "purchase"})
		restore()
		wantErr(t, err, domain.ErrHotStoreUnavailable)
		// "accepted" for a mint that did not happen would let a caller believe credits exist that do not:
		// the same key, retried once the store is back, is the FIRST application.
		r, err := h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 700, IdemKey: "pay-out", Reason: "purchase"})
		mustNoErr(t, err)
		if !r.Applied || hot(t, h, s) != 700 {
			t.Fatalf("the refused grant must not have left a guard: %+v, balance %d", r, hot(t, h, s))
		}
	})

	// ----------------------------------------------------------------------------------- Record --

	sub("record prices the event exactly and names the card that priced it", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		r, err := h.Meter.Record(ctx, event(h, s, "evt-1"))
		mustNoErr(t, err)
		if !r.Applied || r.Deferred || r.Delta != -standardPrice || r.Seq < 1 || r.RateCardVersion != h.CardVersion || r.IdemKey != "evt-1" {
			t.Fatalf("1,200 in + 800 out is exactly %d credits under card %q: %+v", standardPrice, h.CardVersion, r)
		}
		if r.Balance != 100_000-standardPrice {
			t.Fatalf("receipt balance = %d, want %d", r.Balance, 100_000-standardPrice)
		}
		if got := hot(t, h, s); got != 100_000-standardPrice {
			t.Fatalf("balance = %d", got)
		}
	})

	sub("a replayed record applies nothing and the balance is charged once", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		e := event(h, s, "evt-1")
		r1, err := h.Meter.Record(ctx, e)
		mustNoErr(t, err)
		r2, err := h.Meter.Record(ctx, e)
		mustNoErr(t, err)
		if r2.Applied || r2.Deferred || r2.Delta != 0 || r2.Seq != r1.Seq {
			t.Fatalf("a replay reports the original sequence and applies nothing: %+v vs %+v", r2, r1)
		}
		if got := hot(t, h, s); got != 100_000-standardPrice {
			t.Fatalf("double-charged: balance = %d", got)
		}
	})

	sub("a record delivered concurrently charges exactly once", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		e := event(h, s, "race")
		const n = 16
		var applied atomic.Int64
		errs := make(chan error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := h.Meter.Record(ctx, e)
				if err != nil {
					errs <- err
					return
				}
				if r.Applied {
					applied.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("a concurrent delivery of one key is a replay, never an error: %v", err)
		}
		if applied.Load() != 1 || hot(t, h, s) != 100_000-standardPrice {
			t.Fatalf("exactly-once: %d applied, balance %d", applied.Load(), hot(t, h, s))
		}
	})

	sub("a key reused for a different request is a conflict, not a second charge", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		e := event(h, s, "evt-1")
		_, err := h.Meter.Record(ctx, e)
		mustNoErr(t, err)
		other := e
		other.Quantities = []domain.Quantity{{Unit: "in", Amount: 1}, {Unit: "out", Amount: 1}}
		_, err = h.Meter.Record(ctx, other)
		wantErr(t, err, domain.ErrIdemConflict)
		if got := hot(t, h, s); got != 100_000-standardPrice {
			t.Fatalf("the first price stands: balance = %d", got)
		}
	})

	sub("the promotional pool is drawn before the general one, and the transport reads both", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		seed(t, h, s, "promo", 100)
		_, err := h.Meter.Record(ctx, event(h, s, "evt-1")) // 660: all of promo, then 560 of general
		mustNoErr(t, err)
		want := map[domain.Pool]domain.Credits{"promo": 0, domain.GeneralPool: 440}
		samePools(t, h.EngineBalance(t, s), want)
		got, err := h.Balance(ctx, s)
		mustNoErr(t, err)
		samePools(t, got, want)
	})

	sub("an outage defers the record to the journal; recovery applies it once and a retry is a replay", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		needOutage(t, h)
		if h.Replay == nil {
			t.Skip("needs the journal replay tick")
		}
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		_, err := h.Meter.Record(ctx, event(h, s, "evt-1"))
		mustNoErr(t, err)

		restore := h.Outage(t)
		e2 := event(h, s, "evt-2")
		deferred, err := h.Meter.Record(ctx, e2)
		if err != nil {
			restore()
			t.Fatalf("invariant 16: an outage must not fail or drop usage: %v", err)
		}
		again, err2 := h.Meter.Record(ctx, e2) // a producer retry during the outage
		restore()
		mustNoErr(t, err2)
		if !deferred.Applied || !deferred.Deferred || deferred.Seq != 0 || deferred.IdemKey != "evt-2" || deferred.RateCardVersion != h.CardVersion {
			t.Fatalf("a deferred record succeeded, has not been applied, and names the card that priced it: %+v", deferred)
		}
		if !again.Deferred {
			t.Fatalf("the retry is deferred too, not journaled twice: %+v", again)
		}

		h.Replay(t)
		if got := hot(t, h, s); got != 100_000-2*standardPrice {
			t.Fatalf("both events charged exactly once: balance = %d", got)
		}
		final, err := h.Meter.Record(ctx, e2)
		mustNoErr(t, err)
		if final.Applied || final.Deferred || hot(t, h, s) != 100_000-2*standardPrice {
			t.Fatalf("and the producer retrying it now sees a plain replay: %+v", final)
		}
	})

	sub("a record that names no key, no time or no rate is refused, never given a default", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		cases := map[string]func(e *domain.Event){
			"no idem_key":    func(e *domain.Event) { e.IdemKey = "" },
			"no occurred_at": func(e *domain.Event) { e.OccurredAt = time.Time{} },
			"no rate_key":    func(e *domain.Event) { e.RateKey = "" },
			"no unit":        func(e *domain.Event) { e.Quantities = []domain.Quantity{{Unit: "", Amount: 1}} },
			"negative":       func(e *domain.Event) { e.Quantities = []domain.Quantity{{Unit: "in", Amount: -1}} },
		}
		for name, mutate := range cases {
			e := event(h, s, "evt-"+name)
			mutate(&e)
			_, err := h.Meter.Record(ctx, e)
			wantErr(t, err, domain.ErrInvalid)
			// A missing occurred_at must not become the epoch: that would be a stale event, a
			// different error and a different fix for the caller.
			if errors.Is(err, domain.ErrStaleEvent) {
				t.Fatalf("%s: a missing field was filled in with a default: %v", name, err)
			}
		}
		if got := hot(t, h, s); got != 100_000 {
			t.Fatalf("a refused record charges nothing: balance = %d", got)
		}
		if r, err := h.Meter.Record(ctx, event(h, s, "evt-ok")); err != nil || !r.Applied {
			t.Fatalf("and leaves no guard behind: %+v %v", r, err)
		}
	})

	sub("a record outside the dedup window is refused, one inside it is applied", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		stale := event(h, s, "evt-stale")
		stale.OccurredAt = h.Now().Add(-100 * time.Hour).UTC().Truncate(time.Millisecond)
		_, err := h.Meter.Record(ctx, stale)
		wantErr(t, err, domain.ErrStaleEvent)
		future := event(h, s, "evt-future")
		future.OccurredAt = h.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
		_, err = h.Meter.Record(ctx, future)
		wantErr(t, err, domain.ErrInvalid)
		inside := event(h, s, "evt-inside")
		inside.OccurredAt = h.Now().Add(-70 * time.Hour).UTC().Truncate(time.Millisecond)
		if r, err := h.Meter.Record(ctx, inside); err != nil || !r.Applied {
			t.Fatalf("70h is inside the 71h window: %+v %v", r, err)
		}
	})

	sub("an event that cannot be priced fails closed and charges nothing", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		unknownRate := event(h, s, "evt-rate")
		unknownRate.RateKey = "no-such-model"
		_, err := h.Meter.Record(ctx, unknownRate)
		wantErr(t, err, domain.ErrUnpriceable)
		unknownUnit := event(h, s, "evt-unit")
		unknownUnit.Quantities = []domain.Quantity{{Unit: "furlongs", Amount: 3}}
		_, err = h.Meter.Record(ctx, unknownUnit)
		wantErr(t, err, domain.ErrUnpriceable)
		if got := hot(t, h, s); got != 100_000 {
			t.Fatalf("never priced at zero: balance = %d", got)
		}
	})

	sub("an amount above the maximum operation is refused", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 100_000)
		huge := event(h, s, "evt-huge")
		huge.Quantities = []domain.Quantity{{Unit: "in", Amount: 1 << 60}}
		_, err := h.Meter.Record(ctx, huge)
		wantErr(t, err, domain.ErrAmountOutOfRange)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: domain.MaxOperationAmount + 1, IdemKey: "big", Reason: "purchase"})
		wantErr(t, err, domain.ErrAmountOutOfRange)
		if got := hot(t, h, s); got != 100_000 {
			t.Fatalf("balance = %d", got)
		}
	})

	// ------------------------------------------------------------------------------------ Admit --

	sub("admit gates without reserving, and reports the headroom", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 50)
		for i := 0; i < 3; i++ {
			adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat"})
			mustNoErr(t, err)
			if !adm.Allowed || adm.Blocked || adm.Exceeded != nil || adm.Headroom != 50 {
				t.Fatalf("a positive balance admits: %+v", adm)
			}
		}
		if got := hot(t, h, s); got != 50 {
			t.Fatalf("admit reserved: balance = %d", got)
		}
	})

	sub("an exhausted balance is refused as an answer, with its deny code", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		if err != nil {
			t.Fatalf("a refusal is a decision, not a failure: %v", err)
		}
		if adm.Allowed || adm.Blocked || adm.Exceeded == nil {
			t.Fatalf("SC-003: an exhausted ceiling is refused: %+v", adm)
		}
		x := adm.Exceeded
		if x.Name != "scope_balance" || x.DenyCode != domain.DenyPaymentRequired || x.Window != domain.Balance || x.Unit != domain.CreditUnit {
			t.Fatalf("with a classified code and the ceiling that refused: %+v", *x)
		}
	})

	sub("MaxCost bounds the gate", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 50)
		adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 10_000})
		mustNoErr(t, err)
		if adm.Allowed || adm.Headroom != 50 || adm.Exceeded == nil || adm.Exceeded.Name != "scope_balance" {
			t.Fatalf("want refused with headroom 50: %+v", adm)
		}
		adm, err = h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 50})
		mustNoErr(t, err)
		if !adm.Allowed {
			t.Fatalf("a cost equal to the balance fits: %+v", adm)
		}
	})

	sub("a per-run job budget supplied by the caller halts a runaway loop at its own cap", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1_000_000)
		runCap := domain.Limit{Name: "run_cap", Subject: domain.SubjectJob, Unit: domain.CreditUnit, Max: 1000, Window: domain.Job, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
		run1 := domain.Subjects{domain.SubjectJob: "r1"}
		for i := 0; i < 2; i++ {
			e := event(h, s, fmt.Sprintf("step-%d", i))
			e.Subjects = run1
			_, err := h.Meter.Record(ctx, e)
			mustNoErr(t, err)
		}
		adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", Subjects: run1, Limits: []domain.Limit{runCap}, MaxCost: 1})
		mustNoErr(t, err)
		if adm.Allowed || adm.Exceeded == nil {
			t.Fatalf("1,320 spent against a 1,000 budget: a runaway loop must halt at its own cap: %+v", adm)
		}
		x := adm.Exceeded
		if x.Name != "run_cap" || x.DenyCode != domain.DenyLimitReached || x.Window != domain.Job || x.Dur != time.Hour || x.Max != 1000 || x.Subject != domain.SubjectJob {
			t.Fatalf("and says which ceiling, in full: %+v", *x)
		}
		other, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", Subjects: domain.Subjects{domain.SubjectJob: "r2"}, Limits: []domain.Limit{runCap}, MaxCost: 1})
		mustNoErr(t, err)
		if !other.Allowed {
			t.Fatalf("another job's budget is untouched: %+v", other)
		}
		if got := hot(t, h, s); got != 1_000_000-2*standardPrice {
			t.Fatalf("a job budget is a counter, not a granted balance: %d", got)
		}
	})

	userCap := domain.Limit{Name: "user_cap", Subject: "user", Unit: domain.CreditUnit, Max: 1000, Window: domain.Rolling, Dur: time.Hour, WarnAt: 0.5, DenyCode: domain.DenyLimitReached}

	sub("a subject's ceiling warns near its limit and refuses past it, per subject", MeterOptions{Limits: []domain.Limit{userCap}}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1_000_000)
		u1 := domain.Subjects{"user": "u1"}
		gate := func(sj domain.Subjects) domain.Admission {
			adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", Subjects: sj, MaxCost: 1})
			mustNoErr(t, err)
			return adm
		}
		if adm := gate(u1); !adm.Allowed || adm.Warning != nil {
			t.Fatalf("an untouched ceiling neither warns nor refuses: %+v", adm)
		}
		e1 := event(h, s, "evt-1")
		e1.Subjects = u1
		_, err := h.Meter.Record(ctx, e1) // 660 of 1,000
		mustNoErr(t, err)
		adm := gate(u1)
		if !adm.Allowed || adm.Warning == nil || adm.Warning.Name != "user_cap" || adm.Warning.WarnAt != 0.5 || adm.Headroom != 340 {
			t.Fatalf("660/1000 crosses the 0.5 warning and leaves 340: %+v", adm)
		}
		e2 := event(h, s, "evt-2")
		e2.Subjects = u1
		_, err = h.Meter.Record(ctx, e2)
		mustNoErr(t, err)
		adm = gate(u1)
		if adm.Allowed || adm.Warning != nil || adm.Exceeded == nil {
			t.Fatalf("1,320/1000 is refused, and a refusal carries no warning: %+v", adm)
		}
		x := adm.Exceeded
		if x.Name != "user_cap" || x.DenyCode != domain.DenyLimitReached || x.Max != 1000 || x.Window != domain.Rolling || x.Dur != time.Hour || x.Subject != "user" {
			t.Fatalf("with the ceiling that refused, in full: %+v", *x)
		}
		if adm := gate(domain.Subjects{"user": "u2"}); !adm.Allowed {
			t.Fatalf("u2 has a counter of their own: %+v", adm)
		}
	})

	sub("a configured subject ceiling without its subject is refused, never skipped", MeterOptions{Limits: []domain.Limit{userCap}}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		_, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		wantErr(t, err, domain.ErrMissingSubject)
	})

	sub("a caller cannot add a window limit the books do not count", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		madeUp := domain.Limit{Name: "made_up", Unit: domain.CreditUnit, Max: 1, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
		_, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", Limits: []domain.Limit{madeUp}, MaxCost: 1})
		wantErr(t, err, domain.ErrUncountableLimit)
	})

	sub("a blocked account is refused with its own classification", MeterOptions{Policy: domain.BlockAndFlag}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 10)
		_, err := h.Engine.Grant(ctx, domain.Grant{Scope: s, Amount: -100, IdemKey: "cb", Reason: "chargeback"})
		mustNoErr(t, err)
		seed(t, h, s, domain.GeneralPool, 1_000) // a later top-up does not unblock it; only an operator does
		adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		if err != nil {
			t.Fatalf("a block is a decision, not a failure: %v", err)
		}
		if adm.Allowed || !adm.Blocked || adm.Exceeded == nil || adm.Exceeded.DenyCode != domain.DenyAccountBlocked {
			t.Fatalf("block_and_flag holds until an operator clears it: %+v", adm)
		}
	})

	sub("an outage refuses an admit under fail_closed", MeterOptions{AdmitFail: domain.FailClosed}, func(t *testing.T, h MeterHarness) {
		needOutage(t, h)
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		restore := h.Outage(t)
		defer restore()
		_, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		wantErr(t, err, domain.ErrHotStoreUnavailable)
	})

	sub("an outage serves an admit under fail_open, and says the headroom is unknown", MeterOptions{AdmitFail: domain.FailOpen}, func(t *testing.T, h MeterHarness) {
		needOutage(t, h)
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		restore := h.Outage(t)
		defer restore()
		adm, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		mustNoErr(t, err)
		if !adm.Allowed || adm.Headroom != domain.MaxOperationAmount {
			t.Fatalf("served; the headroom is the largest operation, not zero: %+v", adm)
		}
	})

	planCap := domain.Limit{Name: "plan_cap", Unit: domain.CreditUnit, Max: 100, Window: domain.Rolling, Dur: time.Hour, MaxEntitlement: "plan_cap_credits", DenyCode: domain.DenyLimitReached}
	sub("a ceiling sized by an entitlement nobody can resolve fails closed", MeterOptions{Limits: []domain.Limit{planCap}}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		_, err := h.Meter.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		wantErr(t, err, domain.ErrQuotaUnavailable)
	})

	// ---------------------------------------------------------------------------------- Transfer --

	sub("a transfer refuses to overdraw, applies phase 1 once, and conflicts on a reused key", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		from, to := ws(t, h), ws(t, h)
		seed(t, h, from, domain.GeneralPool, 5000)
		_, err := h.Meter.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 9000, IdemKey: "over"})
		wantErr(t, err, domain.ErrInsufficient)
		if hot(t, h, from) != 5000 {
			t.Fatal("a refused transfer changes nothing")
		}
		tr := domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"}
		r, err := h.Meter.Transfer(ctx, tr)
		mustNoErr(t, err)
		if !r.Applied || r.Status != domain.TransferInTransit || r.FromBalance != 3000 || r.Amount != 2000 || r.IdemKey != "alloc-1" {
			t.Fatalf("phase 1: %+v", r)
		}
		r2, err := h.Meter.Transfer(ctx, tr)
		mustNoErr(t, err)
		if r2.Applied || hot(t, h, from) != 3000 {
			t.Fatalf("a replayed allocation is a no-op: %+v, source %d", r2, hot(t, h, from))
		}
		tr.Amount = 1
		_, err = h.Meter.Transfer(ctx, tr)
		wantErr(t, err, domain.ErrIdemConflict)
	})

	sub("a transfer across realms is a mint and a burn, never a transfer", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		from := ws(t, h)
		seed(t, h, from, domain.GeneralPool, 100)
		other := domain.Scope{Realm: h.OtherRealm, Kind: "workspace", ID: "elsewhere"}
		_, err := h.Meter.Transfer(ctx, domain.Transfer{From: from, To: other, Amount: 1, IdemKey: "xr"})
		wantErr(t, err, domain.ErrCrossRealm)
		if hot(t, h, from) != 100 {
			t.Fatal("a refused transfer changes nothing")
		}
	})

	sub("MaxDestBalance caps what the destination may hold", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		from, to := ws(t, h), ws(t, h)
		seed(t, h, from, domain.GeneralPool, 5000)
		seed(t, h, to, domain.GeneralPool, 900)
		_, err := h.Meter.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 200, IdemKey: "cap", MaxDestBalance: 1000})
		wantErr(t, err, domain.ErrDestBalanceCap)
		if hot(t, h, from) != 5000 {
			t.Fatal("a capped transfer must not debit the source")
		}
		if _, err := h.Meter.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 100, IdemKey: "cap-ok", MaxDestBalance: 1000}); err != nil {
			t.Fatalf("900 + 100 == 1000 fits: %v", err)
		}
	})

	sub("a transfer between pools names each pool", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		from, to := ws(t, h), ws(t, h)
		seed(t, h, from, "promo", 400)
		r, err := h.Meter.Transfer(ctx, domain.Transfer{From: from, To: to, FromPool: "promo", ToPool: "promo", Amount: 150, IdemKey: "pools", Reason: "allocation"})
		mustNoErr(t, err)
		if !r.Applied || r.FromBalance != 250 {
			t.Fatalf("the source pool is debited: %+v", r)
		}
		samePools(t, h.EngineBalance(t, from), map[domain.Pool]domain.Credits{"promo": 250})
	})

	// ------------------------------------------------------------------------------ Read & checks --

	sub("the transport reads the hot balance per pool", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 700)
		seed(t, h, s, "promo", 30)
		got, err := h.Balance(ctx, s)
		mustNoErr(t, err)
		samePools(t, got, map[domain.Pool]domain.Credits{domain.GeneralPool: 700, "promo": 30})
	})

	sub("a malformed request is invalid, wherever it is made", MeterOptions{}, func(t *testing.T, h MeterHarness) {
		s := ws(t, h)
		seed(t, h, s, domain.GeneralPool, 1000)
		badScope := domain.Scope{Realm: h.Realm, Kind: "Bad Kind", ID: "x"}
		_, err := h.Meter.Admit(ctx, badScope, domain.AdmitRequest{MaxCost: 1})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Admit(ctx, s, domain.AdmitRequest{MaxCost: -1})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Admit(ctx, s, domain.AdmitRequest{Subjects: domain.Subjects{"user": ""}, MaxCost: 1})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 0, IdemKey: "zero", Reason: "purchase"})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 5, IdemKey: "", Reason: "purchase"})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 5, IdemKey: "no-reason"})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Pool: "Bad Pool", Amount: 5, IdemKey: "bad-pool", Reason: "purchase"})
		wantErr(t, err, domain.ErrInvalid)
		soon := h.Now().Add(time.Hour)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: -5, ExpiresAt: &soon, IdemKey: "neg-exp", Reason: "refund"})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Grant(ctx, domain.Grant{Scope: s, Amount: 5, SourceKey: "pay-1", IdemKey: "pos-src", Reason: "purchase"})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Transfer(ctx, domain.Transfer{From: s, To: s, Amount: 5, IdemKey: "same"})
		wantErr(t, err, domain.ErrInvalid)
		_, err = h.Meter.Transfer(ctx, domain.Transfer{From: s, To: domain.Scope{Realm: h.Realm, Kind: "workspace", ID: "other"}, Amount: 0, IdemKey: "zero"})
		wantErr(t, err, domain.ErrInvalid)
		if got := hot(t, h, s); got != 1000 {
			t.Fatalf("a refused request changes nothing: balance = %d", got)
		}
	})
}
