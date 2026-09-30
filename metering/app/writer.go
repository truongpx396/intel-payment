package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Writer is the ONLY component that writes the account of record (invariant 1, FR-007). It books the
// hot tier's intents in per-scope sequence order, batched, idempotently — and it is where the two
// stores, which never share a transaction, are made to agree (hot-path-consistency.md §3).
//
// One worker owns a shard at a time (Books.Acquire), for ordering and throughput. Correctness does
// NOT rest on that: every booking takes the scope's transaction lock and every effect is guarded by
// a durable idempotency row, so two owners overlapping during a takeover still book each intent once.
type Writer struct {
	cfg metering.Config
	d   Deps
	met ports.Metrics
	reh *rehydrator

	mu   sync.Mutex
	gaps map[string]time.Time // scope tag → when a sequence gap was first seen
}

var _ ports.LedgerWriter = (*Writer)(nil)

// NewWriter validates its dependencies and returns the LedgerWriter.
func NewWriter(cfg metering.Config, d Deps) (*Writer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metering config: %w", err)
	}
	cfg = cfg.WithDefaults()
	var errs []error
	if d.Balance == nil || d.Books == nil || d.Stream == nil {
		errs = append(errs, errors.New("the Balance, Books and Stream ports are required"))
	}
	if d.Clock == nil {
		errs = append(errs, errors.New("a Clock is required"))
	}
	if d.Bus == nil {
		errs = append(errs, errors.New("a Bus is required (balance.low events)"))
	}
	if cfg.LedgerGranularity == metering.GranularityRollup && d.Archive == nil {
		errs = append(errs, errors.New("rollup granularity needs a UsageArchive, or per-event detail is lost"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metering writer: %w", err)
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &Writer{cfg: cfg, d: d, met: met, reh: newRehydrator(cfg, d, met), gaps: map[string]time.Time{}}, nil
}

// Rehydrate rebuilds a stale or brand-new account from booked + open suspense at applied_seq.
func (w *Writer) Rehydrate(ctx context.Context, s domain.Scope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return w.reh.Rehydrate(ctx, s)
}

// Drain books a shard's intents in per-scope sequence order, batched, then acknowledges them. A
// redelivery, a gap and a sequence regression are told apart by the watermark plus the idempotency
// row — never by timing. A regression (or a changed replication id) freezes the shard and runs
// Recover before Drain returns.
func (w *Writer) Drain(ctx context.Context, shard domain.Shard) (int, error) {
	return w.drain(ctx, shard, true, w.cfg.AckWait)
}

func (w *Writer) drain(ctx context.Context, shard domain.Shard, mayRecover bool, minIdle time.Duration) (int, error) {
	if err := w.syncShard(ctx, shard); err != nil {
		return 0, err
	}
	if mayRecover {
		if reason := w.failedOver(ctx, shard); reason != "" {
			return 0, w.Recover(ctx, shard, reason)
		}
	}
	ds, err := w.d.Stream.Read(ctx, shard, w.cfg.DrainBatch, minIdle)
	if err != nil {
		return 0, err
	}
	var (
		ack       []string
		booked    int
		regressed bool
		errs      []error
	)
	// Malformed entries are parked whole, never dropped (FR-043).
	var byScope = map[string][]ports.Delivery{}
	var order []string
	for _, d := range ds {
		if d.ParseErr != nil {
			if err := w.parkMalformed(ctx, d); err != nil {
				errs = append(errs, err)
				continue
			}
			ack = append(ack, d.ID)
			continue
		}
		tag := d.Intent.Scope.Tag()
		if _, seen := byScope[tag]; !seen {
			order = append(order, tag)
		}
		byScope[tag] = append(byScope[tag], d)
	}
	for _, tag := range order {
		res, err := w.processScope(ctx, shard, byScope[tag])
		ack = append(ack, res.ack...)
		booked += res.booked
		regressed = regressed || res.regressed
		if err != nil {
			errs = append(errs, err)
		}
	}
	if err := w.d.Stream.Ack(ctx, shard, ack...); err != nil {
		errs = append(errs, err)
	}
	w.observe(ctx, shard)
	if regressed && mayRecover {
		errs = append(errs, w.Recover(ctx, shard, "regression"))
	}
	return booked, errors.Join(errs...)
}

// syncShard converges the hot mirror of a shard on hot_shards, which is authoritative: a shard that
// has never been opened (first boot, or a Redis that lost everything) is opened at its recorded
// generation; one left behind by a half-finished bump is caught up. A hot generation AHEAD of
// Postgres means the books were restored from an older backup — the one thing that must not be
// papered over.
func (w *Writer) syncShard(ctx context.Context, shard domain.Shard) error {
	rec, err := w.d.Books.EnsureShard(ctx, shard)
	if err != nil {
		return err
	}
	st, err := w.d.Balance.ShardState(ctx, shard)
	if err != nil {
		return err
	}
	switch {
	case !st.Open:
		if err := w.d.Balance.OpenShard(ctx, shard, rec.Gen); err != nil {
			return err
		}
		if rec.Frozen {
			return w.d.Balance.FreezeShard(ctx, shard, rec.Reason)
		}
	case st.Gen < rec.Gen:
		return w.d.Balance.BumpShard(ctx, shard, rec.Gen)
	case st.Gen > rec.Gen:
		return fmt.Errorf("shard %d: the hot tier is at generation %d but the books at %d — the books were restored from an older backup; refusing to serve", shard, st.Gen, rec.Gen)
	}
	return nil
}

// failedOver reports why the shard must be recovered even though no intent has yet shown a
// regression: the replication id of the node serving it changed, so it may have lost a suffix of
// its history (a restart from a truncated AOF, a failover to a lagging replica).
func (w *Writer) failedOver(ctx context.Context, shard domain.Shard) string {
	rec, err := w.d.Books.EnsureShard(ctx, shard)
	if err != nil {
		return ""
	}
	id, err := w.d.Balance.NodeID(ctx, shard)
	if err != nil {
		return "" // cannot tell; the sequence check remains
	}
	if rec.NodeReplID == "" {
		_ = w.d.Books.SetNodeReplID(ctx, shard, id)
		return ""
	}
	if id != rec.NodeReplID {
		return "failover: replication id changed"
	}
	return ""
}

// observe exports the shard's outbox depth and the age of its oldest entry.
func (w *Writer) observe(ctx context.Context, shard domain.Shard) {
	st, err := w.d.Stream.Stats(ctx, shard)
	if err != nil {
		return
	}
	l := ports.Labels{"shard": fmt.Sprint(int(shard))}
	w.met.Gauge(ports.MetricOutboxDepth, st.Pending+st.Undelivered, l)
	age := max(st.OldestPendingAge, st.OldestUndelivered)
	w.met.Gauge(ports.MetricOutboxAge, int64(age.Seconds()), l)

	w.mu.Lock()
	var oldest time.Duration
	now := w.d.Clock.Now()
	for _, since := range w.gaps {
		oldest = max(oldest, now.Sub(since))
	}
	w.mu.Unlock()
	w.met.Gauge(ports.MetricWriterSeqGap, int64(oldest.Seconds()), l)
}

func (w *Writer) noteGap(tag string, gap bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !gap {
		delete(w.gaps, tag)
		return
	}
	if _, ok := w.gaps[tag]; !ok {
		w.gaps[tag] = w.d.Clock.Now()
	}
}

// scopeResult is what booking one scope's run of deliveries achieved.
type scopeResult struct {
	ack       []string
	booked    int
	regressed bool
}

// processScope books one scope's deliveries — in sequence order, in one transaction. If the
// transaction fails, each delivery is retried alone so one poison intent cannot hold the rest of its
// scope hostage; one that has used its retry budget is parked (never dropped) and the watermark
// advances past it through a poison suspense entry.
func (w *Writer) processScope(ctx context.Context, shard domain.Shard, ds []ports.Delivery) (scopeResult, error) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Intent.Seq < ds[j].Intent.Seq })
	res, err := w.bookRun(ctx, shard, ds)
	if err == nil || len(ds) == 1 {
		if err != nil {
			return w.settleFailure(ctx, shard, ds[0], err)
		}
		return res, nil
	}
	var total scopeResult
	var errs []error
	for _, d := range ds {
		r, err := w.bookRun(ctx, shard, []ports.Delivery{d})
		if err != nil {
			r, err = w.settleFailure(ctx, shard, d, err)
			if err != nil {
				errs = append(errs, err)
				break // later sequence numbers of this scope would only be gaps behind it
			}
		}
		total.ack = append(total.ack, r.ack...)
		total.booked += r.booked
		total.regressed = total.regressed || r.regressed
	}
	return total, errors.Join(errs...)
}

// settleFailure decides what a failed booking means: retry later, or — once the delivery budget is
// spent — park it.
func (w *Writer) settleFailure(ctx context.Context, shard domain.Shard, d ports.Delivery, cause error) (scopeResult, error) {
	if d.Attempts < w.cfg.MaxAttempts {
		return scopeResult{}, fmt.Errorf("book %s seq %d (attempt %d of %d): %w", d.Intent.Scope.Tag(), d.Intent.Seq, d.Attempts, w.cfg.MaxAttempts, cause)
	}
	if err := w.parkPoison(ctx, shard, d, cause); err != nil {
		return scopeResult{}, fmt.Errorf("park %s seq %d: %w (after: %w)", d.Intent.Scope.Tag(), d.Intent.Seq, err, cause)
	}
	return scopeResult{ack: []string{d.ID}, booked: 0}, nil
}

// parkMalformed records an entry that cannot even be parsed. It is kept whole for an operator.
func (w *Writer) parkMalformed(ctx context.Context, d ports.Delivery) error {
	dead := ports.DeadIntent{
		Scope: domain.Scope{Realm: domain.Realm(d.Fields["realm"]), Kind: "unknown", ID: "unknown"},
		Op:    domain.IntentOp("unknown"), IdemKey: d.Fields["idem"], Fields: d.Fields, Attempts: d.Attempts, LastErr: d.ParseErr.Error(),
	}
	if s := (domain.Scope{Realm: domain.Realm(d.Fields["realm"]), Kind: d.Fields["kind"], ID: d.Fields["id"]}); s.Validate() == nil {
		dead.Scope = s
	}
	return w.d.Books.ParkDead(ctx, dead)
}

// parkPoison parks an intent that cannot be booked and opens a poison suspense entry for the hot
// movement it carried, so hot == booked + open suspense still holds and the scope's watermark can
// move past it. Replaying the parked intent later books it and closes the entry.
func (w *Writer) parkPoison(ctx context.Context, shard domain.Shard, d ports.Delivery, cause error) error {
	in := d.Intent
	if err := w.d.Books.ParkDead(ctx, ports.DeadIntent{Scope: in.Scope, Op: in.Op, Gen: in.Gen, Seq: in.Seq, IdemKey: in.IdemKey, Fields: d.Fields, Attempts: d.Attempts, LastErr: cause.Error()}); err != nil {
		return err
	}
	return w.d.Books.InTx(ctx, in.Scope, func(ctx context.Context, tx ports.BookTx) error {
		wm, err := tx.Watermark(ctx)
		if err != nil {
			return err
		}
		if len(in.Draws) == 0 {
			in.Draws = []domain.PoolDelta{{Pool: domain.GeneralPool}}
		}
		for _, dr := range in.Draws {
			if _, err := tx.OpenSuspense(ctx, ports.Suspense{Pool: dr.Pool, Delta: dr.Delta, Reason: ports.SuspensePoison, Gen: in.Gen, Seq: in.Seq, IdemKey: in.IdemKey}); err != nil {
				return err
			}
		}
		if in.Seq == wm.AppliedSeq+1 {
			wm.AppliedSeq, wm.Exists = in.Seq, true
			wm.Gen = max(wm.Gen, in.Gen)
			return tx.SetWatermark(ctx, wm, shard)
		}
		return nil
	})
}
