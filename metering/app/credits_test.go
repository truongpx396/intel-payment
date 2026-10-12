package app_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The credits snapshot (FR-036, credits-ui-ports.md): the whole of what a credits panel draws, in one read,
// built from the SAME limits Admit enforces — so a ceiling's cap on the panel is the cap that refuses a call.

func credits(t *testing.T, m ports.Meter) ports.CreditsReader {
	t.Helper()
	c, ok := m.(ports.CreditsReader)
	if !ok {
		t.Fatal("the Meter the engine returns is not a ports.CreditsReader: GET /v1/credits has nothing to build a snapshot with")
	}
	return c
}

func pools(m map[domain.Pool]domain.Credits) func(domain.Scope) (map[domain.Pool]domain.Credits, error) {
	return func(domain.Scope) (map[domain.Pool]domain.Credits, error) { return m, nil }
}

func snapshotRig(t *testing.T, o rigOpt, bal map[domain.Pool]domain.Credits) (*rig, ports.CreditsReader) {
	t.Helper()
	if o.usage == nil {
		o.usage = &fakeUsage{}
	}
	if o.ledger == nil {
		o.ledger = &fakeLedger{}
	}
	r := newRig(t, o)
	r.bal.balance = pools(bal)
	return r, credits(t, r.m)
}

func ask(t *testing.T, c ports.CreditsReader, q ports.SnapshotQuery) ports.CreditsSnapshot {
	t.Helper()
	if q.Scope == (domain.Scope{}) {
		q.Scope = ws
	}
	snap, err := c.Credits(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Invariant 5: totals shown together reconcile. The pools are the parts, remaining is the total, and they close.
func TestThePoolsAreInDrawOrderAndSumToTheRemainder(t *testing.T) {
	t.Parallel()
	defs := []domain.PoolDef{{Name: "promo", Priority: 10}, {Name: "gpu", Priority: 5, AppliesTo: []string{"gpu.job", "gpu.batch"}}, {Name: "bonus", Priority: 10}}
	for name, bal := range map[string]map[domain.Pool]domain.Credits{
		"ordinary":                       {domain.GeneralPool: 700, "promo": 30, "gpu": 5, "bonus": 1},
		"general absorbed an overshoot":  {domain.GeneralPool: -1050, "promo": 0, "gpu": 0, "bonus": 0},
		"a pool the realm no longer has": {domain.GeneralPool: 10, "retired": 90},
		"nothing at all":                 {},
		"credit and debt":                {domain.GeneralPool: 500, "promo": -20},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, c := snapshotRig(t, rigOpt{pools: defs}, bal)
			snap := ask(t, c, ports.SnapshotQuery{})
			var sum domain.Credits
			for _, p := range snap.Balance.Pools {
				sum += p.Remaining
			}
			if sum != snap.Balance.Remaining {
				t.Fatalf("the parts (%d) must sum to the total (%d): %+v", sum, snap.Balance.Remaining, snap.Balance.Pools)
			}
			var want domain.Credits
			for _, v := range bal {
				want += v
			}
			if snap.Balance.Remaining != want {
				t.Fatalf("remaining = %d, want %d", snap.Balance.Remaining, want)
			}
		})
	}

	t.Run("draw order: by priority then name, general last, an unknown pool after the configured", func(t *testing.T) {
		t.Parallel()
		_, c := snapshotRig(t, rigOpt{pools: defs}, map[domain.Pool]domain.Credits{domain.GeneralPool: 1, "promo": 2, "gpu": 3, "bonus": 4, "retired": 5})
		snap := ask(t, c, ports.SnapshotQuery{})
		var keys []string
		for _, p := range snap.Balance.Pools {
			keys = append(keys, p.Key)
		}
		want := []string{"gpu", "bonus", "promo", "general", "retired"}
		if len(keys) != len(want) {
			t.Fatalf("%v", keys)
		}
		for i := range want {
			if keys[i] != want[i] {
				t.Fatalf("%v, want %v", keys, want)
			}
		}
		if g := snap.Balance.Pools[0]; g.Key != "gpu" || g.AppliesTo != "gpu.job,gpu.batch" || g.Remaining != 3 {
			t.Fatalf("a pool restricted to resources says which: %+v", g)
		}
	})

	t.Run("a total that would wrap is an error, not a wrong number", func(t *testing.T) {
		t.Parallel()
		_, c := snapshotRig(t, rigOpt{}, map[domain.Pool]domain.Credits{domain.GeneralPool: math.MaxInt64, "promo": 1})
		if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); err == nil {
			t.Fatal("an overflowing sum was reported")
		}
	})
}

func TestGrantedIsWhatTheBooksSayWasGranted(t *testing.T) {
	t.Parallel()
	led := &fakeLedger{granted: func(domain.Scope) (domain.Credits, error) { return 15_000, nil }}
	_, c := snapshotRig(t, rigOpt{ledger: led}, map[domain.Pool]domain.Credits{domain.GeneralPool: 12_480})
	if snap := ask(t, c, ports.SnapshotQuery{}); snap.Balance.Granted != 15_000 || snap.Balance.Remaining != 12_480 {
		t.Fatalf("%+v", snap.Balance)
	}
}

// Every field of a limit view comes from the kernel's Limit — no translation table on either side.
func TestEachLimitIsAViewOfTheKernelsLimit(t *testing.T) {
	t.Parallel()
	limits := []domain.Limit{
		{Name: "scope_balance", Unit: domain.CreditUnit, Max: 100, Window: domain.Balance, WarnAt: 0.8, DenyCode: domain.DenyPaymentRequired},
		{Name: "org_daily", Unit: domain.CreditUnit, Max: 1_000_000, Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh", WarnAt: 0.75, DenyCode: domain.DenyLimitReached},
		{Name: "api_hourly", Unit: "api_call", Max: 100, Window: domain.Hourly, WarnAt: 0.5, DenyCode: domain.DenyLimitReached},
		{Name: "org_rolling", Unit: domain.CreditUnit, Max: 5000, Window: domain.Rolling, Dur: 24 * time.Hour, WarnAt: 0.99, DenyCode: domain.DenyLimitReached},
		{Name: "no_warning", Unit: domain.CreditUnit, Max: 10, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached},
	}
	usage := &fakeUsage{fn: func(domain.Scope, domain.Subjects, []domain.Limit) (map[string]int64, error) {
		return map[string]int64{"org_daily": 870, "api_hourly": 3, "org_rolling": 4000, "no_warning": 0}, nil
	}}
	_, c := snapshotRig(t, rigOpt{limits: limits, usage: usage}, map[domain.Pool]domain.Credits{domain.GeneralPool: 50})
	snap := ask(t, c, ports.SnapshotQuery{})
	byKey := map[string]ports.LimitView{}
	for _, l := range snap.Limits {
		byKey[l.Key] = l
	}
	if len(snap.Limits) != 5 || snap.Limits[0].Key != "scope_balance" || snap.Limits[1].Key != "org_daily" {
		t.Fatalf("the realm's order, count included: %+v", snap.Limits)
	}
	for key, want := range map[string]struct {
		window string
		used   int64
		cap    int64
		warn   int
		deny   string
		resets string
	}{
		"scope_balance": {"none", 0, 100, 80, "payment_required", ""},
		"org_daily":     {"day", 870, 1_000_000, 75, "limit_reached", "2026-09-29T17:00:00Z"}, // the limit's own midnight (UTC+7), from the rig's clock
		"api_hourly":    {"hour", 3, 100, 50, "limit_reached", "2026-09-29T13:00:00Z"},
		"org_rolling":   {"period", 4000, 5000, 99, "limit_reached", ""},
		"no_warning":    {"period", 0, 10, 0, "limit_reached", ""},
	} {
		got := byKey[key]
		if got.Window != want.window || got.Used != want.used || got.Cap == nil || *got.Cap != want.cap || got.WarnAtPct != want.warn || got.DenyCode != want.deny {
			t.Fatalf("%s: %+v, want %+v", key, got, want)
		}
		if want.resets == "" && got.ResetsAt != nil {
			t.Fatalf("%s resets at %v: only a calendar window has one", key, got.ResetsAt)
		}
		if want.resets != "" && (got.ResetsAt == nil || !got.ResetsAt.Equal(mustParse(t, want.resets))) {
			t.Fatalf("%s resets at %v, want %s", key, got.ResetsAt, want.resets)
		}
	}
	if snap.Balance.PrimaryLimitKey != "scope_balance" {
		t.Fatalf("the hero mirrors the balance: %q", snap.Balance.PrimaryLimitKey)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A balance has no counter: what it has used is the overdraft it is into.
func TestABalanceLimitHasUsedTheOverdraftItIsInto(t *testing.T) {
	t.Parallel()
	l := []domain.Limit{{Name: "scope_balance", Unit: domain.CreditUnit, Max: 100, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}}
	for name, tc := range map[string]struct {
		bal  map[domain.Pool]domain.Credits
		used int64
	}{
		"in credit":             {map[domain.Pool]domain.Credits{domain.GeneralPool: 50}, 0},
		"exactly empty":         {map[domain.Pool]domain.Credits{domain.GeneralPool: 0}, 0},
		"60 into the overdraft": {map[domain.Pool]domain.Credits{domain.GeneralPool: -60}, 60},
		"debt across pools":     {map[domain.Pool]domain.Credits{domain.GeneralPool: -60, "promo": 20}, 40},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, c := snapshotRig(t, rigOpt{limits: l}, tc.bal)
			if got := ask(t, c, ports.SnapshotQuery{}).Limits[0]; got.Used != tc.used {
				t.Fatalf("used = %d, want %d", got.Used, tc.used)
			}
		})
	}
}

// A ceiling sized by a plan shows the plan's size: the cap on the panel is the cap Admit enforces.
func TestAnEntitlementSizedLimitShowsTheSizeAdmitEnforces(t *testing.T) {
	t.Parallel()
	sized := domain.Limit{Name: "plan_cap", Unit: domain.CreditUnit, Max: 100, Window: domain.Rolling, Dur: time.Hour, MaxEntitlement: "plan_cap_credits", DenyCode: domain.DenyLimitReached}
	t.Run("the plan's quota", func(t *testing.T) {
		t.Parallel()
		_, c := snapshotRig(t, rigOpt{limits: []domain.Limit{sized}, quotas: quota{max: 700}}, nil)
		if l := ask(t, c, ports.SnapshotQuery{}).Limits; len(l) != 1 || l[0].Cap == nil || *l[0].Cap != 700 {
			t.Fatalf("%+v", l)
		}
	})
	t.Run("nothing grants it: the row's own max", func(t *testing.T) {
		t.Parallel()
		_, c := snapshotRig(t, rigOpt{limits: []domain.Limit{sized}, quotas: quota{err: domain.ErrQuotaNotGranted}}, nil)
		if l := ask(t, c, ports.SnapshotQuery{}).Limits; len(l) != 1 || *l[0].Cap != 100 {
			t.Fatalf("%+v", l)
		}
	})
	t.Run("unlimited: no ceiling to draw", func(t *testing.T) {
		t.Parallel()
		_, c := snapshotRig(t, rigOpt{limits: []domain.Limit{sized}, quotas: quota{max: ports.Unlimited}}, nil)
		if l := ask(t, c, ports.SnapshotQuery{}).Limits; len(l) != 0 {
			t.Fatalf("%+v", l)
		}
	})
	t.Run("a quota that cannot be resolved fails closed, as Admit does", func(t *testing.T) {
		t.Parallel()
		_, c := snapshotRig(t, rigOpt{limits: []domain.Limit{sized}, quotas: quota{err: errors.New("entitlement store down")}}, nil)
		if _, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); !errors.Is(err, domain.ErrQuotaUnavailable) {
			t.Fatalf("%v", err)
		}
	})
}

// A member must not learn colleagues' usage from a credits screen.
func memberRig(t *testing.T) (*fakeUsage, *fakeLedger, ports.CreditsReader) {
	t.Helper()
	limits := []domain.Limit{
		{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired},
		{Name: "org_daily", Unit: domain.CreditUnit, Max: 1000, Window: domain.Daily, DenyCode: domain.DenyLimitReached},
		{Name: "user_cap", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached},
		{Name: "key_cap", Subject: "api_key", Unit: domain.CreditUnit, Max: 100, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached},
		{Name: "run_cap", Subject: "job", Unit: domain.CreditUnit, Max: 100, Window: domain.Job, Dur: time.Hour, DenyCode: domain.DenyLimitReached},
	}
	usage := &fakeUsage{fn: func(_ domain.Scope, subj domain.Subjects, _ []domain.Limit) (map[string]int64, error) {
		out := map[string]int64{"org_daily": 900}
		if subj["user"] == "u1" {
			out["user_cap"] = 40
		}
		return out, nil
	}}
	led := &fakeLedger{rows: func(q ports.LedgerQuery) (ports.LedgerPage, error) {
		return ports.LedgerPage{Next: "n", Rows: []ports.LedgerEntry{{ID: "r1", LedgerRow: ports.LedgerRow{Scope: q.Scope, Delta: -10, OperationType: "query"}}}}, nil
	}}
	_, c := snapshotRig(t, rigOpt{limits: limits, usage: usage, ledger: led}, map[domain.Pool]domain.Credits{domain.GeneralPool: 5})
	return usage, led, c
}

func TestAMembersSnapshotHoldsOnlyTheMembersCountersAndRows(t *testing.T) {
	t.Parallel()

	t.Run("the unscoped view: the scope's own ceilings, no hints, no member's counters", func(t *testing.T) {
		t.Parallel()
		_, led, c := memberRig(t)
		got := ask(t, c, ports.SnapshotQuery{Scope: ws, LedgerLimit: 20, LedgerAfter: "cur"})
		var keys []string
		for _, l := range got.Limits {
			keys = append(keys, l.Key)
			if l.ScopeHint != "" {
				t.Fatalf("%s: a hint in the admin view: %q", l.Key, l.ScopeHint)
			}
		}
		if len(keys) != 2 || keys[0] != "scope_balance" || keys[1] != "org_daily" {
			t.Fatalf("a member's ceiling has no single figure to show for the scope: %v", keys)
		}
		q := led.gotQ[len(led.gotQ)-1]
		if q.SubjectKind != "" || q.SubjectID != "" || q.Limit != 20 || q.After != "cur" || q.Scope != ws {
			t.Fatalf("%+v", q)
		}
		if got.LedgerNext != "n" || len(got.Ledger) != 1 {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("a member's view: their ceilings, hinted, with their counters, and their rows only", func(t *testing.T) {
		t.Parallel()
		usage, led, c := memberRig(t)
		got := ask(t, c, ports.SnapshotQuery{Scope: ws, SubjectKind: "user", SubjectID: "u1"})
		hint, used := map[string]string{}, map[string]int64{}
		for _, l := range got.Limits {
			hint[l.Key], used[l.Key] = l.ScopeHint, l.Used
		}
		if len(got.Limits) != 3 || hint["org_daily"] != "all members" || hint["user_cap"] != "you" || used["user_cap"] != 40 || used["org_daily"] != 900 {
			t.Fatalf("%+v", got.Limits)
		}
		if _, other := hint["key_cap"]; other {
			t.Fatal("another kind of subject's ceiling is not this member's")
		}
		if _, job := hint["run_cap"]; job {
			t.Fatal("a job budget is not a member's")
		}
		q := led.gotQ[len(led.gotQ)-1]
		if q.SubjectKind != "user" || q.SubjectID != "u1" {
			t.Fatalf("the ledger must be read for this member only: %+v", q)
		}
		last := usage.calls[len(usage.calls)-1]
		if last.subjects["user"] != "u1" || len(last.subjects) != 1 {
			t.Fatalf("counters are read for this member only: %+v", last)
		}
		for _, n := range last.limits {
			if n == "scope_balance" || n == "key_cap" || n == "run_cap" {
				t.Fatalf("limit %s has no counter to read for this view", n)
			}
		}
	})

	t.Run("half a member is invalid, and nothing is read", func(t *testing.T) {
		t.Parallel()
		_, led, c := memberRig(t)
		for _, q := range []ports.SnapshotQuery{{Scope: ws, SubjectKind: "user"}, {Scope: ws, SubjectID: "u1"}} {
			if _, err := c.Credits(context.Background(), q); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("%v", err)
			}
		}
		if len(led.gotQ) != 0 {
			t.Fatal("the ledger was read for half a member")
		}
	})
}

func TestTheSnapshotHealsAColdAccountAndNeverGuessesDuringAnOutage(t *testing.T) {
	t.Parallel()
	t.Run("a cold account is rebuilt, then read — the balance and the counters", func(t *testing.T) {
		t.Parallel()
		limits := []domain.Limit{{Name: "org_daily", Unit: domain.CreditUnit, Max: 1000, Window: domain.Daily, DenyCode: domain.DenyLimitReached}}
		cold := true
		usage := &fakeUsage{fn: func(domain.Scope, domain.Subjects, []domain.Limit) (map[string]int64, error) {
			if cold {
				return nil, domain.ErrColdScope
			}
			return map[string]int64{"org_daily": 12}, nil
		}}
		r, c := snapshotRig(t, rigOpt{limits: limits, usage: usage}, map[domain.Pool]domain.Credits{domain.GeneralPool: 5})
		r.bal.rehyd = func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
			cold = false
			return ports.RehydrateInstalled, nil
		}
		if got := ask(t, c, ports.SnapshotQuery{}); got.Limits[0].Used != 12 {
			t.Fatalf("%+v", got.Limits)
		}
	})
	for name, set := range map[string]func(*rig, *fakeUsage, *fakeLedger){
		"the balance": func(r *rig, _ *fakeUsage, _ *fakeLedger) {
			r.bal.balance = func(domain.Scope) (map[domain.Pool]domain.Credits, error) { return nil, domain.ErrHotStoreUnavailable }
		},
		"the counters": func(_ *rig, u *fakeUsage, _ *fakeLedger) {
			u.fn = func(domain.Scope, domain.Subjects, []domain.Limit) (map[string]int64, error) {
				return nil, domain.ErrShardFrozen
			}
		},
		"the ledger": func(_ *rig, _ *fakeUsage, l *fakeLedger) {
			l.rows = func(ports.LedgerQuery) (ports.LedgerPage, error) {
				return ports.LedgerPage{}, domain.ErrHotStoreUnavailable
			}
		},
	} {
		t.Run("an outage of "+name+" is an error, not a zero", func(t *testing.T) {
			t.Parallel()
			u, l := &fakeUsage{}, &fakeLedger{}
			limits := []domain.Limit{{Name: "org_daily", Unit: domain.CreditUnit, Max: 1000, Window: domain.Daily, DenyCode: domain.DenyLimitReached}}
			r, c := snapshotRig(t, rigOpt{limits: limits, usage: u, ledger: l}, map[domain.Pool]domain.Credits{domain.GeneralPool: 5})
			set(r, u, l)
			snap, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: ws})
			if err == nil || len(snap.Limits) != 0 || len(snap.Balance.Pools) != 0 {
				t.Fatalf("a half-built snapshot is a wrong one: %+v, %v", snap, err)
			}
		})
	}
}

func TestASnapshotNeedsItsReadersAndAValidScope(t *testing.T) {
	t.Parallel()
	t.Run("the readers are not wired: an error, not a panic", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, rigOpt{})
		r.bal.balance = pools(nil)
		if _, err := credits(t, r.m).Credits(context.Background(), ports.SnapshotQuery{Scope: ws}); err == nil {
			t.Fatal("a snapshot with no ledger or usage reader cannot be whole")
		}
	})
	t.Run("a malformed scope is invalid before anything is read", func(t *testing.T) {
		t.Parallel()
		r, c := snapshotRig(t, rigOpt{}, nil)
		_, err := c.Credits(context.Background(), ports.SnapshotQuery{Scope: domain.Scope{Realm: "t1", Kind: "Bad Kind", ID: "x"}})
		if !errors.Is(err, domain.ErrInvalid) || len(r.bal.balanced) != 0 {
			t.Fatalf("%v (hot reads %d)", err, len(r.bal.balanced))
		}
	})
}
