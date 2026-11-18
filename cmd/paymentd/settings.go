package main

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// settings is everything the process is told, parsed once at start from PAYMENT_* variables (docs/configuration.md).
// Nothing past this file reads the environment.
type settings struct {
	Engine   metering.Config // defaults applied
	Payments envcfg.Payments
	Selling  envcfg.Selling
	// Entitlements is the entitlement cache's TTL and this replica's name for the bus group that keeps it current.
	Entitlements envcfg.Entitlements

	Bus          string // redis_streams | nats_jetstream
	BusURL       string
	BusRetention time.Duration

	HTTPAddr, GRPCAddr string
	// MetricsAddr is where /metrics is served when it has a listener of its own (PAYMENT_METRICS_ADDR); empty means the API
	// listener serves it, which is only allowed while that listener is private.
	MetricsAddr     string
	Auth            string // none | mtls | bearer
	Authenticator   auth.Authenticator
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string
	Legacy429       bool
	LogLevel        slog.Level
	ShutdownTimeout time.Duration // how long a stop waits for the calls in flight
	// RequestTimeout is the deadline every request carries (PAYMENT_REQUEST_TIMEOUT), so a query that stalls cannot hold a
	// pooled connection for ever.
	RequestTimeout time.Duration
}

const (
	// httpWriteTimeout is how long the server allows itself to write a response; a request deadline must end before it.
	httpWriteTimeout      = 30 * time.Second
	defaultRequestTimeout = 25 * time.Second
)

const (
	authNone   = "none"
	authMTLS   = "mtls"
	authBearer = "bearer"
)

// parseSettings reads and checks the whole environment. Every problem is reported at once, each naming the
// variable to fix, and none echoes a credential: a refusal is logged, shipped and pasted into tickets.
func parseSettings(getenv func(string) string) (settings, error) {
	r := envcfg.NewReader(getenv)
	var s settings

	cfg := r.Engine()
	bus := r.Bus()
	s.Bus, s.BusURL, s.BusRetention = bus.Kind, bus.URL, bus.Retention
	s.Payments = r.Payments()
	s.Selling = r.Selling()
	s.Entitlements = r.Entitlements()

	s.HTTPAddr, s.GRPCAddr = r.StrOr("PAYMENT_HTTP_ADDR", ":8080"), r.StrOr("PAYMENT_GRPC_ADDR", ":9090")
	r.Bool("PAYMENT_LEGACY_429_FOR_LIMITS", &s.Legacy429)
	s.ShutdownTimeout = 30 * time.Second
	r.PositiveDuration("PAYMENT_SHUTDOWN_TIMEOUT", &s.ShutdownTimeout, "a stop that waits for nothing drops the calls in flight")
	s.LogLevel = r.LogLevel()
	s.RequestTimeout = defaultRequestTimeout
	r.PositiveDuration("PAYMENT_REQUEST_TIMEOUT", &s.RequestTimeout, "a request with no time to run is refused before it starts")
	if s.RequestTimeout >= httpWriteTimeout { // an unusable value leaves the default, so this reports only a value that parsed
		r.Fail("PAYMENT_REQUEST_TIMEOUT", fmt.Sprintf("must be shorter than the %v the server allows itself to write a response in: a request that outlives it is cut off and not answered", httpWriteTimeout))
	}

	s.TLSCertFile, s.TLSKeyFile, s.TLSClientCAFile = r.Str("PAYMENT_TLS_CERT_FILE"), r.Str("PAYMENT_TLS_KEY_FILE"), r.Str("PAYMENT_TLS_CLIENT_CA_FILE")
	s.Auth = r.StrOr("PAYMENT_SERVICE_AUTH", authBearer)
	switch s.Auth {
	case authBearer:
		a, err := auth.ParseTokens(r.Str("PAYMENT_SERVICE_TOKENS"))
		if err != nil {
			r.Fail("PAYMENT_SERVICE_TOKENS", err.Error())
		}
		s.Authenticator = a
	case authMTLS:
		for _, f := range []struct{ name, v string }{
			{"PAYMENT_TLS_CERT_FILE", s.TLSCertFile}, {"PAYMENT_TLS_KEY_FILE", s.TLSKeyFile}, {"PAYMENT_TLS_CLIENT_CA_FILE", s.TLSClientCAFile},
		} {
			if f.v == "" {
				r.Fail(f.name, "is required with PAYMENT_SERVICE_AUTH=mtls: the listener verifies the caller's certificate chain against the private CA")
			}
		}
		a, err := auth.ParseCertSubjects(r.Str("PAYMENT_SERVICE_CERTS"))
		if err != nil {
			r.Fail("PAYMENT_SERVICE_CERTS", err.Error())
		}
		s.Authenticator = a
	case authNone:
		// "none" accepts any caller, which is only safe where only the host itself can reach the listener.
		for _, l := range []struct{ name, addr string }{{"PAYMENT_HTTP_ADDR", s.HTTPAddr}, {"PAYMENT_GRPC_ADDR", s.GRPCAddr}} {
			if !envcfg.Private(l.addr) {
				r.Fail("PAYMENT_SERVICE_AUTH", fmt.Sprintf("none is permitted only where nothing else can reach the service — a loopback address or a unix socket — and %s=%q is neither", l.name, l.addr))
			}
		}
		s.Authenticator = auth.None()
	default:
		r.Fail("PAYMENT_SERVICE_AUTH", fmt.Sprintf("must be none, mtls or bearer, got %q", s.Auth))
	}
	if s.Auth != authMTLS && (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		missing := "PAYMENT_TLS_KEY_FILE"
		if s.TLSCertFile == "" {
			missing = "PAYMENT_TLS_CERT_FILE"
		}
		r.Fail(missing, "is required with its pair: a certificate needs its key and a key its certificate")
	}

	s.MetricsAddr = r.Str("PAYMENT_METRICS_ADDR")
	checkMetricsAddr(r, s)

	if err := r.Err(); err != nil {
		return settings{}, err
	}
	s.Engine = cfg.WithDefaults()
	return s, nil
}

// checkMetricsAddr holds /metrics to the rule PAYMENT_SERVICE_AUTH=none follows. /metrics carries no credential — a scraper
// does not hold a service token — and it names every realm and its traffic, so on a listener others can reach it is public.
// It may therefore share the API's listener only while that listener is private (loopback or a unix socket); otherwise it has
// a listener of its own, and a deployment that did not say where gets a refusal, not a silent absence of metrics (an operator
// writes alerts against them) and not a public one.
func checkMetricsAddr(r *envcfg.Reader, s settings) {
	const name = "PAYMENT_METRICS_ADDR"
	if s.MetricsAddr == "" {
		if !envcfg.Private(s.HTTPAddr) {
			r.Fail(name, fmt.Sprintf("is required while PAYMENT_HTTP_ADDR=%q can be reached by others: /metrics carries no credential, so it is served on a listener of its own (for example :9092, kept off the public network) or the API listener stays on loopback or a unix socket", s.HTTPAddr))
		}
		return
	}
	if err := validListenAddr(s.MetricsAddr); err != nil {
		r.Fail(name, fmt.Sprintf("%v, got %q", err, s.MetricsAddr))
		return
	}
	for _, other := range []struct{ name, addr string }{{"PAYMENT_HTTP_ADDR", s.HTTPAddr}, {"PAYMENT_GRPC_ADDR", s.GRPCAddr}} {
		if s.MetricsAddr == other.addr && !strings.HasSuffix(s.MetricsAddr, ":0") { // port 0 is "any free port": two of them do not collide
			r.Fail(name, fmt.Sprintf("must differ from %s: the metrics are served on a listener of their own", other.name))
		}
	}
}

// validListenAddr accepts "unix:/path" or host:port with a numeric port from 0 to 65535 (an empty host is every interface).
func validListenAddr(addr string) error {
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		if path == "" {
			return fmt.Errorf("a unix socket needs a path")
		}
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port or unix:/path (%v)", err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("the port must be a number from 0 to 65535")
	}
	return nil
}
