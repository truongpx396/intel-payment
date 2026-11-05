//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
)

// R111, end to end: a message the real worker cannot handle is redelivered by the real bus up to PAYMENT_BUS_MAX_ATTEMPTS,
// dead-lettered, and parked by the worker's sweeper in PostgreSQL — where it stays, pages once, and is not delivered again.
func TestAMessageTheWorkerCannotHandleIsParkedOncePagesAndIsNotDeliveredAgain(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_BUS_MAX_ATTEMPTS", "2")))
	awaitOwning(t, pool, w, 8)

	// A low-balance hint the auto top-up handler cannot read: it is an error every time, so the bus gives up on it.
	rdb := hotredisClient(rd)
	defer func() { _ = rdb.Close() }()
	bus, err := redisstreams.NewBus(rdb, redisstreams.BusOptions{Clock: system.Clock{}, Consumer: "publisher"})
	must(t, err)
	const subject = "billing.balance.low.poison"
	poison := []byte("this is not a low-balance message \xff\x00")
	must(t, bus.Publish(context.Background(), subject, poison))

	letters := func() (n int) {
		must(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM dead_letters WHERE subject = $1`, subject).Scan(&n))
		return n
	}
	eventually(t, 30*time.Second, "the poison message to be parked", func() bool { return letters() == 1 })

	var (
		payload  []byte
		attempts int
		realmOf  string
		replayed *time.Time
	)
	must(t, pool.QueryRow(context.Background(), `SELECT payload, attempts, realm, replayed_at FROM dead_letters WHERE subject = $1`, subject).Scan(&payload, &attempts, &realmOf, &replayed))
	if string(payload) != string(poison) || attempts != 2 || realmOf != "(global)" || replayed != nil {
		t.Fatalf("the parked letter: payload %q, attempts %d, realm %q, replayed %v", payload, attempts, realmOf, replayed)
	}

	c := &http.Client{Timeout: 10 * time.Second}
	wbase := "http://" + w.listening(t, "ops")
	assertMetric(t, c, wbase+"/metrics", `metering_dead_lettered_total{realm="(global)"} 1`)
	assertMetric(t, c, wbase+"/metrics", `metering_dead_letters{realm="(global)"} 1`)

	// Nothing delivers it again: not the bus, not the sweeper. Let several ack-waits pass.
	time.Sleep(4 * time.Second)
	if n := letters(); n != 1 {
		t.Fatalf("%d letters after waiting, want 1", n)
	}
	assertMetric(t, c, wbase+"/metrics", `metering_dead_lettered_total{realm="(global)"} 1`)
}
