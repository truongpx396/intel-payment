package domain

// ReconcileKind selects how much a reconcile run examines.
type ReconcileKind string

const (
	ReconcileIncremental ReconcileKind = "incremental" // scopes booked since the last run
	ReconcileFull        ReconcileKind = "full"        // every scope
)

// Reconcile outcomes for a finding.
const (
	OutcomeDrift      = "drift"
	OutcomeRegression = "regression"
)

// ReconcileFinding is one disagreement, per scope AND pool — a +100 on one scope and a −100 on
// another never net to zero.
type ReconcileFinding struct {
	Scope              Scope
	Pool               Pool
	Outcome            string // OutcomeDrift | OutcomeRegression
	HotSeq, AppliedSeq int64
	Hot, Expected      Credits // Expected = booked + open suspense
	Drift              Credits
	Healed, Alarmed    bool
}

// ReconcileRun is the result of one reconcile of one shard.
type ReconcileRun struct {
	Shard                                                 Shard
	Kind                                                  ReconcileKind
	Checked, InSync, Deferred, Cold, Regressions, Drifted int
	AbsDrift                                              Credits // Σ |drift| — never a net figure
	Findings                                              []ReconcileFinding
}

// AuditFinding is one scope and pool whose booked balance disagrees with checkpoint + Σ ledger.
type AuditFinding struct {
	Scope           Scope
	Pool            Pool
	Booked, Summed  Credits
	CheckpointFloor Credits
}

// AuditReport is the result of a deep audit of one shard.
type AuditReport struct {
	Shard      Shard
	Checked    int
	Findings   []AuditFinding
	Advanced   int // checkpoints advanced
	DetachOK   []string
	Mismatches int
}
