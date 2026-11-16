package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// AdjustmentKeyPrefix begins the idempotency key an adjustment is booked under: the prefix and the operation's id. The key is the
// operation's and never the caller's, so that the operation done again — by a worker that took it over from one that died after the grant and
// before it said so — is the same grant, a replay, and not a second adjustment; and so that two operations, which an operator asked for
// separately, are two.
const AdjustmentKeyPrefix = "admin-adjustment:"

// Granter is the part of the Meter an adjustment needs: it moves money through Grant like everything else that does, so it is held to the
// same idempotency, the same negative-balance policy and the same ceilings as a purchase or a refund.
type Granter interface {
	Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error)
}

// AdjustmentExecutor does an operator's adjustment (R112a): an investigated correction to the books, made as a signed Grant with the
// reserved reason admin_adjustment — through the normal path, so the writer books it and the ledger holds it, and nothing edits a row.
//
// Its ref leads back to the operation and the person, and the database has the request and its outcome in audit_log. A positive amount adds
// credits; a negative one removes them and is subject to the negative-balance policy, so a clawback larger than the balance is floored and its
// shortfall written off, which the result reports (and, under block_and_flag, blocks the account: see Unblock).
//
// Running it again is safe, and the result says what the run found. A run that applied the adjustment reports what it moved. A run that found
// it already there — an operation taken over from a worker that died after the grant and before it said so — reports `replayed`, and nothing
// it did not do: the hot tier tells a replay by its key and says nothing moved, and a result of zero recorded for good on an adjustment that
// was applied would be a claim that nothing happened.
//
// A refusal by the hot tier changed nothing and is a failure. A failure the hot tier did not CONFIRM may have been applied — the call went
// out and the answer did not come back — and is returned as ErrRetryOperation: the key is the operation's, so running it again either finds
// it (a replay) or applies it, and the worker does that, up to its limit, instead of leaving a person to read an error and decide whether
// asking again under a new key would adjust twice. The error says what to look for if the attempts are spent.
func AdjustmentExecutor(g Granter) AdminExecutor {
	return func(ctx context.Context, op ports.AdminOperation) (json.RawMessage, error) {
		var p AdjustParams
		if err := json.Unmarshal(op.Params, &p); err != nil {
			return nil, fmt.Errorf("read the arguments: %w", err)
		}
		grant := domain.Grant{
			Scope:   domain.Scope{Realm: domain.Realm(op.Realm), Kind: p.Scope.Kind, ID: p.Scope.ID},
			Pool:    domain.Pool(p.Pool),
			Amount:  domain.Credits(p.Amount),
			IdemKey: AdjustmentKeyPrefix + op.ID,
			Reason:  ports.OperationAdjustment,
			Ref:     map[string]string{"operation": op.ID, "actor": op.Actor},
		}
		if err := grant.Validate(); err != nil {
			return nil, fmt.Errorf("read the arguments: %w", err)
		}
		rc, err := g.Grant(ctx, grant)
		if err != nil {
			if errors.Is(err, domain.ErrHotStoreUnavailable) {
				return nil, fmt.Errorf("%w: the hot tier did not confirm the adjustment, and it may have been applied: look for ledger rows with idem_key %q before asking again, or the account is adjusted twice: %w", ErrRetryOperation, grant.IdemKey, err)
			}
			return nil, fmt.Errorf("adjust %s: %w", grant.Scope.Tag(), err)
		}
		out := struct {
			Scope    ScopeParams `json:"scope"`
			Pool     string      `json:"pool"`
			Amount   int64       `json:"amount"`
			Applied  bool        `json:"applied"`  // the adjustment is in, whichever run put it there
			Replayed bool        `json:"replayed"` // this run found it already booked, and moved nothing
			Delta    *int64      `json:"delta,omitempty"`
			Writeoff *int64      `json:"writeoff,omitempty"`
			Balance  *int64      `json:"balance,omitempty"`
			IdemKey  string      `json:"idem_key"`
		}{Scope: p.Scope, Pool: p.Pool, Amount: p.Amount, Applied: true, Replayed: !rc.Applied, IdemKey: grant.IdemKey}
		if rc.Applied {
			delta, writeoff, balance := int64(rc.Delta), int64(rc.Writeoff), int64(rc.Balance)
			out.Delta, out.Writeoff, out.Balance = &delta, &writeoff, &balance
		}
		return json.Marshal(out)
	}
}
