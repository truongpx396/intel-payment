package resthandler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The operator's door (rest-api.md § Operations). paymentd has no writer, and the operations act on the writer, so a call here is
// recorded — with the person, the reason and the Idempotency-Key — and answered 202; the worker that holds the writer does it, and the
// operation's record says how it went. Every route is behind the `admin` privilege, which the table enforces and the matrix test holds.

type operationJSON struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Status         string          `json:"status"`
	Realm          string          `json:"realm"`
	Params         json.RawMessage `json:"params"`
	Actor          string          `json:"actor"`
	Reason         string          `json:"reason"`
	IdempotencyKey string          `json:"idempotency_key"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
	Attempts       int             `json:"attempts"`
	CreatedAt      string          `json:"created_at"`
	StartedAt      string          `json:"started_at,omitempty"`
	FinishedAt     string          `json:"finished_at,omitempty"`
}

func operationToJSON(o ports.AdminOperation) operationJSON {
	out := operationJSON{
		ID: o.ID, Kind: string(o.Kind), Status: string(o.Status), Realm: o.Realm, Params: o.Params, Actor: o.Actor, Reason: o.Reason,
		IdempotencyKey: o.Key, Result: o.Result, Error: o.Error, Attempts: o.Attempts, CreatedAt: o.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if o.StartedAt != nil {
		out.StartedAt = o.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if o.FinishedAt != nil {
		out.FinishedAt = o.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// who is the person and the reason every operation names, as the body carries them.
type who struct {
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// submit is what every operation route does once it has read its own arguments: check that a person and a reason are named, take the key
// from the header, resolve the realm, record the request and say where to look. params is what the kind takes, as the caller gave it; the
// application reads it again and decides what is stored.
func (h *handler) submit(w http.ResponseWriter, r *http.Request, what string, kind ports.AdminOperationKind, by who, params any) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	ch, err := h.change(p, by.Actor, by.Reason)
	if err != nil {
		h.fail(w, r, what, err)
		return
	}
	key, err := idemKey(r, "")
	if err != nil {
		h.fail(w, r, what, err)
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, what, err)
		return
	}
	raw, err := json.Marshal(params)
	if err != nil {
		h.fail(w, r, what, fmt.Errorf("%w: the arguments could not be written", domain.ErrInvalid))
		return
	}
	op, created, err := h.cfg.Operations.Submit(r.Context(), ports.AdminRequest{
		Realm: string(realm), Kind: kind, Params: raw, Actor: ch.Actor, Reason: ch.Reason, Key: key, Via: ch.Via,
	})
	if err != nil {
		h.fail(w, r, what, err)
		return
	}
	w.Header().Set("Location", "/v1/admin/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, struct {
		Created   bool          `json:"created"`
		Operation operationJSON `json:"operation"`
	}{created, operationToJSON(op)})
}

func (h *handler) opReconcile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		who
		Shard *int   `json:"shard"`
		Kind  string `json:"kind,omitempty"`
	}
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "reconcile", err)
		return
	}
	h.submit(w, r, "reconcile", ports.AdminReconcile, req.who, struct {
		Shard *int   `json:"shard"`
		Kind  string `json:"kind,omitempty"`
	}{req.Shard, req.Kind})
}

func (h *handler) opRecover(w http.ResponseWriter, r *http.Request) {
	var req who
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "recover", err)
		return
	}
	shard, err := strconv.Atoi(r.PathValue("shard"))
	if err != nil || shard < 0 {
		h.fail(w, r, "recover", fmt.Errorf("%w: the shard in the path is a number from zero", domain.ErrInvalid))
		return
	}
	h.submit(w, r, "recover", ports.AdminRecover, req, struct {
		Shard int `json:"shard"`
	}{shard})
}

func (h *handler) opRehydrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		who
		Scope *struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"scope"`
	}
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "rehydrate", err)
		return
	}
	h.submit(w, r, "rehydrate", ports.AdminRehydrate, req.who, struct {
		Scope any `json:"scope"`
	}{req.Scope})
}

func (h *handler) opReplay(w http.ResponseWriter, r *http.Request) {
	var req who
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "replay", err)
		return
	}
	h.submit(w, r, "replay", ports.AdminReplay, req, struct {
		ID string `json:"id"`
	}{r.PathValue("id")})
}

func (h *handler) opSubdrift(w http.ResponseWriter, r *http.Request) {
	var req struct {
		who
		Account string `json:"account"`
	}
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "subdrift", err)
		return
	}
	h.submit(w, r, "subdrift", ports.AdminSubdrift, req.who, struct {
		Account string `json:"account"`
	}{req.Account})
}

func (h *handler) getOperation(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	realm, err := h.adminRealm(r, p)
	if err != nil {
		h.fail(w, r, "get operation", err)
		return
	}
	op, err := h.cfg.Operations.Get(r.Context(), string(realm), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, "get operation", err)
		return
	}
	writeJSON(w, http.StatusOK, operationToJSON(op))
}
