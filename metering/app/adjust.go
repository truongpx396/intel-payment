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
// shortfall written off, which the result reports.
//
// A refusal by the hot tier changed nothing. A failure the hot tier did not CONFIRM may have been applied — the call went out and the answer
// did not come back — and a failed operation is never run again, so the error says so and names the key to look for: an operator who asked
// again under a new key without looking would adjust twice.
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
				return nil, fmt.Errorf("the hot tier did not confirm the adjustment, and it may have been applied: look for ledger rows with idem_key %q before asking again, or the account is adjusted twice: %w", grant.IdemKey, err)
			}
			return nil, fmt.Errorf("adjust %s: %w", grant.Scope.Tag(), err)
		}
		return json.Marshal(struct {
			Scope    ScopeParams `json:"scope"`
			Pool     string      `json:"pool"`
			Amount   int64       `json:"amount"`
			Applied  bool        `json:"applied"`
			Delta    int64       `json:"delta"`
			Writeoff int64       `json:"writeoff"`
			Balance  int64       `json:"balance"`
			IdemKey  string      `json:"idem_key"`
		}{p.Scope, p.Pool, p.Amount, rc.Applied, int64(rc.Delta), int64(rc.Writeoff), int64(rc.Balance), grant.IdemKey})
	}
}
