//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	pricingtable "github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// T134: two adoption walkthroughs, EXECUTED. docs/integration-guide.md says a host adopts the engine with "a scope
// binding, a rate card and a realm's configuration rows" and nothing under metering/ (SC-005). A guide is only as true as
// the last time someone did what it says, so each test below is a new product doing exactly that, from an empty realm.

// guideCurlBody is the JSON the guide's own `curl … <path> … -d '{…}'` sends, read out of the guide so that the test and
// the text cannot drift apart: if the guide's call is wrong, this test is what fails.
func guideCurlBody(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile("../docs/integration-guide.md")
	must(t, err)
	doc := string(b)
	i := strings.Index(doc, path)
	if i < 0 {
		t.Fatalf("the guide no longer shows a call to %s", path)
	}
	rest := doc[i:]
	j := strings.Index(rest, "-d '")
	if j < 0 {
		t.Fatalf("the guide's call to %s has no -d '…' body", path)
	}
	rest = rest[j+len("-d '"):]
	k := strings.Index(rest, "'")
	if k < 0 {
		t.Fatalf("the guide's body for %s is not closed", path)
	}
	return rest[:k]
}

// withWho adds the `actor` and `reason` every admin write must carry (rest-api.md: "Every write names a person and a reason").
func withWho(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	must(t, json.Unmarshal([]byte(body), &m))
	m["actor"], m["reason"] = "adopter@example.com", "onboarding a new product"
	out, err := json.Marshal(m)
	must(t, err)
	return string(out)
}

// TestAdoptionAnEmbeddedGoHostWiresTheLibraryAndMetersItsFirstCustomer is the library path: a Go product that
// embeds the engine in its own process. It is also the compiled form of the guide's "Library" snippet, which names
// constructors that do not exist: this is what the wiring actually takes. The embedded engine still needs the one
// durable writer, `payment-worker`, which opens the shards and books what the hot tier accepted.
func TestAdoptionAnEmbeddedGoHostWiresTheLibraryAndMetersItsFirstCustomer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	realm := fmt.Sprintf("acme-crm-%d", seq.Add(1))
	dsn := pg.NewDB(t) // migrated and EMPTY: no realm, no rate card
	rd := dedicatedRedis(t)
	pool := ledgerPool(t, dsn)

	// What the product brings, as rows (scripts/seed.sql is the complete example): a rate card and a balance ceiling.
	// The pool `general` is implicit. Nothing under metering/ is edited.
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, q, args...)
		must(t, err)
	}
	exec(`INSERT INTO rate_cards (realm, version, pricer) VALUES ($1,'2026-10-01','table')`, realm)
	exec(`INSERT INTO rate_card_entries (realm, version, rate_key, unit, credits_per_block, block_size, cost_micros_per_block) VALUES
		($1,'2026-10-01','standard','enrichment',2000,1,0), ($1,'2026-10-01','standard','api_call',150000,1000000,0)`, realm)
	exec(`INSERT INTO limits (realm, name, unit, max, window_kind, deny_code, sort_order) VALUES ($1,'scope_balance','credit',0,'balance','payment_required',0)`, realm)

	// The one durable writer, as a process.
	w := startWorker(t, workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0"))
	awaitOwning(t, pool, w, 8)

	// The wiring: this is the whole of it.
	cfg := metering.Config{Realm: domain.Realm(realm), BalanceRedisURL: "redis://" + rd + "/0", LedgerDSN: dsn, Shards: 8}.WithDefaults()
	rdb := hotredisClient(rd)
	t.Cleanup(func() { _ = rdb.Close() })
	cfgStore, err := pgadapter.NewConfigStore(ctx, pool, pgadapter.ConfigOptions{})
	must(t, err)
	t.Cleanup(cfgStore.Close)
	hot, err := hotredis.New(rdb, hotredis.Options{Config: cfg, Pools: cfgStore, Limits: cfgStore, Clock: system.Clock{}})
	must(t, err)
	meter, err := app.New(cfg, app.Deps{
		Pricers: ports.PricerRegistry{"table": pricingtable.New(0)},
		Balance: hot, Books: pgadapter.NewBooks(pool), Journal: pgadapter.NewJournal(pool),
		Limits: cfgStore, Pools: cfgStore, RateCards: cfgStore,
		Bus: &nullBus{}, Clock: system.Clock{}, IDs: &system.IDs{},
	})
	must(t, err)

	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: "acme-1"}
	if adm, err := meter.Admit(ctx, sc, domain.AdmitRequest{Resource: "crm.enrich", MaxCost: 1000}); err != nil || adm.Allowed {
		t.Fatalf("an unfunded scope must be refused before the work: %+v %v", adm, err)
	}
	var rc domain.Receipt
	for stop := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if rc, err = meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 1_000_000, IdemKey: "g-1", Reason: "purchase"}); err == nil || time.Now().After(stop) {
			break // the worker opens the shard; the first grant may have to wait for it
		}
	}
	must(t, err)
	if !rc.Applied {
		t.Fatalf("the grant was not applied: %+v", rc)
	}
	started := time.Now()
	ev := domain.Event{Scope: sc, Resource: "crm.enrich", RateKey: "standard", IdemKey: "req-0001", OccurredAt: started,
		Quantities: []domain.Quantity{{Unit: "enrichment", Amount: 3}, {Unit: "api_call", Amount: 2000}}}
	first, err := meter.Record(ctx, ev)
	must(t, err)
	if !first.Applied || first.Delta != -6300 { // 3 × 2000, and 2000 calls at 150000 per million
		t.Fatalf("the record was not priced exactly: %+v", first)
	}
	if again, err := meter.Record(ctx, ev); err != nil || again.Applied {
		t.Fatalf("the same charge replayed must not apply twice: %+v %v", again, err)
	}
	ev.Quantities = []domain.Quantity{{Unit: "enrichment", Amount: 4}}
	if _, err := meter.Record(ctx, ev); err == nil {
		t.Fatal("the same key with a different request must be refused, never a silent replay")
	}
	eventually(t, 30*time.Second, "the worker to book the grant and the charge", func() bool { return bookedFor(t, pool, realm, sc).rows == 2 })
	if b := bookedFor(t, pool, realm, sc); b.sum != 1_000_000-6300 || b.distinctKeys != 2 {
		t.Errorf("booked %+v, want the grant less exactly one charge of 6300, under two keys", b)
	}
}

// TestAdoptionANonGoHostFollowsTheGuideFromAnEmptyRealmWithOnlyARateCard is the service path: a product written in
// another language, speaking REST to the containerised service, bringing a rate card and the name of the scope it
// bills. The realm starts EMPTY — migrated, nothing seeded — and the host's first act is the guide's own call.
func TestAdoptionANonGoHostFollowsTheGuideFromAnEmptyRealmWithOnlyARateCard(t *testing.T) {
	t.Parallel()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed, so the non-Go host cannot run")
	}
	realm := fmt.Sprintf("acme-py-%d", seq.Add(1))
	dsn := pg.NewDB(t)
	rd := dedicatedRedis(t)
	pool := ledgerPool(t, dsn)
	w := startWorker(t, workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0"))
	awaitOwning(t, pool, w, 8)
	d := startPaymentd(t, paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", "redis://"+rd+"/0", "PAYMENT_SHARDS", "8"))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 15 * time.Second}
	awaitReady(t, d, c, base)

	// 1. The guide's own rate-card call, exactly as written there.
	literal := guideCurlBody(t, "/v1/admin/rate-cards")
	// Sent as `curl -d` sends it: with the content type curl chooses, since the guide's call names none.
	code, out := httpDo(t, c, "POST", base+"/v1/admin/rate-cards", serviceToken, literal, "Content-Type", "application/x-www-form-urlencoded")
	if code/100 != 2 {
		t.Errorf("the guide's rate-card call, sent exactly as written, was refused: %d %s", code, out)
		code, out = httpDo(t, c, "POST", base+"/v1/admin/rate-cards", serviceToken, withWho(t, literal)) // carry on, so the rest is still walked
	}
	if code/100 != 2 {
		t.Fatalf("a rate card could not be published even with an actor and a reason: %d %s", code, out)
	}
	// 2. Observed, not assumed: what a realm with a rate card but NO balance ceiling does with an unfunded scope.
	code, out = httpDo(t, c, "POST", base+"/v1/admit", serviceToken, `{"scope":{"kind":"workspace","id":"acme-1"},"resource":"crm.enrich","max_cost":1000}`)
	t.Logf("MEASURED T134 before-the-balance-ceiling: an unfunded scope's admission was answered %d %s", code, out)
	unrefusedWithoutCeiling := code == http.StatusOK

	// 3. The balance ceiling: the limit that makes `402 payment_required` happen.
	code, out = httpDo(t, c, "PUT", base+"/v1/admin/limits/scope_balance", serviceToken,
		withWho(t, `{"unit":"credit","max":0,"window":"balance","deny_code":"payment_required"}`))
	if code/100 != 2 {
		t.Fatalf("the balance ceiling could not be configured: %d %s", code, out)
	}

	t.Logf("MEASURED T134 without-the-balance-ceiling-an-unfunded-scope-is-admitted=%v", unrefusedWithoutCeiling)

	// 4. The host itself: not a line of Go.
	cmd := exec.Command(py, "-I", "adoption_host.py", base, serviceToken, "acme-1")
	res, err := cmd.CombinedOutput()
	t.Logf("the non-Go host:\n%s", res)
	if err != nil {
		t.Errorf("the non-Go host failed (%v)", err)
	}

	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: "acme-1"}
	eventually(t, 30*time.Second, "the worker to book the grant and the charge", func() bool { return bookedFor(t, pool, realm, sc).rows == 2 })
	if b := bookedFor(t, pool, realm, sc); b.sum != 1_000_000-6300 || b.distinctKeys != 2 {
		t.Errorf("booked %+v, want the grant less exactly one charge of 6300, under two keys", b)
	}
}
