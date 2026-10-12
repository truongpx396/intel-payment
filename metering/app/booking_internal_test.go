package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
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

// lotTx is a books transaction that only RECORDS what the writer asks of the lots. Every other method
// of the port is left nil on purpose: a booking path that reaches for one the test did not expect panics.
type lotTx struct {
	ports.BookTx
	consumed   []lotDraw
	opened     []ports.Lot
	consumeErr error // returned by ConsumeLots, to prove a failure is not swallowed
}

type lotDraw struct {
	pool   domain.Pool
	n      domain.Credits
	source string
}

func (f *lotTx) UsageIdem(context.Context, string, time.Time) (ports.IdemRecord, bool, error) {
	return ports.IdemRecord{}, false, nil
}
func (f *lotTx) PutUsageIdem(context.Context, string, string, int64, int64) error { return nil }
func (f *lotTx) CreditIdem(context.Context, string, string) (ports.IdemRecord, bool, error) {
	return ports.IdemRecord{}, false, nil
}
func (f *lotTx) PutCreditIdem(context.Context, string, string, string, int64, int64, string) error {
	return nil
}
func (f *lotTx) SettleJournal(context.Context, string) error              { return nil }
func (f *lotTx) OpenTransfer(context.Context, ports.TransferRecord) error { return nil }
func (f *lotTx) SettleTransfer(context.Context, domain.Realm, string) (ports.TransferRecord, bool, error) {
	return ports.TransferRecord{}, false, nil
}
func (f *lotTx) ConsumeLots(_ context.Context, pool domain.Pool, n domain.Credits, source string) (domain.Credits, error) {
	f.consumed = append(f.consumed, lotDraw{pool, n, source})
	if f.consumeErr != nil {
		return 0, f.consumeErr
	}
	return n, nil
}
func (f *lotTx) OpenLot(_ context.Context, l ports.Lot) error {
	f.opened = append(f.opened, l)
	return nil
}

// bookOver books one intent through a real booking over the given recording transaction.
func bookOver(t *testing.T, expiry metering.CreditExpiry, in domain.Intent, tx *lotTx) error {
	t.Helper()
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x", CreditExpiry: expiry}
	w, err := NewWriter(cfg, Deps{
		Balance: struct{ ports.BalanceStore }{}, Books: struct{ ports.BookStore }{}, Bus: struct{ ports.Bus }{},
		Stream: struct{ ports.IntentStream }{}, Clock: fixedClock(time.Unix(1_700_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	in.Seq, in.Gen = 1, 1
	_, err = (&booking{w: w, tx: tx, scope: in.Scope, wm: &ports.Watermark{}}).intent(context.Background(), in)
	return err
}

// bookOne books one intent and fails the test if the booking fails.
func bookOne(t *testing.T, expiry metering.CreditExpiry, in domain.Intent) *lotTx {
	t.Helper()
	tx := &lotTx{}
	if err := bookOver(t, expiry, in, tx); err != nil {
		t.Fatal(err)
	}
	return tx
}

// What the writer asks of the lots is the whole of the draw-order and clawback behaviour at its
// source: the lot store decides the ORDER, but only the writer decides WHICH grant a clawback names and
// that usage and transfers name none. A clawback that dropped its source here would silently fall back
// to the ordinary order, and every integration test would still pass on a scope with one lot.
func TestTheWriterHandsTheLotsExactlyWhatEachIntentConsumesOrOpens(t *testing.T) {
	t.Parallel()
	scope := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	dest := domain.PayloadScopeOf(domain.Scope{Realm: "t1", Kind: "workspace", ID: "w2"})
	expires := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)

	cases := []struct {
		name         string
		in           domain.Intent
		wantConsumed []lotDraw
		wantOpened   []ports.Lot
	}{
		{"usage consumes each pool it drew, naming no source",
			domain.Intent{Op: domain.OpUsage, Scope: scope, IdemKey: "u1",
				Draws: []domain.PoolDelta{{Pool: "promo", Delta: -60}, {Pool: "general", Delta: -40}}},
			[]lotDraw{{"promo", 60, ""}, {"general", 40, ""}}, nil},
		{"a clawback consumes what the hot tier applied and names the grant it reverses",
			domain.Intent{Op: domain.OpGrant, Scope: scope, IdemKey: "refund:pay-2",
				Draws: []domain.PoolDelta{{Pool: "general", Delta: -300}}, Payload: domain.Payload{Reason: "refund", SourceKey: "pay-2"}},
			[]lotDraw{{"general", 300, "pay-2"}}, nil},
		{"a floored clawback consumes only what was applied, not the writeoff",
			domain.Intent{Op: domain.OpGrant, Scope: scope, IdemKey: "refund:pay-3", Writeoff: 90,
				Draws: []domain.PoolDelta{{Pool: "general", Delta: -10}}, Payload: domain.Payload{Reason: "refund", SourceKey: "pay-3"}},
			[]lotDraw{{"general", 10, "pay-3"}}, nil},
		{"a grant opens a lot keyed on itself, with the expiry it named",
			domain.Intent{Op: domain.OpGrant, Scope: scope, IdemKey: "pay-1",
				Draws: []domain.PoolDelta{{Pool: "promo", Delta: 500}}, Payload: domain.Payload{Reason: "promo", ExpiresAt: expires.Format(time.RFC3339Nano)}},
			nil, []ports.Lot{{Pool: "promo", Granted: 500, SourceKey: "pay-1", ExpiresAt: &expires}}},
		{"a grant that never expires opens a lot with no expiry",
			domain.Intent{Op: domain.OpGrant, Scope: scope, IdemKey: "pay-4",
				Draws: []domain.PoolDelta{{Pool: "general", Delta: 80}}, Payload: domain.Payload{Reason: "purchase"}},
			nil, []ports.Lot{{Pool: "general", Granted: 80, SourceKey: "pay-4"}}},
		{"a transfer out consumes the source pool and names no source",
			domain.Intent{Op: domain.OpTransferOut, Scope: scope, IdemKey: "alloc-1",
				Payload: domain.Payload{Counterparty: dest, FromPool: "promo", ToPool: "general", Amount: 25}},
			[]lotDraw{{"promo", 25, ""}}, nil},
		{"a transfer in opens a lot keyed on the transfer",
			domain.Intent{Op: domain.OpTransferIn, Scope: scope, IdemKey: "alloc-1",
				Draws: []domain.PoolDelta{{Pool: "general", Delta: 25}}, Payload: domain.Payload{Counterparty: dest}},
			nil, []ports.Lot{{Pool: "general", Granted: 25, SourceKey: "alloc-1"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tx := bookOne(t, metering.ExpiryLotsFEFO, c.in)
			if len(tx.consumed) != len(c.wantConsumed) {
				t.Fatalf("consumed %+v, want %+v", tx.consumed, c.wantConsumed)
			}
			for i, want := range c.wantConsumed {
				if tx.consumed[i] != want {
					t.Errorf("draw %d: %+v, want %+v", i, tx.consumed[i], want)
				}
			}
			if len(tx.opened) != len(c.wantOpened) {
				t.Fatalf("opened %+v, want %+v", tx.opened, c.wantOpened)
			}
			for i, want := range c.wantOpened {
				got := tx.opened[i]
				if got.Pool != want.Pool || got.Granted != want.Granted || got.SourceKey != want.SourceKey {
					t.Errorf("lot %d: %+v, want %+v", i, got, want)
				}
				if (got.ExpiresAt == nil) != (want.ExpiresAt == nil) || (want.ExpiresAt != nil && !got.ExpiresAt.Equal(*want.ExpiresAt)) {
					t.Errorf("lot %d expires %v, want %v", i, got.ExpiresAt, want.ExpiresAt)
				}
			}
		})
	}
}

// With expiry off there are no lots to draw from or open, whatever the intent says — and a clawback
// that names a source is booked exactly as before.
func TestWithExpiryOffTheWriterTouchesNoLots(t *testing.T) {
	t.Parallel()
	scope := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	dest := domain.PayloadScopeOf(domain.Scope{Realm: "t1", Kind: "workspace", ID: "w2"})
	for name, in := range map[string]domain.Intent{
		"usage":        {Op: domain.OpUsage, Scope: scope, IdemKey: "u1", Draws: []domain.PoolDelta{{Pool: "general", Delta: -40}}},
		"clawback":     {Op: domain.OpGrant, Scope: scope, IdemKey: "r1", Draws: []domain.PoolDelta{{Pool: "general", Delta: -30}}, Payload: domain.Payload{SourceKey: "pay-1"}},
		"grant":        {Op: domain.OpGrant, Scope: scope, IdemKey: "g1", Draws: []domain.PoolDelta{{Pool: "general", Delta: 30}}},
		"transfer out": {Op: domain.OpTransferOut, Scope: scope, IdemKey: "t1", Payload: domain.Payload{Counterparty: dest, Amount: 5}},
		"transfer in":  {Op: domain.OpTransferIn, Scope: scope, IdemKey: "t2", Draws: []domain.PoolDelta{{Pool: "general", Delta: 5}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if tx := bookOne(t, metering.ExpiryOff, in); len(tx.consumed) != 0 || len(tx.opened) != 0 {
				t.Fatalf("lots were touched with expiry off: consumed %+v opened %+v", tx.consumed, tx.opened)
			}
		})
	}
}

// A transfer in that applied nothing opens no lot: a lot of zero credits would be a row that can never
// be drawn and never expires.
func TestATransferInThatAppliedNothingOpensNoLot(t *testing.T) {
	t.Parallel()
	scope := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	in := domain.Intent{Op: domain.OpTransferIn, Scope: scope, IdemKey: "t3", Draws: []domain.PoolDelta{{Pool: "general", Delta: 0}}}
	if tx := bookOne(t, metering.ExpiryLotsFEFO, in); len(tx.opened) != 0 {
		t.Fatalf("a zero-credit lot was opened: %+v", tx.opened)
	}
}

// A lot store that fails must fail the booking: the intent stays unacknowledged and is delivered again.
// Swallowing the error would book the ledger row while the lots drift from it, and nothing would say so.
func TestAFailureDrawingLotsFailsTheBookingInsteadOfBeingSwallowed(t *testing.T) {
	t.Parallel()
	scope := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	dest := domain.PayloadScopeOf(domain.Scope{Realm: "t1", Kind: "workspace", ID: "w2"})
	boom := errors.New("lot store unavailable")
	for name, in := range map[string]domain.Intent{
		"usage":        {Op: domain.OpUsage, Scope: scope, IdemKey: "u1", Draws: []domain.PoolDelta{{Pool: "general", Delta: -40}}},
		"clawback":     {Op: domain.OpGrant, Scope: scope, IdemKey: "r1", Draws: []domain.PoolDelta{{Pool: "general", Delta: -30}}, Payload: domain.Payload{SourceKey: "pay-1"}},
		"transfer out": {Op: domain.OpTransferOut, Scope: scope, IdemKey: "t1", Payload: domain.Payload{Counterparty: dest, Amount: 5}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := bookOver(t, metering.ExpiryLotsFEFO, in, &lotTx{consumeErr: boom}); !errors.Is(err, boom) {
				t.Fatalf("a failed lot draw must fail the booking, got %v", err)
			}
		})
	}
}
