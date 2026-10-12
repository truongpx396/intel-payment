// Package resthandler serves the REST surface of rest-api.md. It is a thin shell over the same engine the
// gRPC server fronts and decides nothing about money: it authenticates, checks the privilege and the realm,
// maps the request to the engine's types and the outcome to the error table.
//
// The registry is the one place a route is declared, and what decides which of three surfaces it is on:
//
//	/v1/*        a host's backend, behind a service credential, realm-bound, with a privilege
//	/webhooks/*  a payment provider: signature only — no auth middleware, no CORS
//	/healthz, /readyz, /metrics   the platform's
//
// A check added to the wrong surface is refused by Validate, which the build runs over the production table.
package resthandler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// Surface is who a route is for.
type Surface int

// The three surfaces.
const (
	V1 Surface = iota + 1
	Webhook
	Platform
)

func (s Surface) String() string {
	switch s {
	case V1:
		return "/v1"
	case Webhook:
		return "webhook"
	case Platform:
		return "platform"
	}
	return fmt.Sprintf("surface(%d)", int(s))
}

// MiddlewareKind classifies a route's own middleware, so the registry can refuse the kinds a surface must not carry.
type MiddlewareKind int

// The kinds the registry tells apart.
const (
	KindOther MiddlewareKind = iota
	KindAuth
	KindCORS
)

// Middleware wraps one route's handler.
type Middleware struct {
	Kind MiddlewareKind
	Wrap func(http.Handler) http.Handler
}

// RequireAuth is the same authentication /v1 routes get. Declaring it on a route is how a check is
// "added later" — and the registry refuses it on a webhook.
func RequireAuth(a auth.Authenticator) Middleware {
	return Middleware{Kind: KindAuth, Wrap: func(next http.Handler) http.Handler { return authenticate(a, 0, nil, next) }}
}

// CORS answers cross-origin requests. No surface of this service wants it.
func CORS(origin string) Middleware {
	return Middleware{Kind: KindCORS, Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			next.ServeHTTP(w, r)
		})
	}}
}

// Route is one method and path, on one surface.
type Route struct {
	Method, Path string
	Surface      Surface
	// Privilege is the one privilege a /v1 route needs. It is zero on every other surface.
	Privilege  auth.Privilege
	Middleware []Middleware
	Handler    http.HandlerFunc
}

func (r Route) String() string { return r.Method + " " + r.Path }

// Registry is the route table.
type Registry struct{ routes []Route }

// NewRegistry returns an empty table.
func NewRegistry() *Registry { return &Registry{} }

// Add appends a route. Nothing is checked until Validate: a table with a mistake in it is still a table
// one can ask what is wrong with.
func (g *Registry) Add(r Route) { g.routes = append(g.routes, r) }

// Routes returns the table, ordered by path then method.
func (g *Registry) Routes() []Route {
	out := append([]Route(nil), g.routes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// surfaceOf is the surface a path belongs to, and false for a path on none.
func surfaceOf(path string) (Surface, bool) {
	switch {
	case strings.HasPrefix(path, "/v1/"):
		return V1, true
	case strings.HasPrefix(path, "/webhooks/"):
		return Webhook, true
	case path == "/healthz" || path == "/readyz" || path == "/metrics":
		return Platform, true
	}
	return 0, false
}

var methods = map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true}

// Validate reports every way the table breaks the rules of the three surfaces.
func (g *Registry) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for _, r := range g.routes {
		bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("%s: %s", r, fmt.Sprintf(format, a...))) }
		switch {
		case !methods[r.Method]:
			bad("method %q is not one the service routes", r.Method)
		case !strings.HasPrefix(r.Path, "/"):
			bad("a path starts with /")
		case r.Handler == nil:
			bad("no handler")
		}
		if seen[r.String()] {
			bad("declared twice: which handler answers would be a coin toss")
		}
		seen[r.String()] = true

		want, ok := surfaceOf(r.Path)
		switch {
		case !ok:
			bad("the path is on none of the surfaces (/v1/*, /webhooks/*, /healthz, /readyz, /metrics)")
			continue
		case r.Surface != want:
			bad("the path belongs on the %s surface and the route says %s", want, r.Surface)
			continue
		}
		var kinds []MiddlewareKind
		for _, m := range r.Middleware {
			kinds = append(kinds, m.Kind)
		}
		has := func(k MiddlewareKind) bool {
			for _, have := range kinds {
				if have == k {
					return true
				}
			}
			return false
		}
		switch r.Surface {
		case V1:
			if r.Privilege != auth.Record && r.Privilege != auth.Grant && r.Privilege != auth.Admin {
				bad("a /v1 route names exactly one privilege (record, grant or admin), got %s", r.Privilege)
			}
			if has(KindCORS) {
				bad("a /v1 route carries no CORS: a browser-reachable metering API is an unmetered-spend API")
			}
		case Webhook:
			if r.Privilege != 0 {
				bad("a webhook route has no privilege: a provider speaks signatures")
			}
			if has(KindAuth) {
				bad("a webhook route carries no auth middleware: a JWT check there rejects the provider (webhook invariant 11)")
			}
			if has(KindCORS) {
				bad("a webhook route carries no CORS: it is server-to-server (webhook invariant 11)")
			}
		case Platform:
			if r.Privilege != 0 || has(KindAuth) || has(KindCORS) {
				bad("a platform route is open: no privilege, no auth, no CORS")
			}
		}
	}
	return errors.Join(errs...)
}

// Options configure the handler a registry builds.
type Options struct {
	// Auth authenticates /v1 routes. Required when the table has any.
	Auth         auth.Authenticator
	Logger       *slog.Logger
	MaxBodyBytes int64 // default 1 MiB
}

const defaultMaxBody = 1 << 20

// Handler builds the http.Handler for a valid table.
func (g *Registry) Handler(o Options) (http.Handler, error) {
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("resthandler: the route table is invalid: %w", err)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = defaultMaxBody
	}
	rt := &router{log: o.Logger}
	for _, r := range g.routes {
		h := http.Handler(r.Handler)
		for i := len(r.Middleware) - 1; i >= 0; i-- {
			h = r.Middleware[i].Wrap(h)
		}
		if r.Surface == V1 {
			if o.Auth == nil {
				return nil, errors.New("resthandler: the table has /v1 routes and no Authenticator: it would serve money to anyone")
			}
			h = authenticate(o.Auth, r.Privilege, o.Logger, h)
		}
		rt.add(r, h)
	}
	return rt.guard(o.MaxBodyBytes), nil
}

// authenticate lifts a Credential off the request, authenticates it, and checks the privilege. need == 0
// authenticates without requiring one (the RequireAuth middleware a webhook is refused for carrying).
func authenticate(a auth.Authenticator, need auth.Privilege, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred := auth.Credential{}
		if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			cred.Bearer = strings.TrimSpace(token)
		}
		if r.TLS != nil {
			cred.PeerCertificates = r.TLS.PeerCertificates
		}
		p, err := a.Authenticate(r.Context(), cred)
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid service credential")
			return
		case err != nil:
			// The credential could not be checked. That is the service failing, and never a stranger's access.
			if log != nil {
				log.Error("authentication failed", "error", err)
			}
			writeError(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		if need != 0 {
			if err := p.Require(need); err != nil {
				writeError(w, http.StatusForbidden, "forbidden", err.Error())
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// router dispatches on method and path template, in the one error shape.
type router struct {
	entries []entry
	log     *slog.Logger
}

type entry struct {
	method   string
	segs     []string // a segment {name} binds a path value
	literals int
	h        http.Handler
}

func split(path string) []string { return strings.Split(strings.Trim(path, "/"), "/") }

func (rt *router) add(r Route, h http.Handler) {
	segs := split(r.Path)
	lit := 0
	for _, s := range segs {
		if !strings.HasPrefix(s, "{") {
			lit++
		}
	}
	rt.entries = append(rt.entries, entry{method: r.Method, segs: segs, literals: lit, h: h})
}

func (e entry) match(segs []string) (map[string]string, bool) {
	if len(segs) != len(e.segs) {
		return nil, false
	}
	vals := map[string]string{}
	for i, s := range e.segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			if segs[i] == "" {
				return nil, false
			}
			vals[s[1:len(s)-1]] = segs[i]
			continue
		}
		if s != segs[i] {
			return nil, false
		}
	}
	return vals, true
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segs := split(r.URL.Path)
	var best *entry
	var bestVals map[string]string
	allow := map[string]bool{}
	for i := range rt.entries {
		e := &rt.entries[i]
		vals, ok := e.match(segs)
		if !ok {
			continue
		}
		allow[e.method] = true
		if e.method == r.Method && (best == nil || e.literals > best.literals) {
			best, bestVals = e, vals
		}
	}
	switch {
	case best != nil:
		for k, v := range bestVals {
			r.SetPathValue(k, v)
		}
		best.h.ServeHTTP(w, r)
	case len(allow) > 0:
		list := make([]string, 0, len(allow))
		for m := range allow {
			list = append(list, m)
		}
		sort.Strings(list)
		w.Header().Set("Allow", strings.Join(list, ", "))
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "this path does not answer "+r.Method)
	default:
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	}
}

// guard caps request bodies and turns a panic into one 500: a Tier-0 service that dies on one bad request
// has turned that request into an outage.
func (rt *router) guard(maxBody int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				rt.log.Error("request panicked", "route", r.Method+" "+r.URL.Path, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		rt.ServeHTTP(w, r)
	})
}
