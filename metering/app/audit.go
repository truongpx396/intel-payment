package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// ErrAuditMismatch is returned by Audit when a booked balance is not provable from the ledger.
// The report still carries every finding.
var ErrAuditMismatch = errors.New("audit mismatch")

var errAuditMismatch = ErrAuditMismatch

// Audit is the deep check (`billing.audit.tick`, off-peak): per scope and pool, booked must equal
// the checkpoint plus every ledger row after it. A pool that proves itself has its checkpoint moved
// forward — and checkpoints are what make it safe to detach and archive old ledger partitions, since
// summing the whole history is the one thing a ledger that grows forever cannot keep doing (FR-050).
//
// Detachable partitions are deployment-wide, so they are reported by shard 0's audit only.
func (w *Writer) Audit(ctx context.Context, shard domain.Shard) (domain.AuditReport, error) {
	rep := domain.AuditReport{Shard: shard}
	var after *domain.Scope
	for {
		page, err := w.d.Books.ScopesSince(ctx, shard, time.Time{}, after, reconcilePage)
		if err != nil {
			return rep, err
		}
		if len(page) == 0 {
			break
		}
		for _, ss := range page {
			rows, err := w.d.Books.AuditScope(ctx, ss.Scope, true)
			if err != nil {
				return rep, err
			}
			rep.Checked++
			for _, r := range rows {
				if r.Advanced {
					rep.Advanced++
				}
				if r.Booked != r.CheckpointBalance+r.SumSince {
					rep.Mismatches++
					rep.Findings = append(rep.Findings, domain.AuditFinding{
						Scope: ss.Scope, Pool: r.Pool, Booked: r.Booked, Summed: r.CheckpointBalance + r.SumSince, CheckpointFloor: r.CheckpointBalance,
					})
				}
			}
		}
		last := page[len(page)-1].Scope
		after = &last
		if len(page) < reconcilePage {
			break
		}
	}
	if shard == 0 {
		var err error
		if rep.DetachOK, err = w.d.Books.DetachableLedgerPartitions(ctx); err != nil {
			return rep, err
		}
		counts, err := w.d.Books.DefaultPartitionRows(ctx)
		if err != nil {
			return rep, err
		}
		for table, n := range counts {
			w.met.Gauge(ports.MetricDefaultPartition, n, ports.Labels{"table": table})
		}
	}
	if rep.Mismatches > 0 {
		w.met.Count(ports.MetricDriftFindings, int64(rep.Mismatches), ports.Labels{"realm": "*", "outcome": "audit"})
		return rep, fmt.Errorf("%w: %d scope/pool(s) whose booked balance is not checkpoint + ledger", errAuditMismatch, rep.Mismatches)
	}
	return rep, nil
}
