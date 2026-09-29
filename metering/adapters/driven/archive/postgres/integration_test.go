//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/internal/pgtest"
	archive "github.com/truongpx396/intel-payment/metering/adapters/driven/archive/postgres"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var pg *pgtest.Postgres

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	if pg, err = pgtest.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = pg.Terminate(ctx)
	if code == 0 {
		if err := goleak.Find(
			goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"),
			goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop"),
		); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func event(s domain.Scope, seq int64, at time.Time) ports.ArchivedEvent {
	return ports.ArchivedEvent{
		Scope: s, Gen: 1, Seq: seq, IdemKey: fmt.Sprintf("k%d", seq),
		PoolDraws: []domain.PoolDelta{{Pool: "promo", Delta: -60}, {Pool: "general", Delta: -40}}, Credits: 100,
		Resource: "llm.chat", RateKey: "gpt", RateCardVersion: "v1", CostMicros: 9,
		Quantities: []domain.Quantity{{Unit: "in", Amount: 10}, {Unit: "out", Amount: 5}},
		Subjects:   domain.Subjects{"user": "u1"}, OccurredAt: at,
	}
}

func TestArchiveRoundTripsEveryEventDetail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, err := pgadapter.Connect(ctx, pg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := archive.New(pool)
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	now := time.Now().UTC().Truncate(time.Microsecond)

	in := []ports.ArchivedEvent{event(s, 1, now.Add(-2*time.Hour)), event(s, 2, now.Add(-time.Hour)), event(s, 3, now)}
	if err := a.Append(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := a.Events(ctx, s, now.Add(-3*time.Hour), now.Add(time.Minute), 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("%d %v", len(got), err)
	}
	for i := range in {
		if !reflect.DeepEqual(got[i], in[i]) {
			t.Fatalf("SC-010: every event must come back exactly as archived, to be re-priced:\n got %+v\nwant %+v", got[i], in[i])
		}
	}
	// Range and limit.
	if mid, _ := a.Events(ctx, s, now.Add(-90*time.Minute), now.Add(-30*time.Minute), 10); len(mid) != 1 || mid[0].Seq != 2 {
		t.Fatalf("%+v", mid)
	}
	if lim, _ := a.Events(ctx, s, now.Add(-3*time.Hour), now.Add(time.Minute), 2); len(lim) != 2 || lim[0].Seq != 1 {
		t.Fatalf("oldest first, bounded: %+v", lim)
	}
	// Another scope sees nothing.
	if none, _ := a.Events(ctx, domain.Scope{Realm: "t1", Kind: "workspace", ID: "other"}, now.Add(-3*time.Hour), now.Add(time.Minute), 10); len(none) != 0 {
		t.Fatal("scopes are isolated")
	}
}

// A retried batch — the writer crashed after archiving and before booking — adds nothing: the row's
// created_at is the event's OccurredAt, so the retry hits the same primary key.
func TestArchiveAppendIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, err := pgadapter.Connect(ctx, pg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := archive.New(pool)
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	batch := []ports.ArchivedEvent{event(s, 1, at), event(s, 2, at)}
	for i := 0; i < 3; i++ {
		if err := a.Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := a.Events(ctx, s, at.Add(-time.Hour), at.Add(time.Hour), 100)
	if len(got) != 2 {
		t.Fatalf("a retried batch must add nothing: %d rows", len(got))
	}
	if err := a.Append(ctx, nil); err != nil {
		t.Fatalf("an empty batch is fine: %v", err)
	}
	if err := a.Append(ctx, []ports.ArchivedEvent{event(s, 9, time.Time{})}); err == nil {
		t.Fatal("an event with no occurred_at has no stable identity and must be refused")
	}
}
