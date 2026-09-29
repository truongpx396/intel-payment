package app

import (
	"context"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

type outcome int

const (
	outBooked     outcome = iota // the intent is now in the books
	outRedelivery                // already booked (a crash between commit and ack): acknowledge only
	outGap                       // an earlier sequence number is still missing: do not acknowledge
	outSuspended                 // booked as a duplicate or conflict: suspense opened, nothing in the ledger
)

// effect is work that follows a COMMIT: a notification, the second leg of a transfer, a hot-side
// correction. Each is idempotent, and none may fail the booking it follows — a sweep repairs any
// that does not land (RedriveTransfers, ReissueCorrections).
type effect struct {
	kind string
	run  func(ctx context.Context) error
}

// bookRun books deliveries of ONE scope, in order, in one transaction, then runs the effects.
func (w *Writer) bookRun(ctx context.Context, shard domain.Shard, ds []ports.Delivery) (scopeResult, error) {
	scope := ds[0].Intent.Scope.Normalized()
	var (
		res     scopeResult
		effects []effect
	)
	err := w.d.Books.InTx(ctx, scope, func(ctx context.Context, tx ports.BookTx) error {
		res, effects = scopeResult{}, nil
		wm, err := tx.Watermark(ctx)
		if err != nil {
			return err
		}
		start := wm
		b := &booking{w: w, tx: tx, scope: scope, wm: &wm}
		gap := false
	deliveries:
		for _, d := range ds {
			out, err := b.intent(ctx, d.Intent)
			if err != nil {
				return err
			}
			switch out {
			case outGap:
				gap = true
				break deliveries
			case outRedelivery:
				res.ack = append(res.ack, d.ID)
			default:
				res.ack = append(res.ack, d.ID)
				res.booked++
			}
		}
		if err := b.flush(ctx); err != nil {
			return err
		}
		if wm != start {
			if err := tx.SetWatermark(ctx, wm, shard); err != nil {
				return err
			}
		}
		res.regressed = b.regressed
		effects = b.effects
		w.noteGap(scope.Tag(), gap)
		return nil
	})
	if err != nil {
		return scopeResult{}, err
	}
	for _, e := range effects {
		if err := e.run(ctx); err != nil {
			w.met.Count("metering_effect_failed_total", 1, ports.Labels{"kind": e.kind}) // a sweep will finish it
		}
	}
	if res.regressed {
		w.met.Count(ports.MetricHotRegression, 1, ports.Labels{"shard": fmt.Sprint(int(shard))})
	}
	return res, nil
}

// booking is the state of one scope's transaction.
type booking struct {
	w         *Writer
	tx        ports.BookTx
	scope     domain.Scope
	wm        *ports.Watermark
	rows      []ports.LedgerRow
	usage     []usageRec
	effects   []effect
	regressed bool
}

// usageRec remembers a usage intent so a rollup ledger can aggregate the batch and the archive can
// keep every event.
type usageRec struct {
	in       domain.Intent
	rows     []ports.LedgerRow
	occurred time.Time
}

// intent applies the watermark rules of hot-path-consistency.md §3 to one intent.
func (b *booking) intent(ctx context.Context, in domain.Intent) (outcome, error) {
	switch {
	case in.Seq == b.wm.AppliedSeq+1:
		return b.apply(ctx, in)
	case in.Seq > b.wm.AppliedSeq+1:
		return outGap, nil
	}
	// seq <= applied: either a redelivery (a crash between commit and ack) or a REGRESSION — the hot
	// tier lost a suffix of its history and is reissuing numbers. The guard row for THIS intent's
	// (gen, seq) tells them apart, never timing.
	done, err := b.alreadyProcessed(ctx, in)
	if err != nil {
		return 0, err
	}
	if done {
		return outRedelivery, nil
	}
	// A real, new operation. Book it, never move the watermark backwards, and freeze the shard.
	b.regressed = true
	return b.apply(ctx, in)
}

// alreadyProcessed reports whether THIS intent (not merely one with the same key) was booked: its
// guard row carries its (gen, seq), or it was suspended and its suspense entry carries them.
func (b *booking) alreadyProcessed(ctx context.Context, in domain.Intent) (bool, error) {
	rec, found, err := b.guard(ctx, in)
	if err != nil {
		return false, err
	}
	if found && rec.Gen == in.Gen && rec.Seq == in.Seq {
		return true, nil
	}
	return b.tx.HasSuspenseAt(ctx, in.Gen, in.Seq)
}

// guard reads the durable idempotency record for an intent's operation.
func (b *booking) guard(ctx context.Context, in domain.Intent) (ports.IdemRecord, bool, error) {
	switch in.Op {
	case domain.OpUsage:
		return b.tx.UsageIdem(ctx, in.IdemKey, b.w.d.Clock.Now().Add(-b.w.cfg.UsageIdemWindow))
	case domain.OpGrant:
		return b.tx.CreditIdem(ctx, ports.IdemOpGrant, in.IdemKey)
	case domain.OpTransferOut:
		return b.tx.CreditIdem(ctx, ports.IdemOpTransfer, in.IdemKey)
	case domain.OpTransferIn:
		return b.tx.CreditIdem(ctx, ports.IdemOpTransferIn, in.IdemKey)
	case domain.OpExpiry:
		return b.tx.CreditIdem(ctx, ports.IdemOpExpiry, in.Payload.LotID)
	case domain.OpCorrection:
		return b.tx.CreditIdem(ctx, ports.IdemOpCorrection, in.IdemKey)
	}
	return ports.IdemRecord{}, false, fmt.Errorf("%w: intent op %q", domain.ErrInvalid, in.Op)
}

func (b *booking) apply(ctx context.Context, in domain.Intent) (outcome, error) {
	var (
		out = outBooked
		err error
	)
	switch in.Op {
	case domain.OpUsage:
		out, err = b.usageIntent(ctx, in)
	case domain.OpGrant:
		out, err = b.grantIntent(ctx, in)
	case domain.OpTransferOut:
		out, err = b.transferOut(ctx, in)
	case domain.OpTransferIn:
		out, err = b.transferIn(ctx, in)
	case domain.OpExpiry:
		out, err = b.expiryIntent(ctx, in)
	case domain.OpCorrection:
		out, err = b.correctionIntent(ctx, in)
	default:
		err = fmt.Errorf("%w: intent op %q", domain.ErrInvalid, in.Op)
	}
	if err != nil {
		return 0, err
	}
	// The watermark only ever moves forward: a regressed intent is booked but does not lower it.
	if in.Seq > b.wm.AppliedSeq {
		b.wm.AppliedSeq = in.Seq
	}
	b.wm.Gen = max(b.wm.Gen, in.Gen)
	b.wm.Exists = true
	if in.LowWater != "" {
		b.effects = append(b.effects, b.lowWaterEffect(in))
	}
	return out, nil
}

// reject suspends an intent the books must not accept — a duplicate the hot guard missed, a key
// reused for a different request. The hot tier DID move, so the movement is recorded as open
// suspense (hot == booked + open suspense still holds at every sequence number) and a correction
// is issued to reverse it on the hot side.
func (b *booking) reject(ctx context.Context, in domain.Intent, reason string) (outcome, error) {
	draws := in.Draws
	if len(draws) == 0 {
		draws = []domain.PoolDelta{{Pool: domain.GeneralPool}}
	}
	for _, dr := range draws {
		id, err := b.tx.OpenSuspense(ctx, ports.Suspense{Pool: dr.Pool, Delta: dr.Delta, Reason: reason, Gen: in.Gen, Seq: in.Seq, IdemKey: in.IdemKey})
		if err != nil {
			return 0, err
		}
		if dr.Delta == 0 {
			// Nothing to reverse: the entry is only a marker that this intent was seen, so a
			// redelivery is recognised. Close it at once.
			if _, err := b.tx.CloseSuspense(ctx, id); err != nil {
				return 0, err
			}
			continue
		}
		b.effects = append(b.effects, b.correctionEffect(in.Scope, dr.Pool, -dr.Delta, id, reason))
	}
	if reason == ports.SuspenseIdemConflict {
		b.w.met.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": string(b.scope.Realm), "op": string(in.Op)})
	} else {
		b.w.met.Count(ports.MetricLateReplay, 1, ports.Labels{"realm": string(b.scope.Realm)})
	}
	if in.Op == domain.OpUsage {
		if err := b.tx.SettleJournal(ctx, in.IdemKey); err != nil {
			return 0, err
		}
	}
	return outSuspended, nil
}

// duplicateOrConflict classifies a found guard record against the intent's fingerprint.
func (b *booking) duplicateOrConflict(ctx context.Context, in domain.Intent, rec ports.IdemRecord) (outcome, error) {
	reason := ports.SuspenseDuplicate
	if rec.Fingerprint != in.Fingerprint {
		reason = ports.SuspenseIdemConflict
	}
	return b.reject(ctx, in, reason)
}

// ---------------------------------------------------------------- usage --

func (b *booking) usageIntent(ctx context.Context, in domain.Intent) (outcome, error) {
	if rec, found, err := b.tx.UsageIdem(ctx, in.IdemKey, b.w.d.Clock.Now().Add(-b.w.cfg.UsageIdemWindow)); err != nil {
		return 0, err
	} else if found {
		return b.duplicateOrConflict(ctx, in, rec)
	}
	if err := b.tx.PutUsageIdem(ctx, in.IdemKey, in.Fingerprint, in.Gen, in.Seq); err != nil {
		return 0, err
	}
	rows, occurred, err := b.usageRows(in)
	if err != nil {
		return 0, err
	}
	b.usage = append(b.usage, usageRec{in: in, rows: rows, occurred: occurred})
	if b.w.cfg.CreditExpiry == metering.ExpiryLotsFIFO {
		for _, d := range in.Draws {
			if d.Delta < 0 {
				if _, err := b.tx.ConsumeLots(ctx, d.Pool, -d.Delta); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := b.tx.SettleJournal(ctx, in.IdemKey); err != nil {
		return 0, err
	}
	return outBooked, nil
}

// usageRows builds one ledger row per pool drawn. The event's quantities and cost ride on the FIRST
// row only: a usage split across two pools must not double its quantities in a breakdown. Every row
// of one event shares its seq and idem_key, which is how they are grouped for re-pricing.
func (b *booking) usageRows(in domain.Intent) ([]ports.LedgerRow, time.Time, error) {
	p := in.Payload
	occurred, err := p.Occurred()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: occurred_at: %w", domain.ErrInvalid, err)
	}
	opType := p.Reason
	if opType == "" {
		opType = "usage"
	}
	draws := in.Draws
	if len(draws) == 0 {
		draws = []domain.PoolDelta{{Pool: domain.GeneralPool}} // a free event still logs
	}
	rows := make([]ports.LedgerRow, 0, len(draws))
	for i, d := range draws {
		r := ports.LedgerRow{
			Scope: b.scope, Pool: d.Pool, Delta: d.Delta, OperationType: opType, IdemKey: in.IdemKey,
			Gen: in.Gen, SeqFrom: in.Seq, SeqTo: in.Seq, EventCount: 1,
			RateCardVersion: p.RateCardVersion, Resource: p.Resource, RateKey: p.RateKey,
			Subjects: p.Subjects, ActorID: p.Actor, Ref: p.Ref, OccurredAt: occurred,
		}
		if i == 0 {
			r.CostMicros, r.Quantities = p.CostMicros, p.QuantitiesOf()
		}
		rows = append(rows, r)
	}
	return rows, occurred, nil
}

// flush writes the transaction's ledger rows. At event granularity that is every usage row as built;
// at rollup granularity the usage rows of the batch collapse to one per (pool, resource, rate key,
// card version, operation) and every event is appended, whole, to the archive first (idempotent), so
// nothing is lost and every event stays re-priceable (SC-010).
func (b *booking) flush(ctx context.Context) error {
	rows := b.rows
	if b.w.cfg.LedgerGranularity == metering.GranularityRollup && len(b.usage) > 0 {
		if err := b.archive(ctx); err != nil {
			return err
		}
		rows = append(rows, rollup(b.usage)...)
	} else {
		for _, u := range b.usage {
			rows = append(rows, u.rows...)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := b.tx.Book(ctx, rows)
	return err
}

func (b *booking) archive(ctx context.Context) error {
	events := make([]ports.ArchivedEvent, 0, len(b.usage))
	for _, u := range b.usage {
		p := u.in.Payload
		var credits domain.Credits
		for _, d := range u.in.Draws {
			credits -= d.Delta
		}
		events = append(events, ports.ArchivedEvent{
			Scope: b.scope, Gen: u.in.Gen, Seq: u.in.Seq, IdemKey: u.in.IdemKey, PoolDraws: u.in.Draws, Credits: credits,
			Resource: p.Resource, RateKey: p.RateKey, RateCardVersion: p.RateCardVersion, Quantities: p.QuantitiesOf(),
			Subjects: p.Subjects, CostMicros: p.CostMicros, OccurredAt: u.occurred,
		})
	}
	return b.w.d.Archive.Append(ctx, events)
}

type rollupKey struct {
	pool                   domain.Pool
	resource, rateKey, ver string
	opType                 string
}

// rollup collapses a batch's usage rows: one per pool/resource/rate key/card version/operation.
func rollup(recs []usageRec) []ports.LedgerRow {
	byKey := map[rollupKey]*ports.LedgerRow{}
	qty := map[rollupKey]map[domain.Unit]int64{}
	var order []rollupKey
	for _, u := range recs {
		for _, r := range u.rows {
			k := rollupKey{r.Pool, r.Resource, r.RateKey, r.RateCardVersion, r.OperationType}
			agg, ok := byKey[k]
			if !ok {
				c := r
				c.Subjects, c.Ref, c.Quantities, c.CostMicros, c.EventCount, c.Delta = nil, nil, nil, 0, 0, 0
				agg = &c
				byKey[k], qty[k] = agg, map[domain.Unit]int64{}
				order = append(order, k)
			}
			agg.Delta += r.Delta
			agg.CostMicros += r.CostMicros
			agg.SeqFrom, agg.SeqTo = min(agg.SeqFrom, r.SeqFrom), max(agg.SeqTo, r.SeqTo)
			if r.Quantities != nil { // the first row of an event carries its event
				agg.EventCount++
				for _, q := range r.Quantities {
					qty[k][q.Unit] += q.Amount
				}
			}
		}
	}
	out := make([]ports.LedgerRow, 0, len(order))
	for _, k := range order {
		agg := byKey[k]
		agg.EventCount = max(agg.EventCount, 1)
		for u, n := range qty[k] {
			agg.Quantities = append(agg.Quantities, domain.Quantity{Unit: u, Amount: n})
		}
		out = append(out, *agg)
	}
	return out
}

// --------------------------------------------------------------- grants --

func (b *booking) grantIntent(ctx context.Context, in domain.Intent) (outcome, error) {
	if rec, found, err := b.tx.CreditIdem(ctx, ports.IdemOpGrant, in.IdemKey); err != nil {
		return 0, err
	} else if found {
		return b.duplicateOrConflict(ctx, in, rec)
	}
	pool, applied := domain.GeneralPool, domain.Credits(0)
	if len(in.Draws) > 0 {
		pool, applied = in.Draws[0].Pool, in.Draws[0].Delta
	}
	// The books record the REQUEST; a floored shortfall is booked as its own writeoff row, so the
	// pool's booked balance equals the hot one (invariant 12): -100 requested, -10 applied, +90 writeoff.
	requested := applied - in.Writeoff
	p := in.Payload
	reason := p.Reason
	if reason == "" {
		reason = "grant"
	}
	occurred, _ := p.Occurred()
	b.rows = append(b.rows, ports.LedgerRow{Scope: b.scope, Pool: pool, Delta: requested, OperationType: reason, IdemKey: in.IdemKey,
		Gen: in.Gen, SeqFrom: in.Seq, SeqTo: in.Seq, EventCount: 1, ActorID: p.Actor, Ref: p.Ref, OccurredAt: occurred})
	if in.Writeoff > 0 {
		b.rows = append(b.rows, ports.LedgerRow{Scope: b.scope, Pool: pool, Delta: in.Writeoff, OperationType: ports.OperationWriteoff, IdemKey: in.IdemKey,
			Gen: in.Gen, SeqFrom: in.Seq, SeqTo: in.Seq, EventCount: 1, ActorID: p.Actor, Ref: p.Ref})
		if b.w.cfg.NegativeBalance == metering.BlockAndFlag {
			b.wm.Blocked = true
		}
	}
	if err := b.tx.PutCreditIdem(ctx, ports.IdemOpGrant, in.IdemKey, in.Fingerprint, in.Gen, in.Seq, ""); err != nil {
		return 0, err
	}
	if b.w.cfg.CreditExpiry == metering.ExpiryLotsFIFO {
		switch {
		case requested > 0:
			lot := ports.Lot{Pool: pool, Granted: requested, SourceKey: in.IdemKey}
			if p.ExpiresAt != "" {
				t, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
				if err != nil {
					return 0, fmt.Errorf("%w: expires_at: %w", domain.ErrInvalid, err)
				}
				lot.ExpiresAt = &t
			}
			if err := b.tx.OpenLot(ctx, lot); err != nil {
				return 0, err
			}
		case applied < 0:
			if _, err := b.tx.ConsumeLots(ctx, pool, -applied); err != nil {
				return 0, err
			}
		}
	}
	return outBooked, nil
}

// ------------------------------------------------------------ transfers --

func (b *booking) transferOut(ctx context.Context, in domain.Intent) (outcome, error) {
	if rec, found, err := b.tx.CreditIdem(ctx, ports.IdemOpTransfer, in.IdemKey); err != nil {
		return 0, err
	} else if found {
		return b.duplicateOrConflict(ctx, in, rec)
	}
	p := in.Payload
	if p.Counterparty == nil || p.Amount <= 0 {
		return 0, fmt.Errorf("%w: a transfer_out needs its destination and amount", domain.ErrInvalid)
	}
	dest := p.Counterparty.Scope()
	fromPool, toPool := domain.Pool(p.FromPool).Or(), domain.Pool(p.ToPool).Or()
	amount := domain.Credits(p.Amount)
	b.rows = append(b.rows, ports.LedgerRow{Scope: b.scope, Pool: fromPool, Delta: -amount, OperationType: ports.OperationAllocOut, Counterparty: &dest,
		IdemKey: in.IdemKey, Gen: in.Gen, SeqFrom: in.Seq, SeqTo: in.Seq, EventCount: 1, ActorID: p.Actor, Ref: p.Ref})
	if err := b.tx.PutCreditIdem(ctx, ports.IdemOpTransfer, in.IdemKey, in.Fingerprint, in.Gen, in.Seq, ""); err != nil {
		return 0, err
	}
	if err := b.tx.OpenTransfer(ctx, ports.TransferRecord{Realm: b.scope.Realm, IdemKey: in.IdemKey, From: b.scope, To: dest, FromPool: fromPool, ToPool: toPool, Amount: amount}); err != nil {
		return 0, err
	}
	if b.w.cfg.CreditExpiry == metering.ExpiryLotsFIFO {
		if _, err := b.tx.ConsumeLots(ctx, fromPool, amount); err != nil {
			return 0, err
		}
	}
	b.effects = append(b.effects, b.w.creditDestination(b.scope, dest, fromPool, toPool, amount, in.IdemKey))
	return outBooked, nil
}

func (b *booking) transferIn(ctx context.Context, in domain.Intent) (outcome, error) {
	if rec, found, err := b.tx.CreditIdem(ctx, ports.IdemOpTransferIn, in.IdemKey); err != nil {
		return 0, err
	} else if found {
		return b.duplicateOrConflict(ctx, in, rec)
	}
	pool, applied := domain.GeneralPool, domain.Credits(0)
	if len(in.Draws) > 0 {
		pool, applied = in.Draws[0].Pool, in.Draws[0].Delta
	}
	var src *domain.Scope
	if in.Payload.Counterparty != nil {
		s := in.Payload.Counterparty.Scope()
		src = &s
	}
	b.rows = append(b.rows, ports.LedgerRow{Scope: b.scope, Pool: pool, Delta: applied, OperationType: ports.OperationAllocIn, Counterparty: src,
		IdemKey: in.IdemKey, Gen: in.Gen, SeqFrom: in.Seq, SeqTo: in.Seq, EventCount: 1, Ref: in.Payload.Ref})
	if err := b.tx.PutCreditIdem(ctx, ports.IdemOpTransferIn, in.IdemKey, in.Fingerprint, in.Gen, in.Seq, ""); err != nil {
		return 0, err
	}
	if b.w.cfg.CreditExpiry == metering.ExpiryLotsFIFO && applied > 0 {
		if err := b.tx.OpenLot(ctx, ports.Lot{Pool: pool, Granted: applied, SourceKey: in.IdemKey}); err != nil {
			return 0, err
		}
	}
	// The transfer settles when its second leg is booked. A missing transfer row (books restored
	// from before it) is not an error here: the credit is still real and must be booked.
	if _, _, err := b.tx.SettleTransfer(ctx, b.scope.Realm, in.IdemKey); err != nil {
		return 0, err
	}
	return outBooked, nil
}

// creditDestination is the transfer's second leg: an idempotent hot credit in the destination's own
// shard, issued by the writer after the source side is durably booked. A destination that is cold
// is rehydrated first; one that cannot be reached now is left for the redrive sweep.
func (w *Writer) creditDestination(from, to domain.Scope, fromPool, toPool domain.Pool, amount domain.Credits, key string) effect {
	return effect{kind: "transfer_in", run: func(ctx context.Context) error {
		delta := ports.HotDelta{
			Op: domain.OpTransferIn, Scope: to, Pool: toPool, Delta: amount, IdemKey: key,
			Payload: domain.Payload{Reason: "transfer", Counterparty: domain.PayloadScopeOf(from), FromPool: string(fromPool), ToPool: string(toPool), Amount: int64(amount)},
		}
		return w.reh.heal(ctx, to, func() error { _, err := w.d.Balance.ApplyDelta(ctx, delta); return err })
	}}
}

// -------------------------------------------------------- expiry, fixes --

func (b *booking) expiryIntent(ctx context.Context, in domain.Intent) (outcome, error) {
	lotID := in.Payload.LotID
	if lotID == "" {
		return 0, fmt.Errorf("%w: an expiry intent must name its lot", domain.ErrInvalid)
	}
	if rec, found, err := b.tx.CreditIdem(ctx, ports.IdemOpExpiry, lotID); err != nil {
		return 0, err
	} else if found {
		return b.duplicateOrConflict(ctx, in, rec)
	}
	pool, hot := domain.GeneralPool, domain.Credits(0)
	if len(in.Draws) > 0 {
		pool, hot = in.Draws[0].Pool, in.Draws[0].Delta // ≤ 0
	}
	// The hot tier pre-applied the lot's remainder AT THE WATERMARK — an upper bound, because usage in
	// flight may have consumed part of the lot since. The books know the exact remainder now.
	exact, _, err := b.tx.ExpireLot(ctx, lotID)
	if err != nil {
		return 0, err
	}
	if exact > 0 {
		b.rows = append(b.rows, ports.LedgerRow{Scope: b.scope, Pool: pool, Delta: -exact, OperationType: ports.OperationExpiry, IdemKey: lotID,
			Gen: in.Gen, SeqFrom: in.Seq, SeqTo: in.Seq, EventCount: 1})
	}
	if err := b.tx.PutCreditIdem(ctx, ports.IdemOpExpiry, lotID, in.Fingerprint, in.Gen, in.Seq, ""); err != nil {
		return 0, err
	}
	// The hot side moved by `hot`; the books by -exact. The difference is hot movement the books did
	// not accept: open suspense, and a correction that brings the hot balance to the exact figure.
	if unbooked := hot + exact; unbooked != 0 {
		id, err := b.tx.OpenSuspense(ctx, ports.Suspense{Pool: pool, Delta: unbooked, Reason: ports.SuspenseExpiryTrueup, Gen: in.Gen, Seq: in.Seq, IdemKey: lotID})
		if err != nil {
			return 0, err
		}
		b.effects = append(b.effects, b.correctionEffect(b.scope, pool, -unbooked, id, ports.SuspenseExpiryTrueup))
	}
	return outBooked, nil
}

func (b *booking) correctionIntent(ctx context.Context, in domain.Intent) (outcome, error) {
	if rec, found, err := b.tx.CreditIdem(ctx, ports.IdemOpCorrection, in.IdemKey); err != nil {
		return 0, err
	} else if found {
		return b.duplicateOrConflict(ctx, in, rec)
	}
	if id := in.Payload.SuspenseID; id != "" {
		closed, err := b.tx.CloseSuspense(ctx, id)
		if err != nil {
			return 0, err
		}
		if !closed {
			b.w.met.Count("metering_correction_orphan_total", 1, ports.Labels{"realm": string(b.scope.Realm)})
		}
	}
	// A correction books NOTHING: it only closes the entry whose hot effect it just reversed.
	return outBooked, b.tx.PutCreditIdem(ctx, ports.IdemOpCorrection, in.IdemKey, in.Fingerprint, in.Gen, in.Seq, "")
}

// correctionEffect issues the hot-side reversal of a suspense entry, keyed by the entry's id so the
// hot guard makes a repeat a REPLAY.
func (b *booking) correctionEffect(scope domain.Scope, pool domain.Pool, delta domain.Credits, suspenseID, reason string) effect {
	return effect{kind: "correction", run: func(ctx context.Context) error {
		return b.w.issueCorrection(ctx, scope, pool, delta, suspenseID, reason)
	}}
}

func (w *Writer) issueCorrection(ctx context.Context, scope domain.Scope, pool domain.Pool, delta domain.Credits, suspenseID, reason string) error {
	d := ports.HotDelta{Op: domain.OpCorrection, Scope: scope, Pool: pool, Delta: delta, IdemKey: suspenseID,
		Payload: domain.Payload{Reason: reason, SuspenseID: suspenseID}}
	return w.reh.heal(ctx, scope, func() error { _, err := w.d.Balance.ApplyDelta(ctx, d); return err })
}

func (b *booking) lowWaterEffect(in domain.Intent) effect {
	return effect{kind: "balance_low", run: func(ctx context.Context) error {
		body := fmt.Sprintf(`{"realm":%q,"scope":%q,"watch":%q,"seq":%d}`, string(b.scope.Realm), b.scope.Tag(), in.LowWater, in.Seq)
		return b.w.d.Bus.Publish(ctx, b.w.cfg.SubjectPrefix+".balance.low."+b.scope.Tag(), []byte(body))
	}}
}
