package resthandler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// GET /v1/admin/drift (rest-api.md § Operations): the recent reconcile runs, newest first, and what they found in the realm the caller names.
// It reads and changes nothing. A run's counters are the run's own; a finding is the realm's alone, and names its scope without a realm — the
// answer is the realm's, and the caller asked about one.

type driftFindingJSON struct {
	Scope           map[string]string `json:"scope"`
	Pool            string            `json:"pool"`
	Outcome         string            `json:"outcome"`
	HotSeq          int64             `json:"hot_seq"`
	AppliedSeq      int64             `json:"applied_seq"`
	HotBalance      int64             `json:"hot_balance"`
	ExpectedBalance int64             `json:"expected_balance"`
	Drift           int64             `json:"drift"`
	Healed          bool              `json:"healed"`
	Alarmed         bool              `json:"alarmed"`
}

type driftRunJSON struct {
	ID            string             `json:"id"`
	Shard         int                `json:"shard"`
	Kind          string             `json:"kind"`
	StartedAt     string             `json:"started_at"`
	FinishedAt    string             `json:"finished_at,omitempty"`
	ScopesChecked int                `json:"scopes_checked"`
	InSync        int                `json:"in_sync"`
	Deferred      int                `json:"deferred"`
	Cold          int                `json:"cold"`
	Regressions   int                `json:"regressions"`
	Drifted       int                `json:"drifted"`
	AbsDrift      int64              `json:"abs_drift"`
	Findings      []driftFindingJSON `json:"findings"`
	FindingsTotal int                `json:"findings_total"`
}

func driftRunToJSON(r ports.DriftRun) driftRunJSON {
	out := driftRunJSON{
		ID: r.ID, Shard: r.Shard, Kind: string(r.Kind), StartedAt: r.StartedAt.UTC().Format(time.RFC3339Nano),
		ScopesChecked: r.Checked, InSync: r.InSync, Deferred: r.Deferred, Cold: r.Cold, Regressions: r.Regressions, Drifted: r.Drifted,
		AbsDrift: int64(r.AbsDrift), Findings: make([]driftFindingJSON, 0, len(r.Findings)), FindingsTotal: r.FindingsTotal,
	}
	if r.FinishedAt != nil {
		out.FinishedAt = r.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, f := range r.Findings {
		out.Findings = append(out.Findings, driftFindingJSON{
			Scope: map[string]string{"kind": f.Scope.Kind, "id": f.Scope.ID}, Pool: string(f.Pool.Or()), Outcome: f.Outcome, HotSeq: f.HotSeq, AppliedSeq: f.AppliedSeq,
			HotBalance: int64(f.Hot), ExpectedBalance: int64(f.Expected), Drift: int64(f.Drift), Healed: f.Healed, Alarmed: f.Alarmed,
		})
	}
	return out
}

func (h *handler) getDrift(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "drift", err)
		return
	}
	q := r.URL.Query()
	dq := ports.DriftQuery{Realm: string(realm)}
	if v := q.Get("shard"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			h.fail(w, r, "drift", fmt.Errorf("%w: shard must be a whole number from zero", domain.ErrInvalid))
			return
		}
		dq.Shard = &n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ports.MaxDriftRuns {
			h.fail(w, r, "drift", fmt.Errorf("%w: limit must be an integer from 1 to %d", domain.ErrInvalid, ports.MaxDriftRuns))
			return
		}
		dq.Limit = n
	}
	switch q.Get("with_findings") {
	case "", "false":
	case "true":
		dq.WithFindings = true
	default:
		h.fail(w, r, "drift", fmt.Errorf("%w: with_findings is true or false", domain.ErrInvalid))
		return
	}
	runs, err := h.cfg.Drift.RecentRuns(r.Context(), dq)
	if err != nil {
		h.fail(w, r, "drift", err)
		return
	}
	out := struct {
		Runs []driftRunJSON `json:"runs"`
	}{Runs: make([]driftRunJSON, 0, len(runs))}
	for _, run := range runs {
		out.Runs = append(out.Runs, driftRunToJSON(run))
	}
	writeJSON(w, http.StatusOK, out)
}
