//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// R110, end to end: the scheduled ticks are published the way an external scheduler would publish them — `payment-worker tick <name>` —
// and worked by a real payment-worker over Redis and PostgreSQL, each under its claim.

// servedTicks are the fourteen ticks of bus-subjects.md less `subdrift`, whose sweep is designed and not built.
var servedTicks = []string{"reconcile", "audit", "transfer", "journal", "webhook", "outbound", "expiry", "dunning", "partitions", "idem.purge", "events.purge", "trim", "usage.rollup"}

// publishTick runs `payment-worker tick …` with the worker's own environment and returns what it said.
func publishTick(t *testing.T, env map[string]string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary(t, "payment-worker"), append([]string{"tick"}, args...)...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("payment-worker tick %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func tickPayload(name string, shard int) []string {
	if name == "reconcile" || name == "audit" {
		return []string{name, fmt.Sprintf(`{"shard":%d}`, shard)}
	}
	return []string{name}
}

func tickCount(body, tick, outcome string) bool {
	return strings.Contains(body, fmt.Sprintf(`metering_tick_total{outcome=%q,tick=%q} 1`, outcome, tick))
}

// fingerprint is every table's row count and newest writer (xmin): it changes the instant a row is inserted, updated or deleted anywhere,
// which the statistics views do not (an idle backend reports its counters ten seconds late).
func fingerprint(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition ORDER BY 1`)
	must(t, err)
	var tables []string
	for rows.Next() {
		var name string
		must(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	rows.Close()
	must(t, rows.Err())
	out := map[string]string{}
	for _, name := range tables {
		var n int64
		var newest string
		must(t, pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*), coalesce(max(xmin::text::bigint), 0)::text FROM %q`, name)).Scan(&n, &newest))
		out[name] = fmt.Sprintf("%d rows, newest writer %s", n, newest)
	}
	return out
}

func TestEveryScheduledTickWithNothingDueIsServedCountedIdleAndWritesNothing(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	pool := ledgerPool(t, dsn)
	// Long timers: the worker's own sweeps stay quiet, so what is measured is the ticks.
	env := paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0",
		"PAYMENT_RECONCILE_INTERVAL", "1h", "PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", "24h", "PAYMENT_EVENTS_TICK_INTERVAL", "1m"))
	w := startWorker(t, env)
	awaitOwning(t, pool, w, 8)
	c := &http.Client{Timeout: 10 * time.Second}
	wbase := "http://" + w.listening(t, "ops")
	time.Sleep(2 * time.Second) // the worker's start-up writes have landed
	before := fingerprint(t, pool)

	for _, name := range servedTicks {
		publishTick(t, env, tickPayload(name, 3)...)
	}
	publishTick(t, env, "subdrift") // not served: published, never consumed
	for _, name := range servedTicks {
		eventually(t, 30*time.Second, name+" to be served", func() bool {
			_, body := httpDo(t, c, "GET", wbase+"/metrics", "", "")
			return tickCount(body, name, "idle") || tickCount(body, name, "ran")
		})
	}
	_, body := httpDo(t, c, "GET", wbase+"/metrics", "", "")
	for _, name := range servedTicks {
		for _, bad := range []string{"failed", "skipped"} {
			if tickCount(body, name, bad) {
				t.Errorf("the %s tick came to %s with nothing due:\n%s", name, bad, body)
			}
		}
	}
	if strings.Contains(body, `tick="subdrift"`) {
		t.Error("a tick this process does not serve was consumed")
	}
	for _, name := range []string{"reconcile", "audit", "transfer", "journal", "outbound", "expiry", "dunning", "partitions", "idem.purge", "events.purge", "usage.rollup"} {
		if !tickCount(body, name, "idle") {
			t.Errorf("the %s tick had nothing due and was not counted idle", name)
		}
	}

	after := fingerprint(t, pool)
	for table, state := range after {
		if state != before[table] {
			t.Errorf("a round of ticks with nothing due changed %s: %s, then %s", table, before[table], state)
		}
	}
}

func TestPartitionsAreRemadeAndOldUsageGuardsDroppedByTheirTicksOnceEach(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	pool := ledgerPool(t, dsn)
	env := paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0"))
	w := startWorker(t, env)
	awaitOwning(t, pool, w, 8)
	c := &http.Client{Timeout: 10 * time.Second}
	wbase := "http://" + w.listening(t, "ops")
	ctx := t.Context()
	exists := func(name string) bool {
		var n int
		must(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = $1`, name).Scan(&n))
		return n == 1
	}

	// A partition that is missing is made, and the same tick delivered twice at once is one run.
	day := time.Now().UTC().AddDate(0, 0, 2).Format("20060102")
	for _, parent := range []string{"usage_idem", "usage_events"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE %s_d%s`, parent, day)); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { publishTick(t, env, "partitions"); done <- struct{}{} }()
	}
	<-done
	<-done
	eventually(t, 30*time.Second, "the missing partitions to be made", func() bool {
		return exists("usage_idem_d"+day) && exists("usage_events_d"+day)
	})
	eventually(t, 30*time.Second, "the partitions tick to be counted", func() bool {
		_, body := httpDo(t, c, "GET", wbase+"/metrics", "", "")
		return tickCount(body, "partitions", "ran")
	})
	_, body := httpDo(t, c, "GET", wbase+"/metrics", "", "")
	if strings.Contains(body, `metering_tick_total{outcome="ran",tick="partitions"} 2`) {
		t.Fatalf("two ticks published at once made partitions twice:\n%s", body)
	}

	// A day of the usage guard past its window is dropped; today's, and the default partition, are not.
	for _, d := range [][3]string{{"usage_idem_d20200101", "2020-01-01", "2020-01-02"}, {"usage_idem_d20200102", "2020-01-02", "2020-01-03"}} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s PARTITION OF usage_idem FOR VALUES FROM ('%s') TO ('%s')`, d[0], d[1], d[2])); err != nil {
			t.Fatal(err)
		}
	}
	publishTick(t, env, "idem.purge")
	eventually(t, 30*time.Second, "the old usage guards to be dropped", func() bool {
		return !exists("usage_idem_d20200101") && !exists("usage_idem_d20200102")
	})
	if !exists("usage_idem_d"+time.Now().UTC().Format("20060102")) || !exists("usage_idem_default") {
		t.Fatal("the idem.purge tick dropped a partition inside the window, or the default one")
	}
}

func TestAReconcileTickDeliveredTwiceRecordsOneRunAndThenFindsNothingDue(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	env := paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_RECONCILE_INTERVAL", "1h", "PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", "24h"))
	w := startWorker(t, env)
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8")))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	grantWhenReady(t, c, base, scopeJSON)
	record(t, c, base, scopeJSON, "r-1")
	eventually(t, 30*time.Second, "the usage to be booked", func() bool { return bookedFor(t, pool, realm, sc).rows >= 2 })

	shard := int(sc.Shard(8))
	runs := func() (n int) {
		must(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM reconcile_runs WHERE shard = $1 AND kind = 'incremental'`, shard).Scan(&n))
		return n
	}
	payload := fmt.Sprintf(`{"shard":%d}`, shard)
	done := make(chan struct{}, 2)
	for range 2 {
		go func() { publishTick(t, env, "reconcile", payload); done <- struct{}{} }()
	}
	<-done
	<-done
	eventually(t, 30*time.Second, "the reconcile run to be recorded", func() bool { return runs() >= 1 })
	time.Sleep(2 * time.Second)
	if n := runs(); n != 1 {
		t.Fatalf("a reconcile tick delivered twice recorded %d runs, want 1", n)
	}
	// Nothing has been booked since: another tick finds nothing due, and records nothing.
	publishTick(t, env, "reconcile", payload)
	time.Sleep(3 * time.Second)
	if n := runs(); n != 1 {
		t.Fatalf("a reconcile tick with nothing due recorded a run: %d", n)
	}
	wbase := "http://" + w.listening(t, "ops")
	_, body := httpDo(t, c, "GET", wbase+"/metrics", "", "")
	if !strings.Contains(body, `metering_tick_total{outcome="idle",tick="reconcile"}`) {
		t.Fatalf("the idle reconcile was not counted:\n%s", body)
	}
}

func TestTheUsageRollupTickBuildsTheDailyRowsFromWhatWasBookedAndThenFindsNothingToDo(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	env := paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_RECONCILE_INTERVAL", "1h", "PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", "24h"))
	w := startWorker(t, env)
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8")))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	grantWhenReady(t, c, base, scopeJSON)
	var spent int64
	for i := range 3 {
		spent += -record(t, c, base, scopeJSON, fmt.Sprintf("r-%d", i)) // a record's delta is negative
	}
	eventually(t, 30*time.Second, "the usage to be booked", func() bool { return bookedFor(t, pool, realm, sc).usage == 3 })

	rows := func() (credits, events int64, n int) {
		must(t, pool.QueryRow(t.Context(), `SELECT coalesce(sum(credits), 0), coalesce(sum(events), 0), count(*) FROM usage_daily WHERE realm = $1 AND scope_id = $2`, realm, sc.ID).Scan(&credits, &events, &n))
		return credits, events, n
	}
	if _, _, n := rows(); n != 0 {
		t.Fatalf("%d rows before any rollup: the rollup is a tick, not a side effect of booking", n)
	}
	publishTick(t, env, "usage.rollup")
	eventually(t, 30*time.Second, "the daily rows to be built", func() bool { _, _, n := rows(); return n > 0 })
	credits, events, _ := rows()
	if credits != spent || events != 3 {
		t.Fatalf("the daily rows say %d credits over %d events, the books say %d over 3", credits, events, spent)
	}
	var dim string
	must(t, pool.QueryRow(t.Context(), `SELECT dimension FROM usage_daily WHERE realm = $1 AND scope_id = $2 LIMIT 1`, realm, sc.ID).Scan(&dim))
	if dim != "llm.chat" {
		t.Fatalf("the dimension is the resource that was metered: %q", dim)
	}

	// Nothing booked since: the same window again, and a backfill of thirty days, change nothing and are counted idle.
	wbase := "http://" + w.listening(t, "ops")
	publishTick(t, env, "usage.rollup", `{"days":30}`)
	publishTick(t, env, "usage.rollup")
	eventually(t, 30*time.Second, "the idle rollup to be counted", func() bool {
		_, body := httpDo(t, c, "GET", wbase+"/metrics", "", "")
		return strings.Contains(body, `metering_tick_total{outcome="idle",tick="usage.rollup"}`)
	})
	if c2, e2, _ := rows(); c2 != credits || e2 != events {
		t.Fatalf("a window nothing changed in changed the rows: %d/%d, then %d/%d", credits, events, c2, e2)
	}
}
