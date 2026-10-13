package resthandler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// GET /v1/credits is the credits-ui contract's CreditsSnapshot, in that contract's own names (camelCase): the
// wire is a pass-through, so the package needs no translation table on either side. Every figure is an
// integer. A burn rate, a series, a breakdown by dimension, an anchor and notices are absent — not empty,
// not invented — until the usage_daily rollup and the host-resolved anchor exist.

type creditsJSON struct {
	Scope      scopeJSON       `json:"scope"`
	Balance    balanceViewJSON `json:"balance"`
	Limits     []limitViewJSON `json:"limits"`
	Breakdown  []struct{}      `json:"breakdown"`
	Ledger     []ledgerRowJSON `json:"ledger"`
	LedgerNext *string         `json:"ledgerNext"`
}

type balanceViewJSON struct {
	Remaining       int64          `json:"remaining"`
	Granted         int64          `json:"granted"`
	PrimaryLimitKey string         `json:"primaryLimitKey"`
	Pools           []poolViewJSON `json:"pools"`
}

type poolViewJSON struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Remaining int64  `json:"remaining"`
	AppliesTo string `json:"appliesTo,omitempty"`
}

type limitViewJSON struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Used      int64  `json:"used"`
	Cap       *int64 `json:"cap"`
	Window    string `json:"window"`
	ResetsAt  string `json:"resetsAt,omitempty"`
	WarnAtPct int    `json:"warnAtPct"`
	DenyCode  string `json:"denyCode,omitempty"`
	ScopeHint string `json:"scopeHint,omitempty"`
}

type actorJSON struct {
	Label string `json:"label"`
}

type ledgerRowJSON struct {
	ID            string            `json:"id"`
	OperationType string            `json:"operationType"`
	Delta         int64             `json:"delta"`
	At            string            `json:"at"`
	Actor         *actorJSON        `json:"actor,omitempty"`
	Attributes    map[string]string `json:"attributes"`
	// The engine's own columns ride beside the host's attribute bag, which stays the host's.
	Pool            string            `json:"pool"`
	Resource        string            `json:"resource,omitempty"`
	RateKey         string            `json:"rateKey,omitempty"`
	RateCardVersion string            `json:"rateCardVersion,omitempty"`
	IdemKey         string            `json:"idemKey"`
	EventCount      int               `json:"eventCount"`
	Quantities      []quantityJSON    `json:"quantities,omitempty"`
	Subjects        map[string]string `json:"subjects,omitempty"`
}

func creditsToJSON(c ports.CreditsSnapshot) creditsJSON {
	out := creditsJSON{
		Scope: scopeJSON{Realm: string(c.Scope.Realm), Kind: c.Scope.Kind, ID: c.Scope.ID},
		Balance: balanceViewJSON{
			Remaining: int64(c.Balance.Remaining), Granted: int64(c.Balance.Granted), PrimaryLimitKey: c.Balance.PrimaryLimitKey,
			Pools: make([]poolViewJSON, 0, len(c.Balance.Pools)),
		},
		Limits:    make([]limitViewJSON, 0, len(c.Limits)),
		Breakdown: []struct{}{},
		Ledger:    make([]ledgerRowJSON, 0, len(c.Ledger)),
	}
	for _, p := range c.Balance.Pools {
		out.Balance.Pools = append(out.Balance.Pools, poolViewJSON{Key: p.Key, Label: p.Key, Remaining: int64(p.Remaining), AppliesTo: p.AppliesTo})
	}
	for _, l := range c.Limits {
		v := limitViewJSON{Key: l.Key, Label: l.Key, Used: l.Used, Cap: l.Cap, Window: l.Window, WarnAtPct: l.WarnAtPct, DenyCode: l.DenyCode, ScopeHint: l.ScopeHint}
		if l.ResetsAt != nil {
			v.ResetsAt = l.ResetsAt.UTC().Format(time.RFC3339Nano)
		}
		out.Limits = append(out.Limits, v)
	}
	for _, e := range c.Ledger {
		at := e.OccurredAt
		if at.IsZero() {
			at = e.CreatedAt
		}
		row := ledgerRowJSON{
			ID: e.ID, OperationType: e.OperationType, Delta: int64(e.Delta), At: at.UTC().Format(time.RFC3339Nano), Attributes: map[string]string{},
			Pool: string(e.Pool), Resource: e.Resource, RateKey: e.RateKey, RateCardVersion: e.RateCardVersion, IdemKey: e.IdemKey, EventCount: e.EventCount,
			Subjects: mapOrNil(e.Subjects),
		}
		for k, v := range e.Ref {
			row.Attributes[k] = v
		}
		if e.ActorID != "" {
			row.Actor = &actorJSON{Label: e.ActorID}
		}
		for _, q := range e.Quantities {
			row.Quantities = append(row.Quantities, quantityJSON{Unit: string(q.Unit), Amount: q.Amount})
		}
		out.Ledger = append(out.Ledger, row)
	}
	if c.LedgerNext != "" {
		out.LedgerNext = &c.LedgerNext
	}
	return out
}

// credits serves GET /v1/credits.
func (h *handler) credits(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("scope_kind") == "":
		h.fail(w, r, "credits", required("scope_kind"))
		return
	case q.Get("scope_id") == "":
		h.fail(w, r, "credits", required("scope_id"))
		return
	case (q.Get("subject_kind") == "") != (q.Get("subject_id") == ""):
		h.fail(w, r, "credits", fmt.Errorf("%w: subject_kind and subject_id go together: half a member would be read as everyone", domain.ErrInvalid))
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ports.MaxLedgerPage {
			h.fail(w, r, "credits", fmt.Errorf("%w: limit must be an integer from 1 to %d", domain.ErrInvalid, ports.MaxLedgerPage))
			return
		}
		limit = n
	}
	// days sizes the spend series, which needs the usage_daily rollup. It is accepted and checked so a caller's
	// request keeps its meaning when the series arrives; until then the series is absent.
	if v := q.Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 366 {
			h.fail(w, r, "credits", fmt.Errorf("%w: days must be an integer from 1 to 366", domain.ErrInvalid))
			return
		}
	}
	scope, err := p.Resolve(domain.Scope{Realm: domain.Realm(q.Get("scope_realm")), Kind: q.Get("scope_kind"), ID: q.Get("scope_id")})
	if err != nil {
		h.fail(w, r, "credits", err)
		return
	}
	snap, err := h.cfg.Credits.Credits(r.Context(), ports.SnapshotQuery{
		Scope: scope, SubjectKind: q.Get("subject_kind"), SubjectID: q.Get("subject_id"), LedgerAfter: q.Get("cursor"), LedgerLimit: limit,
	})
	if err != nil {
		h.fail(w, r, "credits", err)
		return
	}
	writeJSON(w, http.StatusOK, creditsToJSON(snap))
}
