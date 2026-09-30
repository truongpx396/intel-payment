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

// LedgerOptions configure one ledger under test. Each subtest builds its own, on a fresh scope, so
// the subtests run in parallel without sharing a balance.
type LedgerOptions struct {
	Pools  []domain.PoolDef
	Limits []domain.Limit
	Policy domain.NegativeBalancePolicy // default ClampToZero
	// Now is the starting instant of the ledger's fake clock (default 2026-09-29 12:00 UTC).
	Now time.Time
}

// LedgerHarness is a ledger under test plus the levers a suite needs. Drain and the booking probes
// are present only when a writer is wired: the hot-tier behaviours (idempotency, draw order, the
// gate, counters, clamping, phase 1 of a transfer) are checked without one, and the behaviours
// that are about the BOOKS run when it exists — and are reported as skipped, by name, when it does
// not, never silently passed.
type LedgerHarness struct {
	Ledger ports.Ledger
	Clock  *FakeClock

	// Drain runs the writer to quiescence so eventual effects can be asserted. nil = no writer.
	Drain func(t *testing.T)
	// Booked is the ledger's running balance for a scope and pool.
	Booked func(t *testing.T, s domain.Scope, p domain.Pool) domain.Credits
	// HasRow reports whether the ledger holds a row of this type and delta for the scope.
	HasRow func(t *testing.T, s domain.Scope, operationType string, delta domain.Credits) bool
	// InTransit is Σ of transfers not yet settled in a realm.
	InTransit func(t *testing.T, realm domain.Realm) domain.Credits
	// LedgerSum is Σ credit_ledger.delta for a realm.
	LedgerSum func(t *testing.T, realm domain.Realm) domain.Credits
}

func (h LedgerHarness) needsBooks(t *testing.T) {
	t.Helper()
	if h.Drain == nil {
		t.Skip("needs the durable writer (Phase 1e); the hot-tier half of this behaviour is asserted in its own subtest")
	}
}

// LedgerContract runs against a REAL ports.Ledger (Testcontainers Redis, and Postgres once the
// writer is wired). It is the specification of invariants 2, 3, 12, 14, 16, 17 and FR-003…005,
// FR-015, FR-017a…d at the hot tier. newHarness builds one from options; the suite never sees an
// adapter type, so the same suite runs against any implementation of the port.
func LedgerContract(t *testing.T, newHarness func(t *testing.T, o LedgerOptions) LedgerHarness) {
	t.Helper()
	ctx := context.Background()
	balanceOnly := domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Max: 0, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}

	// Every subtest takes its own scope: parallel tests never contend on a balance, and a re-run
	// against a long-lived Redis never sees the last run's state.
	var seq atomic.Int64
	scope := func(realm domain.Realm, kind string) domain.Scope {
		return domain.Scope{Realm: realm, Kind: kind, ID: fmt.Sprintf("%s-%d-%d", t.Name(), time.Now().UnixNano(), seq.Add(1))}
	}
	ws := func() domain.Scope { return scope("t1", "workspace") }

	type env struct {
		h LedgerHarness
		l ports.Ledger
	}
	setup := func(t *testing.T, o LedgerOptions) env {
		t.Helper()
		h := newHarness(t, o)
		return env{h, h.Ledger}
	}
	hot := func(t *testing.T, l ports.Ledger, s domain.Scope) domain.Credits {
		t.Helper()
		b, err := l.Balance(ctx, s)
		mustNoErr(t, err)
		var sum domain.Credits
		for _, v := range b {
			sum += v
		}
		return sum
	}
	pools := func(t *testing.T, l ports.Ledger, s domain.Scope) map[domain.Pool]domain.Credits {
		t.Helper()
		b, err := l.Balance(ctx, s)
		mustNoErr(t, err)
		return b
	}
	grant := func(t *testing.T, l ports.Ledger, s domain.Scope, pool domain.Pool, n domain.Credits) {
		t.Helper()
		key := fmt.Sprintf("seed-%s-%d", pool, seq.Add(1))
		r, err := l.Grant(ctx, domain.Grant{Scope: s, Pool: pool, Amount: n, IdemKey: key, Reason: "purchase"})
		mustNoErr(t, err)
		if !r.Applied {
			t.Fatalf("seed grant did not apply: %+v", r)
		}
	}
	seed := func(t *testing.T, l ports.Ledger, s domain.Scope, n domain.Credits) {
		grant(t, l, s, domain.GeneralPool, n)
	}
	sub := func(name string, f func(t *testing.T)) { t.Run(name, func(t *testing.T) { t.Parallel(); f(t) }) }

	sub("debit is idempotent on idem_key", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 1000)
		c := domain.Charge{Scope: s, Amount: 100, IdemKey: "chg-1", Reason: "query", Resource: "llm.chat"}
		r1, err := e.l.Debit(ctx, c)
		mustNoErr(t, err)
		if !r1.Applied || r1.Delta != -100 || r1.Seq < 1 {
			t.Fatalf("first debit should apply: %+v", r1)
		}
		r2, err := e.l.Debit(ctx, c)
		mustNoErr(t, err)
		if r2.Applied || r2.Seq != r1.Seq || r2.Delta != 0 {
			t.Fatalf("replay must be a no-op reporting the original seq: %+v vs %+v", r2, r1)
		}
		if bal := hot(t, e.l, s); bal != 900 {
			t.Fatalf("double-charged: balance=%d want 900", bal)
		}
	})

	sub("a key delivered concurrently charges exactly once", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 10_000)
		c := domain.Charge{Scope: s, Amount: 100, IdemKey: "race-1", Reason: "query"}
		const n = 32
		var applied atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := e.l.Debit(ctx, c)
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
			t.Fatal(err)
		}
		if applied.Load() != 1 {
			t.Fatalf("%d of %d concurrent deliveries applied; exactly one must (SC-002)", applied.Load(), n)
		}
		if bal := hot(t, e.l, s); bal != 9900 {
			t.Fatalf("balance %d, want 9900", bal)
		}
	})

	sub("a reused key with a different request is a conflict, not a silent no-op", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 1000)
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 100, IdemKey: "k", Reason: "query"})
		mustNoErr(t, err)
		_, err = e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 700, IdemKey: "k", Reason: "query"})
		if !errors.Is(err, domain.ErrIdemConflict) {
			t.Fatalf("want ErrIdemConflict, got %v", err)
		}
		if bal := hot(t, e.l, s); bal != 900 {
			t.Fatalf("a conflicting reuse must change nothing: %d", bal)
		}
	})

	sub("a grant key is idempotent and conflicts on a different amount", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		g := domain.Grant{Scope: s, Amount: 500, IdemKey: "pay-1", Reason: "purchase"}
		r1, err := e.l.Grant(ctx, g)
		mustNoErr(t, err)
		r2, err := e.l.Grant(ctx, g)
		mustNoErr(t, err)
		if !r1.Applied || r2.Applied || r2.Seq != r1.Seq {
			t.Fatalf("grant replay: %+v %+v", r1, r2)
		}
		g.Amount = 501
		if _, err := e.l.Grant(ctx, g); !errors.Is(err, domain.ErrIdemConflict) {
			t.Fatalf("want ErrIdemConflict, got %v", err)
		}
		if bal := hot(t, e.l, s); bal != 500 {
			t.Fatalf("minted twice: %d", bal)
		}
	})

	sub("a usage key and a grant key with the same text do not collide", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 100)
		_, err := e.l.Grant(ctx, domain.Grant{Scope: s, Amount: 50, IdemKey: "x", Reason: "promo"})
		mustNoErr(t, err)
		r, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 10, IdemKey: "x", Reason: "query"})
		mustNoErr(t, err)
		if !r.Applied {
			t.Fatal("a usage key must not be swallowed by a grant key of the same text")
		}
	})

	sub("every mutation advances the sequence by exactly one", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		var prev int64
		for i := 0; i < 5; i++ {
			r, err := e.l.Grant(ctx, domain.Grant{Scope: s, Amount: 10, IdemKey: fmt.Sprintf("g-%d", i), Reason: "promo"})
			mustNoErr(t, err)
			if prev != 0 && r.Seq != prev+1 {
				t.Fatalf("seq jumped %d → %d: the writer's gap detection relies on +1", prev, r.Seq)
			}
			prev = r.Seq
		}
		r, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 1, IdemKey: "u1"})
		mustNoErr(t, err)
		if r.Seq != prev+1 {
			t.Fatalf("a debit must take the next seq: %d after %d", r.Seq, prev)
		}
		if again, _ := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 1, IdemKey: "u1"}); again.Seq != r.Seq {
			t.Fatal("a replay must not consume a sequence number")
		}
	})

	sub("an amount above MaxOperationAmount is refused", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 1)
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: domain.MaxOperationAmount + 1, IdemKey: "big"})
		if !errors.Is(err, domain.ErrAmountOutOfRange) {
			t.Fatalf("want ErrAmountOutOfRange, got %v", err)
		}
		_, err = e.l.Grant(ctx, domain.Grant{Scope: s, Amount: domain.MaxOperationAmount + 1, IdemKey: "big-g", Reason: "purchase"})
		if !errors.Is(err, domain.ErrAmountOutOfRange) {
			t.Fatalf("a grant is bounded too: %v", err)
		}
	})

	sub("admit gates without reserving", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 50)
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}})
		mustNoErr(t, err)
		if !adm.Allowed {
			t.Fatal("positive balance should admit")
		}
		if bal := hot(t, e.l, s); bal != 50 {
			t.Fatalf("admit pre-debited: %d", bal)
		}
	})

	sub("an empty balance is refused with 402", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 5)
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 5, IdemKey: "drain"})
		mustNoErr(t, err)
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}})
		mustNoErr(t, err)
		if adm.Allowed || adm.Exceeded == nil || adm.Exceeded.DenyCode != domain.DenyPaymentRequired {
			t.Fatalf("SC-003: an exhausted ceiling is refused with a classified code: %+v", adm)
		}
	})

	sub("MaxCost bounds overshoot at the gate and reports headroom", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 50)
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}, MaxCost: 10_000})
		mustNoErr(t, err)
		if adm.Allowed || adm.Headroom != 50 {
			t.Fatalf("want refused with headroom 50: %+v", adm)
		}
	})

	sub("promotional credits are drawn before paid ones; general absorbs overshoot", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{Pools: []domain.PoolDef{{Name: "promo", Priority: 10}}}), ws()
		grant(t, e.l, s, "general", 1000)
		grant(t, e.l, s, "promo", 100)
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 150, IdemKey: "p1", Resource: "llm.chat"})
		mustNoErr(t, err)
		if b := pools(t, e.l, s); b["promo"] != 0 || b["general"] != 950 {
			t.Fatalf("wrong draw order: %v", b)
		}
		// Overshoot: nothing left in promo, so general goes further than it holds.
		_, err = e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 2000, IdemKey: "p2", Resource: "llm.chat"})
		mustNoErr(t, err)
		if b := pools(t, e.l, s); b["general"] != -1050 || b["promo"] != 0 {
			t.Fatalf("general must absorb overshoot: %v", b)
		}
	})

	sub("a pool restricted to a resource neither pays for nor admits another", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{Pools: []domain.PoolDef{{Name: "gpu", Priority: 5, AppliesTo: []string{"gpu.job"}}}}), ws()
		grant(t, e.l, s, "gpu", 1000)
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Resource: "llm.chat", Limits: []domain.Limit{balanceOnly}})
		mustNoErr(t, err)
		if adm.Allowed {
			t.Fatal("GPU-only credits must not admit an LLM call")
		}
		adm, _ = e.l.Admit(ctx, s, domain.AdmitRequest{Resource: "gpu.job", Limits: []domain.Limit{balanceOnly}})
		if !adm.Allowed {
			t.Fatal("…but they admit a GPU job")
		}
		_, err = e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 100, IdemKey: "llm-1", Resource: "llm.chat"})
		mustNoErr(t, err)
		if b := pools(t, e.l, s); b["gpu"] != 1000 || b["general"] != -100 {
			t.Fatalf("an LLM call must not draw the GPU pool: %v", b)
		}
	})

	sub("idem keys do not collide across realms", func(t *testing.T) {
		e := setup(t, LedgerOptions{})
		a, b := scope("prod-a", "org"), scope("prod-b", "org")
		_, err := e.l.Grant(ctx, domain.Grant{Scope: a, Amount: 100, IdemKey: "invoice-1", Reason: "purchase"})
		mustNoErr(t, err)
		r, err := e.l.Grant(ctx, domain.Grant{Scope: b, Amount: 100, IdemKey: "invoice-1", Reason: "purchase"})
		mustNoErr(t, err)
		if !r.Applied || hot(t, e.l, b) != 100 {
			t.Fatal("realm B's grant was swallowed by realm A's key")
		}
		if e.h.Drain != nil {
			e.h.Drain(t)
			if e.h.Booked(t, b, domain.GeneralPool) != 100 {
				t.Fatal("realm B's grant was not booked")
			}
		}
	})

	for _, tc := range []struct {
		policy   domain.NegativeBalancePolicy
		start    domain.Credits
		refund   domain.Credits
		wantHot  domain.Credits
		wantWO   domain.Credits
		blocked  bool
		describe string
	}{
		{domain.ClampToZero, 10, -100, 0, 90, false, "clamp_to_zero floors at zero and reports a +90 writeoff"},
		{domain.ClampToZero, -30, -100, -30, 100, false, "a floor never forgives debt that already existed: min(balance_before, 0)"},
		{domain.BlockAndFlag, 10, -100, 0, 90, true, "block_and_flag also refuses spend until an operator clears it"},
		{domain.AllowDebt, 10, -100, -90, 0, false, "allow_debt records the negative and keeps serving"},
	} {
		tc := tc
		sub(tc.describe, func(t *testing.T) {
			e, s := setup(t, LedgerOptions{Policy: tc.policy}), ws()
			if tc.start >= 0 {
				seed(t, e.l, s, tc.start)
			} else {
				seed(t, e.l, s, 1)
				_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 1 - tc.start, IdemKey: "into-debt"})
				mustNoErr(t, err)
			}
			r, err := e.l.Grant(ctx, domain.Grant{Scope: s, Amount: tc.refund, IdemKey: "cb-1", Reason: "chargeback"})
			mustNoErr(t, err)
			if r.Writeoff != tc.wantWO {
				t.Fatalf("writeoff %d, want %d (%+v)", r.Writeoff, tc.wantWO, r)
			}
			if got := hot(t, e.l, s); got != tc.wantHot {
				t.Fatalf("hot balance %d, want %d", got, tc.wantHot)
			}
			adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{{Name: "b", Unit: domain.CreditUnit, Window: domain.Balance, Max: 1_000_000, DenyCode: domain.DenyPaymentRequired}}})
			mustNoErr(t, err)
			if adm.Blocked != tc.blocked {
				t.Fatalf("blocked=%v, want %v: %+v", adm.Blocked, tc.blocked, adm)
			}
		})
	}

	sub("clamp_to_zero books a compensating writeoff so balance == SUM(ledger)", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{Policy: domain.ClampToZero}), ws()
		e.h.needsBooks(t)
		seed(t, e.l, s, 10)
		_, err := e.l.Grant(ctx, domain.Grant{Scope: s, Amount: -100, IdemKey: "cb-1", Reason: "chargeback"})
		mustNoErr(t, err)
		e.h.Drain(t)
		if hot(t, e.l, s) != 0 || !e.h.HasRow(t, s, "writeoff", 90) {
			t.Fatal("clamp must floor and book a +90 writeoff row")
		}
		if e.h.Booked(t, s, domain.GeneralPool) != hot(t, e.l, s) {
			t.Fatal("balance != SUM(ledger) after a clamp")
		}
	})

	sub("a job budget halts work without touching any calendar ceiling", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 1_000_000)
		run := domain.Subjects{"job": "r1"}
		runCap := domain.Limit{Name: "run_cap", Subject: "job", Unit: domain.CreditUnit, Max: 500, Window: domain.Job, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
		for i := 0; i < 5; i++ {
			_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 100, IdemKey: fmt.Sprintf("step-%d", i), Reason: "agent_step", Subjects: run})
			mustNoErr(t, err)
		}
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Subjects: run, Limits: []domain.Limit{runCap}, MaxCost: 100})
		mustNoErr(t, err)
		if adm.Allowed {
			t.Fatal("a runaway loop must halt at its own cap")
		}
		other, _ := e.l.Admit(ctx, s, domain.AdmitRequest{Subjects: domain.Subjects{"job": "r2"}, Limits: []domain.Limit{runCap}, MaxCost: 100})
		if !other.Allowed {
			t.Fatal("another job's budget is untouched")
		}
		if hot(t, e.l, s) != 999_500 {
			t.Fatal("a job budget is a counter, not a granted balance")
		}
	})

	sub("a subject limit without its subject is refused, never skipped", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{}), ws()
		seed(t, e.l, s, 1000)
		daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 10, Window: domain.Daily, DenyCode: domain.DenyLimitReached}
		if _, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{daily}}); !errors.Is(err, domain.ErrMissingSubject) {
			t.Fatalf("a ceiling that silently does not apply is the worst kind: %v", err)
		}
	})

	sub("a subject's daily ceiling is counted per subject, inside the charged scope", func(t *testing.T) {
		daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, WarnAt: 0.8, DenyCode: domain.DenyLimitReached}
		e, s := setup(t, LedgerOptions{Limits: []domain.Limit{daily}}), ws()
		seed(t, e.l, s, 1_000_000)
		u1, u2 := domain.Subjects{"user": "u1"}, domain.Subjects{"user": "u2"}
		gate := func(sj domain.Subjects) domain.Admission {
			adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Subjects: sj, Limits: []domain.Limit{daily}})
			mustNoErr(t, err)
			return adm
		}
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 85, IdemKey: "a", Subjects: u1})
		mustNoErr(t, err)
		if adm := gate(u1); !adm.Allowed || adm.Warning == nil {
			t.Fatalf("85%% of 100 warns but admits: %+v", adm)
		}
		_, err = e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 15, IdemKey: "b", Subjects: u1})
		mustNoErr(t, err)
		if adm := gate(u1); adm.Allowed || adm.Exceeded.DenyCode != domain.DenyLimitReached {
			t.Fatalf("user u1 is at their ceiling: %+v", adm)
		}
		if adm := gate(u2); !adm.Allowed || adm.Warning != nil {
			t.Fatalf("user u2's counter is their own: %+v", adm)
		}
	})

	sub("a daily ceiling resets at midnight in the limit's time zone", func(t *testing.T) {
		daily := domain.Limit{Name: "d", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh", DenyCode: domain.DenyLimitReached}
		// 16:30 UTC is 23:30 in Ho Chi Minh: half an hour before the local day ends.
		e, s := setup(t, LedgerOptions{Limits: []domain.Limit{daily}, Now: time.Date(2026, 9, 29, 16, 30, 0, 0, time.UTC)}), ws()
		seed(t, e.l, s, 1_000_000)
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 100, IdemKey: "spent"})
		mustNoErr(t, err)
		gate := func() bool {
			adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{daily}})
			mustNoErr(t, err)
			return adm.Allowed
		}
		if gate() {
			t.Fatal("the day's allowance is spent")
		}
		e.h.Clock.Advance(20 * time.Minute) // still the same local day (23:50)
		if gate() {
			t.Fatal("still the same local day")
		}
		e.h.Clock.Advance(20 * time.Minute) // 00:10 the next local day
		if !gate() {
			t.Fatal("a new local day has a new allowance")
		}
	})

	sub("a rolling window slides", func(t *testing.T) {
		rolling := domain.Limit{Name: "burst", Unit: domain.CreditUnit, Max: 100, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
		e, s := setup(t, LedgerOptions{Limits: []domain.Limit{rolling}}), ws()
		seed(t, e.l, s, 1_000_000)
		_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 100, IdemKey: "burst-1"})
		mustNoErr(t, err)
		gate := func() bool {
			adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{rolling}})
			mustNoErr(t, err)
			return adm.Allowed
		}
		if gate() {
			t.Fatal("burst allowance spent")
		}
		e.h.Clock.Advance(30 * time.Minute)
		if gate() {
			t.Fatal("half an hour later the burst is still inside the window")
		}
		e.h.Clock.Advance(35 * time.Minute) // > 1h + one bucket since the charge
		if !gate() {
			t.Fatal("past the window the burst has slid out")
		}
	})

	sub("a metered-unit counter sums quantities, not credits", func(t *testing.T) {
		calls := domain.Limit{Name: "api_hourly", Unit: "api_call", Max: 3, Window: domain.Hourly, DenyCode: domain.DenyLimitReached}
		e, s := setup(t, LedgerOptions{Limits: []domain.Limit{calls}}), ws()
		seed(t, e.l, s, 1_000_000)
		for i := 0; i < 3; i++ {
			_, err := e.l.Debit(ctx, domain.Charge{Scope: s, Amount: 5_000, IdemKey: fmt.Sprintf("call-%d", i), Quantities: []domain.Quantity{{Unit: "api_call", Amount: 1}}})
			mustNoErr(t, err)
		}
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{calls}, MaxCost: 999_999})
		mustNoErr(t, err)
		if adm.Allowed {
			t.Fatal("the 4th call in the hour is refused, however many credits each cost")
		}
	})

	sub("a blocked account refuses spend", func(t *testing.T) {
		e, s := setup(t, LedgerOptions{Policy: domain.BlockAndFlag}), ws()
		seed(t, e.l, s, 10)
		_, err := e.l.Grant(ctx, domain.Grant{Scope: s, Amount: -100, IdemKey: "cb", Reason: "chargeback"})
		mustNoErr(t, err)
		seed(t, e.l, s, 1_000) // a later top-up does not unblock it; only an operator does
		adm, err := e.l.Admit(ctx, s, domain.AdmitRequest{Limits: []domain.Limit{balanceOnly}})
		mustNoErr(t, err)
		if adm.Allowed || !adm.Blocked {
			t.Fatalf("block_and_flag holds until an operator clears it: %+v", adm)
		}
	})

	sub("transfer: refuses to overdraw, then applies phase 1 once", func(t *testing.T) {
		e := setup(t, LedgerOptions{})
		from, to := scope("t1", "organization"), scope("t1", "workspace")
		seed(t, e.l, from, 5000)
		if _, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 9000, IdemKey: "over"}); !errors.Is(err, domain.ErrInsufficient) {
			t.Fatalf("a transfer must refuse rather than overdraw: %v", err)
		}
		if hot(t, e.l, from) != 5000 {
			t.Fatal("a refused transfer changes nothing")
		}
		r, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"})
		mustNoErr(t, err)
		if r.Status != domain.TransferInTransit || r.FromBalance != 3000 || !r.Applied {
			t.Fatalf("phase 1: %+v", r)
		}
		r2, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"})
		mustNoErr(t, err)
		if r2.Applied || hot(t, e.l, from) != 3000 {
			t.Fatalf("a replayed allocation must be a no-op: %+v", r2)
		}
		if _, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 1, IdemKey: "alloc-1", Reason: "allocation"}); !errors.Is(err, domain.ErrIdemConflict) {
			t.Fatalf("a reused transfer key with a different amount conflicts: %v", err)
		}
	})

	sub("transfer: a cross-realm transfer is a mint and a burn, never a transfer", func(t *testing.T) {
		e := setup(t, LedgerOptions{})
		from := scope("t1", "organization")
		seed(t, e.l, from, 100)
		_, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: scope("t2", "workspace"), Amount: 1, IdemKey: "xr"})
		if !errors.Is(err, domain.ErrCrossRealm) {
			t.Fatalf("want ErrCrossRealm, got %v", err)
		}
	})

	sub("transfer: MaxDestBalance caps what the destination may hold", func(t *testing.T) {
		e := setup(t, LedgerOptions{})
		from, to := scope("t1", "organization"), scope("t1", "workspace")
		seed(t, e.l, from, 5000)
		seed(t, e.l, to, 900)
		_, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 200, IdemKey: "cap", MaxDestBalance: 1000})
		if !errors.Is(err, domain.ErrDestBalanceCap) {
			t.Fatalf("900 + 200 > 1000: %v", err)
		}
		if hot(t, e.l, from) != 5000 {
			t.Fatal("a capped transfer must not debit the source")
		}
		if _, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 100, IdemKey: "cap-ok", MaxDestBalance: 1000}); err != nil {
			t.Fatalf("900 + 100 == 1000 fits: %v", err)
		}
	})

	sub("transfer: with the writer, both sides apply exactly once and the realm is conserved", func(t *testing.T) {
		e := setup(t, LedgerOptions{})
		e.h.needsBooks(t)
		from, to := scope("t1", "organization"), scope("t1", "workspace")
		seed(t, e.l, from, 5000)
		e.h.Drain(t)
		before := e.h.LedgerSum(t, "t1") + e.h.InTransit(t, "t1")
		_, err := e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"})
		mustNoErr(t, err)
		if mid := e.h.LedgerSum(t, "t1") + e.h.InTransit(t, "t1"); mid != before {
			t.Fatalf("Σ ledger + Σ in-transit must be invariant mid-flight: %d → %d", before, mid)
		}
		e.h.Drain(t)
		if hot(t, e.l, to) != 2000 || e.h.Booked(t, to, domain.GeneralPool) != 2000 || e.h.Booked(t, from, domain.GeneralPool) != 3000 {
			t.Fatal("both sides must apply")
		}
		_, _ = e.l.Transfer(ctx, domain.Transfer{From: from, To: to, Amount: 2000, IdemKey: "alloc-1", Reason: "allocation"})
		e.h.Drain(t)
		if e.h.Booked(t, from, domain.GeneralPool) != 3000 {
			t.Fatal("a replayed allocation must be a no-op")
		}
	})
}
