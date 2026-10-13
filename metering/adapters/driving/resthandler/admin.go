package resthandler

import (
	"net/http"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The admin API: configuration is data (constitution VIII) and this is its supported interface (FR-049). Every
// write names a person (`actor`) and a reason, and is refused without both; the database writes the audit row
// in the same transaction. The realm is the credential's, named with ?realm= when it spans several.

// defaultWarnAt is the near-limit fraction a limit takes when its write names none: the database's own default.
const defaultWarnAt = 0.8

type adminEntryJSON struct {
	RateKey            string `json:"rate_key"`
	Unit               string `json:"unit"`
	CreditsPerBlock    int64  `json:"credits_per_block"`
	BlockSize          int64  `json:"block_size"`
	CostMicrosPerBlock *int64 `json:"cost_micros_per_block,omitempty"`
}

type cardRequest struct {
	Actor          string           `json:"actor"`
	Reason         string           `json:"reason"`
	Version        string           `json:"version"`
	Pricer         string           `json:"pricer"`
	RetirePrevious bool             `json:"retire_previous"`
	Entries        []adminEntryJSON `json:"entries"`
}

type cardInfoJSON struct {
	Version     string           `json:"version"`
	Pricer      string           `json:"pricer"`
	Active      bool             `json:"active"`
	PublishedAt string           `json:"published_at"`
	RetiredAt   string           `json:"retired_at,omitempty"`
	Entries     []adminEntryJSON `json:"entries"`
}

func cardInfoToJSON(c ports.RateCardInfo) cardInfoJSON {
	out := cardInfoJSON{
		Version: c.Card.Version, Pricer: c.Card.Pricer, Active: c.RetiredAt == nil,
		PublishedAt: c.PublishedAt.UTC().Format(time.RFC3339Nano), Entries: make([]adminEntryJSON, 0, len(c.Card.Entries)),
	}
	if c.RetiredAt != nil {
		out.RetiredAt = c.RetiredAt.UTC().Format(time.RFC3339Nano)
	}
	for _, e := range c.Card.Entries {
		j := adminEntryJSON{RateKey: e.RateKey, Unit: string(e.Unit), CreditsPerBlock: e.CreditsPerBlock, BlockSize: e.BlockSize}
		if e.CostMicrosPerBlock != 0 {
			cost := e.CostMicrosPerBlock
			j.CostMicrosPerBlock = &cost
		}
		out.Entries = append(out.Entries, j)
	}
	return out
}

type limitRequest struct {
	Actor          string   `json:"actor"`
	Reason         string   `json:"reason"`
	SubjectKind    string   `json:"subject_kind"`
	Resource       string   `json:"resource"`
	Unit           string   `json:"unit"`
	Max            int64    `json:"max"`
	MaxEntitlement string   `json:"max_entitlement"`
	Window         string   `json:"window"`
	DurSeconds     int64    `json:"dur_seconds"`
	TZ             string   `json:"tz"`
	WarnAt         *float64 `json:"warn_at"`
	DenyCode       string   `json:"deny_code"`
	SortOrder      *int     `json:"sort_order"`
}

type limitConfigJSON struct {
	Name           string  `json:"name"`
	SubjectKind    string  `json:"subject_kind,omitempty"`
	Resource       string  `json:"resource,omitempty"`
	Unit           string  `json:"unit"`
	Max            int64   `json:"max"`
	MaxEntitlement string  `json:"max_entitlement,omitempty"`
	Window         string  `json:"window"`
	DurSeconds     int64   `json:"dur_seconds,omitempty"`
	TZ             string  `json:"tz,omitempty"`
	WarnAt         float64 `json:"warn_at"`
	DenyCode       string  `json:"deny_code"`
	SortOrder      *int    `json:"sort_order,omitempty"`
}

func limitConfigToJSON(l ports.LimitConfig) limitConfigJSON {
	return limitConfigJSON{
		Name: l.Limit.Name, SubjectKind: l.Limit.Subject, Resource: l.Limit.Resource, Unit: string(l.Limit.Unit), Max: l.Limit.Max,
		MaxEntitlement: l.Limit.MaxEntitlement, Window: l.Limit.Window.String(), DurSeconds: int64(l.Limit.Dur / time.Second), TZ: l.Limit.TZ,
		WarnAt: l.Limit.WarnAt, DenyCode: l.Limit.DenyCode, SortOrder: l.SortOrder,
	}
}

type poolRequest struct {
	Actor       string   `json:"actor"`
	Reason      string   `json:"reason"`
	Priority    int      `json:"priority"`
	AppliesTo   []string `json:"applies_to"`
	Description string   `json:"description"`
}

type poolConfigJSON struct {
	Name        string   `json:"name"`
	Priority    int      `json:"priority"`
	AppliesTo   []string `json:"applies_to,omitempty"`
	Description string   `json:"description,omitempty"`
}

// adminRealm resolves the realm an admin call addresses: the credential's, or ?realm= when it spans several.
func (h *handler) adminRealm(r *http.Request, p auth.Principal) (domain.Realm, error) {
	s, err := p.Resolve(domain.Scope{Realm: domain.Realm(r.URL.Query().Get("realm")), Kind: "admin", ID: "config"})
	return s.Realm, err
}

// change is who and why, from a body's fields or (for a delete) the query. Both are required.
func (h *handler) change(p auth.Principal, actor, reason string) (ports.ConfigChange, error) {
	switch {
	case strings.TrimSpace(actor) == "":
		return ports.ConfigChange{}, required("actor")
	case strings.TrimSpace(reason) == "":
		return ports.ConfigChange{}, required("reason")
	}
	return ports.ConfigChange{Actor: actor, Reason: reason, Via: p.CallerID}, nil
}

func (h *handler) listCards(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "list cards", err)
		return
	}
	cards, err := h.cfg.Admin.RateCards(r.Context(), realm)
	if err != nil {
		h.fail(w, r, "list cards", err)
		return
	}
	out := struct {
		Data []cardInfoJSON `json:"data"`
	}{Data: make([]cardInfoJSON, 0, len(cards))}
	for _, c := range cards {
		out.Data = append(out.Data, cardInfoToJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handler) publishCard(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req cardRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "publish card", err)
		return
	}
	ch, err := h.change(p, req.Actor, req.Reason)
	if err != nil {
		h.fail(w, r, "publish card", err)
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "publish card", err)
		return
	}
	pub := ports.CardPublication{Version: req.Version, Pricer: req.Pricer, RetirePrevious: req.RetirePrevious}
	for _, e := range req.Entries {
		re := domain.RateEntry{RateKey: e.RateKey, Unit: domain.Unit(e.Unit), CreditsPerBlock: e.CreditsPerBlock, BlockSize: e.BlockSize}
		if e.CostMicrosPerBlock != nil {
			re.CostMicrosPerBlock = *e.CostMicrosPerBlock
		}
		pub.Entries = append(pub.Entries, re)
	}
	info, err := h.cfg.Admin.PublishRateCard(r.Context(), ch, realm, pub)
	if err != nil {
		h.fail(w, r, "publish card", err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		cardInfoJSON
		Published bool `json:"published"`
	}{cardInfoToJSON(info), true})
}

func (h *handler) getLimit(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "get limit", err)
		return
	}
	l, err := h.cfg.Admin.Limit(r.Context(), realm, r.PathValue("name"))
	if err != nil {
		h.fail(w, r, "get limit", err)
		return
	}
	writeJSON(w, http.StatusOK, limitConfigToJSON(l))
}

func (h *handler) putLimit(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req limitRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "put limit", err)
		return
	}
	ch, err := h.change(p, req.Actor, req.Reason)
	if err != nil {
		h.fail(w, r, "put limit", err)
		return
	}
	// The window is required: a default would make an unfinished limit a balance.
	dl, err := limitFromJSON(limitJSON{
		Name: r.PathValue("name"), Subject: req.SubjectKind, Resource: req.Resource, Unit: req.Unit, Max: req.Max, Window: req.Window,
		DurSeconds: req.DurSeconds, TZ: req.TZ, DenyCode: req.DenyCode, MaxEntitlement: req.MaxEntitlement,
	}, "limit")
	if err != nil {
		h.fail(w, r, "put limit", err)
		return
	}
	dl.WarnAt = defaultWarnAt
	if req.WarnAt != nil {
		dl.WarnAt = *req.WarnAt
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "put limit", err)
		return
	}
	lc := ports.LimitConfig{Limit: dl, SortOrder: req.SortOrder}
	created, err := h.cfg.Admin.PutLimit(r.Context(), ch, realm, lc)
	if err != nil {
		h.fail(w, r, "put limit", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, limitConfigToJSON(lc))
}

func (h *handler) deleteLimit(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ch, err := h.change(p, q.Get("actor"), q.Get("reason"))
	if err != nil {
		h.fail(w, r, "delete limit", err)
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "delete limit", err)
		return
	}
	if err := h.cfg.Admin.DeleteLimit(r.Context(), ch, realm, r.PathValue("name")); err != nil {
		h.fail(w, r, "delete limit", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func poolConfigToJSON(p ports.PoolConfig) poolConfigJSON {
	return poolConfigJSON{Name: string(p.Pool.Name), Priority: p.Pool.Priority, AppliesTo: p.Pool.AppliesTo, Description: p.Description}
}

func (h *handler) getPool(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "get pool", err)
		return
	}
	pc, err := h.cfg.Admin.Pool(r.Context(), realm, r.PathValue("name"))
	if err != nil {
		h.fail(w, r, "get pool", err)
		return
	}
	writeJSON(w, http.StatusOK, poolConfigToJSON(pc))
}

func (h *handler) putPool(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req poolRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "put pool", err)
		return
	}
	ch, err := h.change(p, req.Actor, req.Reason)
	if err != nil {
		h.fail(w, r, "put pool", err)
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "put pool", err)
		return
	}
	pc := ports.PoolConfig{Pool: domain.PoolDef{Name: domain.Pool(r.PathValue("name")), Priority: req.Priority, AppliesTo: req.AppliesTo}, Description: req.Description}
	created, err := h.cfg.Admin.PutPool(r.Context(), ch, realm, pc)
	if err != nil {
		h.fail(w, r, "put pool", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, poolConfigToJSON(pc))
}
