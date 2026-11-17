package resthandler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// stubCredits is a programmable CreditsReader.
type stubCredits struct {
	mu    sync.Mutex
	fn    func(context.Context, ports.SnapshotQuery) (ports.CreditsSnapshot, error)
	got   ports.SnapshotQuery
	calls int
}

var _ ports.CreditsReader = (*stubCredits)(nil)

func (c *stubCredits) Credits(ctx context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
	c.mu.Lock()
	c.got = q
	c.calls++
	c.mu.Unlock()
	if c.fn == nil {
		return ports.CreditsSnapshot{Scope: q.Scope}, nil
	}
	return c.fn(ctx, q)
}

func creditsRoute(query string) route {
	return route{"credits", "GET", "/v1/credits?scope_kind=workspace&scope_id=w1" + query, "", false}
}

func i64(n int64) *int64 { return &n }

// The snapshot as the credits-ui contract names it (camelCase: the wire is a pass-through of CreditsSnapshot, so
// the package needs no translation table), with every figure an integer.
func fullSnapshot(q ports.SnapshotQuery) ports.CreditsSnapshot {
	at := mustTime("2026-09-30T00:00:00Z")
	row := mustTime("2026-09-29T11:59:59.5Z")
	return ports.CreditsSnapshot{
		Scope: q.Scope,
		Balance: ports.BalanceView{
			Remaining: 12_480, Granted: 15_000, PrimaryLimitKey: "scope_balance",
			Pools: []ports.PoolView{{Key: "promo", Remaining: 30, AppliesTo: "llm.chat"}, {Key: "general", Remaining: 12_450}},
		},
		Limits: []ports.LimitView{
			{Key: "scope_balance", Used: 0, Cap: i64(0), Window: "none", WarnAtPct: 80, DenyCode: "payment_required"},
			{Key: "org_daily", Used: 870, Cap: i64(1_000_000), Window: "day", ResetsAt: &at, WarnAtPct: 75, DenyCode: "limit_reached", ScopeHint: "all members"},
		},
		Ledger: []ports.LedgerEntry{{ID: "id-1", CreatedAt: row, LedgerRow: ports.LedgerRow{
			Pool: "general", Delta: -660, OperationType: "query", IdemKey: "evt-1", SeqTo: 7, SeqFrom: 7, EventCount: 1, RateCardVersion: "v1", Resource: "llm.chat",
			RateKey: "gpt", Subjects: domain.Subjects{"user": "u1"}, Ref: map[string]string{"model": "gpt-4o"}, ActorID: "u1", OccurredAt: row,
		}}},
		LedgerNext: "cur-2",
	}
}

func TestTheSnapshotIsTheCreditsContractsShape(t *testing.T) {
	t.Parallel()
	c := &stubCredits{fn: func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
		return fullSnapshot(q), nil
	}}
	rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	mustDecode(t, rec.Body.Bytes(), &m)

	if sc, _ := m["scope"].(map[string]any); sc["kind"] != "workspace" || sc["id"] != "w1" || sc["realm"] != "acme" {
		t.Fatalf("scope: %v", m["scope"])
	}
	bal, _ := m["balance"].(map[string]any)
	if bal["remaining"] != float64(12_480) || bal["granted"] != float64(15_000) || bal["primaryLimitKey"] != "scope_balance" {
		t.Fatalf("balance: %v", bal)
	}
	if _, has := bal["burnRatePerDay"]; has {
		t.Fatal("a burn rate needs the usage_daily rollup, which is not built: absent, not invented")
	}
	pools, _ := bal["pools"].([]any)
	if len(pools) != 2 || pools[0].(map[string]any)["key"] != "promo" || pools[0].(map[string]any)["label"] != "promo" || pools[0].(map[string]any)["appliesTo"] != "llm.chat" || pools[1].(map[string]any)["remaining"] != float64(12_450) {
		t.Fatalf("pools: %v", pools)
	}
	limits, _ := m["limits"].([]any)
	l0, l1 := limits[0].(map[string]any), limits[1].(map[string]any)
	if l0["key"] != "scope_balance" || l0["window"] != "none" || l0["cap"] != float64(0) || l0["warnAtPct"] != float64(80) || l0["denyCode"] != "payment_required" {
		t.Fatalf("limit 0: %v", l0)
	}
	if _, has := l0["resetsAt"]; has {
		t.Fatalf("a balance does not reset: %v", l0)
	}
	if l1["window"] != "day" || l1["used"] != float64(870) || l1["resetsAt"] != "2026-09-30T00:00:00Z" || l1["scopeHint"] != "all members" || l1["label"] != "org_daily" {
		t.Fatalf("limit 1: %v", l1)
	}
	if bd, ok := m["breakdown"].([]any); !ok || len(bd) != 0 {
		t.Fatalf("breakdown is an empty list until the usage_daily rollup exists: %v", m["breakdown"])
	}
	for _, k := range []string{"series", "anchor", "notices"} {
		if _, has := m[k]; has {
			t.Fatalf("%q is absent, not empty or invented", k)
		}
	}
	ledger, _ := m["ledger"].([]any)
	r0 := ledger[0].(map[string]any)
	if r0["id"] != "id-1" || r0["operationType"] != "query" || r0["delta"] != float64(-660) || r0["at"] != "2026-09-29T11:59:59.5Z" {
		t.Fatalf("ledger row: %v", r0)
	}
	if attrs, _ := r0["attributes"].(map[string]any); attrs["model"] != "gpt-4o" {
		t.Fatalf("attributes are the host's own bag, passed through: %v", r0["attributes"])
	}
	if actor, _ := r0["actor"].(map[string]any); actor["label"] != "u1" {
		t.Fatalf("actor: %v", r0["actor"])
	}
	if r0["pool"] != "general" || r0["resource"] != "llm.chat" || r0["rateKey"] != "gpt" || r0["rateCardVersion"] != "v1" || r0["idemKey"] != "evt-1" {
		t.Fatalf("the engine's own columns ride beside the bag: %v", r0)
	}
	if m["ledgerNext"] != "cur-2" {
		t.Fatalf("ledgerNext: %v", m["ledgerNext"])
	}
}

// Invariant 4: money is integer, end to end. A decoder that rejects a fraction is the proof.
func TestEveryNumberInTheSnapshotIsAnInteger(t *testing.T) {
	t.Parallel()
	c := &stubCredits{fn: func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
		return fullSnapshot(q), nil
	}}
	rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	n := 0
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case json.Number:
			n++
			if strings.ContainsAny(x.String(), ".eE") {
				t.Errorf("%s = %s is not an integer", path, x)
			}
			if _, err := x.Int64(); err != nil {
				t.Errorf("%s = %s does not fit int64", path, x)
			}
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(path+"[]", e)
				_ = i
			}
		}
	}
	walk("$", v)
	if n < 10 {
		t.Fatalf("walked only %d numbers: is the walk reading the body?", n)
	}
}

func TestThePartsOfTheSnapshotSumToItsTotal(t *testing.T) {
	t.Parallel()
	c := &stubCredits{fn: func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
		return fullSnapshot(q), nil
	}}
	rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
	var got struct {
		Balance struct {
			Remaining int64 `json:"remaining"`
			Pools     []struct {
				Remaining int64 `json:"remaining"`
			} `json:"pools"`
		} `json:"balance"`
	}
	mustDecode(t, rec.Body.Bytes(), &got)
	var sum int64
	for _, p := range got.Balance.Pools {
		sum += p.Remaining
	}
	if sum != got.Balance.Remaining {
		t.Fatalf("the panel shows the parts and the total: they must close, or an explicit residual be shown (%d vs %d)", sum, got.Balance.Remaining)
	}
}

func TestTheSnapshotQueryIsTheCallersAndTheCredentials(t *testing.T) {
	t.Parallel()
	c := &stubCredits{}
	a := newAPI(t, &stubMeter{}, apiOpts{credits: c})
	if rec := a.do("record", creditsRoute("&subject_kind=user&subject_id=u1&limit=20&cursor=cur-1&days=14")); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	q := c.got
	if q.Scope != (domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}) || q.SubjectKind != "user" || q.SubjectID != "u1" || q.LedgerLimit != 20 || q.LedgerAfter != "cur-1" {
		t.Fatalf("%+v", q)
	}
	if rec := a.do("record", creditsRoute("&scope_realm=globex")); rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestABadSnapshotQueryIsRefusedBeforeAnyRead(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]string{
		"half a member (kind)": "&subject_kind=user", "half a member (id)": "&subject_id=u1", "limit zero": "&limit=0", "limit past the maximum": "&limit=201",
		"days zero": "&days=0", "days past a year": "&days=367", "days not a number": "&days=week", "days a fraction": "&days=1.5",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &stubCredits{}
			rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(q))
			if rec.Code != 400 || decodeErr(t, rec).Error.Code != "invalid_request" || c.calls != 0 {
				t.Fatalf("%d %s (reads %d)", rec.Code, rec.Body.String(), c.calls)
			}
		})
	}
	t.Run("no scope", func(t *testing.T) {
		t.Parallel()
		c := &stubCredits{}
		rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).call("record", "GET", "/v1/credits?scope_id=w1", "")
		if rec.Code != 400 || c.calls != 0 {
			t.Fatalf("%d", rec.Code)
		}
	})
}

func TestASnapshotThatCannotBeBuiltIsTheTablesAnswerNotAHalfSnapshot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
	}{
		"hot tier down": {domain.ErrHotStoreUnavailable, 503}, "quota unresolved": {domain.ErrQuotaUnavailable, 503},
		"an invalid request": {domain.ErrInvalid, 400}, "anything else": {errors.New("pq: db.internal:5432 refused"), 500},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &stubCredits{fn: func(context.Context, ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
				return ports.CreditsSnapshot{}, tc.err
			}}
			rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
			if rec.Code != tc.status || strings.Contains(rec.Body.String(), "db.internal") {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTheSnapshotRouteExistsOnlyWhenItsReaderDoes(t *testing.T) {
	t.Parallel()
	if rec := newAPI(t, &stubMeter{}, apiOpts{}).do("all", creditsRoute("")); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

// FR-039: the spend section, in the credits contract's own names. A snapshot with a Spend carries `balance.burnRatePerDay`, `series` and a
// populated `breakdown`; one without carries none of the three (the shape test above holds that side).
func spendSnapshot(q ports.SnapshotQuery) ports.CreditsSnapshot {
	s := fullSnapshot(q)
	s.Spend = &ports.SpendView{
		BurnRatePerDay: 111,
		Series:         []domain.DailyRow{{Day: "2026-09-27", Credits: 300}, {Day: "2026-09-28"}, {Day: "2026-09-29", Credits: 805}},
		Breakdown: []domain.BreakdownRow{
			{Dimension: "gpt", Credits: 1000, CostMicros: 55, Events: 9}, {Dimension: "embed", Credits: 105}, {Dimension: "", Credits: 3},
		},
	}
	return s
}

func TestTheSpendSectionIsTheCreditsContractsShape(t *testing.T) {
	t.Parallel()
	c := &stubCredits{fn: func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
		return spendSnapshot(q), nil
	}}
	rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	mustDecode(t, rec.Body.Bytes(), &m)

	if bal, _ := m["balance"].(map[string]any); bal["burnRatePerDay"] != float64(111) {
		t.Errorf("balance.burnRatePerDay: %v", bal["burnRatePerDay"])
	}
	series, _ := m["series"].([]any)
	if len(series) != 3 {
		t.Fatalf("series: %v", m["series"])
	}
	for i, want := range []struct {
		at     string
		amount float64
	}{{"2026-09-27T00:00:00Z", 300}, {"2026-09-28T00:00:00Z", 0}, {"2026-09-29T00:00:00Z", 805}} {
		p, _ := series[i].(map[string]any)
		if p["at"] != want.at || p["amount"] != want.amount || len(p) != 2 { // a point is the day's start instant and the spend, and nothing else
			t.Errorf("series[%d] = %v, want %s %v", i, p, want.at, want.amount)
		}
	}
	rows, _ := m["breakdown"].([]any)
	if len(rows) != 3 {
		t.Fatalf("breakdown: %v", m["breakdown"])
	}
	for i, want := range []struct {
		key    string
		amount float64
	}{{"gpt", 1000}, {"embed", 105}, {"unspecified", 3}} { // largest first; a row that names no resource is named, not blank
		r, _ := rows[i].(map[string]any)
		if r["key"] != want.key || r["label"] != want.key || r["amount"] != want.amount || len(r) != 3 {
			t.Errorf("breakdown[%d] = %v, want %s %v (key, label, amount: a cost and an event count are not the contract's)", i, r, want.key, want.amount)
		}
	}
}

func TestAQuietWindowIsAZeroBurnAndZeroPointsNotAnAbsence(t *testing.T) {
	t.Parallel()
	c := &stubCredits{fn: func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
		s := fullSnapshot(q)
		s.Spend = &ports.SpendView{Series: []domain.DailyRow{{Day: "2026-09-28"}, {Day: "2026-09-29"}}}
		return s, nil
	}}
	rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
	var m map[string]any
	mustDecode(t, rec.Body.Bytes(), &m)
	if bal, _ := m["balance"].(map[string]any); bal["burnRatePerDay"] != float64(0) {
		t.Errorf("a window with no spend says so (0), it does not say nothing: %v", bal["burnRatePerDay"])
	}
	if s, _ := m["series"].([]any); len(s) != 2 {
		t.Errorf("two quiet days are two points: %v", m["series"])
	}
	if b, ok := m["breakdown"].([]any); !ok || len(b) != 0 {
		t.Errorf("no dimensions is an empty list, not null and not absent: %v", m["breakdown"])
	}
}

func TestTheDaysTheCallerAsksForReachTheSnapshotAndNoneMeansTheEnginesDefault(t *testing.T) {
	t.Parallel()
	c := &stubCredits{}
	a := newAPI(t, &stubMeter{}, apiOpts{credits: c})
	if rec := a.do("record", creditsRoute("&days=30")); rec.Code != 200 || c.got.Days != 30 {
		t.Fatalf("%d, days %d", rec.Code, c.got.Days)
	}
	for _, edge := range []int{1, domain.MaxSeriesDays} { // the shortest and the longest window are both windows
		if rec := a.do("record", creditsRoute(fmt.Sprintf("&days=%d", edge))); rec.Code != 200 || c.got.Days != edge {
			t.Fatalf("days=%d: %d, days %d", edge, rec.Code, c.got.Days)
		}
	}
	if rec := a.do("record", creditsRoute("")); rec.Code != 200 || c.got.Days != 0 {
		t.Fatalf("%d, days %d: no ?days= is the engine's default (0), not a number the handler picks", rec.Code, c.got.Days)
	}
}

func TestEveryNumberInTheSpendSectionIsAnInteger(t *testing.T) {
	t.Parallel()
	c := &stubCredits{fn: func(_ context.Context, q ports.SnapshotQuery) (ports.CreditsSnapshot, error) {
		return spendSnapshot(q), nil
	}}
	rec := newAPI(t, &stubMeter{}, apiOpts{credits: c}).do("record", creditsRoute(""))
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	seen := 0
	check := func(path string, x any) {
		n, ok := x.(json.Number)
		if !ok {
			t.Errorf("%s = %v is not a number", path, x)
			return
		}
		seen++
		if _, err := n.Int64(); err != nil || strings.ContainsAny(n.String(), ".eE") {
			t.Errorf("%s = %s is not an integer", path, n)
		}
	}
	bal, _ := v["balance"].(map[string]any)
	check("balance.burnRatePerDay", bal["burnRatePerDay"])
	series, _ := v["series"].([]any) // an absent section is a failure below, not a panic here
	for _, p := range series {
		pt, _ := p.(map[string]any)
		check("series[].amount", pt["amount"])
	}
	rows, _ := v["breakdown"].([]any)
	for _, r := range rows {
		row, _ := r.(map[string]any)
		check("breakdown[].amount", row["amount"])
	}
	if seen != 7 {
		t.Fatalf("checked %d figures, want 7: is the walk reading the spend section?", seen)
	}
}
