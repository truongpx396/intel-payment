package contracts

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// What an operator reads to see whether the two copies of a balance agree (R112a): the recent reconcile runs, and what they found in one realm.
// The suite is the specification of ports.DriftReader. The runs are written the way the writer's reconcile writes them (SaveReconcile), so a
// reader that reads something the writer does not write fails here and not in production.

// DriftWorld is a reader under test and the writer's own way of leaving a run behind for it to read.
type DriftWorld struct {
	Reader ports.DriftReader
	Save   func(tb testing.TB, r ports.ReconcileRecord)
}

// DriftFactory builds a world with no runs in it: a case that counts runs must not see another's.
type DriftFactory func(tb testing.TB) DriftWorld

// DriftCase is one named obligation of ports.DriftReader.
type DriftCase struct {
	Name string
	Run  func(tb testing.TB, f DriftFactory)
}

// The names of the cases, so a test that plants a defect can say which case must catch it.
const (
	CaseDRWhole          = "a run is read back whole: its counters, kind and times, and its realm's findings with every field"
	CaseDRNewestFirst    = "runs come newest first, the limit is honoured, and none given is the default"
	CaseDRLimitBounds    = "a limit below one or above the most is refused"
	CaseDRShard          = "a shard narrows the runs to that shard's, and shard zero is a shard"
	CaseDRRealm          = "another realm's findings are never in the answer, nor in the count of them"
	CaseDRWithFindings   = "asked for runs with findings, a realm gets only the runs that found something of its own"
	CaseDRWorstFirst     = "findings come worst first, capped, with how many there were, and a plus and a minus are two"
	CaseDREmptyAndRealms = "an empty store is an empty answer, and a realm that is not one is refused"
)

// DriftCases are the obligations of the drift reader.
func DriftCases() []DriftCase {
	return []DriftCase{
		{CaseDRWhole, drWhole},
		{CaseDRNewestFirst, drNewestFirst},
		{CaseDRLimitBounds, drLimitBounds},
		{CaseDRShard, drShard},
		{CaseDRRealm, drRealm},
		{CaseDRWithFindings, drWithFindings},
		{CaseDRWorstFirst, drWorstFirst},
		{CaseDREmptyAndRealms, drEmptyAndRealms},
	}
}

// DriftContract runs every case, in parallel, each against a world of its own.
func DriftContract(t *testing.T, f DriftFactory) {
	t.Helper()
	for _, c := range DriftCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

var drBase = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func drFinding(realm domain.Realm, id string, pool domain.Pool, drift domain.Credits) domain.ReconcileFinding {
	return domain.ReconcileFinding{
		Scope: domain.Scope{Realm: realm, Kind: "workspace", ID: id}, Pool: pool, Outcome: domain.OutcomeDrift, HotSeq: 41, AppliedSeq: 40,
		Hot: 1000 + drift, Expected: 1000, Drift: drift, Healed: drift < 0, Alarmed: true,
	}
}

func drRun(shard int, kind domain.ReconcileKind, started time.Time, findings ...domain.ReconcileFinding) ports.ReconcileRecord {
	run := domain.ReconcileRun{Shard: domain.Shard(shard), Kind: kind, Checked: 10 + len(findings), InSync: 10, Deferred: 1, Cold: 2, Regressions: 3, Drifted: len(findings), Findings: findings}
	for _, f := range findings {
		d := f.Drift
		if d < 0 {
			d = -d
		}
		run.AbsDrift += d
	}
	return ports.ReconcileRecord{Run: run, StartedAt: started}
}

func drRead(tb testing.TB, w DriftWorld, q ports.DriftQuery) []ports.DriftRun {
	tb.Helper()
	runs, err := w.Reader.RecentRuns(context.Background(), q)
	if err != nil {
		tb.Fatalf("RecentRuns(%+v): %v", q, err)
	}
	return runs
}

func drWhole(tb testing.TB, f DriftFactory) {
	w := f(tb)
	a, b := drFinding("t1", "w1", "general", -100), drFinding("t1", "w2", "promo", 30)
	rec := drRun(3, domain.ReconcileFull, drBase, a, b)
	w.Save(tb, rec)
	runs := drRead(tb, w, ports.DriftQuery{Realm: "t1"})
	if len(runs) != 1 {
		tb.Fatalf("%d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.ID == "" || r.Shard != 3 || r.Kind != domain.ReconcileFull || !r.StartedAt.Equal(drBase) || r.FinishedAt == nil || r.Checked != 12 || r.InSync != 10 || r.Deferred != 1 ||
		r.Cold != 2 || r.Regressions != 3 || r.Drifted != 2 || r.AbsDrift != 130 || r.FindingsTotal != 2 {
		tb.Fatalf("the run came back as %+v, want %+v", r, rec.Run)
	}
	if len(r.Findings) != 2 || r.Findings[0] != a || r.Findings[1] != b {
		tb.Fatalf("the findings came back as %+v, want %+v then %+v: every field, worst first", r.Findings, a, b)
	}
}

func drNewestFirst(tb testing.TB, f DriftFactory) {
	w := f(tb)
	for i := range 12 {
		w.Save(tb, drRun(i%4, domain.ReconcileIncremental, drBase.Add(time.Duration(i)*time.Minute)))
	}
	runs := drRead(tb, w, ports.DriftQuery{Realm: "t1", Limit: 3})
	if len(runs) != 3 {
		tb.Fatalf("a limit of 3 gave %d runs", len(runs))
	}
	for i, want := range []int{11, 10, 9} {
		if at := drBase.Add(time.Duration(want) * time.Minute); !runs[i].StartedAt.Equal(at) {
			tb.Fatalf("run %d started at %s, want %s: newest first", i, runs[i].StartedAt, at)
		}
	}
	if all := drRead(tb, w, ports.DriftQuery{Realm: "t1"}); len(all) != ports.DefaultDriftRuns {
		tb.Fatalf("no limit gave %d runs, want the default, %d", len(all), ports.DefaultDriftRuns)
	}
	if one := drRead(tb, w, ports.DriftQuery{Realm: "t1", Limit: 1}); len(one) != 1 || !one[0].StartedAt.Equal(drBase.Add(11*time.Minute)) {
		tb.Fatalf("a limit of 1 gave %+v", one)
	}
}

func drLimitBounds(tb testing.TB, f DriftFactory) {
	w := f(tb)
	for _, n := range []int{-1, ports.MaxDriftRuns + 1} {
		if runs, err := w.Reader.RecentRuns(context.Background(), ports.DriftQuery{Realm: "t1", Limit: n}); !errors.Is(err, domain.ErrInvalid) || runs != nil {
			tb.Fatalf("a limit of %d: %v, %v, want %v", n, runs, err, domain.ErrInvalid)
		}
	}
	if _, err := w.Reader.RecentRuns(context.Background(), ports.DriftQuery{Realm: "t1", Limit: ports.MaxDriftRuns}); err != nil {
		tb.Fatalf("the largest limit is refused: %v", err)
	}
}

func drShard(tb testing.TB, f DriftFactory) {
	w := f(tb)
	for i := range 6 {
		w.Save(tb, drRun(i%3, domain.ReconcileIncremental, drBase.Add(time.Duration(i)*time.Minute)))
	}
	two, zero, minus := 2, 0, -1
	if runs := drRead(tb, w, ports.DriftQuery{Realm: "t1", Shard: &two}); len(runs) != 2 || runs[0].Shard != 2 || runs[1].Shard != 2 {
		tb.Fatalf("shard 2 gave %+v", runs)
	}
	if runs := drRead(tb, w, ports.DriftQuery{Realm: "t1", Shard: &zero}); len(runs) != 2 || runs[0].Shard != 0 || runs[1].Shard != 0 {
		tb.Fatalf("shard 0 gave %+v: zero is a shard, not no shard", runs)
	}
	if runs, err := w.Reader.RecentRuns(context.Background(), ports.DriftQuery{Realm: "t1", Shard: &minus}); !errors.Is(err, domain.ErrInvalid) || runs != nil {
		tb.Fatalf("shard -1: %v, %v, want %v", runs, err, domain.ErrInvalid)
	}
	if runs := drRead(tb, w, ports.DriftQuery{Realm: "t1"}); len(runs) != 6 {
		tb.Fatalf("no shard gave %d runs, want all 6", len(runs))
	}
}

func drRealm(tb testing.TB, f DriftFactory) {
	w := f(tb)
	w.Save(tb, drRun(1, domain.ReconcileFull, drBase,
		drFinding("t1", "mine-1", "general", -100), drFinding("t1", "mine-2", "general", -5),
		drFinding("t2", "secret-1", "general", 700), drFinding("t2", "secret-2", "promo", 9), drFinding("t2", "secret-3", "promo", -2)))
	for _, c := range []struct {
		realm domain.Realm
		want  int
	}{{"t1", 2}, {"t2", 3}, {"t3", 0}} {
		runs := drRead(tb, w, ports.DriftQuery{Realm: string(c.realm)})
		if len(runs) != 1 {
			tb.Fatalf("realm %s: %d runs, want the one run, whatever it found elsewhere", c.realm, len(runs))
		}
		r := runs[0]
		if len(r.Findings) != c.want || r.FindingsTotal != c.want {
			tb.Fatalf("realm %s: %d findings listed and %d counted, want %d of its own", c.realm, len(r.Findings), r.FindingsTotal, c.want)
		}
		for _, x := range r.Findings {
			if x.Scope.Realm != c.realm {
				tb.Fatalf("realm %s was shown %+v", c.realm, x)
			}
		}
		// The counters are the run's own, for every realm: they carry no account, and they say whether the shard is healthy.
		if r.Checked != 15 || r.Drifted != 5 || r.AbsDrift != 816 {
			tb.Fatalf("realm %s: the run's counters are %+v", c.realm, r)
		}
	}
}

func drWithFindings(tb testing.TB, f DriftFactory) {
	w := f(tb)
	w.Save(tb, drRun(1, domain.ReconcileIncremental, drBase, drFinding("t1", "w1", "general", -1)))
	w.Save(tb, drRun(2, domain.ReconcileIncremental, drBase.Add(time.Minute), drFinding("t2", "x1", "general", -1)))
	w.Save(tb, drRun(3, domain.ReconcileIncremental, drBase.Add(2*time.Minute)))
	only := drRead(tb, w, ports.DriftQuery{Realm: "t1", WithFindings: true})
	if len(only) != 1 || only[0].Shard != 1 || len(only[0].Findings) != 1 {
		tb.Fatalf("with findings, realm t1 got %+v, want only the run that found something of its own", only)
	}
	if all := drRead(tb, w, ports.DriftQuery{Realm: "t1"}); len(all) != 3 {
		tb.Fatalf("without the filter realm t1 got %d runs, want all 3", len(all))
	}
	if none := drRead(tb, w, ports.DriftQuery{Realm: "t3", WithFindings: true}); len(none) != 0 {
		tb.Fatalf("realm t3 found nothing and was given %+v", none)
	}
	// The limit counts runs that qualify: the newest run that found nothing of this realm's does not use up a place.
	if got := drRead(tb, w, ports.DriftQuery{Realm: "t1", WithFindings: true, Limit: 1}); len(got) != 1 || got[0].Shard != 1 {
		tb.Fatalf("a limit of 1 with findings gave %+v, want the one run that qualifies", got)
	}
}

func drWorstFirst(tb testing.TB, f DriftFactory) {
	w := f(tb)
	var fs []domain.ReconcileFinding
	for i := range ports.MaxDriftFindings + 30 {
		d := domain.Credits(i + 1)
		if i%2 == 1 {
			d = -d
		}
		fs = append(fs, drFinding("t1", fmt.Sprintf("w-%03d", i), "general", d))
	}
	// A +100 and a -100 on two scopes are two findings, never one that nets to nothing.
	fs = append(fs, drFinding("t1", "plus", "general", 1000), drFinding("t1", "minus", "general", -1000))
	w.Save(tb, drRun(1, domain.ReconcileFull, drBase, fs...))
	r := drRead(tb, w, ports.DriftQuery{Realm: "t1"})[0]
	if r.FindingsTotal != len(fs) || len(r.Findings) != ports.MaxDriftFindings {
		tb.Fatalf("%d findings listed and %d counted, want %d and %d", len(r.Findings), r.FindingsTotal, ports.MaxDriftFindings, len(fs))
	}
	abs := func(c domain.Credits) domain.Credits {
		if c < 0 {
			return -c
		}
		return c
	}
	for i := 1; i < len(r.Findings); i++ {
		if abs(r.Findings[i-1].Drift) < abs(r.Findings[i].Drift) {
			tb.Fatalf("finding %d (%d) is worse than finding %d (%d): worst first", i, r.Findings[i].Drift, i-1, r.Findings[i-1].Drift)
		}
	}
	ids := map[string]domain.Credits{r.Findings[0].Scope.ID: r.Findings[0].Drift, r.Findings[1].Scope.ID: r.Findings[1].Drift}
	if ids["plus"] != 1000 || ids["minus"] != -1000 {
		tb.Fatalf("the two worst are %+v: a +1000 and a -1000 are two findings", ids)
	}
}

func drEmptyAndRealms(tb testing.TB, f DriftFactory) {
	w := f(tb)
	if runs := drRead(tb, w, ports.DriftQuery{Realm: "t1"}); len(runs) != 0 {
		tb.Fatalf("an empty store gave %+v", runs)
	}
	for _, realm := range []string{"", "Not A Realm", "a b"} {
		if runs, err := w.Reader.RecentRuns(context.Background(), ports.DriftQuery{Realm: realm}); !errors.Is(err, domain.ErrInvalid) || runs != nil {
			tb.Fatalf("the realm %q: %v, %v, want %v", realm, runs, err, domain.ErrInvalid)
		}
	}
}
