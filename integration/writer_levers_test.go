//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// faultBalance is the hot tier as the WRITER sees it, with one switch: while down, every mutation the
// writer issues on its own account — the second leg of a transfer, a correction, an expiry — is
// refused as if Redis were unreachable. Producers reach the store directly and are unaffected.
type faultBalance struct {
	ports.BalanceStore
	down atomic.Bool
}

func (f *faultBalance) ApplyDelta(ctx context.Context, d ports.HotDelta) (domain.Receipt, error) {
	if f.down.Load() {
		return domain.Receipt{}, fmt.Errorf("%w: the hot tier is refusing the writer (test)", domain.ErrHotStoreUnavailable)
	}
	return f.BalanceStore.ApplyDelta(ctx, d)
}

// newWriter builds a further writer over the same books and the same consumer group.
func (s *wstack) newWriter(consumer string) *app.Writer {
	s.t.Helper()
	c := s.client()
	s.t.Cleanup(func() { _ = c.Close() })
	inner, err := redisstreams.NewStream(c, redisstreams.IntentOptions{Consumer: consumer, Group: s.group, Clock: s.clock})
	must(s.t, err)
	w, err := app.NewWriter(s.cfg, s.deps(&scopedStream{inner: inner, mine: s.owns, injected: s.stream.injected, mu: s.stream.mu}))
	must(s.t, err)
	return w
}

// ---------------------------------------------------------------- hot side --

func (s *wstack) acct(sc domain.Scope) string {
	return hotredis.AcctKey(sc.Normalized().Shard(s.cfg.Shards), sc)
}

func (s *wstack) hotSeq(sc domain.Scope) int64 {
	s.t.Helper()
	n, err := s.rdb.HGet(context.Background(), s.acct(sc), "seq").Int64()
	must(s.t, err)
	return n
}

func (s *wstack) setHotSeq(sc domain.Scope, n int64) {
	s.t.Helper()
	must(s.t, s.rdb.HSet(context.Background(), s.acct(sc), "seq", n).Err())
}

func (s *wstack) tamperHot(sc domain.Scope, p domain.Pool, delta domain.Credits) {
	s.t.Helper()
	must(s.t, s.rdb.HIncrBy(context.Background(), s.acct(sc), "b:"+string(p), int64(delta)).Err())
}

func (s *wstack) forgetGuard(sc domain.Scope, key string) {
	s.t.Helper()
	sh := sc.Normalized().Shard(s.cfg.Shards)
	if n, err := s.rdb.Del(context.Background(), hotredis.UsageIdemKey(sh, sc, key)).Result(); err != nil || n != 1 {
		s.t.Fatalf("the hot guard for %q was not there to expire: %d %v", key, n, err)
	}
}

// ------------------------------------------------------------------ stream --

func (s *wstack) intents(sc domain.Scope) []domain.Intent {
	s.t.Helper()
	var out []domain.Intent
	for _, m := range s.entries(sc) {
		in, err := domain.IntentFromFields(toStrings(m.Values))
		must(s.t, err)
		if in.Scope.Normalized() == sc.Normalized() {
			out = append(out, in)
		}
	}
	return out
}

func (s *wstack) entries(sc domain.Scope) []goredis.XMessage {
	s.t.Helper()
	ms, err := s.rdb.XRange(context.Background(), hotredis.OutboxKey(sc.Normalized().Shard(s.cfg.Shards)), "-", "+").Result()
	must(s.t, err)
	var out []goredis.XMessage
	for _, m := range ms {
		f := toStrings(m.Values)
		if f["realm"] == string(sc.Realm.Or()) && f["kind"] == sc.Kind && f["id"] == sc.ID {
			out = append(out, m)
		}
	}
	return out
}

func toStrings(v map[string]any) map[string]string {
	out := make(map[string]string, len(v))
	for k, x := range v {
		out[k] = fmt.Sprint(x)
	}
	return out
}

func (s *wstack) inject(in domain.Intent) {
	s.t.Helper()
	s.track(in.Scope)
	f, err := in.Fields()
	must(s.t, err)
	s.xadd(in.Scope, f)
}

func (s *wstack) xadd(sc domain.Scope, fields map[string]string) string {
	s.t.Helper()
	vals := make(map[string]any, len(fields))
	for k, v := range fields {
		vals[k] = v
	}
	id, err := s.rdb.XAdd(context.Background(), &goredis.XAddArgs{Stream: hotredis.OutboxKey(sc.Normalized().Shard(s.cfg.Shards)), Values: vals}).Result()
	must(s.t, err)
	return id
}

func (s *wstack) injectRaw(sc domain.Scope, fields map[string]string) {
	s.t.Helper()
	sh := sc.Normalized().Shard(s.cfg.Shards)
	s.mu.Lock()
	s.extra[sh] = true
	s.mu.Unlock()
	if _, ok := fields["realm"]; !ok {
		fields["realm"], fields["kind"], fields["id"] = string(sc.Realm.Or()), sc.Kind, sc.ID
	}
	id := s.xadd(sc, fields)
	s.stream.mu.Lock()
	s.stream.injected[id] = true
	s.stream.mu.Unlock()
}

func (s *wstack) dropIntent(sc domain.Scope, seq int64) {
	s.t.Helper()
	for _, m := range s.entries(sc) {
		if toStrings(m.Values)["seq"] == fmt.Sprint(seq) {
			must(s.t, s.rdb.XDel(context.Background(), hotredis.OutboxKey(sc.Normalized().Shard(s.cfg.Shards)), m.ID).Err())
			return
		}
	}
	s.t.Fatalf("no entry with seq %d for %s", seq, sc.Tag())
}

// ------------------------------------------------------------------- books --

func (s *wstack) watermark(sc domain.Scope) ports.Watermark {
	s.t.Helper()
	st, err := s.books.Booked(context.Background(), sc.Normalized())
	must(s.t, err)
	return st.Watermark
}

func (s *wstack) suspense(sc domain.Scope) []ports.Suspense {
	s.t.Helper()
	sc = sc.Normalized()
	rows, err := s.rawPool.Query(context.Background(), `
		SELECT id::text, pool, delta, reason, gen, seq, idem_key, status, created_at FROM credit_suspense
		WHERE realm=$1 AND scope_kind=$2 AND scope_id=$3 ORDER BY created_at, seq`, string(sc.Realm), sc.Kind, sc.ID)
	must(s.t, err)
	defer rows.Close()
	var out []ports.Suspense
	for rows.Next() {
		e := ports.Suspense{Scope: sc}
		var pool string
		must(s.t, rows.Scan(&e.ID, &pool, &e.Delta, &e.Reason, &e.Gen, &e.Seq, &e.IdemKey, &e.Status, &e.CreatedAt))
		e.Pool = domain.Pool(pool)
		out = append(out, e)
	}
	must(s.t, rows.Err())
	return out
}

func (s *wstack) rows(sc domain.Scope) []ports.LedgerRow {
	s.t.Helper()
	sc = sc.Normalized()
	rs, err := s.rawPool.Query(context.Background(), `
		SELECT pool, delta, operation_type, idem_key, gen, seq_from, seq_to, event_count, COALESCE(cost_micros,0), quantities
		FROM credit_ledger WHERE realm=$1 AND scope_kind=$2 AND scope_id=$3 ORDER BY seq_from, created_at`,
		string(sc.Realm), sc.Kind, sc.ID)
	must(s.t, err)
	defer rs.Close()
	var out []ports.LedgerRow
	for rs.Next() {
		r := ports.LedgerRow{Scope: sc}
		var pool string
		var q []byte
		must(s.t, rs.Scan(&pool, &r.Delta, &r.OperationType, &r.IdemKey, &r.Gen, &r.SeqFrom, &r.SeqTo, &r.EventCount, &r.CostMicros, &q))
		r.Pool = domain.Pool(pool)
		if len(q) > 0 {
			var pq []domain.PayloadQuantity
			must(s.t, json.Unmarshal(q, &pq))
			r.Quantities = domain.Payload{Quantities: pq}.QuantitiesOf()
		}
		out = append(out, r)
	}
	must(s.t, rs.Err())
	return out
}

func (s *wstack) shardGen(sc domain.Scope) int64 {
	s.t.Helper()
	rec, err := s.books.EnsureShard(context.Background(), sc.Normalized().Shard(s.cfg.Shards))
	must(s.t, err)
	return rec.Gen
}

func (s *wstack) dead(sc domain.Scope) int {
	sc = sc.Normalized()
	return int(s.scalar(`SELECT count(*) FROM credit_outbox_dead WHERE realm=$1 AND scope_kind=$2 AND scope_id=$3`, string(sc.Realm), sc.Kind, sc.ID))
}

func (s *wstack) archived(sc domain.Scope) int {
	sc = sc.Normalized()
	return int(s.scalar(`SELECT count(*) FROM usage_events WHERE realm=$1 AND scope_kind=$2 AND scope_id=$3`, string(sc.Realm), sc.Kind, sc.ID))
}

// ------------------------------------------------------------------ harness --

func writerHarness(t *testing.T, o contracts.WriterOptions) contracts.WriterHarness {
	t.Helper()
	w := wopts{LedgerOptions: o.LedgerOptions, dedicated: o.Dedicated}
	w.cfg.ReconcileTolerance, w.cfg.MaxAttempts = o.Tolerance, o.MaxAttempts
	if o.Rollup {
		w.cfg.LedgerGranularity = "rollup"
	}
	if o.Lots {
		w.cfg.CreditExpiry = "lots_fifo"
	}
	return newWriterStack(t, w).writerHarness()
}

func (s *wstack) writerHarness() contracts.WriterHarness {
	ctx := context.Background()
	return contracts.WriterHarness{
		LedgerHarness: s.ledgerHarness(),
		Writer:        s.writer,
		Second:        func(t *testing.T) ports.LedgerWriter { t.Helper(); return s.newWriter("w2") },

		Expire: func(t *testing.T) int {
			t.Helper()
			n, err := s.expirer.Tick(ctx, 100)
			must(t, err)
			return n
		},
		Redrive: func(t *testing.T, d time.Duration) int {
			t.Helper()
			n, err := s.writer.RedriveTransfers(ctx, d, 100)
			must(t, err)
			return n
		},
		Reissue: func(t *testing.T, d time.Duration) int {
			t.Helper()
			n, err := s.writer.ReissueCorrections(ctx, d, 100)
			must(t, err)
			return n
		},

		ShardOf:   func(sc domain.Scope) domain.Shard { return sc.Normalized().Shard(s.cfg.Shards) },
		HotSeq:    func(t *testing.T, sc domain.Scope) int64 { t.Helper(); return s.hotSeq(sc) },
		SetHotSeq: func(t *testing.T, sc domain.Scope, n int64) { t.Helper(); s.setHotSeq(sc, n) },
		TamperHot: func(t *testing.T, sc domain.Scope, p domain.Pool, d domain.Credits) {
			t.Helper()
			s.tamperHot(sc, p, d)
		},
		ForgetGuard: func(t *testing.T, sc domain.Scope, key string) { t.Helper(); s.forgetGuard(sc, key) },
		EffectsDown: func(_ *testing.T, down bool) { s.fault.down.Store(down) },

		Intents:    func(t *testing.T, sc domain.Scope) []domain.Intent { t.Helper(); return s.intents(sc) },
		Inject:     func(t *testing.T, in domain.Intent) { t.Helper(); s.inject(in) },
		InjectRaw:  func(t *testing.T, sc domain.Scope, f map[string]string) { t.Helper(); s.injectRaw(sc, f) },
		DropIntent: func(t *testing.T, sc domain.Scope, n int64) { t.Helper(); s.dropIntent(sc, n) },

		Watermark: func(t *testing.T, sc domain.Scope) ports.Watermark { t.Helper(); return s.watermark(sc) },
		Suspense:  func(t *testing.T, sc domain.Scope) []ports.Suspense { t.Helper(); return s.suspense(sc) },
		Rows:      func(t *testing.T, sc domain.Scope) []ports.LedgerRow { t.Helper(); return s.rows(sc) },
		ShardGen:  func(t *testing.T, sc domain.Scope) int64 { t.Helper(); return s.shardGen(sc) },
		Dead:      func(t *testing.T, sc domain.Scope) int { t.Helper(); return s.dead(sc) },
		Archived:  func(t *testing.T, sc domain.Scope) int { t.Helper(); return s.archived(sc) },
		Metric:    s.met.get,
		Gauge:     s.met.gauge,
		Published: s.bus.count,
	}
}

func TestLedgerWriterContract(t *testing.T) {
	t.Parallel()
	contracts.LedgerWriterContract(t, writerHarness)
}
