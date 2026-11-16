package ports

import (
	"context"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// What an operator reads to see whether the two copies of a balance agree: the recent reconcile runs, and what they found. It reads and
// changes nothing; the runs are the writer's (reconcile_runs, reconcile_findings).

// Limits of a drift read.
const (
	DefaultDriftRuns = 10  // the runs given when none is asked for
	MaxDriftRuns     = 100 // the most runs one read gives
	MaxDriftFindings = 200 // the most findings one run lists; the run says how many there were
)

// DriftQuery asks for the recent runs, newest first.
type DriftQuery struct {
	// Realm is the realm whose findings are read. Required: a finding names an account, and an account is a realm's own.
	Realm string
	// Shard narrows the runs to one shard's. nil is every shard, and zero is shard zero.
	Shard *int
	// Limit is how many runs: 1..MaxDriftRuns, and 0 means DefaultDriftRuns.
	Limit int
	// WithFindings keeps only the runs that found something of the realm's own. The limit counts the runs that qualify.
	WithFindings bool
}

// DriftRun is one reconcile run of one shard. The counters are the run's own, across every realm that has accounts in the shard: they carry no
// account, and they say whether the shard is healthy. Findings are the asking realm's alone.
type DriftRun struct {
	ID         string
	Shard      int
	Kind       domain.ReconcileKind
	StartedAt  time.Time
	FinishedAt *time.Time // nil while the run has not finished
	// Scopes checked, in sync, deferred (an intent in flight: not drift), cold, regressions (which froze the shard), and drifted; AbsDrift
	// is Σ|drift|, never a net figure.
	Checked, InSync, Deferred, Cold, Regressions, Drifted int
	AbsDrift                                              domain.Credits
	// Findings are the realm's findings in this run, worst first (largest |drift|), at most MaxDriftFindings; FindingsTotal is how many the
	// realm has in the run. A +100 on one scope and a -100 on another are two findings, never one that nets to nothing.
	Findings      []domain.ReconcileFinding
	FindingsTotal int
}

// DriftReader reads the reconcile runs. domain.ErrInvalid is a realm that is not one, a limit outside 1..MaxDriftRuns (or negative), and a
// negative shard; nothing else about the query can be wrong.
type DriftReader interface {
	RecentRuns(ctx context.Context, q DriftQuery) ([]DriftRun, error)
}
