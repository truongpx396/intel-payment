//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// R143 (FR-039), end to end: GET /v1/credits of a real paymentd carries what the usage_daily rollup holds — the breakdown by resource, a
// fortnight of days, and a burn rate — once the durable writer has booked the spend and the rollup has run, and a member's snapshot carries
// none of it. Until the snapshot was wired to the rollup the three were absent, so no screen could draw them from a real server.

type spendSnapshot struct {
	Balance struct {
		BurnRatePerDay *int64
	}
	Series []struct {
		At     string
		Amount int64
	}
	Breakdown []struct {
		Key, Label string
		Amount     int64
	}
}

func TestPaymentdServesTheSpendTheRollupHoldsAndAMembersViewHasNone(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	rd := dedicatedRedis(t)
	rdURL := "redis://" + rd + "/0"
	pool := ledgerPool(t, dsn)
	w := startWorker(t, paymentsEnv(workerEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL)))
	awaitOwning(t, pool, w, 8)
	tokens := "integration:" + auth.HashToken(serviceToken) + ":" + realm + ":record+grant"
	d := startPaymentd(t, paymentsEnv(paymentdEnv(realm, dsn, "PAYMENT_BALANCE_REDIS_URL", rdURL, "PAYMENT_SHARDS", "8", "PAYMENT_SERVICE_TOKENS", tokens)))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)

	id := fmt.Sprintf("w-%d", seq.Add(1))
	scope := fmt.Sprintf(`{"kind":"workspace","id":%q}`, id)
	grantWhenReady(t, c, base, scope)
	// three ordinary events at 660 credits each: two chats and an embedding
	for i, resource := range []string{"llm.chat", "llm.chat", "llm.embed"} {
		if code, body := httpDo(t, c, "POST", base+"/v1/record", serviceToken, shutdownRecordBody(scope, resource), "Idempotency-Key", fmt.Sprintf("spend-%d", i)); code != 200 {
			t.Fatalf("record %d = %d %s", i, code, body)
		}
	}
	// the balance moves at once; the books, and so the rollup, follow when the writer has booked
	eventually(t, 30*time.Second, "the writer to book the three events", func() bool {
		return countRows(t, pool, `SELECT count(*) FROM credit_ledger WHERE realm = $1 AND scope_kind = 'workspace' AND scope_id = $2 AND rate_key IS NOT NULL`, realm, id) == 3
	})
	now := time.Now().UTC()
	if _, err := pgadapter.NewUsage(pool).Rollup(t.Context(), now.AddDate(0, 0, -2), now.AddDate(0, 0, 1), time.UTC); err != nil {
		t.Fatal(err)
	}

	read := func(query string) (int, string, spendSnapshot) {
		t.Helper()
		code, body := httpDo(t, c, "GET", base+"/v1/credits?scope_kind=workspace&scope_id="+id+query, serviceToken, "")
		var s spendSnapshot
		must(t, json.Unmarshal([]byte(body), &s))
		return code, body, s
	}

	code, body, s := read("")
	if code != 200 {
		t.Fatalf("GET /v1/credits = %d %s", code, body)
	}
	if len(s.Breakdown) != 2 || s.Breakdown[0].Key != "llm.chat" || s.Breakdown[0].Amount != 1320 || s.Breakdown[1].Key != "llm.embed" || s.Breakdown[1].Amount != 660 {
		t.Errorf("by resource, largest first: %+v (%s)", s.Breakdown, body)
	}
	if len(s.Series) != 14 {
		t.Fatalf("a fortnight of days: %d points (%s)", len(s.Series), body)
	}
	var total, byResource int64
	first := -1
	for i, p := range s.Series {
		total += p.Amount
		if p.Amount > 0 && first < 0 {
			first = i
		}
	}
	for _, r := range s.Breakdown {
		byResource += r.Amount
	}
	if total != 1980 || byResource != total {
		t.Errorf("the days add up to %d and the resources to %d, want 1980 for both", total, byResource)
	}
	// The spend happened a moment ago, so it is today's — or, if the test straddled midnight UTC, yesterday's and today's: either way the
	// burn rate is the smallest whole figure that covers it over the days since it began.
	if s.Balance.BurnRatePerDay == nil || first < 0 {
		t.Fatalf("no burn rate over a window with spend: %s", body)
	}
	days, burn := int64(len(s.Series)-first), *s.Balance.BurnRatePerDay
	if burn*days < total || (burn-1)*days >= total {
		t.Errorf("burn rate %d over %d days does not just cover %d", burn, days, total)
	}
	for i := 1; i < len(s.Series); i++ {
		if s.Series[i-1].At >= s.Series[i].At { // RFC 3339 instants in one zone sort as text
			t.Errorf("the series is not oldest first at %s", s.Series[i].At)
		}
	}

	// ?days= sets the window
	if _, _, three := read("&days=3"); len(three.Series) != 3 {
		t.Errorf("days=3 gave %d points", len(three.Series))
	}

	// A member's snapshot has no spend section at all: the rollup is keyed by an actor ordinary events do not carry, so a member's days cannot
	// be told from a colleague's. Absent is the honest answer and the safe one.
	code, body, member := read("&subject_kind=user&subject_id=u1")
	if code != 200 {
		t.Fatalf("a member's GET /v1/credits = %d %s", code, body)
	}
	if member.Balance.BurnRatePerDay != nil || len(member.Series) != 0 || len(member.Breakdown) != 0 {
		t.Errorf("a member's snapshot carries spend: %s", body)
	}
}
