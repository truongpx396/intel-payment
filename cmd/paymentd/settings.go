package main

import (
	"fmt"
	"log/slog"
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

	Bus          string // redis_streams | nats_jetstream
	BusURL       string
	BusRetention time.Duration

	HTTPAddr, GRPCAddr string
	Auth               string // none | mtls | bearer
	Authenticator      auth.Authenticator
	TLSCertFile        string
	TLSKeyFile         string
	TLSClientCAFile    string
	Legacy429          bool
	LogLevel           slog.Level
	ShutdownTimeout    time.Duration // how long a stop waits for the calls in flight
}

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

	s.HTTPAddr, s.GRPCAddr = r.StrOr("PAYMENT_HTTP_ADDR", ":8080"), r.StrOr("PAYMENT_GRPC_ADDR", ":9090")
	r.Bool("PAYMENT_LEGACY_429_FOR_LIMITS", &s.Legacy429)
	s.ShutdownTimeout = 30 * time.Second
	r.PositiveDuration("PAYMENT_SHUTDOWN_TIMEOUT", &s.ShutdownTimeout, "a stop that waits for nothing drops the calls in flight")
	s.LogLevel = r.LogLevel()

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

	if err := r.Err(); err != nil {
		return settings{}, err
	}
	s.Engine = cfg.WithDefaults()
	return s, nil
}
