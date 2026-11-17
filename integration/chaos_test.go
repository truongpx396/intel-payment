//go:build integration && hardening

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/redis/redistest"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// T131: chaos. Each test below breaks something the design claims to survive, while traffic is running, and ends in
// the two checks the task names: nothing is charged twice, and a page fires where the contract says one does.
//
// The shape is the same each time. A real Redis in a container (SIGKILLed, rolled back), the real payment-worker
// binary booking into a real Postgres, and an embedded Meter driven by concurrent callers that remember every key
// they ever sent. That memory is the oracle: with it, "no double charge" is not "the numbers look plausible" but
// "the books hold exactly the keys that were sent, each once, and each scope's balance is its grant less exactly
// those charges". Where the contract allows a window of loss (a rolled-back AOF), the test says so, measures it,
// and then has the callers retry — which is what the contract says converges it.

const (
	// A chaos world is deliberately small: 16 shards (one Postgres connection and one polling loop each) at the worker's
	// default poll interval. The point is what breaks, not how much load the laptop can be made to carry.
	chaosShards    = 16
	chaosScopes    = 48
	chaosGrant     = 5_000_000
	chargePerEvent = 660 // the seeded card prices the standard event (1,200 in, 800 out) at exactly 660 credits
)

type chaosWorld struct {
	*loadStack
	met    *metrics
	worker *daemon
	ops    string // the worker's ops listener, where /metrics is
	http   *http.Client
}

func newChaosWorld(t *testing.T, ropts redistest.Options, kv ...string) *chaosWorld {
	t.Helper()
	return newChaosWorldN(t, chaosScopes, ropts, kv...)
}

func newChaosWorldN(t *testing.T, scopes int, ropts redistest.Options, kv ...string) *chaosWorld {
	t.Helper()
	met := newMetrics()
	s := newLoadStackWith(t, scopes, loadOpts{redis: ropts, metrics: met, grant: chaosGrant, short: true, shards: chaosShards})
	env := workerEnv(string(s.realm), s.dsn, append([]string{
		"PAYMENT_BALANCE_REDIS_URL", "redis://" + s.srv.Addr + "/0", "PAYMENT_SHARDS", strconv.Itoa(chaosShards),
		"PAYMENT_DRAIN_BATCH", "100", "PAYMENT_BUS_ACK_WAIT", "2s",
		"PAYMENT_JOURNAL_REPLAY_INTERVAL", "500ms", "PAYMENT_LOG_LEVEL", "info"}, kv...)...)
	w := startWorker(t, env)
	awaitOwning(t, s.pool, w, chaosShards)
	return &chaosWorld{loadStack: s, met: met, worker: w, ops: "http://" + w.listening(t, "ops"), http: &http.Client{Timeout: 10 * time.Second}}
}

// exec runs a statement against the stack's database, failing the test on an error.
func (s *loadStack) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		s.t.Fatalf("%s: %v", sql, err)
	}
}

// metric sums a counter or gauge from the worker's /metrics over the series whose line carries every label fragment.
func (cw *chaosWorld) metric(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	_, body := httpDo(t, cw.http, "GET", cw.ops+"/metrics", "", "")
	var sum float64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		match := true
		for _, l := range labels {
			match = match && strings.Contains(line, l)
		}
		f := strings.Fields(line)
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); match && err == nil {
			sum += v
		}
	}
	return sum
}

func (cw *chaosWorld) shardOf(sc domain.Scope) int { return int(sc.Normalized().Shard(cw.cfg.Shards)) }

// killRedis is a SIGKILL of the container: no shutdown, no final save. With appendfsync always, what it had
// acknowledged is on disk; everything else is gone.
func (cw *chaosWorld) killRedis(t *testing.T) {
	t.Helper()
	now := time.Duration(0)
	must(t, cw.srv.Container.Stop(context.Background(), &now))
}

func (cw *chaosWorld) startRedis(t *testing.T) {
	t.Helper()
	must(t, cw.srv.Start(context.Background()))
}

// recovered counts the shards whose generation the writer has bumped: each is one run of the freeze, drain, bump,
// unfreeze procedure.
func (cw *chaosWorld) recovered(t *testing.T) int {
	t.Helper()
	var n int
	must(t, cw.pool.QueryRow(context.Background(), `SELECT count(*) FROM hot_shards WHERE gen > 1`).Scan(&n))
	return n
}

// ------------------------------------------------------------------ the oracle --

type keyed struct {
	ev    domain.Event
	scope int
}

// traffic is concurrent callers that remember every key they sent and which of them were told it was accepted.
type traffic struct {
	cw       *chaosWorld
	mu       sync.Mutex
	sent     map[string]keyed
	acked    map[string]bool // Record returned without an error: applied, replayed, or deferred to the journal
	n        atomic.Int64
	errs     atomic.Int64
	deferred atomic.Int64
	stop     chan struct{}
	wg       sync.WaitGroup
}

func (cw *chaosWorld) newTraffic() *traffic {
	return &traffic{cw: cw, sent: map[string]keyed{}, acked: map[string]bool{}, stop: make(chan struct{})}
}

// run starts `workers` callers. Every thirtieth operation is immediately sent a second time, as a retrying client
// would, so duplicates are in flight throughout.
func (tr *traffic) run(workers int, think time.Duration) {
	for w := 0; w < workers; w++ {
		tr.wg.Add(1)
		go func() {
			defer tr.wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for i := 0; ; i++ {
				select {
				case <-tr.stop:
					return
				default:
				}
				idx := r.IntN(len(tr.cw.scopes))
				key := fmt.Sprintf("ch-%d", tr.n.Add(1))
				ev := tr.cw.event(tr.cw.scopes[idx], key, time.Now())
				tr.mu.Lock()
				tr.sent[key] = keyed{ev: ev, scope: idx}
				tr.mu.Unlock()
				for attempt := 0; attempt < 1+b2i(i%30 == 29); attempt++ {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					rc, err := tr.cw.meter.Record(ctx, ev)
					cancel()
					if err != nil {
						tr.errs.Add(1)
						continue
					}
					tr.mu.Lock()
					tr.acked[key] = true
					tr.mu.Unlock()
					if rc.Deferred {
						tr.deferred.Add(1)
					}
				}
				time.Sleep(think)
			}
		}()
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (tr *traffic) halt() { close(tr.stop); tr.wg.Wait() }

// retryAll is every caller whose acknowledgement may have been lost sending again, which the contract says makes the
// books converge. It returns how many of the retries applied a charge (one the system did not have) and how many
// were replays (one it did).
func (tr *traffic) retryAll(t *testing.T) (applied, replayed int64) {
	t.Helper()
	tr.mu.Lock()
	keys := make([]string, 0, len(tr.sent))
	for k := range tr.sent {
		keys = append(keys, k)
	}
	tr.mu.Unlock()
	sort.Strings(keys)
	var a, r atomic.Int64
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, k := range keys {
		ev := tr.sent[k].ev
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			deadline := time.Now().Add(90 * time.Second)
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				rc, err := tr.cw.meter.Record(ctx, ev)
				cancel()
				if err == nil {
					if rc.Applied && !rc.Deferred {
						a.Add(1)
					} else {
						r.Add(1)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Errorf("key %s could not be retried for 90s: %v", k, err)
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	return a.Load(), r.Load()
}

type bookState struct {
	usage, dupes int
	sums         map[string]int64
	keys         map[string]bool
}

func (cw *chaosWorld) booked(t *testing.T) bookState {
	t.Helper()
	ctx := context.Background()
	st := bookState{sums: map[string]int64{}, keys: map[string]bool{}}
	must(t, cw.pool.QueryRow(ctx, `SELECT count(*), count(*) - count(DISTINCT (scope_id, idem_key)) FROM credit_ledger WHERE realm = $1 AND rate_key = 'gpt'`, string(cw.realm)).Scan(&st.usage, &st.dupes))
	rs, err := cw.pool.Query(ctx, `SELECT scope_id, sum(delta) FROM credit_ledger WHERE realm = $1 GROUP BY scope_id`, string(cw.realm))
	must(t, err)
	for rs.Next() {
		var id string
		var sum int64
		must(t, rs.Scan(&id, &sum))
		st.sums[id] = sum
	}
	must(t, rs.Err())
	ks, err := cw.pool.Query(ctx, `SELECT idem_key FROM credit_ledger WHERE realm = $1 AND rate_key = 'gpt'`, string(cw.realm))
	must(t, err)
	for ks.Next() {
		var k string
		must(t, ks.Scan(&k))
		st.keys[k] = true
	}
	must(t, ks.Err())
	return st
}

// diagnoseJournal records what Postgres itself says about claiming the next batch of the journal while a backlog exists:
// the table's live and dead tuples, and the plan and timing of the very statement the replayer runs (Journal.Due),
// executed here inside a transaction that is rolled back so it changes nothing. It is evidence for a finding, not an
// assertion: when the replay of a backlog is slow, this says where the time goes.
func (cw *chaosWorld) diagnoseJournal(t *testing.T, label string) {
	t.Helper()
	ctx := context.Background()
	var live, dead int64
	var heap, idx string
	must(t, cw.pool.QueryRow(ctx, `SELECT n_live_tup, n_dead_tup, pg_size_pretty(pg_table_size('spend_journal')), pg_size_pretty(pg_indexes_size('spend_journal'))
		FROM pg_stat_user_tables WHERE relname = 'spend_journal'`).Scan(&live, &dead, &heap, &idx))
	tx, err := cw.pool.Begin(ctx)
	must(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) WITH due AS (
			SELECT id FROM spend_journal WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at, created_at LIMIT 200 FOR UPDATE SKIP LOCKED)
		UPDATE spend_journal j SET next_attempt_at = now() + interval '60 seconds' FROM due WHERE j.id = due.id RETURNING j.id`)
	must(t, err)
	var plan []string
	for rows.Next() {
		var l string
		must(t, rows.Scan(&l))
		plan = append(plan, "    "+l)
	}
	must(t, rows.Err())
	t.Logf("DIAGNOSTIC %s: spend_journal live=%d dead=%d heap=%s indexes=%s; the replayer's claim statement:\n%s", label, live, dead, heap, idx, strings.Join(plan, "\n"))
}

// journalPending is what the journal really holds: entries deferred during an outage that the writer has not yet booked.
func (cw *chaosWorld) journalPending(t *testing.T) int {
	t.Helper()
	var n int
	must(t, cw.pool.QueryRow(context.Background(), `SELECT count(*) FROM spend_journal WHERE realm = $1 AND status = 'pending'`, string(cw.realm)).Scan(&n))
	return n
}

func (cw *chaosWorld) openSuspense(t *testing.T) int {
	t.Helper()
	var n int
	must(t, cw.pool.QueryRow(context.Background(), `SELECT count(*) FROM credit_suspense WHERE realm = $1 AND status = 'open'`, string(cw.realm)).Scan(&n))
	return n
}

// lost is the keys callers were told were accepted that the books do not (yet) hold: the loss window, measured.
func (tr *traffic) lost(st bookState) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var n int
	for k := range tr.acked {
		if !st.keys[k] {
			n++
		}
	}
	return n
}

// assertExact is the end state every scenario must reach: the books hold each key that was sent exactly once, each
// scope is its grant less exactly those charges, the hot tier equals the books, and no suspense is left open.
func (tr *traffic) assertExact(t *testing.T) {
	t.Helper()
	cw := tr.cw
	tr.mu.Lock()
	perScope := map[string]int{}
	for _, ke := range tr.sent {
		perScope[cw.scopes[ke.scope].ID]++
	}
	sent := len(tr.sent)
	tr.mu.Unlock()
	want := func(id string) int64 { return cw.grant - int64(perScope[id])*chargePerEvent }

	var st bookState
	diff := func() string {
		st = cw.booked(t)
		var bad []string
		for _, sc := range cw.scopes {
			if got := st.sums[sc.ID]; got != want(sc.ID) {
				bad = append(bad, fmt.Sprintf("%s: books %d, grant less its charges %d (off by %d)", sc.ID, got, want(sc.ID), got-want(sc.ID)))
			}
		}
		switch {
		case st.dupes != 0:
			return fmt.Sprintf("%d usage rows share a key with another: something was booked twice", st.dupes)
		case st.usage != sent:
			return fmt.Sprintf("%d usage rows booked for %d keys sent", st.usage, sent)
		case len(bad) > 0:
			return fmt.Sprintf("%d scopes disagree, e.g. %s", len(bad), strings.Join(bad[:min(3, len(bad))], "; "))
		}
		return ""
	}
	deadline := time.Now().Add(120 * time.Second)
	for d := diff(); d != ""; d = diff() {
		if time.Now().After(deadline) {
			t.Fatalf("after 120s the books are not exact: %s", d)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// A retry that reaches a hot tier which has lost its idempotency guards is charged there AGAIN and told Applied: the
	// books refuse the duplicate on their durable key and the writer reverses the hot side with a correction
	// (hot-path-consistency.md §3, "duplicate"). Those corrections are asynchronous, so they are waited for BEFORE the hot
	// tier is compared with the books; until they have drained the hot balance is lower than the truth by exactly the
	// duplicates, which is the safe direction.
	deadline = time.Now().Add(120 * time.Second)
	for cw.openSuspense(t) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d suspense entries are still open after the scenario converged", cw.openSuspense(t))
		}
		time.Sleep(300 * time.Millisecond)
	}
	// The hot tier is rebuilt lazily from the books: ask each scope for its balance the way a caller would (an
	// admission heals a cold account), then compare with what Redis holds.
	for _, sc := range cw.scopes {
		var err error
		for stop := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if _, err = cw.meter.Admit(context.Background(), sc, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1}); err == nil || time.Now().After(stop) {
				break
			}
		}
		must(t, err)
		if hot, books := int64(cw.hotSum(sc)), st.sums[sc.ID]; hot != books {
			t.Errorf("scope %s: the hot tier holds %d and the books %d", sc.ID, hot, books)
		}
	}
}

func (cw *chaosWorld) assertWorkerAlive(t *testing.T) {
	t.Helper()
	select {
	case <-cw.worker.exited:
		t.Fatalf("the worker died during the scenario (%v); logs:\n%s", cw.worker.err, cw.worker.logs.String())
	default:
	}
}

// ---------------------------------------------------------------- 1. kill Redis --

// "Kill Redis." A SIGKILL, under load, with callers retrying throughout. Invariant 16: an outage never fails or drops
// usage — it defers it to the journal, and the page's condition (deferred usage the writer cannot drain) holds while
// the hot tier is down and clears when the journal has drained. With the AOF fsynced on every command, nothing the
// hot tier acknowledged may be lost, so retrying every key afterwards must charge nothing.
func TestChaosRedisKilledUnderLoadLosesNothingAndChargesNothingTwice(t *testing.T) { //nolint:paralleltest // chaos: it owns the machine's Docker for its duration
	cw := newChaosWorld(t, redistest.Options{AppendFsync: "always", FixedPort: true})
	tr := cw.newTraffic()
	tr.run(4, 10*time.Millisecond) // a few hundred calls a second: an outage's backlog is rate × duration, and replay is its own measurement
	time.Sleep(2 * time.Second)
	before := tr.n.Load()

	cw.killRedis(t)
	killed := time.Now()
	eventually(t, 90*time.Second, "usage to be deferred to the journal and the writer to report it pending (the MeteringDeferredUsageNotDraining condition)", func() bool {
		return cw.metric(t, "metering_journal_pending") > 0
	})
	pagedAfter := time.Since(killed)
	time.Sleep(2 * time.Second) // callers keep calling the dead hot tier
	cw.startRedis(t)
	outage := time.Since(killed)
	time.Sleep(3 * time.Second) // and across the recovery
	tr.halt()
	cw.assertWorkerAlive(t)
	backlog := int64(cw.metric(t, "metering_journal_pending"))
	cw.diagnoseJournal(t, "right after the hot tier came back, with the backlog in place")
	measured(t, "T131", "redis-killed.traffic", "sent", tr.n.Load(), "before_kill", before, "deferred", tr.deferred.Load(), "errors", tr.errs.Load(),
		"outage", outage.Round(time.Millisecond), "page_condition_true_after", pagedAfter.Round(time.Millisecond), "journal_pending_at_halt", backlog, "shards_recovered", cw.recovered(t))

	if tr.errs.Load() != 0 {
		t.Errorf("invariant 16: %d Record calls FAILED across the outage; usage must be deferred, never refused or dropped", tr.errs.Load())
	}
	if tr.deferred.Load() == 0 {
		t.Fatal("no Record was deferred: the traffic did not cross the outage, so nothing was tested")
	}
	// How long the deferred usage takes to drain is measured from the journal itself, not from the gauge that reports it:
	// it is the rate the worker replays at (up to 20 batches of 200 per tick) plus the time the writer takes to book what
	// was replayed, and it is the host's exposure window — the hot balance does not yet reflect usage still in the journal.
	drainStart := time.Now()
	eventually(t, 10*time.Minute, "the journal to hold no unsettled entry", func() bool { return cw.journalPending(t) == 0 })
	drained := time.Since(drainStart)
	measured(t, "T131", "redis-killed.journal-drain", "entries", backlog, "seconds", fmt.Sprintf("%.1f", drained.Seconds()), "per_s", int64(float64(backlog)/max(drained.Seconds(), 0.001)), "replay_interval", "500ms")
	// And the page must clear with it. The gauge is refreshed on the worker's next replay tick, so give it a few.
	eventually(t, 30*time.Second, "metering_journal_pending to read zero over an empty journal (the MeteringDeferredUsageNotDraining page clears)", func() bool {
		return cw.metric(t, "metering_journal_pending") == 0
	})

	// Before any retry: every key a caller was told was accepted is booked. fsync always means nothing was lost.
	deadline := time.Now().Add(180 * time.Second)
	var st bookState
	for {
		st = cw.booked(t)
		if lost := tr.lost(st); lost == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("%d keys the callers were told were accepted are not in the books 180s after the hot tier came back and the journal drained, with the AOF fsynced on every command", lost)
		}
		time.Sleep(300 * time.Millisecond)
	}
	if st.dupes != 0 {
		t.Errorf("%d usage rows share a key: a charge was booked twice", st.dupes)
	}
	applied, replayed := tr.retryAll(t)
	measured(t, "T131", "redis-killed.retry-everything", "applied", applied, "replayed", replayed)
	if applied != 0 {
		t.Errorf("retrying every key charged %d of them again or for the first time: with nothing lost, every retry must be a replay", applied)
	}
	tr.assertExact(t)
}

// ----------------------------------------------------------- 2. truncated AOF --

// "Restart it from a truncated AOF." The AOF loses a suffix it had acknowledged: the loss window the contract states
// (an under-bill, never an over-bill). The writer must notice by the node's replication id, freeze, drain, bump the
// generation and rebuild from the books; nothing may be charged twice at any point; and once the callers whose
// acknowledgements were lost retry, the books are exact.
func TestChaosRedisRestartedFromATruncatedAOFLosesOnlyTheWindowAndChargesNothingTwice(t *testing.T) { //nolint:paralleltest // chaos: it owns the machine's Docker for its duration
	cw := newChaosWorld(t, redistest.Options{AppendFsync: "always", FixedPort: true})
	ctx := context.Background()
	tr := cw.newTraffic()
	tr.run(16, 2*time.Millisecond)
	time.Sleep(1500 * time.Millisecond)
	// A checkpoint of the disk. The writer needs to have drained up to here for "the books already hold it" to be true,
	// which is the case the contract's first scenario is about; what lands after it is the loss window.
	size, err := cw.srv.AOFSize(ctx)
	must(t, err)
	time.Sleep(2 * time.Second)
	regBefore := cw.metric(t, "metering_hot_regression_total")

	cw.killRedis(t)
	must(t, cw.srv.TruncateAOF(ctx, size))
	cw.startRedis(t)
	time.Sleep(3 * time.Second)
	tr.halt()
	cw.assertWorkerAlive(t)

	// The writer notices on its own and recovers the shards.
	eventually(t, 90*time.Second, "the writer to recover at least one shard (generation bumped)", func() bool { return cw.recovered(t) > 0 })
	eventually(t, 5*time.Minute, "the journal to hold no unsettled entry", func() bool { return cw.journalPending(t) == 0 })
	time.Sleep(2 * time.Second)

	st := cw.booked(t)
	lost := tr.lost(st)
	measured(t, "T131", "truncated-aof.loss-window", "sent", tr.n.Load(), "acked_but_not_booked", lost, "shards_recovered", cw.recovered(t),
		"hot_regression_total_before", regBefore, "hot_regression_total_after", cw.metric(t, "metering_hot_regression_total"))
	if st.dupes != 0 {
		t.Errorf("%d usage rows share a key after the rollback: a charge was booked twice", st.dupes)
	}
	tr.mu.Lock()
	sent := len(tr.sent)
	tr.mu.Unlock()
	if st.usage > sent {
		t.Errorf("the books hold %d usage rows for %d keys sent: a charge the callers never made", st.usage, sent)
	}

	applied, replayed := tr.retryAll(t)
	measured(t, "T131", "truncated-aof.retry-everything", "applied", applied, "replayed", replayed, "lost_before", lost)
	tr.assertExact(t)
}

// ------------------------------------------------------------------ 3. drift --

// "Inject drift." At equal sequence numbers any non-zero drift is a defect by construction, so (SC-009) steady traffic
// produces ZERO findings and every injected drift is found, paged, and — per the contract — recorded as a finding,
// healed on the hot side only, and never written to the ledger.
func TestChaosInjectedDriftIsFoundPagedHealedOnTheHotSideAndNeverBooked(t *testing.T) { //nolint:paralleltest // chaos: it owns the machine's Docker for its duration
	cw := newChaosWorld(t, redistest.Options{}, "PAYMENT_RECONCILE_INTERVAL", "1h", "PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", "24h")
	tokens := "integration:" + auth.HashToken(serviceToken) + ":" + string(cw.realm) + ":record+grant+admin"
	d := startPaymentd(t, paymentdEnv(string(cw.realm), cw.dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+cw.srv.Addr+"/0", "PAYMENT_SHARDS", strconv.Itoa(chaosShards), "PAYMENT_SERVICE_TOKENS", tokens, "PAYMENT_LOG_LEVEL", "info"))
	base := d.httpURL(t)
	awaitReady(t, d, cw.http, base)

	var reconciles atomic.Int64
	reconcile := func(shard int) {
		t.Helper()
		who := `"actor":"chaos@example.com","reason":"T131: reconcile under injected drift"`
		code, out := httpDo(t, cw.http, "POST", base+"/v1/admin/reconcile?realm="+string(cw.realm), serviceToken,
			fmt.Sprintf(`{"shard":%d,"kind":"full",%s}`, shard, who), "Idempotency-Key", fmt.Sprintf("chaos-drift-%d", reconciles.Add(1)))
		if code != http.StatusAccepted {
			t.Fatalf("POST /v1/admin/reconcile = %d %s", code, out)
		}
		var acc struct{ Operation opView }
		must(t, json.Unmarshal([]byte(out), &acc))
		eventually(t, 60*time.Second, "the reconcile of shard "+strconv.Itoa(shard)+" to finish", func() bool {
			code, out := httpDo(t, cw.http, "GET", base+"/v1/admin/operations/"+acc.Operation.ID+"?realm="+string(cw.realm), serviceToken, "")
			var v opView
			return code == 200 && json.Unmarshal([]byte(out), &v) == nil && v.Status == "succeeded"
		})
	}
	type finding struct {
		Scope  map[string]string
		Pool   string
		Drift  int64
		Healed bool
	}
	type run struct {
		Shard         int
		Findings      []finding
		FindingsTotal int `json:"findings_total"`
	}
	findings := func() (all []finding, shards []int) {
		t.Helper()
		code, body := httpDo(t, cw.http, "GET", base+"/v1/admin/drift?realm="+string(cw.realm)+"&with_findings=true&limit=100", serviceToken, "")
		if code != 200 {
			t.Fatalf("GET /v1/admin/drift = %d %s", code, body)
		}
		var out struct{ Runs []run }
		must(t, json.Unmarshal([]byte(body), &out))
		for _, r := range out.Runs {
			all = append(all, r.Findings...)
			shards = append(shards, r.Shard)
		}
		return all, shards
	}

	// Steady traffic: reconcile the shards that carry it, while it runs, and again once it has drained.
	tr := cw.newTraffic()
	tr.run(8, 5*time.Millisecond)
	time.Sleep(time.Second)
	touched := map[int]bool{}
	for _, sc := range cw.scopes {
		touched[cw.shardOf(sc)] = true
	}
	var shardList []int
	for sh := range touched {
		shardList = append(shardList, sh)
	}
	sort.Ints(shardList)
	for _, sh := range shardList[:min(6, len(shardList))] {
		reconcile(sh)
	}
	time.Sleep(time.Second)
	tr.halt()
	eventually(t, 60*time.Second, "the writer to book everything before the quiet reconcile", func() bool { return cw.booked(t).usage == len(tr.sent) })
	for _, sh := range shardList[:min(6, len(shardList))] {
		reconcile(sh)
	}
	if f, _ := findings(); len(f) != 0 || cw.metric(t, "metering_drift_findings_total", `outcome="drift"`) != 0 {
		t.Fatalf("SC-009: steady traffic produced %d drift findings (counter %.0f): drift is a defect by construction at equal sequence numbers, so there must be none",
			len(f), cw.metric(t, "metering_drift_findings_total", `outcome="drift"`))
	}
	measured(t, "T131", "drift.steady-state", "shards_reconciled_twice", min(6, len(shardList)), "findings", 0)

	// Inject: +100 into one scope's hot balance, -250 into another's, straight in Redis, past the engine.
	ctx := context.Background()
	inject := func(sc domain.Scope, by int64) {
		t.Helper()
		key := fmt.Sprintf("acct:{s%d}:%s", cw.shardOf(sc), sc.Normalized().Tag())
		if _, err := cw.rdb.HGet(ctx, key, "b:general").Result(); err != nil {
			t.Fatalf("the account hash %s has no b:general to tamper with: %v", key, err)
		}
		must(t, cw.rdb.HIncrBy(ctx, key, "b:general", by).Err())
	}
	up, down := cw.scopes[0], cw.scopes[1]
	rowsBefore := cw.booked(t).usage
	var sumBefore int64
	for _, v := range cw.booked(t).sums {
		sumBefore += v
	}
	inject(up, +100)
	inject(down, -250)
	shardsToCheck := map[int]bool{cw.shardOf(up): true, cw.shardOf(down): true}
	for sh := range shardsToCheck {
		reconcile(sh)
	}

	all, _ := findings()
	got := map[string]int64{}
	healed := true
	for _, f := range all {
		got[f.Scope["id"]] += f.Drift
		healed = healed && f.Healed
	}
	if len(all) != 2 || got[up.ID] != 100 || got[down.ID] != -250 {
		t.Errorf("the injected drift was found as %+v (all findings %+v), want exactly +100 on %s and -250 on %s: a +100 and a -250 are two findings and never net", got, all, up.ID, down.ID)
	}
	if !healed {
		t.Errorf("a finding was not healed on the hot side: %+v", all)
	}
	eventually(t, 30*time.Second, "the page: metering_drift_findings_total{outcome=drift} to count both findings", func() bool {
		return cw.metric(t, "metering_drift_findings_total", `outcome="drift"`) >= 2
	})
	// "Reconcile never writes a ledger row": the books are exactly as they were.
	after := cw.booked(t)
	var sumAfter int64
	for _, v := range after.sums {
		sumAfter += v
	}
	if after.usage != rowsBefore || sumAfter != sumBefore {
		t.Errorf("reconcile changed the books: %d usage rows (was %d), sum %d (was %d); the contract says it never writes a ledger row", after.usage, rowsBefore, sumAfter, sumBefore)
	}
	// Healed on the hot side: the hot tier equals the books again.
	for _, sc := range []domain.Scope{up, down} {
		if hot, books := int64(cw.hotSum(sc)), after.sums[sc.ID]; hot != books {
			t.Errorf("after the heal scope %s holds %d in the hot tier and %d in the books", sc.ID, hot, books)
		}
	}
	measured(t, "T131", "drift.injected", "findings", len(all), "paged", cw.metric(t, "metering_drift_findings_total", `outcome="drift"`), "ledger_rows_written", after.usage-rowsBefore)
}

// ------------------------------------------------------------- 4. poison intent --

// "Poison an intent." An intent the hot tier produced legitimately and the books cannot take. Rather than forge one
// (a forged sequence number would corrupt a scope), a trigger in this test's own database refuses one idempotency
// key, so the real hot function's real intent fails to book every time. The contract: it is retried to the budget,
// parked in credit_outbox_dead with a poison suspense entry, the watermark advances so one bad entry does not block
// its scope, and the open suspense pages. A malformed outbox entry is parked whole and blocks nothing either.
func TestChaosAPoisonedIntentIsParkedPagesAndDoesNotBlockItsScope(t *testing.T) { //nolint:paralleltest // chaos: it owns the machine's Docker for its duration
	cw := newChaosWorld(t, redistest.Options{}, "PAYMENT_BUS_MAX_ATTEMPTS", "3")
	ctx := context.Background()
	cw.exec(`CREATE FUNCTION refuse_poison() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.idem_key = 'poison-1' THEN RAISE EXCEPTION 'T131: this intent cannot be booked'; END IF; RETURN NEW; END $$`)
	cw.exec(`CREATE TRIGGER refuse_poison BEFORE INSERT ON credit_ledger FOR EACH ROW EXECUTE FUNCTION refuse_poison()`)

	sc := cw.scopes[0]
	rc, err := cw.meter.Record(ctx, cw.event(sc, "poison-1", time.Now()))
	must(t, err)
	if !rc.Applied || rc.Delta != -chargePerEvent {
		t.Fatalf("the poisoned record was not applied in the hot tier: %+v", rc)
	}
	// A malformed entry in the same shard's outbox, and then ordinary traffic behind both, on the SAME scope.
	must(t, cw.rdb.XAdd(ctx, &goredis.XAddArgs{Stream: hotOutbox(cw.shardOf(sc)), Values: map[string]any{"junk": "this is not an intent"}}).Err())
	var behind []string
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("behind-%d", i)
		_, err := cw.meter.Record(ctx, cw.event(sc, key, time.Now()))
		must(t, err)
		behind = append(behind, key)
	}

	eventually(t, 90*time.Second, "the poisoned intent to be parked with a poison suspense entry", func() bool {
		var n int
		must(t, cw.pool.QueryRow(ctx, `SELECT count(*) FROM credit_outbox_dead WHERE realm = $1 AND idem_key = 'poison-1'`, string(cw.realm)).Scan(&n))
		var s int
		must(t, cw.pool.QueryRow(ctx, `SELECT count(*) FROM credit_suspense WHERE realm = $1 AND reason = 'poison' AND status = 'open'`, string(cw.realm)).Scan(&s))
		return n == 1 && s == 1
	})
	// The scope was not held hostage: everything sent behind the poison is booked.
	eventually(t, 60*time.Second, "the five records behind the poison to be booked", func() bool {
		st := cw.booked(t)
		for _, k := range behind {
			if !st.keys[k] {
				return false
			}
		}
		return true
	})
	// The malformed entry was kept whole, not dropped.
	var malformed int
	must(t, cw.pool.QueryRow(ctx, `SELECT count(*) FROM credit_outbox_dead WHERE scope_kind = 'unknown'`).Scan(&malformed))
	if malformed != 1 {
		t.Errorf("%d malformed entries were parked, want 1: it must be kept whole, never dropped", malformed)
	}
	// The page, and the invariant the suspense exists to keep: hot = booked + open suspense.
	eventually(t, 30*time.Second, "the page: metering_suspense_open to count the poison", func() bool {
		return cw.metric(t, "metering_suspense_open", `realm="`+string(cw.realm)+`"`) >= 1
	})
	st := cw.booked(t)
	var suspense int64
	must(t, cw.pool.QueryRow(ctx, `SELECT coalesce(sum(delta), 0) FROM credit_suspense WHERE realm = $1 AND scope_id = $2 AND status = 'open'`, string(cw.realm), sc.ID).Scan(&suspense))
	if hot, want := int64(cw.hotSum(sc)), st.sums[sc.ID]+suspense; hot != want {
		t.Errorf("hot %d != booked %d + open suspense %d: the two stores disagree about the poisoned scope", hot, st.sums[sc.ID], suspense)
	}
	measured(t, "T131", "poison.parked", "behind_booked", len(behind), "malformed_parked", malformed, "open_suspense_delta", suspense, "page", cw.metric(t, "metering_suspense_open"))
}

func hotOutbox(shard int) string { return "billing:outbox:{s" + strconv.Itoa(shard) + "}" }

// ------------------------------------------- 5. webhooks at volume, processor killed --

// "Replay webhooks at volume" and "kill a processor mid-event", together, because that is when they matter. A few
// hundred distinct payments are each delivered several times, concurrently and shuffled, with tampered copies mixed
// in; the processor is then held at the one instant that is dangerous — after it has minted the credits and before
// it has recorded that it did — and SIGKILLed there; a replacement finishes. Each payment must be fulfilled exactly
// once, the tampered copies must be refused and must page, and nothing may die.
func TestChaosPaymentsReplayedAtVolumeWithTheProcessorKilledMidEventAreFulfilledExactlyOnce(t *testing.T) { //nolint:paralleltest // chaos: it owns the machine's Docker for its duration
	const (
		events   = 120
		copies   = 4
		tampered = 16
	)
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	seedPayments(t, pool, realm, sc)
	ctx := context.Background()

	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8")))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 20 * time.Second}
	awaitReady(t, d, c, base)
	hook := base + "/webhooks/stripe/" + itAccount

	// The hold: a test-only trigger, in this test's database, makes the write that marks an event processed wait on an
	// advisory lock the test owns. The processor is then provably between minting and acknowledging.
	const holdLock = 7131
	_, err := pool.Exec(ctx, `CREATE FUNCTION hold_processed() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.status = 'processed' AND OLD.status IS DISTINCT FROM 'processed' THEN PERFORM pg_advisory_xact_lock(7131); END IF; RETURN NEW; END $$`)
	must(t, err)
	_, err = pool.Exec(ctx, `CREATE TRIGGER hold_processed BEFORE UPDATE ON payment_events FOR EACH ROW EXECUTE FUNCTION hold_processed()`)
	must(t, err)
	holder, err := pool.Acquire(ctx)
	must(t, err)
	_, err = holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, holdLock)
	must(t, err)
	held := true
	release := func() {
		if held {
			_, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, holdLock)
			holder.Release()
			held = false
		}
	}
	t.Cleanup(release)

	// Every delivery, signed now: distinct payments × several copies, plus tampered ones, shuffled.
	type delivered struct {
		body, sig string
		genuine   bool
	}
	var all []delivered
	for i := 0; i < events; i++ {
		body, sig := delivery(t, fmt.Sprintf("evt_chaos_%d", i), fmt.Sprintf("pi_chaos_%d", i))
		for c := 0; c < copies; c++ {
			all = append(all, delivered{body, sig, true})
		}
		if i < tampered {
			all = append(all, delivered{body + " ", sig, false}) // one byte changed after signing
		}
	}
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	var bad, refused atomic.Int64
	jobs := make(chan delivered)
	var wg sync.WaitGroup
	for w := 0; w < 24; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for dl := range jobs {
				code, _ := httpDo(t, c, "POST", hook, "", dl.body, "Stripe-Signature", dl.sig, "Content-Type", "application/json")
				switch {
				case dl.genuine && code != http.StatusOK:
					bad.Add(1)
				case !dl.genuine && code >= 200 && code < 300:
					bad.Add(1)
				case !dl.genuine:
					refused.Add(1)
				}
			}
		}()
	}
	for _, dl := range all {
		jobs <- dl
	}
	close(jobs)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d deliveries were answered wrongly: a genuine one not acknowledged, or a tampered one accepted", bad.Load())
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_events WHERE provider_account = $1`, itAccount); n != events {
		t.Fatalf("the inbox holds %d events for %d distinct payments delivered %d times each: replays must be deduplicated at the door", n, events, copies)
	}
	// The page where the contract says: tampered deliveries are refused and counted.
	_, doorMetrics := httpDo(t, c, "GET", base+"/metrics", "", "")
	if !metricAtLeast(doorMetrics, "billing_webhook_verify_failed_total", tampered) {
		t.Errorf("billing_webhook_verify_failed_total did not count the %d tampered deliveries (the BillingWebhookVerificationFailures page)", tampered)
	}

	// The processor starts, mints for the first event, and stops at the hold.
	w1 := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL)))
	awaitOwning(t, pool, w1, 8)
	eventually(t, 60*time.Second, "the processor to be held between minting and acknowledging", func() bool {
		return countRows(t, pool, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'`) > 0
	})
	mintedWhenKilled := hotBalance(t, rd, realm, dsn, sc)
	if mintedWhenKilled < itCredits {
		t.Fatalf("the processor is held but nothing was minted yet (%d credits): it is not between minting and acknowledging", mintedWhenKilled)
	}
	must(t, w1.cmd.Process.Signal(syscall.SIGKILL))
	<-w1.exited
	release()
	// Its lease would lapse in two minutes; the clock is the test's to move.
	_, err = pool.Exec(ctx, `UPDATE payment_events SET lease_until = now() - interval '1 second' WHERE status = 'processing'`)
	must(t, err)

	w2 := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL)))
	awaitOwning(t, pool, w2, 8)
	eventually(t, 120*time.Second, "the replacement to process every event", func() bool {
		return countRows(t, pool, `SELECT count(*) FROM payment_events WHERE provider_account = $1 AND status = 'processed'`, itAccount) == events
	})
	eventually(t, 60*time.Second, "the books to hold every grant", func() bool { return bookedFor(t, pool, realm, sc).rows == events })

	got := bookedFor(t, pool, realm, sc)
	if got.rows != events || got.distinctKeys != events || got.sum != int64(events)*itCredits {
		t.Errorf("booked %+v, want %d grants under %d distinct keys summing to %d: a payment was fulfilled twice or not at all", got, events, events, int64(events)*itCredits)
	}
	if hot := hotBalance(t, rd, realm, dsn, sc); int64(hot) != int64(events)*itCredits {
		t.Errorf("the hot balance is %d, want %d", hot, int64(events)*itCredits)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_events WHERE provider_account = $1 AND status = 'dead'`, itAccount); n != 0 {
		t.Errorf("%d events died: a verified payment that will not be fulfilled until a human looks", n)
	}
	measured(t, "T131", "payments.volume-and-kill", "payments", events, "deliveries", len(all), "tampered_refused", refused.Load(), "minted_when_killed", mintedWhenKilled,
		"booked_grants", got.rows, "credits", got.sum)
}

// metricAtLeast reads a Prometheus text exposition for the sum of a counter's series and compares it.
func metricAtLeast(body, name string, atLeast float64) bool {
	var sum float64
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			f := strings.Fields(line)
			if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil {
				sum += v
			}
		}
	}
	return sum >= atLeast
}
