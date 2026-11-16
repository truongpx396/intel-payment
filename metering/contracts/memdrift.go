package contracts

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// MemDriftDefects are the deliberate faults a MemDrift can be given. DriftContract is shown to reject each by name (drift_test.go): a reader
// that shows another realm's accounts, or counts them, or hides the worst finding behind the cap, has not been shown to be tested.
type MemDriftDefects struct {
	ShowsEveryRealm     bool // lists every realm's findings
	CountsEveryRealm    bool // lists its own but counts everyone's
	IgnoresTheShard     bool // a shard narrows nothing
	ShardZeroIsNoShard  bool // shard zero means every shard
	OldestFirst         bool // runs come oldest first
	IgnoresTheLimit     bool // every run, however many were asked for
	NoDefaultLimit      bool // no limit means every run
	AllowsAnyLimit      bool // a limit of -1 or a million is taken
	IgnoresWithFindings bool // the filter does nothing
	AnyRealmsFindings   bool // the filter keeps a run that found something of ANOTHER realm
	LimitBeforeFilter   bool // the limit is taken and then the filter applied, so a qualifying run behind a newer one is lost
	NoCap               bool // every finding, however many
	CountsOnlyWhatShown bool // the total is what was listed, not what there was
	BestFirst           bool // findings come smallest drift first
	NetsFindings        bool // a +x and a -x on two scopes are one finding of zero
	DropsFlags          bool // healed and alarmed are lost
	AcceptsAnyRealm     bool // a realm that is not one, or none, is taken
}

type memDriftRun struct {
	run  ports.DriftRun
	seq  int
	list []domain.ReconcileFinding
}

// MemDrift is an in-memory ports.DriftReader that is also given runs the way the writer gives them.
type MemDrift struct {
	Defects MemDriftDefects
	mu      sync.Mutex
	runs    []memDriftRun
}

var _ ports.DriftReader = (*MemDrift)(nil)

// NewMemDrift returns an empty, honest one.
func NewMemDrift() *MemDrift { return &MemDrift{} }

// Save records a run as SaveReconcile does.
func (m *MemDrift) Save(rec ports.ReconcileRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := rec.Run
	finished := rec.StartedAt
	m.runs = append(m.runs, memDriftRun{
		seq: len(m.runs) + 1,
		run: ports.DriftRun{
			ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(m.runs)+1), Shard: int(r.Shard), Kind: r.Kind, StartedAt: rec.StartedAt, FinishedAt: &finished,
			Checked: r.Checked, InSync: r.InSync, Deferred: r.Deferred, Cold: r.Cold, Regressions: r.Regressions, Drifted: r.Drifted, AbsDrift: r.AbsDrift,
		},
		list: append([]domain.ReconcileFinding(nil), r.Findings...),
	})
}

func absCredits(c domain.Credits) domain.Credits {
	if c < 0 {
		return -c
	}
	return c
}

// RecentRuns implements ports.DriftReader.
func (m *MemDrift) RecentRuns(_ context.Context, q ports.DriftQuery) ([]ports.DriftRun, error) {
	if !m.Defects.AcceptsAnyRealm {
		if q.Realm == "" {
			return nil, fmt.Errorf("%w: realm is required", domain.ErrInvalid)
		}
		if err := domain.Realm(q.Realm).Validate(); err != nil {
			return nil, err
		}
	}
	if q.Limit < 0 && !m.Defects.AllowsAnyLimit || q.Limit > ports.MaxDriftRuns && !m.Defects.AllowsAnyLimit {
		return nil, fmt.Errorf("%w: limit is 1..%d", domain.ErrInvalid, ports.MaxDriftRuns)
	}
	if q.Shard != nil && *q.Shard < 0 {
		return nil, fmt.Errorf("%w: shard is not negative", domain.ErrInvalid)
	}
	limit := q.Limit
	if limit == 0 && !m.Defects.NoDefaultLimit {
		limit = ports.DefaultDriftRuns
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	all := append([]memDriftRun(nil), m.runs...)
	sort.SliceStable(all, func(i, j int) bool {
		if m.Defects.OldestFirst {
			return all[i].run.StartedAt.Before(all[j].run.StartedAt)
		}
		if !all[i].run.StartedAt.Equal(all[j].run.StartedAt) {
			return all[i].run.StartedAt.After(all[j].run.StartedAt)
		}
		return all[i].seq > all[j].seq
	})
	own := func(r memDriftRun) []domain.ReconcileFinding {
		var out []domain.ReconcileFinding
		for _, f := range r.list {
			if m.Defects.ShowsEveryRealm || f.Scope.Normalized().Realm == domain.Realm(q.Realm) {
				out = append(out, f)
			}
		}
		return out
	}
	keeps := func(r memDriftRun) bool {
		if q.Shard != nil && !m.Defects.IgnoresTheShard && (!m.Defects.ShardZeroIsNoShard || *q.Shard != 0) && r.run.Shard != *q.Shard {
			return false
		}
		if q.WithFindings && !m.Defects.IgnoresWithFindings {
			if m.Defects.AnyRealmsFindings {
				return len(r.list) > 0
			}
			return len(own(r)) > 0
		}
		return true
	}
	if m.Defects.LimitBeforeFilter && !m.Defects.IgnoresTheLimit && limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	var out []ports.DriftRun
	for _, r := range all {
		if !keeps(r) {
			continue
		}
		run := r.run
		fs := own(r)
		total := len(fs)
		if m.Defects.CountsEveryRealm {
			total = len(r.list)
		}
		if m.Defects.NetsFindings {
			fs = netFindings(fs)
			total = len(fs)
		}
		sort.SliceStable(fs, func(i, j int) bool {
			if m.Defects.BestFirst {
				return absCredits(fs[i].Drift) < absCredits(fs[j].Drift)
			}
			return absCredits(fs[i].Drift) > absCredits(fs[j].Drift)
		})
		if !m.Defects.NoCap && len(fs) > ports.MaxDriftFindings {
			fs = fs[:ports.MaxDriftFindings]
		}
		if m.Defects.CountsOnlyWhatShown {
			total = len(fs)
		}
		if m.Defects.DropsFlags {
			for i := range fs {
				fs[i].Healed, fs[i].Alarmed = false, false
			}
		}
		run.Findings, run.FindingsTotal = fs, total
		out = append(out, run)
		if !m.Defects.IgnoresTheLimit && limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// netFindings is what a reader that nets does: it adds the drifts of two findings together.
func netFindings(fs []domain.ReconcileFinding) []domain.ReconcileFinding {
	if len(fs) < 2 {
		return fs
	}
	merged := fs[0]
	for _, f := range fs[1:] {
		merged.Drift += f.Drift
	}
	return []domain.ReconcileFinding{merged}
}

// World is the honest or broken reader as a DriftWorld.
func (m *MemDrift) World() DriftWorld {
	return DriftWorld{Reader: m, Save: func(_ testing.TB, r ports.ReconcileRecord) { m.Save(r) }}
}
