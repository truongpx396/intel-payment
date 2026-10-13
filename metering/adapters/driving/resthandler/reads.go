package resthandler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

type ledgerEntryJSON struct {
	ID              string            `json:"id"`
	CreatedAt       string            `json:"created_at"`
	OccurredAt      string            `json:"occurred_at,omitempty"`
	Pool            string            `json:"pool"`
	Delta           int64             `json:"delta"`
	OperationType   string            `json:"operation_type"`
	IdemKey         string            `json:"idem_key"`
	RateCardVersion string            `json:"rate_card_version,omitempty"`
	Seq             int64             `json:"seq"`
	SeqFrom         int64             `json:"seq_from"`
	EventCount      int               `json:"event_count"`
	Resource        string            `json:"resource,omitempty"`
	RateKey         string            `json:"rate_key,omitempty"`
	Quantities      []quantityJSON    `json:"quantities,omitempty"`
	Subjects        map[string]string `json:"subjects,omitempty"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	Counterparty    *scopeJSON        `json:"counterparty,omitempty"`
}

func ledgerEntryToJSON(e ports.LedgerEntry) ledgerEntryJSON {
	out := ledgerEntryJSON{
		ID: e.ID, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano), Pool: string(e.Pool), Delta: int64(e.Delta), OperationType: e.OperationType,
		IdemKey: e.IdemKey, RateCardVersion: e.RateCardVersion, Seq: e.SeqTo, SeqFrom: e.SeqFrom, EventCount: e.EventCount, Resource: e.Resource,
		RateKey: e.RateKey, Subjects: mapOrNil(e.Subjects), Attributes: mapOrNil(e.Ref),
	}
	if !e.OccurredAt.IsZero() {
		out.OccurredAt = e.OccurredAt.UTC().Format(time.RFC3339Nano)
	}
	for _, q := range e.Quantities {
		out.Quantities = append(out.Quantities, quantityJSON{Unit: string(q.Unit), Amount: q.Amount})
	}
	if e.Counterparty != nil {
		out.Counterparty = &scopeJSON{Realm: string(e.Counterparty.Realm), Kind: e.Counterparty.Kind, ID: e.Counterparty.ID}
	}
	return out
}

// ledger serves GET /v1/ledger: the booked rows of one scope, newest first, a page at a time.
func (h *handler) ledger(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("scope_kind") == "":
		h.fail(w, r, "ledger", required("scope_kind"))
		return
	case q.Get("scope_id") == "":
		h.fail(w, r, "ledger", required("scope_id"))
		return
	case (q.Get("subject_kind") == "") != (q.Get("subject_id") == ""):
		h.fail(w, r, "ledger", fmt.Errorf("%w: subject_kind and subject_id go together: half a member would be read as everyone", domain.ErrInvalid))
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ports.MaxLedgerPage {
			h.fail(w, r, "ledger", fmt.Errorf("%w: limit must be an integer from 1 to %d", domain.ErrInvalid, ports.MaxLedgerPage))
			return
		}
		limit = n
	}
	scope, err := p.Resolve(domain.Scope{Realm: domain.Realm(q.Get("scope_realm")), Kind: q.Get("scope_kind"), ID: q.Get("scope_id")})
	if err != nil {
		h.fail(w, r, "ledger", err)
		return
	}
	page, err := h.cfg.Ledger.LedgerRows(r.Context(), ports.LedgerQuery{
		Scope: scope, SubjectKind: q.Get("subject_kind"), SubjectID: q.Get("subject_id"), After: q.Get("cursor"), Limit: limit,
	})
	if err != nil {
		h.fail(w, r, "ledger", err)
		return
	}
	out := struct {
		Data []ledgerEntryJSON `json:"data"`
		Next *string           `json:"next"`
	}{Data: make([]ledgerEntryJSON, 0, len(page.Rows))}
	for _, e := range page.Rows {
		out.Data = append(out.Data, ledgerEntryToJSON(e))
	}
	if page.Next != "" {
		out.Next = &page.Next
	}
	writeJSON(w, http.StatusOK, out)
}

type transferStatusJSON struct {
	IdemKey   string    `json:"idem_key"`
	Status    string    `json:"status"`
	From      scopeJSON `json:"from"`
	To        scopeJSON `json:"to"`
	FromPool  string    `json:"from_pool"`
	ToPool    string    `json:"to_pool"`
	Amount    int64     `json:"amount"`
	CreatedAt string    `json:"created_at"`
	SettledAt string    `json:"settled_at,omitempty"`
}

// transferStatus serves GET /v1/transfers/{idem_key}. A transfer is accepted in the hot tier first and the
// books learn of it when the writer reaches it, so until then the answer is "not yet" (404) and says so.
func (h *handler) transferStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	key := r.PathValue("idem_key")
	scope, err := p.Resolve(domain.Scope{Realm: domain.Realm(r.URL.Query().Get("scope_realm")), Kind: "transfer", ID: key})
	if err != nil {
		h.fail(w, r, "transfer status", err)
		return
	}
	rec, found, err := h.cfg.Transfers.Transfer(r.Context(), scope.Realm, key)
	if err != nil {
		h.fail(w, r, "transfer status", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no transfer with this key has reached the books yet: one accepted a moment ago may not have, so try again shortly")
		return
	}
	out := transferStatusJSON{
		IdemKey: rec.IdemKey, Status: rec.Status,
		From:     scopeJSON{Realm: string(rec.From.Realm), Kind: rec.From.Kind, ID: rec.From.ID},
		To:       scopeJSON{Realm: string(rec.To.Realm), Kind: rec.To.Kind, ID: rec.To.ID},
		FromPool: string(rec.FromPool), ToPool: string(rec.ToPool), Amount: int64(rec.Amount), CreatedAt: rec.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if rec.SettledAt != nil {
		out.SettledAt = rec.SettledAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, out)
}
