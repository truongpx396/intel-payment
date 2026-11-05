// Package eventshttp is the pull door onto the outbound log: GET /v1/events.
package eventshttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/app"
)

// The ways a credential fails the door.
var (
	// ErrUnauthenticated is a call no credential reached. The transport authenticates every call before a handler runs, so it is a wiring
	// slip — and a slip refuses, it never serves.
	ErrUnauthenticated = errors.New("missing or invalid service credential")
	// ErrForbidden is a credential without the privilege the call needs.
	ErrForbidden = errors.New("the credential does not hold the privilege this call needs")
	// ErrRealmMismatch is a realm the credential does not cover.
	ErrRealmMismatch = errors.New("the credential does not cover this realm")
	// ErrRealmRequired is a credential that covers every realm, asked for the feed of none in particular.
	ErrRealmRequired = errors.New("this credential covers every realm: say which one with ?realm=")
)

// Caller says which realms a call may read, from the credential the transport authenticated. The door imports no transport and no
// authenticator.
type Caller interface {
	// Realms is the realms the feed is read for: every realm the credential covers, or just `requested` when it names one the credential
	// covers. The errors above say what was wrong.
	Realms(ctx context.Context, requested string) ([]string, error)
}

// Privilege is the one privilege the route needs. The names are the REST handler's: the binary maps one to the other.
type Privilege uint8

// Record is what the feed needs: it only reads.
const Record Privilege = iota + 1

func (p Privilege) String() string {
	if p == Record {
		return "record"
	}
	return fmt.Sprintf("privilege(%d)", uint8(p))
}

// Route is one method and path with the privilege it needs.
type Route struct {
	Method, Path string
	Privilege    Privilege
	Handler      http.HandlerFunc
}

// Config is what the door is built from.
type Config struct {
	Feed   *app.Feed
	Caller Caller
	Logger *slog.Logger // default slog.Default()
}

// Door is the route of a Config.
type Door struct{ cfg Config }

// New builds the door.
func New(cfg Config) (*Door, error) {
	if cfg.Feed == nil || cfg.Caller == nil {
		return nil, errors.New("eventshttp: a Feed and a Caller are required: without a Caller the door could not tell whose events to serve")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Door{cfg: cfg}, nil
}

// Routes are the door's routes.
func (d *Door) Routes() []Route {
	return []Route{{http.MethodGet, "/v1/events", Record, d.list}}
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, message
	reply(w, status, b)
}

func (d *Door) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var limit int
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_request", "limit is a whole number")
			return
		}
		limit = n
		if limit == 0 { // 0 would silently mean "the default": a typo is not a request
			fail(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("limit is 1 to %d", app.MaxFeedLimit))
			return
		}
	}
	var types []string
	if v := q.Get("types"); v != "" {
		types = strings.Split(v, ",")
	}
	realms, err := d.cfg.Caller.Realms(r.Context(), q.Get("realm"))
	switch {
	case errors.Is(err, ErrUnauthenticated):
		fail(w, http.StatusUnauthorized, "unauthenticated", "A service credential is required.")
		return
	case errors.Is(err, ErrForbidden):
		fail(w, http.StatusForbidden, "forbidden", "The credential does not hold the privilege this call needs.")
		return
	case errors.Is(err, ErrRealmMismatch):
		fail(w, http.StatusForbidden, "realm_mismatch", "The credential does not cover that realm.")
		return
	case errors.Is(err, ErrRealmRequired):
		fail(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	case err != nil:
		d.cfg.Logger.ErrorContext(r.Context(), "events feed: the credential's realms could not be resolved", "error", err.Error())
		fail(w, http.StatusInternalServerError, "internal", "Internal error.")
		return
	}
	page, err := d.cfg.Feed.Page(r.Context(), realms, q.Get("after"), types, limit)
	switch {
	case errors.Is(err, events.ErrInvalid):
		fail(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), events.ErrInvalid.Error()+": "))
		return
	case errors.Is(err, context.DeadlineExceeded):
		fail(w, http.StatusGatewayTimeout, "deadline_exceeded", "The request took too long.")
		return
	case err != nil:
		d.cfg.Logger.ErrorContext(r.Context(), "events feed: the log could not be read", "error", err.Error())
		fail(w, http.StatusInternalServerError, "internal", "Internal error.")
		return
	}
	data := make([]json.RawMessage, 0, len(page.Events))
	for _, e := range page.Events {
		body, err := e.Body()
		if err != nil {
			d.cfg.Logger.ErrorContext(r.Context(), "events feed: an event in the log has no envelope", "event", e.ID, "error", err.Error())
			fail(w, http.StatusInternalServerError, "internal", "Internal error.")
			return
		}
		data = append(data, body)
	}
	var next *string
	if page.Next != "" {
		next = &page.Next
	}
	reply(w, http.StatusOK, struct {
		Data []json.RawMessage `json:"data"`
		Next *string           `json:"next"`
	}{data, next})
}
