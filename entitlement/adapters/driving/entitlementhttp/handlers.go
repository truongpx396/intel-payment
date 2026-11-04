package entitlementhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	pb "github.com/truongpx396/intel-payment/api/entitlementv1"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ---- the wire, as JSON -------------------------------------------------------------------------------------------------

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type scopeJSON struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Realm string `json:"realm,omitempty"` // optional, and the credential's to confirm, never the body's to assert
}

type queryJSON struct {
	Key     string `json:"key"`
	Current *int64 `json:"current,omitempty"`
	Delta   *int64 `json:"delta,omitempty"`
}

type checkRequestJSON struct {
	Scope  *scopeJSON  `json:"scope"`
	Checks []queryJSON `json:"checks"`
}

// DecisionJSON is one answer. value is the plain JSON value of its kind — true, 25, "gold" — with -1 on a quota meaning unlimited,
// and null when nothing grants the key.
type DecisionJSON struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	Allowed     bool   `json:"allowed"`
	Value       any    `json:"value"`
	Limit       *int64 `json:"limit,omitempty"`
	Current     *int64 `json:"current,omitempty"`
	Remaining   *int64 `json:"remaining,omitempty"`
	Reason      string `json:"reason"`
	UpgradePath string `json:"upgrade_path"`
}

// GrantJSON is one effective entitlement and where it came from.
type GrantJSON struct {
	Key       string  `json:"key"`
	Kind      string  `json:"kind"`
	Value     any     `json:"value"`
	Source    string  `json:"source"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	Reason    string  `json:"reason"`
}

func valueJSON(v *pb.Value) any {
	switch k := v.GetKind().(type) {
	case *pb.Value_Flag:
		return k.Flag
	case *pb.Value_Quota:
		return k.Quota
	case *pb.Value_Enum:
		return k.Enum
	}
	return nil
}

// DecisionsJSON renders the wire's decisions. It reads the proto message the gRPC door returns, so the two doors have one source
// of what an answer is.
func DecisionsJSON(r *pb.CheckResponse) []DecisionJSON {
	out := make([]DecisionJSON, len(r.GetDecisions()))
	for i, d := range r.GetDecisions() {
		out[i] = DecisionJSON{
			Key: d.GetKey(), Kind: d.GetKind(), Allowed: d.GetAllowed(), Value: valueJSON(d.GetValue()),
			Limit: d.Limit, Current: d.Current, Remaining: d.Remaining, Reason: d.GetReason(), UpgradePath: d.GetUpgradePath(),
		}
	}
	return out
}

// GrantsJSON renders the wire's grants.
func GrantsJSON(r *pb.ResolveResponse) []GrantJSON {
	out := make([]GrantJSON, len(r.GetGrants()))
	for i, g := range r.GetGrants() {
		out[i] = GrantJSON{Key: g.GetKey(), Kind: g.GetKind(), Value: valueJSON(g.GetValue()), Source: g.GetSource(), Reason: g.GetReason()}
		if g.ExpiresAt != nil {
			s := g.GetExpiresAt().AsTime().UTC().Format(time.RFC3339Nano)
			out[i].ExpiresAt = &s
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store") // entitlements change; an intermediary that kept one would deny a customer who just paid
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, message
	writeJSON(w, status, b)
}

// fail answers an error with a status a client can act on. The caller's own cancellation wins over whatever the call made of it.
// What is not one of the named refusals is the service failing: fixed text for the client, the whole error for the operator's log.
func (d *Door) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	if r.Context().Err() != nil {
		err = r.Context().Err()
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "the request body is over the limit")
		return
	}
	c, ok := entitlementwire.Classify(err)
	if !ok {
		d.cfg.Logger.Log(r.Context(), slog.LevelError, "entitlement call failed", "op", op, "error", err)
		writeError(w, entitlementwire.Internal.HTTP, entitlementwire.Internal.Code, entitlementwire.Internal.Message)
		return
	}
	if c.HTTP == http.StatusServiceUnavailable {
		d.cfg.Logger.Log(r.Context(), slog.LevelError, "entitlements could not be resolved", "op", op, "error", err)
	}
	if c.HTTP == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeError(w, c.HTTP, c.Code, c.Message)
}

// decodeBody reads exactly one JSON value into v, strictly: an unknown field is a typo that would silently not apply, and a second
// value after the first is a request that says two things.
func (d *Door) decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, d.cfg.MaxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			return err
		case errors.Is(err, io.EOF):
			return entitlementwire.Invalid("the request body is empty")
		}
		return entitlementwire.Invalid("the request body is not the JSON expected: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return entitlementwire.Invalid("the request body holds more than one JSON value")
	}
	return nil
}

func (d *Door) scope(ctx context.Context, s md.Scope) (md.Scope, error) {
	return entitlementwire.ResolveScope(ctx, d.cfg.Caller, s)
}

// ---- the routes -------------------------------------------------------------------------------------------------------

// check is POST /v1/entitlements/check.
func (d *Door) check(w http.ResponseWriter, r *http.Request) {
	var body checkRequestJSON
	if err := d.decodeBody(w, r, &body); err != nil {
		d.fail(w, r, "check", err)
		return
	}
	if body.Scope == nil {
		d.fail(w, r, "check", entitlementwire.Invalid("scope is required"))
		return
	}
	req := &pb.CheckRequest{Checks: make([]*pb.Query, len(body.Checks))}
	for i, q := range body.Checks {
		req.Checks[i] = &pb.Query{Key: q.Key, Current: q.Current, Delta: q.Delta}
	}
	scope, err := d.scope(r.Context(), md.Scope{Realm: md.Realm(body.Scope.Realm), Kind: body.Scope.Kind, ID: body.Scope.ID})
	if err != nil {
		d.fail(w, r, "check", err)
		return
	}
	results, err := d.cfg.Checker.Check(r.Context(), scope, entitlementwire.ChecksFromProto(req.Checks))
	if err != nil {
		d.fail(w, r, "check", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": DecisionsJSON(entitlementwire.DecisionsToProto(results))})
}

// resolve is GET /v1/entitlements?scope_kind=&scope_id=[&scope_realm=].
func (d *Door) resolve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case q.Get("scope_kind") == "":
		d.fail(w, r, "resolve", entitlementwire.Invalid("scope_kind is required"))
		return
	case q.Get("scope_id") == "":
		d.fail(w, r, "resolve", entitlementwire.Invalid("scope_id is required"))
		return
	}
	scope, err := d.scope(r.Context(), md.Scope{Realm: md.Realm(q.Get("scope_realm")), Kind: q.Get("scope_kind"), ID: q.Get("scope_id")})
	if err != nil {
		d.fail(w, r, "resolve", err)
		return
	}
	grants, err := d.cfg.Entitler.Resolve(r.Context(), scope)
	if err != nil {
		d.fail(w, r, "resolve", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": GrantsJSON(entitlementwire.GrantsToProto(grants))})
}
