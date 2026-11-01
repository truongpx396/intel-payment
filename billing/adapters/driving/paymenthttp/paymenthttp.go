package paymenthttp

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// Privilege is the one privilege a route needs. The names are the REST handler's: the binary maps one to the other, and
// a test holds the table here to the privileges the document declares.
type Privilege uint8

// The privileges a route here can need.
const (
	Record Privilege = iota + 1
	Grant
	Admin
)

func (p Privilege) String() string {
	switch p {
	case Record:
		return "record"
	case Grant:
		return "grant"
	case Admin:
		return "admin"
	}
	return fmt.Sprintf("privilege(%d)", uint8(p))
}

// Route is one method and path with the privilege it needs. The binary mounts it on the REST handler's /v1 surface, which
// authenticates the caller and checks the privilege before Handler runs.
type Route struct {
	Method, Path string
	Privilege    Privilege
	Handler      http.HandlerFunc
}

// The two ways a credential fails the door.
var (
	// ErrRealmMismatch is a scope whose realm the credential does not cover: 403 realm_mismatch.
	ErrRealmMismatch = errors.New("the credential does not cover this realm")
	// ErrUnauthenticated is a call no credential reached. The registry authenticates every /v1 route before a handler runs,
	// so it is a wiring slip — and a slip refuses, it never serves: 401.
	ErrUnauthenticated = errors.New("missing or invalid service credential")
)

// Caller says whose call this is. The REST handler authenticated it; this is how the door learns what that means for a
// scope, without importing the transport.
type Caller interface {
	// Resolve gives s the realm the credential means: its own when it has one and s names none, s's when the credential
	// covers it. A credential that spans several realms and a scope that names none is domain-level ErrInvalid — a
	// realm is never guessed — a realm the credential does not cover is ErrRealmMismatch, and a request no credential
	// reached is ErrUnauthenticated.
	Resolve(r *http.Request, s domain.Scope) (domain.Scope, error)
	// Via names the credential the request came through, for the operator who asks which tool made a change.
	Via(r *http.Request) string
}

// Config is what the door is built from. A route whose port is absent is not registered: the deployment does not serve it.
type Config struct {
	Catalog   ports.Catalog       // GET /v1/plans
	Checkout  ports.Checkout      // POST /v1/checkout, GET /v1/portal, GET /v1/fulfilment
	Subs      ports.Subs          // GET /v1/subscription
	Changes   ports.Subscriptions // POST /v1/subscription/change and /cancel
	Recharges ports.AutoRecharges // GET and PUT /v1/auto-recharge
	Payments  ports.Payments      // GET /v1/payments
	Admin     ports.Admin         // /v1/admin/plans, /v1/admin/provider-accounts and /v1/admin/customers: the `admin` privilege

	// Caller resolves realms from the credential. Required: without it the door could not tell whose realm a call is for.
	Caller Caller
	// Logger receives the failures an operator must act on, with their cause. Default slog.Default().
	Logger *slog.Logger
	// MaxBody caps a request body in bytes. Default 1 MiB, the REST handler's own cap.
	MaxBody int64
}

const defaultMaxBody = 1 << 20

// Door is the routes of a Config.
type Door struct {
	cfg    Config
	routes []Route
}

// New builds the door.
func New(cfg Config) (*Door, error) {
	if cfg.Caller == nil {
		return nil, errors.New("paymenthttp: a Caller is required: without one the door could not tell whose realm a call is for")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = defaultMaxBody
	}
	d := &Door{cfg: cfg}
	add := func(method, path string, p Privilege, h http.HandlerFunc) {
		d.routes = append(d.routes, Route{Method: method, Path: path, Privilege: p, Handler: h})
	}
	if cfg.Catalog != nil {
		add(http.MethodGet, "/v1/plans", Record, d.plans)
	}
	if cfg.Checkout != nil {
		add(http.MethodPost, "/v1/checkout", Record, d.checkout)
		add(http.MethodGet, "/v1/portal", Record, d.portal)
		add(http.MethodGet, "/v1/fulfilment", Record, d.fulfilment)
	}
	if cfg.Subs != nil {
		add(http.MethodGet, "/v1/subscription", Record, d.subscription)
	}
	if cfg.Changes != nil {
		add(http.MethodPost, "/v1/subscription/change", Grant, d.change)
		add(http.MethodPost, "/v1/subscription/cancel", Grant, d.cancel)
	}
	if cfg.Recharges != nil {
		add(http.MethodGet, "/v1/auto-recharge", Record, d.getRecharge)
		add(http.MethodPut, "/v1/auto-recharge", Grant, d.putRecharge)
	}
	if cfg.Payments != nil {
		add(http.MethodGet, "/v1/payments", Record, d.payments)
	}
	if cfg.Admin != nil {
		add(http.MethodGet, "/v1/admin/plans", Admin, d.listPlans)
		add(http.MethodPost, "/v1/admin/plans", Admin, d.createPlan)
		add(http.MethodGet, "/v1/admin/plans/{code}", Admin, d.getPlan)
		add(http.MethodPatch, "/v1/admin/plans/{code}", Admin, d.patchPlan)
		add(http.MethodPut, "/v1/admin/plans/{code}/provider-prices/{account}/{currency}", Admin, d.putProviderPrice)
		add(http.MethodGet, "/v1/admin/provider-accounts", Admin, d.listAccounts)
		add(http.MethodGet, "/v1/admin/provider-accounts/{id}", Admin, d.getAccount)
		add(http.MethodPut, "/v1/admin/provider-accounts/{id}", Admin, d.putAccount)
		add(http.MethodGet, "/v1/admin/customers", Admin, d.getCustomer)
		add(http.MethodPut, "/v1/admin/customers", Admin, d.putCustomer)
	}
	if len(d.routes) == 0 {
		return nil, errors.New("paymenthttp: no port was given, so the door would serve nothing: that is a wiring mistake")
	}
	return d, nil
}

// Routes are the routes the binary mounts.
func (d *Door) Routes() []Route { return append([]Route(nil), d.routes...) }
