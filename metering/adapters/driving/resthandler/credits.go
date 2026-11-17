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
// integer. The burn rate, the series and the breakdown come from the usage_daily rollup: a scope-wide snapshot carries them, and a member's
// carries none (the rollup cannot tell a member's days from a colleague's), so the burn rate and the series are absent there — not empty, not
// invented — and the breakdown is an empty list. An anchor and notices are the host's, and absent here.

type creditsJSON struct {
	Scope      scopeJSON         `json:"scope"`
	Balance    balanceViewJSON   `json:"balance"`
	Limits     []limitViewJSON   `json:"limits"`
	Series     []seriesPointJSON `json:"series,omitempty"`
	Breakdown  []breakdownJSON   `json:"breakdown"`
	Ledger     []ledgerRowJSON   `json:"ledger"`
	LedgerNext *string           `json:"ledgerNext"`
}

type balanceViewJSON struct {
	Remaining       int64  `json:"remaining"`
	Granted         int64  `json:"granted"`
	PrimaryLimitKey string `json:"primaryLimitKey"`
	// BurnRatePerDay is a pointer so that a real zero (a window with no spend) is on the wire as 0 and an absent one (no spend section) is not.
	BurnRatePerDay *int64         `json:"burnRatePerDay,omitempty"`
	Pools          []poolViewJSON `json:"pools"`
}

// seriesPointJSON is one day of spend: the instant the day starts (UTC, as the rollup keeps its days) and what was spent.
type seriesPointJSON struct {
	At     string `json:"at"`
	Amount int64  `json:"amount"`
}

// breakdownJSON is one dimension of the spend: the credits contract's BreakdownSeries. The host names its own dimensions; the engine's is the
// resource that was metered, so key and label are both that, and the cost and event count the rollup also holds are not part of the contract.
type breakdownJSON struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Amount int64  `json:"amount"`
}

// unspecified names the dimension of spend whose ledger row names no resource: a blank key or label would be a row nobody can read.
const unspecified = "unspecified"

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
		Breakdown: []breakdownJSON{},
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
	if c.Spend != nil {
		burn := int64(c.Spend.BurnRatePerDay)
		out.Balance.BurnRatePerDay = &burn
		out.Series = make([]seriesPointJSON, 0, len(c.Spend.Series))
		for _, p := range c.Spend.Series {
			// a day is YYYY-MM-DD, so its first instant in UTC is that date at midnight
			out.Series = append(out.Series, seriesPointJSON{At: p.Day + "T00:00:00Z", Amount: int64(p.Credits)})
		}
		for _, r := range c.Spend.Breakdown {
			key := r.Dimension
			if key == "" {
				key = unspecified
			}
			out.Breakdown = append(out.Breakdown, breakdownJSON{Key: key, Label: key, Amount: int64(r.Credits)})
		}
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
	// days sizes the spend series (and the window the breakdown and the burn rate cover): 1 to a year's worth, and absent means the
	// engine's default, which is the engine's to choose and not this handler's.
	days := 0
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > domain.MaxSeriesDays {
			h.fail(w, r, "credits", fmt.Errorf("%w: days must be an integer from 1 to %d", domain.ErrInvalid, domain.MaxSeriesDays))
			return
		}
		days = n
	}
	scope, err := p.Resolve(domain.Scope{Realm: domain.Realm(q.Get("scope_realm")), Kind: q.Get("scope_kind"), ID: q.Get("scope_id")})
	if err != nil {
		h.fail(w, r, "credits", err)
		return
	}
	snap, err := h.cfg.Credits.Credits(r.Context(), ports.SnapshotQuery{
		Scope: scope, SubjectKind: q.Get("subject_kind"), SubjectID: q.Get("subject_id"), LedgerAfter: q.Get("cursor"), LedgerLimit: limit, Days: days,
	})
	if err != nil {
		h.fail(w, r, "credits", err)
		return
	}
	writeJSON(w, http.StatusOK, creditsToJSON(snap))
}
