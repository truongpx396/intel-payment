package paymenthttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/truongpx396/intel-payment/billing/domain"
)

// errorBody is the one error shape of the REST API.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
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

// fail answers an error with a status a client can act on. The caller's own cancellation wins over whatever the call made
// of it. What is not one of the named refusals is the service failing: fixed text for the client, the whole error for the
// operator's log — the chain names the topology (a dial address, a DSN).
func (d *Door) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	if r.Context().Err() != nil {
		err = r.Context().Err()
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", fmt.Sprintf("the request body is over %d bytes", tooBig.Limit))
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "cancelled", "the caller went away")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "deadline_exceeded", "the deadline passed")
	case errors.Is(err, ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "unauthenticated", ErrUnauthenticated.Error())
	case errors.Is(err, ErrRealmMismatch):
		writeError(w, http.StatusForbidden, "realm_mismatch", ErrRealmMismatch.Error())
	case errors.Is(err, domain.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrUnknownPrice):
		writeError(w, http.StatusUnprocessableEntity, "unknown_price", err.Error())
	case errors.Is(err, domain.ErrUnknownAccount):
		writeError(w, http.StatusNotFound, "unknown_account", err.Error())
	case errors.Is(err, domain.ErrBelowUsage):
		writeError(w, http.StatusConflict, "below_usage", err.Error())
	case errors.Is(err, domain.ErrUnsupported):
		writeError(w, http.StatusUnprocessableEntity, "unsupported", domain.ErrUnsupported.Error())
	case errors.Is(err, domain.ErrIdemConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "the Idempotency-Key was already used for a different request")
	case errors.Is(err, domain.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", "it already exists")
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	default:
		d.cfg.Logger.Log(r.Context(), slog.LevelError, "payment call failed", "op", op, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalid, fmt.Sprintf(format, a...))
}

func required(field string) error { return invalid("%s is required", field) }

// decodeBody reads exactly one JSON value into v, strictly: an unknown field is a typo that would silently not apply, and a
// second value after the first is a request that says two things.
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
			return invalid("the request body is empty")
		}
		return invalid("the request body is not the JSON expected: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return invalid("the request body holds more than one JSON value")
	}
	return nil
}

// scopeJSON is a scope on the wire. The realm is optional and is the credential's to confirm, never the body's to assert.
type scopeJSON struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Realm string `json:"realm,omitempty"`
}

func (d *Door) resolve(r *http.Request, s domain.Scope) (domain.Scope, error) {
	scope, err := d.cfg.Caller.Resolve(r, s)
	if err != nil {
		return domain.Scope{}, err
	}
	if err := scope.Validate(); err != nil {
		return domain.Scope{}, err
	}
	return scope.Normalized(), nil
}

func (d *Door) scopeOf(r *http.Request, s *scopeJSON) (domain.Scope, error) {
	if s == nil {
		return domain.Scope{}, required("scope")
	}
	return d.resolve(r, domain.Scope{Realm: domain.Realm(s.Realm), Kind: s.Kind, ID: s.ID})
}

// scopeFromQuery reads ?scope_kind=&scope_id= (and &scope_realm= when the credential spans several).
func (d *Door) scopeFromQuery(r *http.Request) (domain.Scope, error) {
	q := r.URL.Query()
	switch {
	case q.Get("scope_kind") == "":
		return domain.Scope{}, required("scope_kind")
	case q.Get("scope_id") == "":
		return domain.Scope{}, required("scope_id")
	}
	return d.resolve(r, domain.Scope{Realm: domain.Realm(q.Get("scope_realm")), Kind: q.Get("scope_kind"), ID: q.Get("scope_id")})
}

// idemKey is the operation's identity: the Idempotency-Key header, exactly one, not blank. A server-made key would make
// every retry a new session.
func idemKey(r *http.Request) (string, error) {
	vals := r.Header.Values("Idempotency-Key")
	switch {
	case len(vals) == 0 || strings.TrimSpace(vals[0]) == "":
		return "", invalid("the Idempotency-Key header is required: it is this operation's identity, and a retry carries the same one")
	case len(vals) > 1:
		return "", invalid("send exactly one Idempotency-Key header")
	}
	return strings.TrimSpace(vals[0]), nil
}

// limitOf reads ?limit=: absent is 0 (the store's default), present is 1 to 200.
func limitOf(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 200 {
		return 0, invalid("limit must be a whole number from 1 to 200, got %q", v)
	}
	return n, nil
}
