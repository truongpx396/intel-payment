package app

import (
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func usage(seq int64, pool domain.Pool, delta domain.Credits, qty ...domain.Quantity) usageRec {
	row := ports.LedgerRow{
		Pool: pool, Delta: delta, OperationType: "query", Resource: "llm.chat", RateKey: "gpt", RateCardVersion: "v1",
		SeqFrom: seq, SeqTo: seq, EventCount: 1, CostMicros: 10, Quantities: qty,
	}
	return usageRec{rows: []ports.LedgerRow{row}}
}

func TestRollupCollapsesABatchToOneRowPerPoolAndRateKey(t *testing.T) {
	t.Parallel()
	in := domain.Quantity{Unit: "in", Amount: 100}
	rows := rollup([]usageRec{
		usage(5, "general", -30, in),
		usage(6, "general", -20, in, domain.Quantity{Unit: "out", Amount: 7}),
		usage(7, "promo", -10, in),
	})
	if len(rows) != 2 {
		t.Fatalf("one row per pool: %+v", rows)
	}
	g := rows[0]
	if g.Pool != "general" || g.Delta != -50 || g.EventCount != 2 || g.SeqFrom != 5 || g.SeqTo != 6 || g.CostMicros != 20 {
		t.Fatalf("general: %+v", g)
	}
	sum := map[domain.Unit]int64{}
	for _, q := range g.Quantities {
		sum[q.Unit] += q.Amount
	}
	if sum["in"] != 200 || sum["out"] != 7 {
		t.Fatalf("quantities are summed per unit: %v", sum)
	}
	if p := rows[1]; p.Pool != "promo" || p.Delta != -10 || p.EventCount != 1 {
		t.Fatalf("promo: %+v", p)
	}
}

func TestRollupKeepsDifferentCardVersionsApart(t *testing.T) {
	t.Parallel()
	a, b := usage(1, "general", -10), usage(2, "general", -10)
	b.rows[0].RateCardVersion = "v2" // a re-priced batch must stay re-priceable per version
	if got := rollup([]usageRec{a, b}); len(got) != 2 {
		t.Fatalf("rows: %+v", got)
	}
}

func TestRollupKeepsTheSumOfDeltasExact(t *testing.T) {
	t.Parallel()
	var recs []usageRec
	var want domain.Credits
	for i := int64(1); i <= 50; i++ {
		d := -domain.Credits(i)
		want += d
		recs = append(recs, usage(i, "general", d))
	}
	var got domain.Credits
	for _, r := range rollup(recs) {
		got += r.Delta
	}
	if got != want {
		t.Fatalf("a rollup must not change the balance: %d != %d", got, want)
	}
}

func TestUsageRowsPutQuantitiesAndCostOnTheFirstRowOnly(t *testing.T) {
	t.Parallel()
	b := &booking{scope: domain.Scope{Realm: "t1", Kind: "workspace", ID: "w"}}
	in := domain.Intent{
		Op: domain.OpUsage, Scope: b.scope, Gen: 1, Seq: 9, IdemKey: "k",
		Draws:   []domain.PoolDelta{{Pool: "promo", Delta: -30}, {Pool: "general", Delta: -70}},
		Payload: domain.Payload{Resource: "llm.chat", CostMicros: 500, Quantities: []domain.PayloadQuantity{{Unit: "in", Amount: 1200}}, OccurredAt: "2026-09-29T12:00:00Z"},
	}
	rows, _, err := b.usageRows(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || len(rows[0].Quantities) != 1 || rows[0].CostMicros != 500 || len(rows[1].Quantities) != 0 || rows[1].CostMicros != 0 {
		t.Fatalf("an event split across pools must not double its quantities or cost: %+v", rows)
	}
	for _, r := range rows {
		if r.SeqFrom != 9 || r.SeqTo != 9 || r.IdemKey != "k" {
			t.Fatalf("every row of one event shares its seq and key: %+v", rows)
		}
	}
}

func TestAFreeEventStillLogs(t *testing.T) {
	t.Parallel()
	b := &booking{scope: domain.Scope{Realm: "t1", Kind: "workspace", ID: "w"}}
	rows, _, err := b.usageRows(domain.Intent{Op: domain.OpUsage, Scope: b.scope, Gen: 1, Seq: 1, IdemKey: "free",
		Payload: domain.Payload{OccurredAt: "2026-09-29T12:00:00Z"}})
	if err != nil || len(rows) != 1 || rows[0].Delta != 0 || rows[0].OperationType != "usage" {
		t.Fatalf("a zero-credit event is still one row, so it is metered: %+v %v", rows, err)
	}
}
