package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

var balanceOnly = domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Max: 0, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}

func TestEvaluateBalance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		snap     domain.Snapshot
		req      domain.AdmitRequest
		allowed  bool
		headroom domain.Credits
	}{
		{"positive balance admits", domain.Snapshot{Eligible: 50}, domain.AdmitRequest{}, true, 50},
		{"zero balance with no overdraft refuses", domain.Snapshot{Eligible: 0}, domain.AdmitRequest{}, false, 0},
		{"negative balance refuses", domain.Snapshot{Eligible: -5}, domain.AdmitRequest{}, false, 0},
		{"MaxCost above the balance refuses and reports headroom", domain.Snapshot{Eligible: 50}, domain.AdmitRequest{MaxCost: 10_000}, false, 50},
		{"MaxCost within the balance admits", domain.Snapshot{Eligible: 50}, domain.AdmitRequest{MaxCost: 50}, true, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			adm, err := domain.Evaluate(c.req, []domain.Limit{balanceOnly}, c.snap)
			if err != nil {
				t.Fatal(err)
			}
			if adm.Allowed != c.allowed || adm.Headroom != c.headroom {
				t.Fatalf("got %+v, want allowed=%v headroom=%d", adm, c.allowed, c.headroom)
			}
			if !adm.Allowed && (adm.Exceeded == nil || adm.Exceeded.DenyCode != domain.DenyPaymentRequired) {
				t.Fatalf("a refusal must name the ceiling and a deny code: %+v", adm)
			}
		})
	}
}

func TestEvaluateOverdraftIsHeadroom(t *testing.T) {
	t.Parallel()
	overdraft := balanceOnly
	overdraft.Max = 100
	adm, err := domain.Evaluate(domain.AdmitRequest{MaxCost: 40}, []domain.Limit{overdraft}, domain.Snapshot{Eligible: -50})
	if err != nil || !adm.Allowed || adm.Headroom != 50 {
		t.Fatalf("balance -50 with a 100 overdraft has 50 headroom: %+v %v", adm, err)
	}
}

func TestEvaluateBlockedAccountIsRefused(t *testing.T) {
	t.Parallel()
	adm, _ := domain.Evaluate(domain.AdmitRequest{}, []domain.Limit{balanceOnly}, domain.Snapshot{Eligible: 1_000_000, Blocked: true})
	if adm.Allowed || !adm.Blocked || adm.Exceeded.DenyCode != domain.DenyAccountBlocked {
		t.Fatalf("block_and_flag refuses spend until an operator clears it: %+v", adm)
	}
}

func TestEvaluateJobBudgetIsACounterNotABalance(t *testing.T) {
	t.Parallel()
	runCap := domain.Limit{Name: "run_cap", Subject: domain.SubjectJob, Unit: domain.CreditUnit, Max: 500, Window: domain.Job, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
	req := domain.AdmitRequest{Subjects: domain.Subjects{"job": "r1"}, MaxCost: 100}
	rich := domain.Snapshot{Eligible: 1_000_000, Counters: map[string]int64{"run_cap": 500}}
	adm, err := domain.Evaluate(req, []domain.Limit{balanceOnly, runCap}, rich)
	if err != nil || adm.Allowed || adm.Exceeded.Name != "run_cap" {
		t.Fatalf("a runaway loop must halt at its own cap even with a huge balance: %+v %v", adm, err)
	}
	fresh := domain.Snapshot{Eligible: 1_000_000, Counters: map[string]int64{"run_cap": 300}}
	adm, _ = domain.Evaluate(req, []domain.Limit{balanceOnly, runCap}, fresh)
	if !adm.Allowed || adm.Headroom != 200 {
		t.Fatalf("300 spent of 500 leaves 200: %+v", adm)
	}
}

func TestEvaluateRefusesAMissingSubjectNeverSkips(t *testing.T) {
	t.Parallel()
	daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 10, Window: domain.Daily, DenyCode: domain.DenyLimitReached}
	_, err := domain.Evaluate(domain.AdmitRequest{}, []domain.Limit{daily}, domain.Snapshot{})
	if !errors.Is(err, domain.ErrMissingSubject) {
		t.Fatalf("a ceiling that silently does not apply is the worst kind: %v", err)
	}
	// A limit for another resource does not need a subject the call cannot know about.
	daily.Resource = "llm.chat"
	if _, err := domain.Evaluate(domain.AdmitRequest{Resource: "storage"}, []domain.Limit{daily}, domain.Snapshot{Eligible: 1}); err != nil && errors.Is(err, domain.ErrMissingSubject) {
		t.Fatalf("a limit that does not govern this resource must not demand its subject: %v", err)
	}
}

func TestEvaluateWarnsBelowMaxAndRefusesAtMax(t *testing.T) {
	t.Parallel()
	daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, WarnAt: 0.8, DenyCode: domain.DenyLimitReached}
	req := domain.AdmitRequest{Subjects: domain.Subjects{"user": "u1"}}
	at := func(used int64) domain.Admission {
		adm, err := domain.Evaluate(req, []domain.Limit{balanceOnly, daily}, domain.Snapshot{Eligible: 1000, Counters: map[string]int64{"user_daily": used}})
		if err != nil {
			t.Fatal(err)
		}
		return adm
	}
	if a := at(50); !a.Allowed || a.Warning != nil {
		t.Fatalf("50%% is silent: %+v", a)
	}
	if a := at(80); !a.Allowed || a.Warning == nil || a.Warning.Name != "user_daily" {
		t.Fatalf("80%% warns: %+v", a)
	}
	if a := at(100); a.Allowed || a.Exceeded.DenyCode != domain.DenyLimitReached || a.Warning != nil {
		t.Fatalf("100%% refuses, and a refusal carries no warning: %+v", a)
	}
}

func TestEvaluateMeteredUnitLimitRefusesAtItsMax(t *testing.T) {
	t.Parallel()
	calls := domain.Limit{Name: "api_hourly", Subject: "api_key", Unit: "api_call", Max: 3, Window: domain.Hourly, DenyCode: domain.DenyLimitReached}
	req := domain.AdmitRequest{Subjects: domain.Subjects{"api_key": "k7"}, MaxCost: 999_999}
	adm, _ := domain.Evaluate(req, []domain.Limit{calls}, domain.Snapshot{Counters: map[string]int64{"api_hourly": 2}})
	if !adm.Allowed {
		t.Fatalf("MaxCost is in credits and must not be compared with a call counter: %+v", adm)
	}
	adm, _ = domain.Evaluate(req, []domain.Limit{calls}, domain.Snapshot{Counters: map[string]int64{"api_hourly": 3}})
	if adm.Allowed {
		t.Fatalf("the 4th call in the hour is refused: %+v", adm)
	}
}

func TestMergeLimitsOnlyTightens(t *testing.T) {
	t.Parallel()
	configured := []domain.Limit{
		{Name: "a", Max: 100, DenyCode: domain.DenyLimitReached},
		{Name: "b", Max: 100, DenyCode: domain.DenyLimitReached},
	}
	caller := []domain.Limit{
		{Name: "a", Max: 50},                              // tighter: wins
		{Name: "b", Max: 500, DenyCode: "something else"}, // looser: ignored, nothing else taken
		{Name: "run_cap", Max: 7, Window: domain.Job},     // new: appended
	}
	got := domain.MergeLimits(configured, caller)
	if len(got) != 3 || got[0].Max != 50 || got[1].Max != 100 || got[1].DenyCode != domain.DenyLimitReached || got[2].Name != "run_cap" {
		t.Fatalf("%+v", got)
	}
	if configured[0].Max != 100 {
		t.Fatal("merging must not mutate the realm's configured limits")
	}
}

func TestLimitValidate(t *testing.T) {
	t.Parallel()
	good := domain.Limit{Name: "d", Unit: domain.CreditUnit, Max: 1, Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh", DenyCode: domain.DenyLimitReached}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(*domain.Limit)) domain.Limit { l := good; f(&l); return l }
	bad := map[string]domain.Limit{
		"no name":              mut(func(l *domain.Limit) { l.Name = "" }),
		"negative max":         mut(func(l *domain.Limit) { l.Max = -1 }),
		"rolling no duration":  mut(func(l *domain.Limit) { l.Window = domain.Rolling }),
		"balance with subject": mut(func(l *domain.Limit) { l.Window = domain.Balance; l.Subject = "user" }),
		"job on a user":        mut(func(l *domain.Limit) { l.Window = domain.Job; l.Dur = time.Hour; l.Subject = "user" }),
		"bad warn":             mut(func(l *domain.Limit) { l.WarnAt = 1.5 }),
		"bad deny code":        mut(func(l *domain.Limit) { l.DenyCode = "nope" }),
		"unknown zone":         mut(func(l *domain.Limit) { l.TZ = "Mars/Olympus" }),
		"no unit":              mut(func(l *domain.Limit) { l.Unit = "" }),
	}
	for name, l := range bad {
		if err := l.Validate(); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestParseWindowRoundTrips(t *testing.T) {
	t.Parallel()
	for _, w := range []domain.Window{domain.Balance, domain.Daily, domain.Hourly, domain.Rolling, domain.Job} {
		got, err := domain.ParseWindow(w.String())
		if err != nil || got != w {
			t.Errorf("%v: %v %v", w, got, err)
		}
	}
	if _, err := domain.ParseWindow("weekly"); err == nil {
		t.Fatal("an unknown window kind must be refused")
	}
}
