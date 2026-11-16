package resthandler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R112a: GET /v1/admin/drift — the recent reconcile runs, and the findings of the realm asked about. An operator reads it to see whether the
// two copies of a balance agree, so it must say what the runs found and never another realm's accounts. It reads and changes nothing.

type stubDrift struct {
	mu   sync.Mutex
	got  []ports.DriftQuery
	runs []ports.DriftRun
	err  error
}

var _ ports.DriftReader = (*stubDrift)(nil)

func (s *stubDrift) RecentRuns(_ context.Context, q ports.DriftQuery) ([]ports.DriftRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, q)
	return s.runs, s.err
}

func (s *stubDrift) queries() []ports.DriftQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ports.DriftQuery(nil), s.got...)
}

func sampleDriftRuns() []ports.DriftRun {
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	done := at.Add(1500 * time.Millisecond)
	return []ports.DriftRun{
		{
			ID: "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b", Shard: 3, Kind: domain.ReconcileFull, StartedAt: at, FinishedAt: &done,
			Checked: 120, InSync: 117, Deferred: 1, Cold: 0, Regressions: 0, Drifted: 2, AbsDrift: 130,
			Findings: []domain.ReconcileFinding{
				{Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Pool: "general", Outcome: domain.OutcomeDrift, HotSeq: 41, AppliedSeq: 41, Hot: 900, Expected: 1000, Drift: -100, Healed: true, Alarmed: true},
				{Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w2"}, Pool: "promo", Outcome: domain.OutcomeDrift, HotSeq: 7, AppliedSeq: 7, Hot: 30, Expected: 0, Drift: 30, Alarmed: true},
			},
			FindingsTotal: 2,
		},
		{ID: "6c1f7a3b-2d4e-4f60-9bac-1d2e3f4a5b6c", Shard: 5, Kind: domain.ReconcileIncremental, StartedAt: at.Add(-time.Minute), Checked: 4, InSync: 4},
	}
}

func TestTheDriftReadAsksForTheCredentialsRealmWithTheQueryItWasGiven(t *testing.T) {
	t.Parallel()
	three, zero := 3, 0
	for _, c := range []struct {
		query string
		want  ports.DriftQuery
	}{
		{"realm=acme", ports.DriftQuery{Realm: "acme"}},
		{"realm=acme&shard=3&limit=5&with_findings=true", ports.DriftQuery{Realm: "acme", Shard: &three, Limit: 5, WithFindings: true}},
		{"realm=acme&shard=0", ports.DriftQuery{Realm: "acme", Shard: &zero}}, // shard zero is a shard, not "no shard"
		{"realm=acme&with_findings=false", ports.DriftQuery{Realm: "acme"}},
		{fmt.Sprintf("realm=acme&limit=%d", ports.MaxDriftRuns), ports.DriftQuery{Realm: "acme", Limit: ports.MaxDriftRuns}},
		{"realm=acme&limit=1", ports.DriftQuery{Realm: "acme", Limit: 1}},
	} {
		t.Run(c.query, func(t *testing.T) {
			t.Parallel()
			d := &stubDrift{runs: sampleDriftRuns()}
			a := newAPI(t, &stubMeter{}, apiOpts{drift: d})
			rec := a.call("admin", "GET", "/v1/admin/drift?"+c.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			got := d.queries()
			if len(got) != 1 || got[0].Realm != c.want.Realm || got[0].Limit != c.want.Limit || got[0].WithFindings != c.want.WithFindings || (got[0].Shard == nil) != (c.want.Shard == nil) ||
				(got[0].Shard != nil && *got[0].Shard != *c.want.Shard) {
				t.Fatalf("the reader was asked %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestTheDriftReadRefusesAQueryItCannotReadBeforeTheReaderIsAsked(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"shard=-1", "shard=x", "shard=1.5", "shard=1e2", "shard=99999999999999999999",
		"limit=0", "limit=-1", fmt.Sprintf("limit=%d", ports.MaxDriftRuns+1), "limit=x", "limit=2.5",
		"with_findings=maybe", "with_findings=1", "with_findings=TRUE", "with_findings=yes",
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			d := &stubDrift{runs: sampleDriftRuns()}
			a := newAPI(t, &stubMeter{}, apiOpts{drift: d})
			rec := a.call("admin", "GET", "/v1/admin/drift?realm=acme&"+q, "")
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
				t.Fatalf("%d %s, want 400 invalid_request", rec.Code, rec.Body)
			}
			if n := len(d.queries()); n != 0 {
				t.Fatalf("the reader was asked %d times about a query that cannot be read", n)
			}
		})
	}
}

func TestTheDriftReadIsRefusedToAnyCredentialWithoutTheAdminPrivilegeAndToNoCredential(t *testing.T) {
	t.Parallel()
	d := &stubDrift{runs: sampleDriftRuns()}
	a := newAPI(t, &stubMeter{}, apiOpts{drift: d})
	for _, token := range []string{"all", "record", "grant"} {
		if rec := a.call(token, "GET", "/v1/admin/drift?realm=acme", ""); rec.Code != http.StatusForbidden {
			t.Errorf("the %q credential: %d %s, want 403", token, rec.Code, rec.Body)
		}
	}
	if rec := a.call("", "GET", "/v1/admin/drift?realm=acme", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: %d, want 401", rec.Code)
	}
	if n := len(d.queries()); n != 0 {
		t.Fatalf("the reader was asked %d times for a caller who may not read", n)
	}
}

// A credential that spans realms must say which, and one that does not cover the realm is refused: the findings are a realm's own.
func TestTheDriftReadOfARealmTheCredentialDoesNotCoverIsRefused(t *testing.T) {
	t.Parallel()
	d := &stubDrift{runs: sampleDriftRuns()}
	a := newAPI(t, &stubMeter{}, apiOpts{drift: d})
	if rec := a.call("admin", "GET", "/v1/admin/drift?realm=initech", ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusBadRequest {
		t.Fatalf("a realm the credential does not cover: %d %s", rec.Code, rec.Body)
	}
	if n := len(d.queries()); n != 0 {
		t.Fatalf("the reader was asked %d times about a realm the caller does not cover", n)
	}
}

func TestTheDriftReadAnswersEachRunWithItsCountersAndTheRealmsFindings(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{drift: &stubDrift{runs: sampleDriftRuns()}})
	rec := a.call("admin", "GET", "/v1/admin/drift?realm=acme", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Runs []struct {
			ID            string `json:"id"`
			Shard         int    `json:"shard"`
			Kind          string `json:"kind"`
			StartedAt     string `json:"started_at"`
			FinishedAt    string `json:"finished_at"`
			ScopesChecked int    `json:"scopes_checked"`
			InSync        int    `json:"in_sync"`
			Deferred      int    `json:"deferred"`
			Cold          int    `json:"cold"`
			Regressions   int    `json:"regressions"`
			Drifted       int    `json:"drifted"`
			AbsDrift      int64  `json:"abs_drift"`
			Findings      []struct {
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
			} `json:"findings"`
			FindingsTotal int `json:"findings_total"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Runs) != 2 {
		t.Fatalf("%d runs in %s", len(out.Runs), rec.Body)
	}
	r := out.Runs[0]
	if r.ID != "5b0e6f2a-1c3d-4e5f-8a9b-0c1d2e3f4a5b" || r.Shard != 3 || r.Kind != "full" || r.StartedAt != "2026-11-16T09:00:00Z" || r.FinishedAt != "2026-11-16T09:00:01.5Z" ||
		r.ScopesChecked != 120 || r.InSync != 117 || r.Deferred != 1 || r.Cold != 0 || r.Regressions != 0 || r.Drifted != 2 || r.AbsDrift != 130 || r.FindingsTotal != 2 || len(r.Findings) != 2 {
		t.Fatalf("the first run came back as %+v", r)
	}
	f := r.Findings[0]
	if f.Scope["kind"] != "workspace" || f.Scope["id"] != "w1" || f.Pool != "general" || f.Outcome != "drift" || f.HotSeq != 41 || f.AppliedSeq != 41 ||
		f.HotBalance != 900 || f.ExpectedBalance != 1000 || f.Drift != -100 || !f.Healed || !f.Alarmed {
		t.Fatalf("the first finding came back as %+v", f)
	}
	if _, has := f.Scope["realm"]; has {
		t.Fatalf("a finding names its realm (%v): the caller asked about one realm and the answer is that realm's", f.Scope)
	}
	// A run that found nothing says so with an empty list, not a missing one, and has no finish time it was not given.
	if !strings.Contains(rec.Body.String(), `"findings":[]`) {
		t.Fatalf("a run with no findings must carry an empty list: %s", rec.Body)
	}
	if out.Runs[1].FinishedAt != "" {
		t.Fatalf("a run that has not finished has a finish time: %q", out.Runs[1].FinishedAt)
	}
}

func TestTheDriftReadWithNoRunsIsAnEmptyListAndNotNull(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{drift: &stubDrift{}})
	rec := a.call("admin", "GET", "/v1/admin/drift?realm=acme", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runs":[]`) {
		t.Fatalf("%d %s, want 200 with an empty list", rec.Code, rec.Body)
	}
}

func TestTheDriftReadMapsAnInvalidQueryTo400AndAnythingElseTo500WithFixedText(t *testing.T) {
	t.Parallel()
	bad := newAPI(t, &stubMeter{}, apiOpts{drift: &stubDrift{err: fmt.Errorf("%w: a realm that is not one", domain.ErrInvalid)}})
	if rec := bad.call("admin", "GET", "/v1/admin/drift?realm=acme", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("an invalid query: %d %s, want 400", rec.Code, rec.Body)
	}
	broken := newAPI(t, &stubMeter{}, apiOpts{drift: &stubDrift{err: errors.New("pq: connection to 10.0.3.7 refused")}})
	rec := broken.call("admin", "GET", "/v1/admin/drift?realm=acme", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "10.0.3.7") {
		t.Fatalf("a failing reader: %d %s, want 500 and no internals", rec.Code, rec.Body)
	}
}

// A deployment that has not wired the reader serves no route for it: a route that answers an empty list for a reader that is not there would
// tell an operator that nothing drifted.
func TestWithoutADriftReaderThereIsNoDriftRoute(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{})
	if rec := a.call("admin", "GET", "/v1/admin/drift?realm=acme", ""); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d, want it not to exist", rec.Code)
	}
}
