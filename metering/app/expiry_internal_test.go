package app

import (
	"context"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

type dueBooks struct {
	ports.BookStore
	lots []ports.Lot
}

func (b dueBooks) DueLots(context.Context, time.Time, int) ([]ports.Lot, error) { return b.lots, nil }
func (dueBooks) EnsureShard(context.Context, domain.Shard) (ports.ShardRecord, error) {
	return ports.ShardRecord{Gen: 1}, nil
}
func (dueBooks) Booked(context.Context, domain.Scope) (ports.BookedState, error) {
	return ports.BookedState{}, nil
}

// coldOnce answers the first expiry as a cold account, and every later one as applied.
type coldOnce struct {
	ports.BalanceStore
	applied int
}

func (b *coldOnce) ApplyDelta(context.Context, ports.HotDelta) (domain.Receipt, error) {
	b.applied++
	if b.applied == 1 {
		return domain.Receipt{}, domain.ErrColdScope
	}
	return domain.Receipt{Applied: true}, nil
}

func (b *coldOnce) Rehydrate(context.Context, domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error) {
	return ports.RehydrateInstalled, nil
}

// An expirer must report through the Metrics it was built with: an expiry that had to rebuild a cold
// account is counted, because a rebuild on the expiry path is something an operator wants to see.
func TestTheExpirerReportsThroughTheMetricsItWasGiven(t *testing.T) {
	t.Parallel()
	rec := &metricRecorder{}
	scope := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	lots := []ports.Lot{{ID: "lot-1", Scope: scope, Pool: domain.GeneralPool, Remaining: 40}}
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x", CreditExpiry: metering.ExpiryLotsFIFO}
	bal := &coldOnce{}
	e, err := NewExpirer(cfg, Deps{Balance: bal, Books: dueBooks{lots: lots}, Clock: fixedClock(time.Unix(1_700_000_000, 0)), Metrics: rec})
	if err != nil {
		t.Fatal(err)
	}
	n, err := e.Tick(context.Background(), 10)
	if err != nil || n != 1 {
		t.Fatalf("one lot expired after rebuilding its cold account: %d, %v", n, err)
	}
	if got := rec.counts[ports.MetricRehydrate]; got != 1 {
		t.Fatalf("the rebuild must be counted on the recorder the expirer was given, got %d", got)
	}
}
