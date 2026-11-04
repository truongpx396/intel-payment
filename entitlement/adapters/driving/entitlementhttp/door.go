package entitlementhttp

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	"github.com/truongpx396/intel-payment/entitlement/ports"
)

// Privilege is the one privilege a route needs. The names are the REST handler's: the binary maps one to the other, and a test
// holds the table here to the privileges the document declares.
type Privilege uint8

// Record is what both routes need: they only read.
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
	Entitler ports.Entitler // GET /v1/entitlements
	Checker  ports.Checker  // POST /v1/entitlements/check
	// Caller resolves realms from the credential. Required: without it the door could not tell whose realm a call is for.
	Caller entitlementwire.Caller
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

// New builds the door. A route whose port is absent is not registered: the deployment does not serve it.
func New(cfg Config) (*Door, error) {
	if cfg.Caller == nil {
		return nil, errors.New("entitlementhttp: a Caller is required: without one the door could not tell whose realm a call is for")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = defaultMaxBody
	}
	d := &Door{cfg: cfg}
	if cfg.Entitler != nil {
		d.routes = append(d.routes, Route{http.MethodGet, "/v1/entitlements", Record, d.resolve})
	}
	if cfg.Checker != nil {
		d.routes = append(d.routes, Route{http.MethodPost, "/v1/entitlements/check", Record, d.check})
	}
	if len(d.routes) == 0 {
		return nil, errors.New("entitlementhttp: an Entitler or a Checker is required: a door with no routes serves nothing")
	}
	return d, nil
}

// Routes are the door's routes, in a stable order.
func (d *Door) Routes() []Route { return append([]Route(nil), d.routes...) }
