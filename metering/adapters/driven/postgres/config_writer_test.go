//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Configuration is data, written by anything that holds a connection. The audit row and the cache-invalidation
// signal are written BY THE DATABASE, in the same transaction as the change (D37); the writer's part is to
// tell the trigger who and why, and to be atomic.

var change = ports.ConfigChange{Actor: "alice@example.com", Reason: "raise the free tier", Via: "ops-tool"}

type auditRow struct {
	realm, actor, action, kind, subject string
	reason                              *string
	before, after                       *string
}

func audit(t *testing.T, p *pgxpool.Pool, kind string) []auditRow {
	t.Helper()
	rows, err := p.Query(context.Background(),
		`SELECT realm, actor, action, subject_kind, coalesce(subject_id, ''), reason, before::text, after::text FROM audit_log WHERE subject_kind = $1 ORDER BY created_at, id`, kind)
	must(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		must(t, rows.Scan(&r.realm, &r.actor, &r.action, &r.kind, &r.subject, &r.reason, &r.before, &r.after))
		out = append(out, r)
	}
	must(t, rows.Err())
	return out
}

func newWriter(t *testing.T) (*pgadapter.ConfigWriter, *pgxpool.Pool) {
	t.Helper()
	p := db(t)
	return pgadapter.NewConfigWriter(p), p
}

func dailyLimit(name string) domain.Limit {
	return domain.Limit{Name: name, Subject: "user", Resource: "llm.chat", Unit: domain.CreditUnit, Max: 10_000_000, MaxEntitlement: "daily_user_credits",
		Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh", WarnAt: 0.8, DenyCode: domain.DenyLimitReached}
}

func TestAWriteThroughTheAPIIsAuditedWithWhoWhyAndThroughWhat(t *testing.T) {
	t.Parallel()
	w, p := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))

	created, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: dailyLimit("user_daily")})
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	rows := audit(t, p, "limits")
	if len(rows) != 1 {
		t.Fatalf("%d audit rows", len(rows))
	}
	r := rows[0]
	if r.realm != string(realm) || r.actor != "alice@example.com (via ops-tool)" || r.action != "config.insert" || r.reason == nil || *r.reason != "raise the free tier" || r.before != nil || r.after == nil || !strings.Contains(*r.after, `"user_daily"`) {
		t.Fatalf("the audit row names the person, the reason and the credential: %+v", r)
	}
	// An update records before AND after; a delete records what was lost.
	lim := dailyLimit("user_daily")
	lim.Max = 20_000_000
	if created, err := w.PutLimit(ctx, ports.ConfigChange{Actor: "bob", Reason: "doubled it"}, realm, ports.LimitConfig{Limit: lim}); err != nil || created {
		t.Fatalf("%v %v", created, err)
	}
	must(t, w.DeleteLimit(ctx, ports.ConfigChange{Actor: "carol", Reason: "retired the tier"}, realm, "user_daily"))
	rows = audit(t, p, "limits")
	if len(rows) != 3 || rows[1].action != "config.update" || rows[1].before == nil || !strings.Contains(*rows[1].before, `10000000`) || !strings.Contains(*rows[1].after, `20000000`) ||
		rows[2].action != "config.delete" || rows[2].actor != "carol" || rows[2].after != nil || rows[2].before == nil || *rows[2].reason != "retired the tier" {
		t.Fatalf("%+v", rows)
	}
}

// "A write that bypasses the API is still audited" — attributed to the database role, with no reason, because
// nobody gave one.
func TestAWriteThatBypassesTheAPIIsStillAuditedAndAttributedToTheRole(t *testing.T) {
	t.Parallel()
	w, p := newWriter(t)
	ctx := context.Background()
	realm := fmt.Sprintf("r%d", seq.Add(1))
	// First through the API, on this very connection pool: the person must not leak into the next write.
	if _, err := w.PutLimit(ctx, change, domain.Realm(realm), ports.LimitConfig{Limit: dailyLimit("via_api")}); err != nil {
		t.Fatal(err)
	}
	exec(t, p, `INSERT INTO limits (realm, name, unit, max, window_kind, deny_code) VALUES ($1,'by_hand','credit',5,'balance','payment_required')`, realm)
	var got auditRow
	must(t, p.QueryRow(ctx, `SELECT actor, action, reason FROM audit_log WHERE subject_kind = 'limits' AND after->>'name' = 'by_hand'`).Scan(&got.actor, &got.action, &got.reason))
	if !strings.HasPrefix(got.actor, "db:") || strings.Contains(got.actor, "alice") || got.action != "config.insert" || got.reason != nil {
		t.Fatalf("a change made directly in SQL is audited as the role that made it, and the last writer's person does not leak: %+v", got)
	}
}

func TestACardIsPublishedWithItsEntriesAndRetiresThePreviousOneAtomically(t *testing.T) {
	t.Parallel()
	w, p := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	entries := []domain.RateEntry{
		{RateKey: "gpt", Unit: "in", CreditsPerBlock: 150_000, BlockSize: 1_000_000, CostMicrosPerBlock: 100_000},
		{RateKey: "gpt", Unit: "out", CreditsPerBlock: 600_000, BlockSize: 1_000_000},
	}
	v1, err := domain.NewRateCard(realm, "v1", "table", entries, nil)
	must(t, err)
	info, err := w.PublishRateCard(ctx, change, realm, v1, false)
	if err != nil || info.Card.Version != "v1" || info.PublishedAt.IsZero() || info.RetiredAt != nil {
		t.Fatalf("%+v %v", info, err)
	}
	v2, _ := domain.NewRateCard(realm, "v2", "table", entries, nil)
	if _, err := w.PublishRateCard(ctx, change, realm, v2, true); err != nil {
		t.Fatal(err)
	}

	cards, err := w.RateCards(ctx, realm)
	must(t, err)
	if len(cards) != 2 || cards[0].Card.Version != "v1" || cards[0].RetiredAt == nil || cards[1].Card.Version != "v2" || cards[1].RetiredAt != nil {
		t.Fatalf("v1 retired, v2 active: %+v", cards)
	}
	got := cards[1].Card
	if got.Pricer != "table" || len(got.Entries) != 2 || got.Realm != realm {
		t.Fatalf("%+v", got)
	}
	by := map[domain.Unit]domain.RateEntry{}
	for _, e := range got.Entries {
		by[e.Unit] = e
	}
	if by["in"].CostMicrosPerBlock != 100_000 || by["out"].CostMicrosPerBlock != 0 || by["in"].CreditsPerBlock != 150_000 || by["out"].BlockSize != 1_000_000 {
		t.Fatalf("entries round-trip: %+v", by)
	}
	// The audit trail names the publication, its entries, and the retirement.
	if rows := audit(t, p, "rate_cards"); len(rows) != 3 || rows[2].actor != "alice@example.com (via ops-tool)" || rows[2].reason == nil {
		t.Fatalf("%+v", rows)
	}
	if rows := audit(t, p, "rate_card_entries"); len(rows) != 4 {
		t.Fatalf("every entry is audited: %d", len(rows))
	}
}

func TestAFailedPublicationLeavesTheActiveCardActiveAndNothingAudited(t *testing.T) {
	t.Parallel()
	w, p := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	good, _ := domain.NewRateCard(realm, "v1", "table", []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 1, BlockSize: 1}}, nil)
	if _, err := w.PublishRateCard(ctx, change, realm, good, false); err != nil {
		t.Fatal(err)
	}
	before := len(audit(t, p, "rate_cards")) + len(audit(t, p, "rate_card_entries"))

	// A card the application's own checks would have refused, hand-built so it reaches the database: the CHECK
	// on credits_per_block refuses it, and the retirement of v1 that preceded it in the transaction is undone.
	broken := domain.RateCard{Realm: realm, Version: "v2", Pricer: "table", Entries: []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: -5, BlockSize: 1}}}
	_, err := w.PublishRateCard(ctx, change, realm, broken, true)
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a check the database enforces is ErrInvalid: %v", err)
	}
	cards, _ := w.RateCards(ctx, realm)
	if len(cards) != 1 || cards[0].RetiredAt != nil {
		t.Fatalf("v1 is still the one active card, and v2 does not exist: %+v", cards)
	}
	if after := len(audit(t, p, "rate_cards")) + len(audit(t, p, "rate_card_entries")); after != before {
		t.Fatalf("a rolled-back change leaves no audit row: %d → %d", before, after)
	}
}

func TestAVersionOrASecondActiveCardIsAlreadyExists(t *testing.T) {
	t.Parallel()
	w, _ := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	card := func(v string) domain.RateCard {
		c, _ := domain.NewRateCard(realm, v, "table", []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 1, BlockSize: 1}}, nil)
		return c
	}
	if _, err := w.PublishRateCard(ctx, change, realm, card("v1"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.PublishRateCard(ctx, change, realm, card("v1"), true); !errors.Is(err, domain.ErrAlreadyExists) || !strings.Contains(err.Error(), "v1") {
		t.Fatalf("a version is published once: %v", err)
	}
	if _, err := w.PublishRateCard(ctx, change, realm, card("v2"), false); !errors.Is(err, domain.ErrAlreadyExists) || !strings.Contains(err.Error(), "retire") {
		t.Fatalf("a realm has exactly one active card, and the message says what to do: %v", err)
	}
	cards, _ := w.RateCards(ctx, realm)
	if len(cards) != 1 || cards[0].RetiredAt != nil {
		t.Fatalf("neither refusal changed anything: %+v", cards)
	}
}

func TestPublishingTheSameVersionConcurrentlyHasOneWinner(t *testing.T) {
	t.Parallel()
	w, _ := newWriter(t)
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	card, _ := domain.NewRateCard(realm, "v1", "table", []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 1, BlockSize: 1}}, nil)
	const n = 8
	var ok, exists atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := w.PublishRateCard(context.Background(), change, realm, card, true)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, domain.ErrAlreadyExists):
				exists.Add(1)
			default:
				t.Errorf("%v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || exists.Load() != n-1 {
		t.Fatalf("%d published, %d already existed", ok.Load(), exists.Load())
	}
}

func TestLimitsAreCreatedReplacedReadAndDeletedByName(t *testing.T) {
	t.Parallel()
	w, _ := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))

	if _, err := w.Limit(ctx, realm, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	for i, name := range []string{"a_first", "b_second", "c_third"} {
		if created, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: dailyLimit(name)}); err != nil || !created {
			t.Fatalf("%d: %v %v", i, created, err)
		}
	}
	order := func(name string) int {
		l, err := w.Limit(ctx, realm, name)
		must(t, err)
		return *l.SortOrder
	}
	if order("a_first") >= order("b_second") || order("b_second") >= order("c_third") {
		t.Fatalf("a new limit goes last: %d %d %d", order("a_first"), order("b_second"), order("c_third"))
	}
	// Replace keeps its place unless told otherwise.
	was := order("b_second")
	lim := dailyLimit("b_second")
	lim.Max = 1
	if created, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: lim}); err != nil || created {
		t.Fatalf("%v %v", created, err)
	}
	if order("b_second") != was {
		t.Fatalf("a replaced limit keeps its place: %d → %d", was, order("b_second"))
	}
	first := 1
	if _, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: dailyLimit("c_third"), SortOrder: &first}); err != nil || order("c_third") != 1 {
		t.Fatalf("an explicit order is honoured: %v %d", err, order("c_third"))
	}

	got, err := w.Limit(ctx, realm, "a_first")
	must(t, err)
	want := dailyLimit("a_first")
	if got.Limit != want {
		t.Fatalf("every field round-trips:\n got %+v\nwant %+v", got.Limit, want)
	}

	if err := w.DeleteLimit(ctx, change, realm, "a_first"); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteLimit(ctx, change, realm, "a_first"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleting what is not there: %v", err)
	}
	if _, err := w.Limit(ctx, realm, "a_first"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestAJobOrRollingLimitKeepsItsDuration(t *testing.T) {
	t.Parallel()
	w, _ := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	l := domain.Limit{Name: "run_cap", Subject: "job", Unit: domain.CreditUnit, Max: 500, Window: domain.Job, Dur: time.Hour, WarnAt: 0.5, DenyCode: domain.DenyLimitReached, TZ: "UTC"}
	if _, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: l}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Limit(ctx, realm, "run_cap")
	if err != nil || got.Limit != l {
		t.Fatalf("%+v %v", got.Limit, err)
	}
}

func TestPoolsAreCreatedReplacedAndRead(t *testing.T) {
	t.Parallel()
	w, _ := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	if _, err := w.Pool(ctx, realm, "gpu"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	gpu := ports.PoolConfig{Pool: domain.PoolDef{Name: "gpu", Priority: 5, AppliesTo: []string{"gpu.job", "gpu.batch"}}, Description: "for gpu work"}
	if created, err := w.PutPool(ctx, change, realm, gpu); err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	got, err := w.Pool(ctx, realm, "gpu")
	if err != nil || got.Pool.Name != "gpu" || got.Pool.Priority != 5 || len(got.Pool.AppliesTo) != 2 || got.Pool.AppliesTo[1] != "gpu.batch" || got.Description != "for gpu work" {
		t.Fatalf("%+v %v", got, err)
	}
	gpu.Pool.Priority, gpu.Pool.AppliesTo = 1, nil
	if created, err := w.PutPool(ctx, change, realm, gpu); err != nil || created {
		t.Fatalf("%v %v", created, err)
	}
	if got, _ := w.Pool(ctx, realm, "gpu"); got.Pool.Priority != 1 || len(got.Pool.AppliesTo) != 0 {
		t.Fatalf("a pool that applies to every resource says so: %+v", got)
	}
	if _, err := w.PutPool(ctx, change, realm, ports.PoolConfig{Pool: domain.PoolDef{Name: domain.GeneralPool, Priority: 1}}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a constraint the database enforces, reaching the writer, is ErrInvalid not a 500: %v", err)
	}
}

func TestOneRealmsConfigurationIsNotAnothers(t *testing.T) {
	t.Parallel()
	w, _ := newWriter(t)
	ctx := context.Background()
	a, b := domain.Realm(fmt.Sprintf("ra%d", seq.Add(1))), domain.Realm(fmt.Sprintf("rb%d", seq.Add(1)))
	if _, err := w.PutLimit(ctx, change, a, ports.LimitConfig{Limit: dailyLimit("user_daily")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Limit(ctx, b, "user_daily"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := w.DeleteLimit(ctx, change, b, "user_daily"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestWhoAndWhyRoundTripExactlyWhateverTheyContain(t *testing.T) {
	t.Parallel()
	w, p := newWriter(t)
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	ch := ports.ConfigChange{Actor: "O'Brien \"the ops\" 李雷\nline two", Reason: "because; DROP TABLE limits; --\ttab \\ backslash 🙂"}
	if _, err := w.PutLimit(context.Background(), ch, realm, ports.LimitConfig{Limit: dailyLimit("user_daily")}); err != nil {
		t.Fatal(err)
	}
	rows := audit(t, p, "limits")
	if len(rows) != 1 || rows[0].actor != ch.Actor || rows[0].reason == nil || *rows[0].reason != ch.Reason {
		t.Fatalf("a value is a value, never SQL: %+v", rows)
	}
}

// The configuration cache learns of a change from the database's own notification: no deploy, no TTL wait.
func TestAChangeReachesTheConfigurationCacheWithoutADeploy(t *testing.T) {
	t.Parallel()
	w, p := newWriter(t)
	ctx := context.Background()
	realm := domain.Realm(fmt.Sprintf("r%d", seq.Add(1)))
	cache, err := pgadapter.NewConfigStore(ctx, p, pgadapter.ConfigOptions{})
	must(t, err)
	t.Cleanup(cache.Close)
	if ls, _ := cache.Limits(ctx, realm); len(ls) != 0 {
		t.Fatalf("%v", ls)
	}
	if _, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: dailyLimit("user_daily")}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the cache to see the new limit", func() bool { ls, _ := cache.Limits(ctx, realm); return len(ls) == 1 && ls[0].Max == 10_000_000 })
	lim := dailyLimit("user_daily")
	lim.Max = 42
	if _, err := w.PutLimit(ctx, change, realm, ports.LimitConfig{Limit: lim}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the cache to see the changed ceiling", func() bool { ls, _ := cache.Limits(ctx, realm); return len(ls) == 1 && ls[0].Max == 42 })

	// A card published through the writer is the realm's active one, as the Meter will price under it.
	card, _ := domain.NewRateCard(realm, "v1", "table", []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 1, BlockSize: 1}}, nil)
	if _, err := w.PublishRateCard(ctx, change, realm, card, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the cache to serve the new card", func() bool { c, err := cache.Active(ctx, realm); return err == nil && c.Version == "v1" })
	if err := w.DeleteLimit(ctx, change, realm, "user_daily"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "the cache to forget the limit", func() bool { ls, _ := cache.Limits(ctx, realm); return len(ls) == 0 })
}
