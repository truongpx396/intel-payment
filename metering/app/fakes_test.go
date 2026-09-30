package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var epoch = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// fakeBalance is a programmable hot tier. Embedding the interface makes any method a test did not
// script panic on a nil receiver — an unexpected call is a test failure, not a silent zero.
type fakeBalance struct {
	ports.BalanceStore
	mu sync.Mutex

	admit    func(domain.Scope, domain.AdmitRequest) (domain.Admission, error)
	debit    func(domain.Charge) (domain.Receipt, error)
	grant    func(domain.Grant) (domain.Receipt, error)
	transfer func(domain.Transfer) (domain.TransferReceipt, error)
	rehyd    func(domain.Scope, ports.RehydrateState) (ports.RehydrateOutcome, error)

	admitted []domain.AdmitRequest
	debited  []domain.Charge
	rehydOn  []ports.RehydrateState
	order    *[]string
}

func (f *fakeBalance) note(s string) {
	if f.order != nil {
		*f.order = append(*f.order, s)
	}
}

func (f *fakeBalance) Admit(_ context.Context, s domain.Scope, r domain.AdmitRequest) (domain.Admission, error) {
	f.mu.Lock()
	f.admitted = append(f.admitted, r)
	f.mu.Unlock()
	return f.admit(s, r)
}

func (f *fakeBalance) Debit(_ context.Context, c domain.Charge) (domain.Receipt, error) {
	f.mu.Lock()
	f.debited = append(f.debited, c)
	f.note("debit")
	f.mu.Unlock()
	return f.debit(c)
}

func (f *fakeBalance) Grant(_ context.Context, g domain.Grant) (domain.Receipt, error) {
	return f.grant(g)
}

func (f *fakeBalance) Transfer(_ context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	return f.transfer(t)
}

func (f *fakeBalance) Rehydrate(_ context.Context, s domain.Scope, st ports.RehydrateState) (ports.RehydrateOutcome, error) {
	f.mu.Lock()
	f.rehydOn = append(f.rehydOn, st)
	f.mu.Unlock()
	if f.rehyd == nil {
		return ports.RehydrateInstalled, nil
	}
	return f.rehyd(s, st)
}

// fakeBooks serves canned booked state.
type fakeBooks struct {
	ports.BookStore
	mu       sync.Mutex
	state    ports.BookedState
	gen      int64
	transfer *ports.TransferRecord
	booked   int
	gate     chan struct{} // if set, Booked blocks until it is closed
}

func (f *fakeBooks) EnsureShard(_ context.Context, sh domain.Shard) (ports.ShardRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gen == 0 {
		f.gen = 1
	}
	return ports.ShardRecord{Shard: sh, Gen: f.gen}, nil
}

func (f *fakeBooks) Booked(_ context.Context, _ domain.Scope) (ports.BookedState, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.booked++
	return f.state, nil
}

func (f *fakeBooks) Transfer(context.Context, domain.Realm, string) (ports.TransferRecord, bool, error) {
	if f.transfer == nil {
		return ports.TransferRecord{}, false, nil
	}
	return *f.transfer, true, nil
}

// fakeJournal records Put order and can fail or conflict.
type fakeJournal struct {
	mu      sync.Mutex
	entries []ports.JournalEntry
	putErr  error
	due     []ports.JournalEntry
	retries []retry
	pending int64
	order   *[]string
}

type retry struct {
	id       string
	attempts int
	next     time.Time
}

func (j *fakeJournal) Put(_ context.Context, e ports.JournalEntry) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.order != nil {
		*j.order = append(*j.order, "journal")
	}
	if j.putErr != nil {
		return false, j.putErr
	}
	j.entries = append(j.entries, e)
	return true, nil
}

func (j *fakeJournal) Due(context.Context, time.Time, int) ([]ports.JournalEntry, error) {
	return j.due, nil
}

func (j *fakeJournal) Retry(_ context.Context, id string, attempts int, next time.Time, _ string) error {
	j.mu.Lock()
	j.retries = append(j.retries, retry{id, attempts, next})
	j.mu.Unlock()
	return nil
}

func (j *fakeJournal) Pending(context.Context, domain.Realm) (int64, error) { return j.pending, nil }

// fakeBus records publishes.
type fakeBus struct {
	ports.Bus
	mu   sync.Mutex
	sent []string
}

func (b *fakeBus) Publish(_ context.Context, subject string, _ []byte) error {
	b.mu.Lock()
	b.sent = append(b.sent, subject)
	b.mu.Unlock()
	return nil
}

func (b *fakeBus) subjects() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

// recMetrics records every metric so a test can assert "observable" (invariant 15).
type recMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]int64
	obs    []string
}

func newMetrics() *recMetrics {
	return &recMetrics{counts: map[string]int64{}, gauges: map[string]int64{}}
}

func (m *recMetrics) Count(name string, n int64, l ports.Labels) {
	m.mu.Lock()
	m.counts[name] += n
	m.counts[name+"{"+l["reason"]+l["outcome"]+l["op"]+l["cause"]+l["limit"]+"}"] += n
	m.mu.Unlock()
}

func (m *recMetrics) Gauge(name string, v int64, _ ports.Labels) {
	m.mu.Lock()
	m.gauges[name] = v
	m.mu.Unlock()
}

func (m *recMetrics) Observe(_ string, _ float64, l ports.Labels) {
	m.mu.Lock()
	m.obs = append(m.obs, l["outcome"])
	m.mu.Unlock()
}

func (m *recMetrics) count(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[name]
}

// quota is a scripted QuotaSource.
type quota struct {
	max int64
	err error
}

func (q quota) Quota(context.Context, domain.Scope, string) (int64, error) { return q.max, q.err }

// rig is a Meter over fakes.
type rig struct {
	m       ports.Meter
	bal     *fakeBalance
	books   *fakeBooks
	journal *fakeJournal
	bus     *fakeBus
	met     *recMetrics
	clock   *contracts.FakeClock
	order   []string
	deps    app.Deps
	cfg     metering.Config
}

type rigOpt struct {
	cfg      metering.Config
	limits   []domain.Limit
	pools    []domain.PoolDef
	quotas   ports.QuotaSource
	cards    contracts.StaticCards
	pricers  ports.PricerRegistry
	noQuotas bool
}

func tokenCard(t *testing.T) domain.RateCard {
	t.Helper()
	c, err := domain.NewRateCard("t1", "v1", "table", []domain.RateEntry{
		{RateKey: "gpt", Unit: "in", CreditsPerBlock: 150000, BlockSize: 1000000, CostMicrosPerBlock: 100000},
		{RateKey: "gpt", Unit: "out", CreditsPerBlock: 600000, BlockSize: 1000000},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newRig(t *testing.T, o rigOpt) *rig {
	t.Helper()
	r := &rig{bal: &fakeBalance{}, books: &fakeBooks{}, journal: &fakeJournal{}, bus: &fakeBus{}, met: newMetrics(), clock: contracts.NewFakeClock(epoch)}
	r.bal.order, r.journal.order = &r.order, &r.order
	r.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
		return domain.Admission{Allowed: true, Headroom: 100}, nil
	}
	r.bal.debit = func(c domain.Charge) (domain.Receipt, error) {
		return domain.Receipt{IdemKey: c.IdemKey, Applied: true, Delta: -c.Amount, Seq: 7}, nil
	}
	r.bal.grant = func(g domain.Grant) (domain.Receipt, error) {
		return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Seq: 1}, nil
	}

	cfg := o.cfg
	cfg.BalanceRedisURL, cfg.LedgerDSN = "redis://x", "postgres://x"
	r.cfg = cfg.WithDefaults()
	cards := o.cards
	if cards == nil {
		cards = contracts.StaticCards{"t1": {tokenCard(t)}}
	}
	pricers := o.pricers
	if pricers == nil {
		pricers = ports.PricerRegistry{"table": table.New(0)}
	}
	r.deps = app.Deps{
		Pricers: pricers, Balance: r.bal, Books: r.books, Limits: contracts.StaticLimits(o.limits), Pools: contracts.StaticPools(o.pools),
		RateCards: cards, Journal: r.journal, Bus: r.bus, Clock: r.clock, Metrics: r.met,
	}
	if !o.noQuotas {
		r.deps.Quotas = o.quotas
	}
	m, err := app.New(cfg, r.deps)
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	return r
}

var ws = domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}

func event(key string) domain.Event {
	return domain.Event{Scope: ws, Resource: "llm.chat", RateKey: "gpt", IdemKey: key, OccurredAt: epoch.Add(-time.Second),
		Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}}}
}

var balanceLimit = domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}

// newMeter rebuilds a Meter from a rig's (possibly modified) deps.
func newMeter(t *testing.T, r *rig) (ports.Meter, error) {
	t.Helper()
	cfg := r.cfg
	m, err := app.New(cfg, r.deps)
	if err != nil {
		t.Fatal(err)
	}
	return m, nil
}
