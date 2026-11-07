package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The operations an operator can ask for (R112), as this worker does them. paymentd records the request; the worker that holds the sole
// durable writer is the one that can act. Each executor is run again if its worker dies, so each is safe to run again: a reconcile is
// under the tick's claim, a recovery under the shard's lease, a rehydrate installs only into a stale account, a replay is claimed in the
// store, and a sweep writes only what is newer.
func (w *tickWork) adminExecutors(ticks *app.Ticks, dead *app.DeadLetters, letters ports.DeadLetterStore) map[ports.AdminOperationKind]app.AdminExecutor {
	return map[ports.AdminOperationKind]app.AdminExecutor{
		// The same work as the reconcile tick and under the same claim, so the two cannot run over each other. The scheduled tick asks
		// whether anything was booked since the last run; an operator asked for the run, and gets it.
		ports.AdminReconcile: func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error) {
			var p app.ReconcileParams
			if err := json.Unmarshal(op.Params, &p); err != nil {
				return nil, fmt.Errorf("read the arguments: %w", err)
			}
			payload, err := json.Marshal(app.ShardPayload{Shard: &p.Shard, Kind: p.Kind, Force: true})
			if err != nil {
				return nil, err
			}
			outcome, err := ticks.Do(ctx, "reconcile", payload)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"shard": p.Shard, "kind": p.Kind, "outcome": outcome})
		},
		// Freeze, drain, bump the generation, unfreeze — on the shard's lease, taken for the duration if this worker does not own it. A
		// shard whose owner is another worker is left to that worker's own loops, and the operation says so.
		ports.AdminRecover: func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error) {
			var p app.RecoverParams
			if err := json.Unmarshal(op.Params, &p); err != nil {
				return nil, fmt.Errorf("read the arguments: %w", err)
			}
			sh := domain.Shard(p.Shard)
			_, err := w.onShard(ctx, sh, func() (int, error) {
				return 0, w.writer.Recover(ctx, sh, "operator: "+op.Actor+": "+op.Reason)
			})
			if err != nil {
				if errors.Is(err, app.ErrTickSkipped) {
					return nil, fmt.Errorf("shard %d is owned by another worker, which must recover it: %w", p.Shard, err)
				}
				return nil, err
			}
			return json.Marshal(map[string]any{"shard": p.Shard})
		},
		ports.AdminRehydrate: func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error) {
			var p app.RehydrateParams
			if err := json.Unmarshal(op.Params, &p); err != nil {
				return nil, fmt.Errorf("read the arguments: %w", err)
			}
			sc := domain.Scope{Realm: domain.Realm(op.Realm), Kind: p.Scope.Kind, ID: p.Scope.ID}
			if err := w.writer.Rehydrate(ctx, sc); err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"scope": p.Scope})
		},
		// A dead letter belongs to the realm its payload named; an operator of another realm is told it is not there, as for any record of
		// a realm that is not theirs. A letter that names no realm (one nobody could read) is the deployment's and not any realm's to replay.
		ports.AdminReplay: func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error) {
			var p app.ReplayParams
			if err := json.Unmarshal(op.Params, &p); err != nil {
				return nil, fmt.Errorf("read the arguments: %w", err)
			}
			l, err := letters.Get(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			if l.Realm != op.Realm {
				return nil, fmt.Errorf("%w: dead letter %s is not in realm %s", domain.ErrNotFound, p.ID, op.Realm)
			}
			outcome, l, err := dead.Replay(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"outcome": outcome, "subject": l.Subject})
		},
		// Called directly and not through the tick: an operator who mistypes an account is not a scheduled tick failing, and must not page as
		// one. The write is conditional on being newer, so a sweep that overlaps the scheduled one repairs once.
		ports.AdminSubdrift: func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error) {
			var p struct {
				Account string `json:"account"`
			}
			if err := json.Unmarshal(op.Params, &p); err != nil {
				return nil, fmt.Errorf("read the arguments: %w", err)
			}
			repaired, err := w.pay.Subdrift(ctx, p.Account)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"account": p.Account, "repaired": repaired})
		},
	}
}
