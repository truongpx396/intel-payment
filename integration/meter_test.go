//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestRecordAcrossAnOutageIsDeferredThenAppliedExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStack(t, stackOpt{})
	sc := s.scope()
	s.open(sc)

	rc, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 100_000, IdemKey: "pay-1", Reason: "purchase"})
	must(t, err)
	if !rc.Applied || s.hot(sc) != 100_000 {
		t.Fatalf("%+v hot=%d", rc, s.hot(sc))
	}
	first, err := s.meter.Record(ctx, s.event(sc, "evt-1"))
	must(t, err)
	if !first.Applied || first.Deferred || first.Delta != -660 || first.RateCardVersion != "v1" {
		t.Fatalf("1,200 in + 800 out at the seeded card is exactly 660 credits: %+v", first)
	}

	// The hot store goes away mid-traffic.
	s.proxy.Cut()
	ev2 := s.event(sc, "evt-2") // a retry MUST carry the same OccurredAt; s.event stamps time.Now()
	deferred, err := s.meter.Record(ctx, ev2)
	if err != nil {
		t.Fatalf("invariant 16: an outage must not fail or drop usage: %v", err)
	}
	if !deferred.Applied || !deferred.Deferred || deferred.Seq != 0 {
		t.Fatalf("%+v", deferred)
	}
	if n, _ := s.journal.Pending(ctx, s.realm); n != 1 {
		t.Fatalf("the charge is in the journal: %d", n)
	}
	// Admit under fail_closed refuses; the same outage under fail_open serves and says so.
	if _, err := s.meter.Admit(ctx, sc, domain.AdmitRequest{MaxCost: 1}); !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("fail_closed: %v", err)
	}
	open := s.cfg
	open.AdmitFail = metering.FailOpen
	m2, err := app.New(open, s.deps)
	must(t, err)
	if adm, err := m2.Admit(ctx, sc, domain.AdmitRequest{MaxCost: 1}); err != nil || !adm.Allowed {
		t.Fatalf("fail_open serves (usage is still journaled, never unmetered): %+v %v", adm, err)
	}
	if s.met.get(ports.MetricRecordDeferred) != 1 {
		t.Fatal("deferral is observable")
	}
	// A retry of the deferred event during the outage is not journaled twice.
	again, err := s.meter.Record(ctx, ev2)
	must(t, err)
	if !again.Deferred {
		t.Fatalf("%+v", again)
	}
	if n, _ := s.journal.Pending(ctx, s.realm); n != 1 {
		t.Fatalf("the journal is idempotent on the key: %d entries", n)
	}

	// Redis comes back. The replay tick applies the journal through the hot function.
	s.proxy.Restore()
	res, err := s.replay.Replay(ctx, 10)
	must(t, err)
	if res.Claimed != 1 || res.Applied != 1 || res.Retried != 0 {
		t.Fatalf("%+v", res)
	}
	if got := s.hot(sc); got != 100_000-660-660 {
		t.Fatalf("both events charged exactly once: hot = %d", got)
	}

	// Replaying again — a duplicated tick, an entry the hot tier already has — charges nothing more.
	s.exec(`UPDATE spend_journal SET next_attempt_at = now() - interval '1 minute'`)
	res, _ = s.replay.Replay(ctx, 10)
	if res.Applied != 0 || res.Replayed != 1 {
		t.Fatalf("a replay of an applied entry is a REPLAY: %+v", res)
	}
	if final, err := s.meter.Record(ctx, ev2); err != nil || final.Applied || final.Deferred {
		t.Fatalf("and the producer retrying it now sees a plain replay: %+v %v", final, err)
	}
	if got := s.hot(sc); got != 100_000-660-660 {
		t.Fatalf("SC-002: no operation is charged twice under retry, outage and replay: hot = %d", got)
	}
}

func TestBackpressureDefersInsteadOfDroppingOrTrimming(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStack(t, stackOpt{cfg: metering.Config{BusMaxLen: 2}})
	sc := s.scope()
	s.open(sc)
	// Two intents fill the stream (a stream is per SHARD; this scope is alone in a fresh Redis
	// namespace only by hashing luck, so fill it until the function refuses).
	var deferred domain.Receipt
	for i := 0; i < 20 && !deferred.Deferred; i++ {
		rc, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 1000, IdemKey: "g" + string(rune('a'+i)), Reason: "purchase"})
		if err != nil {
			if !errors.Is(err, domain.ErrBackpressure) {
				t.Fatalf("a full stream refuses a grant loudly: %v", err)
			}
			break
		}
		_ = rc
	}
	rc, err := s.meter.Record(ctx, s.event(sc, "evt-bp"))
	must(t, err)
	if !rc.Deferred {
		t.Fatalf("FR-047: at the cap the hot function refuses and Record defers to the journal — nothing is dropped: %+v", rc)
	}
}

func TestAColdAccountIsRebuiltFromTheBooksBeforeItIsUsed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStack(t, stackOpt{})
	sc := s.scope()
	s.open(sc)
	sh := sc.Shard(s.cfg.Shards)

	// The books hold 500 general + 30 promo, an open duplicate suspense of -100, and applied_seq 41:
	// as if the hot tier had lost this account (a fresh Redis, a bumped generation).
	must(t, s.books.InTx(ctx, sc, func(ctx context.Context, tx ports.BookTx) error {
		if _, err := tx.Book(ctx, []ports.LedgerRow{
			{Scope: sc, Delta: 500, OperationType: "purchase", IdemKey: "p1", Gen: 1, SeqFrom: 40, SeqTo: 40},
			{Scope: sc, Pool: "promo", Delta: 30, OperationType: "signup_grant", IdemKey: "p2", Gen: 1, SeqFrom: 41, SeqTo: 41},
		}); err != nil {
			return err
		}
		if _, err := tx.OpenSuspense(ctx, ports.Suspense{Pool: "general", Delta: -100, Reason: ports.SuspenseDuplicate, Gen: 1, Seq: 41, IdemKey: "dup"}); err != nil {
			return err
		}
		return tx.SetWatermark(ctx, ports.Watermark{Gen: 1, AppliedSeq: 41}, sh)
	}))

	// Nothing in Redis yet. The first Admit finds the account cold, rebuilds it, and answers.
	adm, err := s.meter.Admit(ctx, sc, domain.AdmitRequest{MaxCost: 400})
	must(t, err)
	if !adm.Allowed || adm.Headroom != 430 { // 500 - 100 (hot == booked + open suspense) + 30
		t.Fatalf("a rebuilt account holds booked + open suspense: %+v", adm)
	}
	if got := s.hot(sc); got != 430 {
		t.Fatalf("hot = %d", got)
	}
	// The hot sequence RESUMES at applied_seq: no sequence number is ever reissued.
	rc, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 1, IdemKey: "next", Reason: "promo"})
	must(t, err)
	if rc.Seq != 42 {
		t.Fatalf("seq = %d, want 42", rc.Seq)
	}
	if s.met.get(ports.MetricRehydrate) != 1 {
		t.Fatal("the rebuild is counted")
	}
}

// The contract's own edge: a retry after a price change conflicts, loudly, rather than silently
// keeping the first price or charging a second amount.
func TestARetryAfterAPriceChangeIsAConflictNotADoubleCharge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStack(t, stackOpt{})
	sc := s.scope()
	s.open(sc)
	_, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 100_000, IdemKey: "pay", Reason: "purchase"})
	must(t, err)

	e := s.event(sc, "evt-1")
	first, err := s.meter.Record(ctx, e)
	must(t, err)

	// Same card: a retry is a plain replay.
	if again, err := s.meter.Record(ctx, e); err != nil || again.Applied {
		t.Fatalf("%+v %v", again, err)
	}

	// Publish a dearer card; the database notifies the cache.
	s.exec(`UPDATE rate_cards SET retired_at = now() WHERE realm = $1 AND version = 'v1'`, string(s.realm))
	s.exec(`INSERT INTO rate_cards (realm, version) VALUES ($1,'v2')`, string(s.realm))
	s.exec(`INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size) VALUES
		($1,'v2','gpt','in',300000,1000000), ($1,'v2','gpt','out',1200000,1000000)`, string(s.realm))
	deadline := time.Now().Add(5 * time.Second)
	for {
		card, err := s.config.Active(ctx, s.realm)
		if err == nil && card.Version == "v2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new card never became active")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err = s.meter.Record(ctx, e)
	if !errors.Is(err, domain.ErrIdemConflict) {
		t.Fatalf("the same key now prices differently: that is a conflict, not a second charge: %v", err)
	}
	if got := s.hot(sc); got != 100_000+first.Delta {
		t.Fatalf("the first price stands: hot = %d", got)
	}
	// A NEW event is priced under the new card, and records its version.
	fresh, err := s.meter.Record(ctx, s.event(sc, "evt-2"))
	must(t, err)
	if fresh.RateCardVersion != "v2" || fresh.Delta != -1320 {
		t.Fatalf("%+v", fresh)
	}
}

func TestConfiguredCeilingsBindThroughTheRealHotPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStack(t, stackOpt{quotas: staticQuota{"daily_user_credits": 700}})
	s.exec(`INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, deny_code, sort_order)
		VALUES ($1,'user_daily','user','credit',10,'daily_user_credits','daily','limit_reached',10)`, string(s.realm))
	// The limits cache learns of the row from the database's own notification.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ls, _ := s.config.Limits(ctx, s.realm)
		if len(ls) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new limit never reached the cache")
		}
		time.Sleep(20 * time.Millisecond)
	}
	sc := s.scope()
	s.open(sc)
	_, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 1_000_000, IdemKey: "pay", Reason: "purchase"})
	must(t, err)

	u1 := domain.Subjects{"user": "u1"}
	gate := func(sj domain.Subjects) domain.Admission {
		adm, err := s.meter.Admit(ctx, sc, domain.AdmitRequest{Resource: "llm.chat", Subjects: sj, MaxCost: 1})
		must(t, err)
		return adm
	}
	if adm := gate(u1); !adm.Allowed {
		t.Fatalf("%+v", adm)
	}
	e := s.event(sc, "evt-1")
	e.Subjects = u1
	if _, err := s.meter.Record(ctx, e); err != nil { // 660 of the entitlement-sized 700
		t.Fatal(err)
	}
	if adm := gate(u1); !adm.Allowed || adm.Warning == nil || adm.Headroom != 40 {
		t.Fatalf("660/700 warns and leaves 40: %+v", adm)
	}
	e2 := s.event(sc, "evt-2")
	e2.Subjects = u1
	if _, err := s.meter.Record(ctx, e2); err != nil {
		t.Fatal(err)
	}
	if adm := gate(u1); adm.Allowed || adm.Exceeded == nil || adm.Exceeded.Name != "user_daily" || adm.Exceeded.Max != 700 {
		t.Fatalf("u1 is over the plan-sized ceiling: %+v", adm)
	}
	if adm := gate(domain.Subjects{"user": "u2"}); !adm.Allowed {
		t.Fatalf("u2 has their own counter: %+v", adm)
	}
	if _, err := s.meter.Admit(ctx, sc, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1}); !errors.Is(err, domain.ErrMissingSubject) {
		t.Fatalf("a call without its subject is refused, never unlimited: %v", err)
	}
}

type staticQuota map[string]int64

func (q staticQuota) Quota(_ context.Context, _ domain.Scope, key string) (int64, error) {
	if v, ok := q[key]; ok {
		return v, nil
	}
	return 0, domain.ErrQuotaNotGranted
}

var _ = system.Clock{}
