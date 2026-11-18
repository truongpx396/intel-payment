//go:build integration && hardening

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// T133: recovery rehearsals, timed. SC-008 says affected shards are rebuilt from the books before serving resumes,
// "within the host's stated RTO" — and the repository states no RTO, because it is the host's number. So these
// measure how long recovery takes and assert that it is CORRECT (nothing lost, nothing doubled, the hot tier equal to
// the books afterwards); whether the time fits a host's RTO is for the host to read off the MEASURED lines.
//
// Not rehearsed here: the Postgres half of a region failover (promoting a standby and repointing the services).
// That needs a streaming replica and a second Redis in another failure domain; docs/hardening.md says so plainly.

// TestRecoveryAShardRecoveredByAnOperatorUnderTrafficIsTimedAndLosesNothing is the operator's recover, asked for over
// the admin API while callers keep calling: freeze, drain, bump, unfreeze, with Record deferring to the journal while
// frozen (invariant 16) and the books exact at the end.
func TestRecoveryAShardRecoveredByAnOperatorUnderTrafficIsTimedAndLosesNothing(t *testing.T) { //nolint:paralleltest // a timed rehearsal: it owns the machine's Docker for its duration
	cw := newChaosWorld(t, redistestStandalone())
	tokens := "integration:" + auth.HashToken(serviceToken) + ":" + string(cw.realm) + ":record+grant+admin"
	d := startPaymentd(t, paymentdEnv(string(cw.realm), cw.dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+cw.srv.Addr+"/0", "PAYMENT_SHARDS", strconv.Itoa(chaosShards), "PAYMENT_SERVICE_TOKENS", tokens, "PAYMENT_LOG_LEVEL", "info"))
	base := d.httpURL(t)
	awaitReady(t, d, cw.http, base)

	// The busiest shard: recovering it hurts most.
	perShard := map[int]int{}
	for _, sc := range cw.scopes {
		perShard[cw.shardOf(sc)]++
	}
	var shards []int
	for sh := range perShard {
		shards = append(shards, sh)
	}
	sort.Slice(shards, func(i, j int) bool {
		return perShard[shards[i]] > perShard[shards[j]] || (perShard[shards[i]] == perShard[shards[j]] && shards[i] < shards[j])
	})
	shard := shards[0]

	tr := cw.newTraffic()
	tr.run(4, 10*time.Millisecond)
	time.Sleep(2 * time.Second)
	genBefore := cw.generation(t, shard)
	deferredBefore := tr.deferred.Load()

	asked := time.Now()
	code, out := httpDo(t, cw.http, "POST", fmt.Sprintf("%s/v1/admin/shards/%d/recover?realm=%s", base, shard, cw.realm), serviceToken,
		`{"actor":"chaos@example.com","reason":"T133: a recovery rehearsal under traffic"}`, "Idempotency-Key", "recover-rehearsal-1")
	if code != http.StatusAccepted {
		t.Fatalf("POST /v1/admin/shards/%d/recover = %d %s", shard, code, out)
	}
	var acc struct{ Operation opView }
	must(t, json.Unmarshal([]byte(out), &acc))
	var done opView
	eventually(t, 120*time.Second, "the recovery to finish", func() bool {
		code, out := httpDo(t, cw.http, "GET", base+"/v1/admin/operations/"+acc.Operation.ID+"?realm="+string(cw.realm), serviceToken, "")
		return code == 200 && json.Unmarshal([]byte(out), &done) == nil && (done.Status == "succeeded" || done.Status == "failed")
	})
	took := time.Since(asked)
	if done.Status != "succeeded" {
		t.Fatalf("the recovery %s: %s", done.Status, done.Error)
	}
	time.Sleep(2 * time.Second) // callers keep calling across the unfreeze
	tr.halt()
	cw.assertWorkerAlive(t)
	genAfter := cw.generation(t, shard)
	measured(t, "T133", "shard-recover-under-traffic", "shard", shard, "scopes_on_it", perShard[shard], "asked_to_succeeded", took.Round(time.Millisecond),
		"generation", fmt.Sprintf("%d->%d", genBefore, genAfter), "deferred_during", tr.deferred.Load()-deferredBefore, "errors", tr.errs.Load(), "traffic_calls", tr.n.Load())
	if genAfter <= genBefore {
		t.Errorf("shard %d is still at generation %d: the recover did not bump it", shard, genAfter)
	}
	if tr.errs.Load() != 0 {
		t.Errorf("invariant 16: %d Record calls failed while the shard was frozen and rebuilt; usage must be deferred, never refused", tr.errs.Load())
	}
	eventually(t, 5*time.Minute, "the journal to hold no unsettled entry", func() bool { return cw.journalPending(t) == 0 })
	applied, replayed := tr.retryAll(t)
	measured(t, "T133", "shard-recover-under-traffic.retry-everything", "applied", applied, "replayed", replayed)
	if applied != 0 {
		t.Errorf("retrying every key charged %d again or for the first time after an operator's recover: it must lose nothing", applied)
	}
	tr.assertExact(t)
}

func (cw *chaosWorld) generation(t *testing.T, shard int) int64 {
	t.Helper()
	var g int64
	must(t, cw.pool.QueryRow(context.Background(), `SELECT coalesce(max(gen), 1) FROM hot_shards WHERE shard = $1`, shard).Scan(&g))
	return g
}

// TestRecoveryAnEmptyHotTierIsRebuiltFromTheBooksAndTimed is the Redis half of the DR runbook's second step ("Start
// Redis (empty, noeviction, AOF) … Every shard opens at a new generation; scopes rebuild from the books"): the hot tier
// is destroyed outright and a new, empty one stands in its place, with the hot-path functions installed the way
// `payment-migrate functions` installs them. Timed from the moment the new Redis answers to (a) every shard open and
// (b) every one of the scopes serving an admission, each rebuilt lazily from the books on its first call.
func TestRecoveryAnEmptyHotTierIsRebuiltFromTheBooksAndTimed(t *testing.T) { //nolint:paralleltest // a timed rehearsal: it owns the machine's Docker for its duration
	const scopes = 256
	cw := newChaosWorldN(t, scopes, redistestStandalone())
	ctx := context.Background()

	// Some history, then quiet, with everything booked: what the books hold is exactly what the hot tier held.
	tr := cw.newTraffic()
	tr.run(4, 10*time.Millisecond)
	time.Sleep(4 * time.Second)
	tr.halt()
	eventually(t, 180*time.Second, "the writer to book everything", func() bool { return cw.booked(t).usage == len(tr.sent) })
	booksBefore := cw.booked(t)
	gensBefore := cw.recovered(t)

	// The hot tier is destroyed: no keys, no functions, no history.
	must(t, cw.rdb.FlushAll(ctx).Err())
	must(t, cw.rdb.Do(ctx, "FUNCTION", "FLUSH").Err())
	// (The live worker re-opens each shard's `meta` key within milliseconds, so "no keys at all" would be a race; what the
	// destruction must have taken is every account, and so every balance.)
	for _, sc := range cw.scopes[:min(8, len(cw.scopes))] {
		if n, err := cw.rdb.Exists(ctx, fmt.Sprintf("acct:{s%d}:%s", cw.shardOf(sc), sc.Normalized().Tag())).Result(); err != nil || n != 0 {
			t.Fatalf("the hot tier was not emptied: the account of %s still exists (%d, %v)", sc.ID, n, err)
		}
	}
	// A new node: the deployment installs the functions (the only step that needs the deploy credential), and the
	// clock starts here.
	started := time.Now()
	_, err := hotredis.EnsureInstalled(ctx, cw.rdb)
	must(t, err)
	functions := time.Since(started)

	openShards := func() int {
		var n int
		for sh := 0; sh < cw.cfg.Shards; sh++ {
			if ok, err := cw.rdb.Exists(ctx, fmt.Sprintf("meta:{s%d}", sh)).Result(); err == nil && ok == 1 {
				n++
			}
		}
		return n
	}
	var shardsOpenAfter time.Duration
	eventually(t, 120*time.Second, "every shard to be open again", func() bool {
		if openShards() == cw.cfg.Shards {
			shardsOpenAfter = time.Since(started)
			return true
		}
		return false
	})

	// Every scope serving: the first admission rebuilds it from the books.
	lat := make([]time.Duration, 0, scopes)
	for _, sc := range cw.scopes {
		t0 := time.Now()
		var err error
		for stop := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var adm domain.Admission
			if adm, err = cw.meter.Admit(ctx, sc, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1}); err == nil && adm.Allowed {
				break
			} else if time.Now().After(stop) {
				t.Fatalf("scope %s did not serve within 60s of the new hot tier answering (%v)", sc.ID, err)
			}
		}
		lat = append(lat, time.Since(t0))
	}
	allServing := time.Since(started)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	measured(t, "T133", "empty-hot-tier-rebuild", "scopes", scopes, "functions_installed_after", functions.Round(time.Millisecond), "every_shard_open_after", shardsOpenAfter.Round(time.Millisecond),
		"every_scope_serving_after", allServing.Round(time.Millisecond), "first_admit_p50", lat[len(lat)/2], "first_admit_p99", lat[len(lat)*99/100], "first_admit_max", lat[len(lat)-1])

	// Correct, not just quick: every scope's rebuilt hot balance is exactly what the books hold, and nothing was written
	// to the books by the rebuild.
	booksAfter := cw.booked(t)
	var wrong int
	for _, sc := range cw.scopes {
		if hot, books := int64(cw.hotSum(sc)), booksAfter.sums[sc.ID]; hot != books {
			wrong++
			if wrong <= 3 {
				t.Errorf("scope %s: rebuilt to %d but the books hold %d", sc.ID, hot, books)
			}
		}
	}
	if wrong != 0 {
		t.Errorf("%d of %d scopes were rebuilt to something other than what the books hold", wrong, scopes)
	}
	if booksAfter.usage != booksBefore.usage || booksAfter.dupes != 0 {
		t.Errorf("the rebuild changed the books: %d usage rows (was %d), %d duplicates", booksAfter.usage, booksBefore.usage, booksAfter.dupes)
	}
	// The runbook says every shard "opens at a new generation". It does not: a shard whose `meta` key is missing is opened at
	// the generation the books record, and nothing is bumped (an empty Redis has no old-generation account or intent to
	// tell apart). Recorded, not asserted; docs/hardening.md and the runbook say so.
	measured(t, "T133", "empty-hot-tier-rebuild.generations", "shards_above_generation_1_before", gensBefore, "after", cw.recovered(t))
	cw.assertWorkerAlive(t)
	// And callers who retry everything charge nothing.
	// Callers whose acknowledgements may have been lost retry everything. The hot tier has lost its idempotency guards, so it
	// charges each retry AGAIN and says Applied; the books, which remember every key, refuse every duplicate, and the writer's
	// corrections put the hot side right. That is the documented behaviour of a late replay, and the number of them is the
	// size of the window a total loss opens: measured here, and the end state must still be exact.
	late, replayed := tr.retryAll(t)
	measured(t, "T133", "empty-hot-tier-rebuild.retry-everything", "charged_again_in_the_hot_tier", late, "replayed", replayed, "keys", len(tr.sent))
	tr.assertExact(t)
}

func redistestStandalone() redistest.Options { return redistest.Options{} }

// TestRecoveryAJournalBacklogFromAnOutageDrainsAtAMeasuredRate times the part of recovery a host feels longest: usage that
// was deferred to the journal while the hot tier was down is not on the hot balance until it has been replayed and booked,
// so for as long as it drains the hot tier over-admits by that much. The outage is held until a large backlog exists, the
// hot tier is restored, and the journal is then sampled every second until it holds nothing.
//
// The first half and the second half of the drain are reported separately on purpose. The statement the replayer uses to
// claim its next batch asks for 200 rows ordered by (next_attempt_at, created_at) but the index covers only the first
// column, so Postgres reads and sorts EVERY due row to return 200 (the plan is printed below). If that is what limits the
// drain, the half that starts with the bigger backlog is the slower one.
func TestRecoveryAJournalBacklogFromAnOutageDrainsAtAMeasuredRate(t *testing.T) { //nolint:paralleltest // a timed rehearsal: it owns the machine's Docker for its duration
	const backlogWanted = 12_000
	cw := newChaosWorld(t, redistestFsyncAlways())
	tr := cw.newTraffic()
	tr.run(8, 5*time.Millisecond)
	time.Sleep(time.Second)

	cw.killRedis(t)
	killed := time.Now()
	for cw.journalPending(t) < backlogWanted {
		if time.Since(killed) > 3*time.Minute {
			t.Fatalf("only %d entries were deferred in 3 minutes of outage", cw.journalPending(t))
		}
		time.Sleep(250 * time.Millisecond)
	}
	outage := time.Since(killed)
	cw.startRedis(t)
	restored := time.Now()
	time.Sleep(2 * time.Second) // callers keep calling across the recovery
	tr.halt()
	cw.assertWorkerAlive(t)
	backlog := cw.journalPending(t)
	cw.diagnoseJournal(t, "the moment the hot tier came back")

	type point struct {
		at time.Duration
		n  int
	}
	series := []point{{time.Since(restored), backlog}}
	deadline := time.Now().Add(15 * time.Minute)
	for series[len(series)-1].n != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after 15 minutes the journal still holds %d of %d entries", series[len(series)-1].n, backlog)
		}
		time.Sleep(time.Second)
		series = append(series, point{time.Since(restored), cw.journalPending(t)})
	}
	total := series[len(series)-1].at
	var half time.Duration // when the backlog first fell to half
	for _, p := range series {
		if p.n <= backlog/2 {
			half = p.at
			break
		}
	}
	rate := func(n int, d time.Duration) int64 { return int64(float64(n) / max(d.Seconds(), 0.001)) }
	measured(t, "T133", "journal-backlog-drain", "backlog", backlog, "outage", outage.Round(100*time.Millisecond), "seconds_to_empty", fmt.Sprintf("%.0f", total.Seconds()),
		"overall_per_s", rate(backlog, total), "first_half_per_s", rate(backlog/2, half), "second_half_per_s", rate(backlog/2, total-half), "replay_interval", "500ms")

	// The page clears with it, and nothing was lost or charged twice.
	eventually(t, 30*time.Second, "metering_journal_pending to read zero over an empty journal", func() bool { return cw.metric(t, "metering_journal_pending") == 0 })
	applied, replayed := tr.retryAll(t)
	measured(t, "T133", "journal-backlog-drain.retry-everything", "applied", applied, "replayed", replayed)
	if applied != 0 {
		t.Errorf("retrying every key charged %d again or for the first time: with the AOF fsynced on every command nothing may have been lost", applied)
	}
	tr.assertExact(t)
}

func redistestFsyncAlways() redistest.Options {
	return redistest.Options{AppendFsync: "always", FixedPort: true}
}
