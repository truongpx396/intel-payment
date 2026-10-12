//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R068: cmd/payment-worker is the SOLE durable writer. It owns shards (one session advisory lock each), drains the
// Redis outbox into the books, and hands a shard on when it stops or dies. These tests run it as a process beside
// paymentd, against the real Postgres and Redis.

const lockClass = 0x494D5357 // the advisory-lock class of shard ownership ("IMSW")

func workerEnv(realm, dsn string, kv ...string) map[string]string {
	m := paymentdEnv(realm, dsn,
		"PAYMENT_SHARDS", "8", "PAYMENT_DRAIN_BATCH", "5", "PAYMENT_BUS_ACK_WAIT", "1s",
		"PAYMENT_DRAIN_INTERVAL", "50ms", "PAYMENT_OWNERSHIP_RETRY", "300ms", "PAYMENT_WORKER_ADDR", "127.0.0.1:0")
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func startWorker(t *testing.T, env map[string]string) *daemon {
	t.Helper()
	return startProcess(t, "payment-worker", env)
}

func ledgerPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgadapter.Connect(t.Context(), dsn)
	must(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// shardsOwned is how many shard locks are held on this database, and by how many sessions.
func shardsOwned(t *testing.T, pool *pgxpool.Pool) (locks, sessions int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		SELECT count(*), count(DISTINCT pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = $1 AND granted
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`, int32(lockClass)).Scan(&locks, &sessions)
	must(t, err)
	return locks, sessions
}

func awaitOwning(t *testing.T, pool *pgxpool.Pool, d *daemon, shards int) {
	t.Helper()
	eventually(t, 30*time.Second, fmt.Sprintf("the worker to own %d shards", shards), func() bool {
		select {
		case <-d.exited:
			t.Fatalf("the worker exited (%v) before it owned the shards; logs:\n%s", d.err, d.logs.String())
		default:
		}
		n, _ := shardsOwned(t, pool)
		return n == shards
	})
}

type booked struct {
	rows, usage, distinctKeys int
	sum                       int64
}

func bookedFor(t *testing.T, pool *pgxpool.Pool, realm string, sc domain.Scope) booked {
	t.Helper()
	var b booked
	must(t, pool.QueryRow(context.Background(), `
		SELECT count(*), count(*) FILTER (WHERE operation_type = 'usage'), count(DISTINCT idem_key), coalesce(sum(delta), 0)
		FROM credit_ledger WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`, realm, sc.Kind, sc.ID).Scan(&b.rows, &b.usage, &b.distinctKeys, &b.sum))
	return b
}

// grantWhenReady grants through paymentd, retrying while the shard is not open yet: the worker opens it.
func grantWhenReady(t *testing.T, c *http.Client, base, scopeJSON string) {
	t.Helper()
	eventually(t, 30*time.Second, "the grant to be accepted", func() bool {
		code, _ := httpDo(t, c, "POST", base+"/v1/grant", serviceToken, `{"scope":`+scopeJSON+`,"amount":1000,"reason":"purchase"}`, "Idempotency-Key", "g-1")
		return code == 200
	})
}

func record(t *testing.T, c *http.Client, base, scopeJSON, key string) int64 {
	t.Helper()
	code, body := httpDo(t, c, "POST", base+"/v1/record", serviceToken, shutdownRecordBody(scopeJSON, "llm.chat"), "Idempotency-Key", key)
	if code != 200 {
		t.Fatalf("record %s = %d %s", key, code, body)
	}
	var rc struct{ Delta int64 }
	must(t, json.Unmarshal([]byte(body), &rc))
	return rc.Delta
}

func TestAWorkerBooksWhatPaymentdAccepted(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	w := startWorker(t, workerEnv(realm, dsn))
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentdEnv(realm, dsn, "PAYMENT_SHARDS", "8"))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)

	grantWhenReady(t, c, base, scopeJSON)
	var spent int64
	for i := 0; i < 3; i++ {
		spent += record(t, c, base, scopeJSON, fmt.Sprintf("r-%d", i))
	}

	// The books catch up with the hot tier: a grant and three usages, once each, summing to what the service said.
	eventually(t, 30*time.Second, "the worker to book the grant and three records", func() bool { return bookedFor(t, pool, realm, sc).rows == 4 })
	b := bookedFor(t, pool, realm, sc)
	if b.usage != 3 || b.distinctKeys != 4 || b.sum != 1000+spent {
		t.Errorf("booked %+v, want 1 grant and 3 usages under 4 distinct keys summing to %d", b, 1000+spent)
	}
	if hot := hotBalance(t, redis.Addr, realm, dsn, sc); int64(hot) != b.sum {
		t.Errorf("the hot balance is %d and the books say %d: the two stores disagree", hot, b.sum)
	}
}

func TestAWorkerKilledMidBatchIsReplacedAndNothingIsBookedTwice(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	env := workerEnv(realm, dsn)
	a := startWorker(t, env)
	awaitOwning(t, pool, a, 8)
	d := startPaymentd(t, paymentdEnv(realm, dsn, "PAYMENT_SHARDS", "8"))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	grantWhenReady(t, c, base, scopeJSON)
	eventually(t, 30*time.Second, "the grant to be booked", func() bool { return bookedFor(t, pool, realm, sc).rows == 1 })

	// Hold the books shut: the worker reads the next records from the outbox and then cannot book them. This is
	// "mid-batch" without a race: the entries are delivered to it, unacknowledged, with its transaction waiting.
	hold, err := pool.Begin(t.Context())
	must(t, err)
	t.Cleanup(func() { _ = hold.Rollback(context.Background()) })
	_, err = hold.Exec(t.Context(), `LOCK TABLE account_watermarks IN ACCESS EXCLUSIVE MODE`)
	must(t, err)

	var spent int64
	for i := 0; i < 6; i++ {
		spent += record(t, c, base, scopeJSON, fmt.Sprintf("r-%d", i))
	}
	eventually(t, 15*time.Second, "the worker to be waiting on the books with the records in hand", func() bool {
		var n int
		must(t, pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n))
		return n > 0
	})

	// Kill it where it stands, then let the books open.
	must(t, a.cmd.Process.Signal(syscall.SIGKILL))
	<-a.exited
	must(t, hold.Rollback(context.Background()))

	// Another worker takes the shards over, claims what the dead one had been handed, and books it.
	b := startWorker(t, env)
	awaitOwning(t, pool, b, 8)
	eventually(t, 30*time.Second, "the replacement to book the six records", func() bool { return bookedFor(t, pool, realm, sc).usage == 6 })

	got := bookedFor(t, pool, realm, sc)
	if got.rows != 7 || got.distinctKeys != 7 || got.sum != 1000+spent {
		t.Errorf("booked %+v, want the grant and six usages once each under 7 distinct keys, summing to %d", got, 1000+spent)
	}
	var idem int
	must(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_idem WHERE realm = $1 AND scope_id = $2`, realm, sc.ID).Scan(&idem))
	if idem != 6 {
		t.Errorf("%d usage guards, want 6", idem)
	}
	if hot := hotBalance(t, redis.Addr, realm, dsn, sc); int64(hot) != got.sum {
		t.Errorf("the hot balance is %d and the books say %d: the two stores disagree", hot, got.sum)
	}
}

func TestAWorkerThatIsToldToStopReleasesItsShards(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	env := workerEnv(realm, dsn)
	a := startWorker(t, env)
	awaitOwning(t, pool, a, 8)
	b := startWorker(t, env) // a second replica owns nothing while the first holds everything
	time.Sleep(time.Second)
	if locks, sessions := shardsOwned(t, pool); locks != 8 || sessions != 1 {
		t.Fatalf("%d shard locks held by %d sessions, want 8 by 1: ownership is exclusive", locks, sessions)
	}

	must(t, a.cmd.Process.Signal(syscall.SIGTERM))
	if code := a.waitExit(t, 20*time.Second); code != 0 {
		t.Errorf("the worker exited %d on SIGTERM; logs:\n%s", code, a.logs.String())
	}
	if released := strings.Count(a.logs.String(), `"msg":"shard released"`); released != 8 {
		t.Errorf("the worker released %d shards by name, want 8; logs:\n%s", released, a.logs.String())
	}
	// The second replica takes everything over.
	awaitOwning(t, pool, b, 8)
	if _, sessions := shardsOwned(t, pool); sessions != 1 {
		t.Errorf("%d sessions hold the shards, want the replica's 1", sessions)
	}
}

func TestAWorkerRefusesToStartAgainstASchemaThatIsNotAtHeadOrAShardCountThatDisagrees(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	must(t, pgadapter.NewBooks(pool).EnsureHotConfig(t.Context(), 64))
	w := startWorker(t, workerEnv(realm, dsn, "PAYMENT_SHARDS", "128"))
	if code := w.waitExit(t, 30*time.Second); code == 0 {
		t.Fatalf("the worker exited 0 with the wrong shard count; logs:\n%s", w.logs.String())
	}
	if out := w.logs.String(); !strings.Contains(out, "64") || !strings.Contains(out, "128") {
		t.Errorf("the refusal does not say which counts disagree:\n%s", out)
	}
}

func TestAWorkerServesItsHealthAndMetricsOnItsOwnAddress(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool := ledgerPool(t, dsn)
	w := startWorker(t, workerEnv(realm, dsn))
	awaitOwning(t, pool, w, 8)
	base := "http://" + w.listening(t, "ops")
	c := &http.Client{Timeout: 10 * time.Second}
	if code, body := httpDo(t, c, "GET", base+"/healthz", "", ""); code != 200 {
		t.Errorf("/healthz = %d %s", code, body)
	}
	if code, body := httpDo(t, c, "GET", base+"/metrics", "", ""); code != 200 || !strings.Contains(body, "# TYPE metering_outbox_depth gauge") {
		t.Errorf("/metrics = %d and does not declare the catalogue:\n%.300s", code, body)
	}
}

// eventually polls until cond holds, failing with what it was waiting for.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}
