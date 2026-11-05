//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R113 (FR-039) against PostgreSQL 16: usage_daily is held to the pure model (domain.Rollup / domain.Breakdown) on random ledgers in
// several zones, one of which skipped a calendar date; it is recomputed rather than added to, writes nothing when nothing changed, and a
// member's breakdown contains only that member's rows.

var usageScope = domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}

// insertUsage books a row the way the writer books metered usage: a negative delta, a rate key, and — what the first draft of the rollup got
// wrong — the resource's name as its operation type.
func insertUsage(t *testing.T, p *pgxpool.Pool, i int, u domain.LedgerUse, _ string) {
	t.Helper()
	op := u.Dimension
	if op == "" {
		op = "usage"
	}
	exec(t, p, `INSERT INTO credit_ledger (realm, scope_kind, scope_id, pool, delta, operation_type, idem_key, gen, seq_from, seq_to, event_count, cost_micros, resource, rate_key, actor_id, occurred_at)
		VALUES ($1, $2, $3, 'general', $4, $5, $6, 1, $7, $7, $8, $9, NULLIF($10, ''), 'gpt', NULLIF($11, ''), $12)`,
		u.Scope.Realm.Or(), u.Scope.Kind, u.Scope.ID, -int64(u.Credits), op, fmt.Sprintf("usage-%d", i), int64(i), u.Events, u.CostMicros, u.Dimension, u.Actor, u.At)
}

// insertOther books a row that is not spend: it has no rate key.
func insertOther(t *testing.T, p *pgxpool.Pool, i int, u domain.LedgerUse, op string) {
	t.Helper()
	exec(t, p, `INSERT INTO credit_ledger (realm, scope_kind, scope_id, pool, delta, operation_type, idem_key, gen, seq_from, seq_to, event_count, cost_micros, resource, actor_id, occurred_at)
		VALUES ($1, $2, $3, 'general', $4, $5, $6, 1, $7, $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), $12)`,
		u.Scope.Realm.Or(), u.Scope.Kind, u.Scope.ID, -int64(u.Credits), op, fmt.Sprintf("%s-%d", op, i), int64(i), u.Events, u.CostMicros, u.Dimension, u.Actor, u.At)
}

func usageRows(t *testing.T, p *pgxpool.Pool) map[domain.UsageKey]domain.UsageTotals {
	t.Helper()
	rows, err := p.Query(context.Background(), `SELECT realm, scope_kind, scope_id, day::text, dimension, actor_id, credits, cost_micros, events FROM usage_daily`)
	must(t, err)
	defer rows.Close()
	out := map[domain.UsageKey]domain.UsageTotals{}
	for rows.Next() {
		var k domain.UsageKey
		var realm string
		var c int64
		var v domain.UsageTotals
		must(t, rows.Scan(&realm, &k.Scope.Kind, &k.Scope.ID, &k.Day, &k.Dimension, &k.Actor, &c, &v.CostMicros, &v.Events))
		k.Scope.Realm, v.Credits = domain.Realm(realm), domain.Credits(c)
		out[k] = v
	}
	must(t, rows.Err())
	return out
}

func randomLedger(r *rand.Rand, n int, lo, hi time.Time) []domain.LedgerUse {
	dims := []string{"gpt", "embed", "image", ""}
	actors := []string{"u1", "u2", "u3", ""}
	scopes := []domain.Scope{usageScope, {Realm: "acme", Kind: "workspace", ID: "w2"}, {Realm: "beta", Kind: "user", ID: "x"}}
	out := make([]domain.LedgerUse, n)
	for i := range out {
		out[i] = domain.LedgerUse{
			Scope: scopes[r.IntN(len(scopes))], At: lo.Add(time.Duration(r.Int64N(int64(hi.Sub(lo))))).Truncate(time.Microsecond),
			Dimension: dims[r.IntN(len(dims))], Actor: actors[r.IntN(len(actors))], Credits: domain.Credits(1 + r.Int64N(10_000)),
			CostMicros: r.Int64N(1_000_000), Events: 1 + r.Int64N(5),
		}
	}
	return out
}

func sameRollup(t *testing.T, label string, got, want map[domain.UsageKey]domain.UsageTotals) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d rows, the model has %d", label, len(got), len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s: %+v is %+v (present %v), the model says %+v", label, k, g, ok, w)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: a row the model does not have: %+v", label, k)
		}
	}
}

func TestUsageDailyIsTheModelOnARandomLedgerInEveryZone(t *testing.T) {
	t.Parallel()
	lo, hi := time.Date(2011, 12, 26, 0, 0, 0, 0, time.UTC), time.Date(2012, 1, 3, 0, 0, 0, 0, time.UTC)
	for _, zn := range []string{"UTC", "Pacific/Apia", "America/Los_Angeles", "Australia/Lord_Howe", "Pacific/Kiritimati"} {
		t.Run(zn, func(t *testing.T) {
			t.Parallel()
			loc := zone(t, zn)
			p := db(t)
			u := pgadapter.NewUsage(p)
			uses := randomLedger(rand.New(rand.NewPCG(42, 1)), 400, lo, hi)
			for i, x := range uses {
				insertUsage(t, p, i, x, "usage")
			}
			changed, err := u.Rollup(context.Background(), lo.AddDate(0, 0, -1), hi.AddDate(0, 0, 1), loc)
			if err != nil {
				t.Fatal(err)
			}
			want, err := domain.Rollup(uses, loc)
			if err != nil {
				t.Fatal(err)
			}
			if changed != len(want) {
				t.Errorf("%d rows written, the model has %d", changed, len(want))
			}
			sameRollup(t, zn, usageRows(t, p), want)
			if tot, _ := domain.Totals(want); tot.Events != sumEvents(uses) {
				t.Fatalf("the model's days do not sum to the events: %+v", tot)
			}
			if zn == "Pacific/Apia" {
				for k := range usageRows(t, p) {
					if k.Day == "2011-12-30" {
						t.Fatalf("a row for a day that did not exist: %+v", k)
					}
				}
			}
		})
	}
}

func sumEvents(us []domain.LedgerUse) (n int64) {
	for _, u := range us {
		n += u.Events
	}
	return n
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	must(t, err)
	return loc
}

func TestRollingUpTwiceWritesNothingTheSecondTimeAndALateRowRewritesItsDayInsteadOfAddingToIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	u := pgadapter.NewUsage(p)
	day := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	from, to := day.AddDate(0, 0, -1), day.AddDate(0, 0, 3)
	insertUsage(t, p, 1, domain.LedgerUse{Scope: usageScope, At: day, Dimension: "gpt", Actor: "u1", Credits: 100, CostMicros: 7, Events: 2}, "usage")
	insertUsage(t, p, 2, domain.LedgerUse{Scope: usageScope, At: day.Add(time.Hour), Dimension: "gpt", Actor: "u1", Credits: 50, CostMicros: 3, Events: 1}, "usage")
	if n, err := u.Rollup(ctx, from, to, time.UTC); err != nil || n != 1 {
		t.Fatalf("first run: %d, %v", n, err)
	}
	key := domain.UsageKey{Scope: usageScope, Day: "2026-11-03", Dimension: "gpt", Actor: "u1"}
	if got := usageRows(t, p)[key]; got.Credits != 150 || got.Events != 3 || got.CostMicros != 10 {
		t.Fatalf("%+v", got)
	}
	fingerprint := func() string {
		return scalar[string](t, p, `SELECT count(*)::text || '/' || coalesce(max(xmin::text::bigint), 0)::text FROM usage_daily`)
	}
	before := fingerprint()
	for range 3 {
		if n, err := u.Rollup(ctx, from, to, time.UTC); err != nil || n != 0 {
			t.Fatalf("a window nothing changed in wrote %d rows, %v", n, err)
		}
	}
	if fingerprint() != before {
		t.Fatal("running the same window again touched usage_daily: it adds to what is there, or rewrites rows that were already right")
	}
	// A row booked late for a day already rolled up: the day is recomputed, not incremented.
	insertUsage(t, p, 3, domain.LedgerUse{Scope: usageScope, At: day.Add(2 * time.Hour), Dimension: "gpt", Actor: "u1", Credits: 25, CostMicros: 1, Events: 1}, "usage")
	if n, err := u.Rollup(ctx, from, to, time.UTC); err != nil || n != 1 {
		t.Fatalf("the late row: %d, %v", n, err)
	}
	if got := usageRows(t, p)[key]; got.Credits != 175 || got.Events != 4 || got.CostMicros != 11 {
		t.Fatalf("the day after a late row is %+v, want 175 credits, 4 events", got)
	}
	if n, _ := u.Rollup(ctx, from, to, time.UTC); n != 0 {
		t.Fatalf("and a second run changed %d", n)
	}
}

func TestARowNothingSupportsAnyMoreIsRemovedAndRowsOutsideTheWindowAreNeverTouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	u := pgadapter.NewUsage(p)
	day := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	insertUsage(t, p, 1, domain.LedgerUse{Scope: usageScope, At: day, Dimension: "gpt", Credits: 10, Events: 1}, "usage")
	// A stale row inside the window (a day that held spend once), and a row far outside it that the ledger knows nothing of.
	exec(t, p, `INSERT INTO usage_daily (realm, scope_kind, scope_id, day, dimension, actor_id, credits, cost_micros, events) VALUES
		('acme','workspace','w1','2026-11-04','stale','',99,0,1), ('acme','workspace','w1','2001-01-01','ancient','',77,0,1)`)
	n, err := u.Rollup(ctx, day.AddDate(0, 0, -1), day.AddDate(0, 0, 3), time.UTC)
	if err != nil || n != 2 {
		t.Fatalf("%d, %v: want the one row written and the one stale row deleted", n, err)
	}
	got := usageRows(t, p)
	if _, ok := got[domain.UsageKey{Scope: usageScope, Day: "2026-11-04", Dimension: "stale"}]; ok {
		t.Fatal("a row the ledger does not support survived")
	}
	if got[domain.UsageKey{Scope: usageScope, Day: "2001-01-01", Dimension: "ancient"}].Credits != 77 {
		t.Fatal("a day outside the window was touched")
	}
}

func TestOnlyMeteredUsageIsSpend(t *testing.T) {
	t.Parallel()
	p := db(t)
	day := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	spend := domain.LedgerUse{Scope: usageScope, At: day, Dimension: "gpt", Actor: "u1", Credits: 10, Events: 1}
	insertUsage(t, p, 100, spend, "")
	// Everything else the ledger holds: none of it is a metered event, whatever it is called and whatever its sign.
	for i, op := range []string{"usage", "grant", "purchase", "expiry", "allocation_out", "allocation_in", "writeoff", "admin_adjustment", "subscription_grant", "refund"} {
		insertOther(t, p, i, spend, op)
	}
	if _, err := pgadapter.NewUsage(p).Rollup(context.Background(), day, day, time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := usageRows(t, p)
	if len(rows) != 1 {
		t.Fatalf("%d rows from one priced usage row among ten others: %+v", len(rows), rows)
	}
	for _, v := range rows {
		if v.Credits != 10 || v.Events != 1 {
			t.Fatalf("%+v: grants, expiries, transfers and write-offs are not spend", v)
		}
	}
}

func TestTheBreakdownIsTheModelsAndAMemberReadsOnlyTheirOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	u := pgadapter.NewUsage(p)
	lo, hi := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	uses := randomLedger(rand.New(rand.NewPCG(9, 9)), 500, lo, hi)
	for i, x := range uses {
		insertUsage(t, p, i, x, "usage")
	}
	if _, err := u.Rollup(ctx, lo, hi, time.UTC); err != nil {
		t.Fatal(err)
	}
	model, _ := domain.Rollup(uses, time.UTC)
	for _, q := range []domain.BreakdownQuery{
		{Scope: usageScope, From: "2026-10-28", To: "2026-11-05", AllMembers: true},
		{Scope: usageScope, From: "2026-10-28", To: "2026-11-05", Member: "u1"},
		{Scope: usageScope, From: "2026-10-28", To: "2026-11-05", Member: "u2"},
		{Scope: usageScope, From: "2026-10-25", To: "2026-11-08", Member: "u3"},
		{Scope: domain.Scope{Realm: "beta", Kind: "user", ID: "x"}, From: "2026-10-25", To: "2026-11-08", AllMembers: true},
		{Scope: usageScope, From: "2026-10-28", To: "2026-11-05", Member: "nobody"},
	} {
		got, err := u.Breakdown(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := domain.Breakdown(model, q)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%+v:\n store %+v\n model %+v", q, got, want)
		}
	}
	// A member's rows are theirs: the actor column of every row behind u1's breakdown is u1.
	var foreign int
	must(t, p.QueryRow(ctx, `SELECT count(*) FROM usage_daily WHERE actor_id = 'u1' AND scope_id = 'w1' AND realm = 'acme'`).Scan(&foreign))
	mine, _ := u.Breakdown(ctx, domain.BreakdownQuery{Scope: usageScope, From: "2000-01-01", To: "2099-12-31", Member: "u1"})
	var events int64
	for _, r := range mine {
		events += r.Events
	}
	var inTable int64
	must(t, p.QueryRow(ctx, `SELECT coalesce(sum(events), 0) FROM usage_daily WHERE actor_id = 'u1' AND scope_id = 'w1' AND realm = 'acme'`).Scan(&inTable))
	if foreign == 0 || events != inTable {
		t.Fatalf("u1's breakdown holds %d events and u1's rows %d", events, inTable)
	}
	// By omission there is no aggregate.
	for _, q := range []domain.BreakdownQuery{{Scope: usageScope, From: "2026-10-28", To: "2026-11-05"}, {Scope: usageScope, From: "2026-10-28", To: "2026-11-05", Member: "u1", AllMembers: true}} {
		if _, err := u.Breakdown(ctx, q); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%+v: %v", q, err)
		}
	}
}

func TestARollupWindowThatEndsBeforeItStartsIsRefused(t *testing.T) {
	t.Parallel()
	d := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	if _, err := pgadapter.NewUsage(db(t)).Rollup(context.Background(), d, d.AddDate(0, 0, -2), time.UTC); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal(err)
	}
}
