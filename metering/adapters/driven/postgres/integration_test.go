//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/internal/pgtest"
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

var seq atomic.Int64

// db gives a test its own migrated database and a pool on it.
func db(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgadapter.Connect(context.Background(), pg.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func scope(realm domain.Realm) domain.Scope {
	return domain.Scope{Realm: realm, Kind: "workspace", ID: fmt.Sprintf("w-%d-%d", time.Now().UnixNano(), seq.Add(1))}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func scalar[T any](t *testing.T, p *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := p.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ------------------------------------------------------------ configuration --

func seedRealm(t *testing.T, p *pgxpool.Pool, realm string) {
	t.Helper()
	exec(t, p, `INSERT INTO credit_pools (realm, name, priority, applies_to) VALUES ($1,'promo',10,NULL), ($1,'gpu',20,ARRAY['gpu.job'])`, realm)
	exec(t, p, `INSERT INTO rate_cards (realm, version, pricer) VALUES ($1,'v1','table')`, realm)
	exec(t, p, `INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size, cost_micros_per_block) VALUES
		($1,'v1','gpt','in',150000,1000000,120000), ($1,'v1','gpt','out',600000,1000000,NULL)`, realm)
	exec(t, p, `INSERT INTO limits (realm, name, unit, max, window_kind, deny_code, sort_order) VALUES ($1,'scope_balance','credit',0,'balance','payment_required',0)`, realm)
	exec(t, p, `INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, tz, warn_at, deny_code, sort_order, resource)
		VALUES ($1,'user_daily','user','credit',500,'daily_user_credits','daily','Asia/Ho_Chi_Minh',0.75,'limit_reached',10,'llm.chat')`, realm)
	exec(t, p, `INSERT INTO limits (realm, name, subject_kind, unit, max, window_kind, dur_seconds, deny_code, sort_order)
		VALUES ($1,'run_cap','job','credit',900,'job',3600,'limit_reached',20)`, realm)
	exec(t, p, `INSERT INTO limits (realm, name, unit, max, window_kind, deny_code, active) VALUES ($1,'retired','credit',1,'daily','limit_reached',false)`, realm)
}

func newConfig(t *testing.T, p *pgxpool.Pool, o pgadapter.ConfigOptions) *pgadapter.ConfigStore {
	t.Helper()
	c, err := pgadapter.NewConfigStore(context.Background(), p, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestConfigStoreLoadsTheRealmsConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	seedRealm(t, p, "t1")
	c := newConfig(t, p, pgadapter.ConfigOptions{})

	ls, err := c.Limits(ctx, "t1")
	must(t, err)
	if len(ls) != 3 || ls[0].Name != "scope_balance" || ls[1].Name != "user_daily" || ls[2].Name != "run_cap" {
		t.Fatalf("inactive limits are not served, and order follows sort_order: %+v", ls)
	}
	u := ls[1]
	if u.Subject != "user" || u.MaxEntitlement != "daily_user_credits" || u.Window != domain.Daily || u.TZ != "Asia/Ho_Chi_Minh" ||
		u.WarnAt != 0.75 || u.Max != 500 || u.Resource != "llm.chat" || u.DenyCode != domain.DenyLimitReached {
		t.Fatalf("%+v", u)
	}
	if r := ls[2]; r.Window != domain.Job || r.Dur != time.Hour || r.Subject != domain.SubjectJob {
		t.Fatalf("%+v", r)
	}

	ps, err := c.Pools(ctx, "t1")
	must(t, err)
	if len(ps) != 2 || ps[0].Name != "promo" || ps[1].Name != "gpu" || len(ps[1].AppliesTo) != 1 || ps[1].AppliesTo[0] != "gpu.job" {
		t.Fatalf("%+v", ps)
	}
	if got := domain.EligiblePools(ps, "llm.chat"); len(got) != 2 || got[0] != "promo" {
		t.Fatalf("%v", got)
	}

	card, err := c.Active(ctx, "t1")
	must(t, err)
	if card.Version != "v1" || card.Pricer != "table" || len(card.Rates("gpt")) != 2 || card.Rates("gpt")["in"].BlockSize != 1000000 || card.Rates("gpt")["out"].CostMicrosPerBlock != 0 {
		t.Fatalf("%+v", card)
	}
	if again, _ := c.Get(ctx, "t1", "v1"); again.Version != "v1" {
		t.Fatal("Get serves a version by name")
	}

	// A realm nobody configured has nothing — and cannot price: that fails closed.
	if ls, _ := c.Limits(ctx, "nobody"); len(ls) != 0 {
		t.Fatalf("%v", ls)
	}
	if _, err := c.Active(ctx, "nobody"); !errors.Is(err, domain.ErrUnpriceable) {
		t.Fatalf("no active card must be ErrUnpriceable, got %v", err)
	}
	if _, err := c.Get(ctx, "t1", "no-such"); !errors.Is(err, domain.ErrUnpriceable) {
		t.Fatalf("%v", err)
	}
}

func TestConfigStoreCachesUntilInvalidated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	seedRealm(t, p, "t1")
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	c := newConfig(t, p, pgadapter.ConfigOptions{TTL: time.Minute, Now: now})

	first, _ := c.Limits(ctx, "t1")
	// Change the row WITHOUT the audit trigger, so no notification is sent: the cache cannot know.
	exec(t, p, `ALTER TABLE limits DISABLE TRIGGER limits_audit`)
	exec(t, p, `UPDATE limits SET max = 42 WHERE realm = 't1' AND name = 'user_daily'`)
	cached, _ := c.Limits(ctx, "t1")
	if cached[1].Max != first[1].Max {
		t.Fatal("a cached entry must be served without touching the database")
	}
	// The TTL is the safety net for a notification that never arrives.
	mu.Lock()
	clock = clock.Add(2 * time.Minute)
	mu.Unlock()
	reloaded, _ := c.Limits(ctx, "t1")
	if reloaded[1].Max != 42 {
		t.Fatalf("an expired entry must reload: %+v", reloaded[1])
	}
}

func TestConfigStoreIsInvalidatedByTheDatabasesOwnNotification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	seedRealm(t, p, "t1")
	c := newConfig(t, p, pgadapter.ConfigOptions{TTL: time.Hour})

	if ls, _ := c.Limits(ctx, "t1"); ls[1].Max != 500 {
		t.Fatal("setup")
	}
	before := c.Invalidations()
	// Any write — through psql, the admin API, a migration — notifies in the same transaction.
	exec(t, p, `UPDATE limits SET max = 77 WHERE realm = 't1' AND name = 'user_daily'`)
	eventually(t, 5*time.Second, "the limit change to reach the cache", func() bool {
		ls, err := c.Limits(ctx, "t1")
		return err == nil && ls[1].Max == 77
	})
	if c.Invalidations() <= before {
		t.Fatal("the change must have arrived as a notification, not by TTL")
	}

	// Publishing a new rate card and retiring the old one switches the active card at once.
	if card, _ := c.Active(ctx, "t1"); card.Version != "v1" {
		t.Fatal("setup")
	}
	exec(t, p, `UPDATE rate_cards SET retired_at = now() WHERE realm = 't1' AND version = 'v1'`)
	exec(t, p, `INSERT INTO rate_cards (realm, version) VALUES ('t1','v2')`)
	exec(t, p, `INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size) VALUES ('t1','v2','gpt','in',1,1)`)
	eventually(t, 5*time.Second, "the new card to become active", func() bool {
		card, err := c.Active(ctx, "t1")
		return err == nil && card.Version == "v2"
	})
	// The retired version is still there for replay and audit.
	if old, err := c.Get(ctx, "t1", "v1"); err != nil || len(old.Entries) != 2 {
		t.Fatalf("a retired card stays readable: %v %v", old.Version, err)
	}

	// Another realm's change does not evict this one.
	other := c.Invalidations()
	exec(t, p, `INSERT INTO limits (realm, name, unit, max, window_kind, deny_code) VALUES ('other','x','credit',1,'daily','limit_reached')`)
	eventually(t, 5*time.Second, "the other realm's notification", func() bool { return c.Invalidations() > other })
}

func TestConfigStoreDropsEverythingWhenTheListenerReconnects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	seedRealm(t, p, "t1")
	c := newConfig(t, p, pgadapter.ConfigOptions{TTL: time.Hour})
	if ls, _ := c.Limits(ctx, "t1"); ls[1].Max != 500 {
		t.Fatal("setup")
	}
	// While the listener is down a change can be missed; so it must drop the cache on reconnect.
	exec(t, p, `ALTER TABLE limits DISABLE TRIGGER limits_audit`)
	exec(t, p, `UPDATE limits SET max = 13 WHERE realm = 't1' AND name = 'user_daily'`)
	before := c.Invalidations()
	exec(t, p, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN intelpay_config%' AND pid <> pg_backend_pid()`)
	eventually(t, 10*time.Second, "the listener to reconnect and drop the cache", func() bool {
		ls, err := c.Limits(ctx, "t1")
		return err == nil && ls[1].Max == 13 && c.Invalidations() > before
	})
}

func TestConfigStoreConcurrentReadersShareOneLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	seedRealm(t, p, "t1")
	c := newConfig(t, p, pgadapter.ConfigOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ls, err := c.Limits(ctx, "t1"); err != nil || len(ls) != 3 {
				t.Errorf("%v %v", ls, err)
			}
			if _, err := c.Active(ctx, "t1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// ------------------------------------------------------------------ journal --

func charge(s domain.Scope, key string, amount domain.Credits) domain.Charge {
	return domain.Charge{Scope: s, Amount: amount, IdemKey: key, Reason: "query", Resource: "llm.chat", RateKey: "gpt",
		RateCardVersion: "v1", CostMicros: 9, OccurredAt: time.Date(2026, 9, 29, 12, 0, 0, 5, time.UTC),
		Quantities: []domain.Quantity{{Unit: "in", Amount: 10}}, Subjects: domain.Subjects{"user": "u1"}, Ref: map[string]string{"trace_id": "t"}}
}

func TestJournalPutIsIdempotentAndFingerprinted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	j := pgadapter.NewJournal(p)
	s := scope("t1")
	c := charge(s, "k1", 100)
	e := ports.JournalEntry{Charge: c, Fingerprint: c.Fingerprint()}

	created, err := j.Put(ctx, e)
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	if created, err := j.Put(ctx, e); err != nil || created {
		t.Fatalf("an identical retry is a no-op, not an error: %v %v", created, err)
	}
	other := charge(s, "k1", 700)
	if _, err := j.Put(ctx, ports.JournalEntry{Charge: other, Fingerprint: other.Fingerprint()}); !errors.Is(err, domain.ErrIdemConflict) {
		t.Fatalf("the same key with a different request must conflict: %v", err)
	}
	if n, _ := j.Pending(ctx, "t1"); n != 1 {
		t.Fatalf("pending = %d", n)
	}
	// The same key in another scope, or another realm, is another entry.
	if created, err := j.Put(ctx, ports.JournalEntry{Charge: charge(scope("t1"), "k1", 100), Fingerprint: charge(scope("t1"), "k1", 100).Fingerprint()}); err != nil || !created {
		t.Fatalf("keys are per scope: %v %v", created, err)
	}
}

func TestJournalRoundTripsTheChargeAndClaimsWithALease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	j := pgadapter.NewJournal(p)
	s := scope("t1")
	c := charge(s, "k1", 100)
	_, err := j.Put(ctx, ports.JournalEntry{Charge: c, Fingerprint: c.Fingerprint()})
	must(t, err)

	now := time.Now().Add(time.Second)
	due, err := j.Due(ctx, now, 10)
	must(t, err)
	if len(due) != 1 {
		t.Fatalf("%d due", len(due))
	}
	got := due[0].Charge
	if got.Fingerprint() != c.Fingerprint() {
		t.Fatalf("the replayed charge must be the SAME request (same fingerprint):\n%+v\n%+v", got, c)
	}
	if got.Scope != s || got.RateCardVersion != "v1" || got.CostMicros != 9 || got.Ref["trace_id"] != "t" || !got.OccurredAt.Equal(c.OccurredAt) {
		t.Fatalf("%+v", got)
	}
	// Claimed: a second tick right now takes nothing, until the lease lapses.
	if again, _ := j.Due(ctx, now, 10); len(again) != 0 {
		t.Fatalf("a claimed entry must not be handed out twice: %d", len(again))
	}
	if later, _ := j.Due(ctx, now.Add(time.Minute), 10); len(later) != 1 {
		t.Fatalf("…but it comes back if the replay never finished: %d", len(later))
	}

	must(t, j.Retry(ctx, due[0].ID, 3, now.Add(time.Hour), "boom"))
	if n := scalar[int](t, p, `SELECT attempts FROM spend_journal WHERE id = $1::uuid`, due[0].ID); n != 3 {
		t.Fatalf("attempts = %d", n)
	}
	if err := j.Retry(ctx, "00000000-0000-0000-0000-000000000000", 1, now, ""); err == nil {
		t.Fatal("retrying an entry that does not exist must say so")
	}
}

func TestJournalConcurrentTicksClaimDisjointEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	j := pgadapter.NewJournal(p)
	s := scope("t1")
	for i := 0; i < 40; i++ {
		c := charge(s, fmt.Sprintf("k%d", i), 1)
		if _, err := j.Put(ctx, ports.JournalEntry{Charge: c, Fingerprint: c.Fingerprint()}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	now := time.Now().Add(time.Second)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			due, err := j.Due(ctx, now, 10)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, e := range due {
				seen[e.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("entry %s was claimed %d times", id, n)
		}
	}
	if len(seen) != 40 {
		t.Fatalf("8 ticks of up to 10 must cover the 40 entries between them: got %d", len(seen))
	}
}

// ----------------------------------------------------------------- the books --

func TestBooksWatermarkNeverMovesBackwards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		w, err := tx.Watermark(ctx)
		if err != nil || w.Exists {
			t.Fatalf("a new scope has no watermark: %+v %v", w, err)
		}
		return tx.SetWatermark(ctx, ports.Watermark{Gen: 1, AppliedSeq: 5}, 42)
	}))
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		// A regression is booked, but it must never lower the watermark.
		return tx.SetWatermark(ctx, ports.Watermark{Gen: 1, AppliedSeq: 3, Blocked: true}, 42)
	}))
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		w, _ := tx.Watermark(ctx)
		if !w.Exists || w.AppliedSeq != 5 || !w.Blocked || w.Gen != 1 {
			t.Fatalf("applied_seq must stay at the highest, blocked follows the latest: %+v", w)
		}
		return tx.SetWatermark(ctx, ports.Watermark{Gen: 2, AppliedSeq: 6}, 42)
	}))
	if shard := scalar[int](t, p, `SELECT shard FROM account_watermarks WHERE scope_id = $1`, s.ID); shard != 42 {
		t.Fatalf("the shard is stored so per-shard work is a range scan: %d", shard)
	}
	if g := scalar[int64](t, p, `SELECT gen FROM account_watermarks WHERE scope_id = $1`, s.ID); g != 2 {
		t.Fatalf("gen = %d", g)
	}
}

func TestBooksUsageIdemIsWindowBoundedAndCreditIdemIsPermanent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	now := time.Now()
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		if _, ok, _ := tx.UsageIdem(ctx, "k", now.Add(-72*time.Hour)); ok {
			t.Fatal("nothing yet")
		}
		must(t, tx.PutUsageIdem(ctx, "k", "fp1", 1, 7))
		rec, ok, err := tx.UsageIdem(ctx, "k", now.Add(-72*time.Hour))
		if err != nil || !ok || rec.Fingerprint != "fp1" || rec.Gen != 1 || rec.Seq != 7 {
			t.Fatalf("%+v %v %v", rec, ok, err)
		}
		must(t, tx.PutCreditIdem(ctx, ports.IdemOpGrant, "pay-1", "fpg", 1, 8, ""))
		return nil
	}))

	// Age the usage row past the window: a probe over the window must no longer see it. That is the
	// bound — an event older than the window is REFUSED by the Meter rather than re-applied.
	exec(t, p, `UPDATE usage_idem SET created_at = now() - interval '100 hours' WHERE idem_key = 'k'`)
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		if _, ok, _ := tx.UsageIdem(ctx, "k", now.Add(-72*time.Hour)); ok {
			t.Fatal("a guard older than the window is out of the window")
		}
		if _, ok, _ := tx.UsageIdem(ctx, "k", now.Add(-200*time.Hour)); !ok {
			t.Fatal("…but it is still there for a wider probe")
		}
		// The usage guard is per SCOPE; another scope's identical key is a different operation.
		return nil
	}))
	must(t, b.InTx(ctx, scope("t1"), func(ctx context.Context, tx ports.BookTx) error {
		if _, ok, _ := tx.UsageIdem(ctx, "k", now.Add(-200*time.Hour)); ok {
			t.Fatal("usage keys are unique per scope")
		}
		return nil
	}))

	// The credit guard is permanent, per (realm, op, key) — and never crosses a realm.
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		rec, ok, _ := tx.CreditIdem(ctx, ports.IdemOpGrant, "pay-1")
		if !ok || rec.Fingerprint != "fpg" || rec.Seq != 8 {
			t.Fatalf("%+v %v", rec, ok)
		}
		if _, ok, _ := tx.CreditIdem(ctx, ports.IdemOpTransfer, "pay-1"); ok {
			t.Fatal("a grant key is not a transfer key")
		}
		return nil
	}))
	must(t, b.InTx(ctx, scope("t2"), func(ctx context.Context, tx ports.BookTx) error {
		if _, ok, _ := tx.CreditIdem(ctx, ports.IdemOpGrant, "pay-1"); ok {
			t.Fatal("realm B's grant must not be swallowed by realm A's key")
		}
		return nil
	}))
	// The database is the guarantee: a second claim of the same key is refused by the constraint.
	err := b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		return tx.PutCreditIdem(ctx, ports.IdemOpGrant, "pay-1", "other", 1, 9, "")
	})
	if err == nil {
		t.Fatal("the durable unique constraint must refuse a second claim")
	}
}

func TestBookKeepsBalanceEqualToTheLedgerSumAndRollsBackAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	other := domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}
	occurred := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	var ids []string
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		var err error
		ids, err = tx.Book(ctx, []ports.LedgerRow{
			{Scope: s, Pool: "promo", Delta: -60, OperationType: "query", IdemKey: "u1", Gen: 1, SeqFrom: 1, SeqTo: 1, EventCount: 1,
				RateCardVersion: "v1", CostMicros: 5, Resource: "llm.chat", RateKey: "gpt", Quantities: []domain.Quantity{{Unit: "in", Amount: 3}},
				Subjects: domain.Subjects{"user": "u1"}, Ref: map[string]string{"trace_id": "t"}, OccurredAt: occurred},
			{Scope: s, Pool: "general", Delta: -40, OperationType: "query", IdemKey: "u1", Gen: 1, SeqFrom: 1, SeqTo: 1},
			{Scope: s, Delta: 1000, OperationType: "purchase", IdemKey: "g1", Gen: 1, SeqFrom: 2, SeqTo: 2},
			{Scope: s, Delta: -1000, OperationType: "allocation_out", Counterparty: &other, IdemKey: "t1", Gen: 1, SeqFrom: 3, SeqTo: 3},
		})
		return err
	}))
	if len(ids) != 4 || ids[0] == ids[1] {
		t.Fatalf("one id per row: %v", ids)
	}
	// balance == SUM(ledger), per pool — the invariant, checked in SQL against the tables themselves.
	for pool, want := range map[string]int64{"promo": -60, "general": -40} {
		sum := scalar[int64](t, p, `SELECT coalesce(sum(delta),0)::bigint FROM credit_ledger WHERE scope_id = $1 AND pool = $2`, s.ID, pool)
		bal := scalar[int64](t, p, `SELECT balance FROM account_credits WHERE scope_id = $1 AND pool = $2`, s.ID, pool)
		if sum != want || bal != want {
			t.Fatalf("pool %s: ledger sum %d, balance %d, want %d", pool, sum, bal, want)
		}
	}
	if k := scalar[string](t, p, `SELECT counter_scope_kind FROM credit_ledger WHERE operation_type = 'allocation_out' AND scope_id = $1`, s.ID); k != "organization" {
		t.Fatalf("the counterparty is recorded: %q", k)
	}

	// A failure anywhere in the transaction leaves NOTHING: no rows, no balance change.
	before := scalar[int](t, p, `SELECT count(*) FROM credit_ledger`)
	err := b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		if _, err := tx.Book(ctx, []ports.LedgerRow{{Scope: s, Delta: 5, OperationType: "x", IdemKey: "z", Gen: 1, SeqFrom: 9, SeqTo: 9}}); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if err == nil || scalar[int](t, p, `SELECT count(*) FROM credit_ledger`) != before {
		t.Fatalf("a rolled-back booking must leave nothing: %v", err)
	}
	// A row for another scope in this scope's transaction is a bug, refused.
	err = b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		_, err := tx.Book(ctx, []ports.LedgerRow{{Scope: other, Delta: 1, OperationType: "x", IdemKey: "z", Gen: 1, SeqFrom: 1, SeqTo: 1}})
		return err
	})
	if err == nil {
		t.Fatal("a scope's transaction holds only that scope's lock: it must not book another scope's rows")
	}
	// The ledger is append-only in effect: rows carry the card that priced them.
	if v := scalar[string](t, p, `SELECT rate_card_version FROM credit_ledger WHERE scope_id = $1 AND pool = 'promo'`, s.ID); v != "v1" {
		t.Fatalf("invariant 5: the row names its card: %q", v)
	}
}

// The per-scope advisory lock is what makes two overlapping writers safe: N concurrent bookings on
// one scope serialise, so a read-modify-write of the watermark loses no update.
func TestConcurrentBookingsOfOneScopeSerialise(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	const n = 25
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
				w, err := tx.Watermark(ctx)
				if err != nil {
					return err
				}
				next := w.AppliedSeq + 1
				if _, err := tx.Book(ctx, []ports.LedgerRow{{Scope: s, Delta: 1, OperationType: "purchase", IdemKey: fmt.Sprint(next), Gen: 1, SeqFrom: next, SeqTo: next}}); err != nil {
					return err
				}
				return tx.SetWatermark(ctx, ports.Watermark{Gen: 1, AppliedSeq: next}, 1)
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if seqn := scalar[int64](t, p, `SELECT applied_seq FROM account_watermarks WHERE scope_id = $1`, s.ID); seqn != n {
		t.Fatalf("a lost update: applied_seq = %d, want %d", seqn, n)
	}
	if bal := scalar[int64](t, p, `SELECT balance FROM account_credits WHERE scope_id = $1`, s.ID); bal != n {
		t.Fatalf("balance = %d, want %d", bal, n)
	}
	if rows := scalar[int](t, p, `SELECT count(DISTINCT seq_from) FROM credit_ledger WHERE scope_id = $1`, s.ID); rows != n {
		t.Fatalf("two writers booked the same sequence number: %d distinct of %d", rows, n)
	}
}

func TestSuspenseIsCountedByBookedAndClosesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	var id string
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		var err error
		id, err = tx.OpenSuspense(ctx, ports.Suspense{Pool: "general", Delta: -100, Reason: ports.SuspenseDuplicate, Gen: 1, Seq: 4, IdemKey: "dup"})
		if err != nil {
			return err
		}
		_, err = tx.Book(ctx, []ports.LedgerRow{{Scope: s, Delta: 500, OperationType: "purchase", IdemKey: "g", Gen: 1, SeqFrom: 1, SeqTo: 1}})
		if err != nil {
			return err
		}
		return tx.SetWatermark(ctx, ports.Watermark{Gen: 1, AppliedSeq: 4}, 3)
	}))
	st, err := b.Booked(ctx, s)
	must(t, err)
	// hot == booked + open suspense: 500 booked, -100 hot movement the books did not accept.
	if st.Booked["general"] != 500 || st.Suspense["general"] != -100 || st.Watermark.AppliedSeq != 4 || !st.Watermark.Exists {
		t.Fatalf("%+v", st)
	}
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		ok, err := tx.CloseSuspense(ctx, id)
		if err != nil || !ok {
			t.Fatalf("first close: %v %v", ok, err)
		}
		if ok, _ := tx.CloseSuspense(ctx, id); ok {
			t.Fatal("a suspense entry closes once")
		}
		return nil
	}))
	st, _ = b.Booked(ctx, s)
	if len(st.Suspense) != 0 {
		t.Fatalf("a closed entry no longer counts: %+v", st.Suspense)
	}
	// Another scope cannot close it.
	must(t, b.InTx(ctx, scope("t1"), func(ctx context.Context, tx ports.BookTx) error {
		if ok, _ := tx.CloseSuspense(ctx, id); ok {
			t.Fatal("closing another scope's entry")
		}
		return nil
	}))
}

func TestBookedOfANewScopeIsEmpty(t *testing.T) {
	t.Parallel()
	p := db(t)
	st, err := pgadapter.NewBooks(p).Booked(context.Background(), scope("t1"))
	if err != nil || st.Watermark.Exists || len(st.Booked) != 0 || len(st.Suspense) != 0 {
		t.Fatalf("a brand-new scope rehydrates from nothing: %+v %v", st, err)
	}
}

func TestLotsConsumeFIFOAndExpireAtTheExactRemainder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	exp := time.Now().Add(24 * time.Hour)
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		// Three lots opened in ONE transaction: FIFO must still follow the order they were granted.
		for i, n := range []domain.Credits{100, 100, 100} {
			if err := tx.OpenLot(ctx, ports.Lot{Pool: "general", Granted: n, ExpiresAt: &exp, SourceKey: fmt.Sprintf("g%d", i)}); err != nil {
				return err
			}
		}
		got, err := tx.ConsumeLots(ctx, "general", 150)
		if err != nil || got != 150 {
			t.Fatalf("consumed %d: %v", got, err)
		}
		return nil
	}))
	rem := func(key string) int64 {
		return scalar[int64](t, p, `SELECT remaining FROM credit_lots WHERE source_idem_key = $1 AND scope_id = $2`, key, s.ID)
	}
	if rem("g0") != 0 || rem("g1") != 50 || rem("g2") != 100 {
		t.Fatalf("oldest first: %d %d %d", rem("g0"), rem("g1"), rem("g2"))
	}
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		// More than the lots hold is not an error: the rest predates lots_fifo being switched on.
		if got, _ := tx.ConsumeLots(ctx, "general", 1000); got != 150 {
			t.Fatalf("consumed %d, want the 150 that remained", got)
		}
		if got, _ := tx.ConsumeLots(ctx, "promo", 10); got != 0 {
			t.Fatal("another pool has no lots")
		}
		return nil
	}))
	var lotID string
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		if err := tx.OpenLot(ctx, ports.Lot{Pool: "promo", Granted: 80, SourceKey: "p"}); err != nil {
			return err
		}
		_, _ = tx.ConsumeLots(ctx, "promo", 30)
		return nil
	}))
	lotID = scalar[string](t, p, `SELECT id::text FROM credit_lots WHERE source_idem_key = 'p' AND scope_id = $1`, s.ID)
	must(t, b.InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error {
		r, found, err := tx.ExpireLot(ctx, lotID)
		if err != nil || !found || r != 50 {
			t.Fatalf("the exact remainder at this point in the sequence: %d %v %v", r, found, err)
		}
		if _, found, _ := tx.ExpireLot(ctx, lotID); found {
			t.Fatal("a lot expires once")
		}
		return nil
	}))
}

func TestTransfersOpenIdempotentlyAndSettleOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	from := domain.Scope{Realm: "t1", Kind: "organization", ID: "o1"}
	to := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	rec := ports.TransferRecord{Realm: "t1", IdemKey: "alloc-1", From: from, To: to, FromPool: "general", ToPool: "promo", Amount: 2000}
	must(t, b.InTx(ctx, from, func(ctx context.Context, tx ports.BookTx) error {
		must(t, tx.OpenTransfer(ctx, rec))
		must(t, tx.OpenTransfer(ctx, rec)) // a redelivered transfer_out
		return nil
	}))
	if n := scalar[int](t, p, `SELECT count(*) FROM credit_transfers WHERE status = 'in_transit'`); n != 1 {
		t.Fatalf("%d rows", n)
	}
	must(t, b.InTx(ctx, to, func(ctx context.Context, tx ports.BookTx) error {
		got, found, err := tx.SettleTransfer(ctx, "t1", "alloc-1")
		if err != nil || !found || got.Status != domain.TransferSettled || got.Amount != 2000 || got.From != from || got.To != to || got.ToPool != "promo" {
			t.Fatalf("%+v %v %v", got, found, err)
		}
		if _, found, _ := tx.SettleTransfer(ctx, "t1", "no-such"); found {
			t.Fatal("settling nothing")
		}
		return nil
	}))
	if n := scalar[int](t, p, `SELECT count(*) FROM credit_transfers WHERE status = 'in_transit'`); n != 0 {
		t.Fatalf("still in transit: %d", n)
	}
}

func TestSettleJournalMarksTheRowWhenTheIntentIsBooked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	s := scope("t1")
	j := pgadapter.NewJournal(p)
	c := charge(s, "k1", 100)
	_, err := j.Put(ctx, ports.JournalEntry{Charge: c, Fingerprint: c.Fingerprint()})
	must(t, err)
	must(t, pgadapter.NewBooks(p).InTx(ctx, s, func(ctx context.Context, tx ports.BookTx) error { return tx.SettleJournal(ctx, "k1") }))
	if n, _ := j.Pending(ctx, "t1"); n != 0 {
		t.Fatalf("pending = %d", n)
	}
	if due, _ := j.Due(ctx, time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Fatal("a settled entry is never replayed")
	}
}

// ------------------------------------------------------------------- shards --

func TestHotConfigRefusesADifferentShardCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := pgadapter.NewBooks(db(t))
	if _, ok, _ := b.HotConfig(ctx); ok {
		t.Fatal("nothing recorded before the first start")
	}
	must(t, b.EnsureHotConfig(ctx, 256))
	must(t, b.EnsureHotConfig(ctx, 256))
	if err := b.EnsureHotConfig(ctx, 128); !errors.Is(err, domain.ErrShardCountMismatch) {
		t.Fatalf("two processes hashing scopes differently keep two sets of books: %v", err)
	}
	if n, ok, _ := b.HotConfig(ctx); !ok || n != 256 {
		t.Fatalf("%d %v", n, ok)
	}
}

func TestShardStateAndGenerations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := pgadapter.NewBooks(db(t))
	r, err := b.EnsureShard(ctx, 7)
	must(t, err)
	if r.Gen != 1 || r.Frozen || r.NodeReplID != "" {
		t.Fatalf("%+v", r)
	}
	if again, _ := b.EnsureShard(ctx, 7); again.Gen != 1 {
		t.Fatal("ensure never resets a live shard")
	}
	must(t, b.SetFrozen(ctx, 7, true, "regression"))
	must(t, b.SetNodeReplID(ctx, 7, "abc123"))
	if g, err := b.BumpGen(ctx, 7); err != nil || g != 2 {
		t.Fatalf("%d %v", g, err)
	}
	r, _ = b.EnsureShard(ctx, 7)
	if !r.Frozen || r.Reason != "regression" || r.Gen != 2 || r.NodeReplID != "abc123" {
		t.Fatalf("%+v", r)
	}
	must(t, b.SetFrozen(ctx, 7, false, ""))
	if r, _ = b.EnsureShard(ctx, 7); r.Frozen || r.Reason != "" {
		t.Fatalf("%+v", r)
	}
	if _, err := b.BumpGen(ctx, 99); err == nil {
		t.Fatal("bumping a shard that was never ensured must say so")
	}
	if err := b.SetFrozen(ctx, 99, true, "x"); err == nil {
		t.Fatal("freezing a shard that was never ensured must say so")
	}
}

func TestShardOwnershipIsExclusiveAndSurvivesADeadWorker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)

	a, ok, err := b.Acquire(ctx, 5)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if !a.Alive(ctx) {
		t.Fatal("a fresh lease is alive")
	}
	if _, ok, _ := b.Acquire(ctx, 5); ok {
		t.Fatal("one owner per shard")
	}
	other, ok, err := b.Acquire(ctx, 6)
	if err != nil || !ok {
		t.Fatalf("shards are independent: %v %v", ok, err)
	}
	must(t, other.Release(ctx))

	// A worker that dies releases its lock with its connection, and another takes over.
	exec(t, p, `SELECT pg_terminate_backend(l.pid) FROM pg_locks l WHERE l.locktype = 'advisory' AND l.classid = $1 AND l.objid = 5 AND l.granted`, int32(0x494D5357))
	eventually(t, 5*time.Second, "the dead session's lock to be released", func() bool {
		next, ok, err := b.Acquire(ctx, 5)
		if err == nil && ok {
			must(t, next.Release(ctx))
			return true
		}
		return false
	})
	if a.Alive(ctx) {
		t.Fatal("the lease of a terminated session is not alive: the worker must stop writing")
	}
	_ = a.Release(ctx) // releasing a dead lease must not panic or wedge the pool

	// A clean release lets the next worker in at once.
	c, ok, _ := b.Acquire(ctx, 9)
	if !ok {
		t.Fatal("acquire")
	}
	must(t, c.Release(ctx))
	if d, ok, _ := b.Acquire(ctx, 9); !ok {
		t.Fatal("a released shard can be taken")
	} else {
		must(t, d.Release(ctx))
	}
}

func TestParkDeadKeepsTheRawEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := scope("t1")
	must(t, b.ParkDead(ctx, ports.DeadIntent{Scope: s, Op: domain.OpUsage, Gen: 1, Seq: 9, IdemKey: "k", Fields: map[string]string{"op": "usage", "p": "{}"}, Attempts: 5, LastErr: "boom"}))
	if n := scalar[int](t, p, `SELECT attempts FROM credit_outbox_dead WHERE scope_id = $1 AND seq = 9`, s.ID); n != 5 {
		t.Fatalf("%d", n)
	}
	if v := scalar[string](t, p, `SELECT payload->>'op' FROM credit_outbox_dead WHERE scope_id = $1`, s.ID); v != "usage" {
		t.Fatalf("the raw entry is kept so the intent can be replayed: %q", v)
	}
}
