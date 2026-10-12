package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/truongpx396/intel-payment/metering"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// settings is everything the process is told, parsed once at start from PAYMENT_* variables (docs/configuration.md).
// Nothing past this file reads the environment.
type settings struct {
	Engine metering.Config // defaults applied

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

	busStreams   = "redis_streams"
	busJetStream = "nats_jetstream"
)

// parseSettings reads and checks the whole environment. Every problem is reported at once, each naming the
// variable to fix, and none echoes a credential: a refusal is logged, shipped and pasted into tickets.
func parseSettings(getenv func(string) string) (settings, error) {
	r := &reader{getenv: getenv}
	var cfg metering.Config
	var s settings

	cfg.Realm = domain.Realm(r.str("PAYMENT_REALM"))
	cfg.SubjectPrefix = r.str("PAYMENT_SUBJECT_PREFIX")
	cfg.BalanceRedisURL = r.str("PAYMENT_BALANCE_REDIS_URL")
	cfg.LedgerDSN = r.str("PAYMENT_LEDGER_DSN")
	r.integer("PAYMENT_SHARDS", &cfg.Shards)
	cfg.Settlement = metering.SettlementDurability(r.str("PAYMENT_SETTLEMENT"))
	cfg.HotAckWait = metering.HotAckWait(r.str("PAYMENT_HOT_ACK_WAIT"))
	cfg.LedgerGranularity = metering.LedgerGranularity(r.str("PAYMENT_LEDGER_GRANULARITY"))
	r.integer("PAYMENT_DRAIN_BATCH", &cfg.DrainBatch)
	r.duration("PAYMENT_HOT_IDEM_TTL", &cfg.HotIdemTTL)
	r.duration("PAYMENT_USAGE_IDEM_WINDOW", &cfg.UsageIdemWindow)
	r.duration("PAYMENT_IDEM_SAFETY_MARGIN", &cfg.IdemSafetyMargin)
	r.duration("PAYMENT_MAX_CLOCK_SKEW", &cfg.MaxClockSkew)
	r.duration("PAYMENT_RECONCILE_INTERVAL", &cfg.ReconcileInterval)
	r.duration("PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", &cfg.ReconcileFullSweepInterval)
	r.credits("PAYMENT_RECONCILE_TOLERANCE", &cfg.ReconcileTolerance)
	r.duration("PAYMENT_GAP_TIMEOUT", &cfg.GapTimeout)
	r.duration("PAYMENT_TRANSFER_TRANSIT_ALARM", &cfg.TransferTransitAlarm)
	r.duration("PAYMENT_ADMIT_TIMEOUT", &cfg.AdmitTimeout)
	r.duration("PAYMENT_RECORD_TIMEOUT", &cfg.RecordTimeout)
	cfg.AdmitFail = metering.AdmitFailPolicy(r.str("PAYMENT_ADMIT_FAIL_POLICY"))
	r.boolean("PAYMENT_REQUIRE_MAX_COST", &cfg.RequireMaxCost)
	r.credits("PAYMENT_MAX_OPERATION_AMOUNT", &cfg.MaxOperationAmount)
	cfg.NegativeBalance = metering.NegativeBalancePolicy(r.str("PAYMENT_NEGATIVE_BALANCE_POLICY"))
	cfg.CreditExpiry = metering.CreditExpiry(r.str("PAYMENT_CREDIT_EXPIRY"))
	r.int64("PAYMENT_BUS_MAX_LEN", &cfg.BusMaxLen)
	r.duration("PAYMENT_BUS_ACK_WAIT", &cfg.AckWait)
	r.integer("PAYMENT_BUS_MAX_ATTEMPTS", &cfg.MaxAttempts)

	// The engine's own checks, with their field names put back into the operator's vocabulary.
	if err := cfg.Validate(); err != nil {
		r.add(errors.New(envNames.Replace(err.Error())))
	}
	// A URL is checked for its shape without being dialled, and the parser's complaint is dropped: it quotes the URL.
	if cfg.BalanceRedisURL != "" {
		if c, err := hotredis.Connect(cfg.BalanceRedisURL); err != nil {
			r.fail("PAYMENT_BALANCE_REDIS_URL", "is not a redis:// or redis+cluster:// URL")
		} else {
			_ = c.Close()
		}
	}
	if cfg.LedgerDSN != "" {
		if _, err := pgconn.ParseConfig(cfg.LedgerDSN); err != nil {
			r.fail("PAYMENT_LEDGER_DSN", "is not a PostgreSQL connection string")
		}
	}

	s.Bus = r.str("PAYMENT_BUS")
	switch s.Bus {
	case "":
		s.Bus = busStreams
	case busStreams, busJetStream:
	default:
		r.fail("PAYMENT_BUS", fmt.Sprintf("must be %s or %s, got %q", busStreams, busJetStream, s.Bus))
	}
	s.BusURL = r.str("PAYMENT_BUS_URL")
	s.BusRetention = 7 * 24 * time.Hour
	r.duration("PAYMENT_BUS_RETENTION", &s.BusRetention)
	if s.Bus == busJetStream {
		if s.BusURL == "" {
			r.fail("PAYMENT_BUS_URL", "is required for "+busJetStream)
		} else if u, err := url.Parse(s.BusURL); err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Host == "" {
			r.fail("PAYMENT_BUS_URL", "is not a nats:// or tls:// URL")
		}
	}

	s.HTTPAddr, s.GRPCAddr = r.strOr("PAYMENT_HTTP_ADDR", ":8080"), r.strOr("PAYMENT_GRPC_ADDR", ":9090")
	r.boolean("PAYMENT_LEGACY_429_FOR_LIMITS", &s.Legacy429)
	s.ShutdownTimeout = 30 * time.Second
	r.duration("PAYMENT_SHUTDOWN_TIMEOUT", &s.ShutdownTimeout)
	if s.ShutdownTimeout == 0 {
		r.fail("PAYMENT_SHUTDOWN_TIMEOUT", "must be positive: a stop that waits for nothing drops the calls in flight")
	}
	if v := r.str("PAYMENT_LOG_LEVEL"); v != "" {
		if err := s.LogLevel.UnmarshalText([]byte(v)); err != nil {
			r.fail("PAYMENT_LOG_LEVEL", fmt.Sprintf("must be debug, info, warn or error, got %q", v))
		}
	}

	s.TLSCertFile, s.TLSKeyFile, s.TLSClientCAFile = r.str("PAYMENT_TLS_CERT_FILE"), r.str("PAYMENT_TLS_KEY_FILE"), r.str("PAYMENT_TLS_CLIENT_CA_FILE")
	s.Auth = r.strOr("PAYMENT_SERVICE_AUTH", authBearer)
	switch s.Auth {
	case authBearer:
		a, err := auth.ParseTokens(r.str("PAYMENT_SERVICE_TOKENS"))
		if err != nil {
			r.fail("PAYMENT_SERVICE_TOKENS", err.Error())
		}
		s.Authenticator = a
	case authMTLS:
		for _, f := range []struct{ name, v string }{
			{"PAYMENT_TLS_CERT_FILE", s.TLSCertFile}, {"PAYMENT_TLS_KEY_FILE", s.TLSKeyFile}, {"PAYMENT_TLS_CLIENT_CA_FILE", s.TLSClientCAFile},
		} {
			if f.v == "" {
				r.fail(f.name, "is required with PAYMENT_SERVICE_AUTH=mtls: the listener verifies the caller's certificate chain against the private CA")
			}
		}
		a, err := auth.ParseCertSubjects(r.str("PAYMENT_SERVICE_CERTS"))
		if err != nil {
			r.fail("PAYMENT_SERVICE_CERTS", err.Error())
		}
		s.Authenticator = a
	case authNone:
		// "none" accepts any caller, which is only safe where only the host itself can reach the listener.
		for _, l := range []struct{ name, addr string }{{"PAYMENT_HTTP_ADDR", s.HTTPAddr}, {"PAYMENT_GRPC_ADDR", s.GRPCAddr}} {
			if !private(l.addr) {
				r.fail("PAYMENT_SERVICE_AUTH", fmt.Sprintf("none is permitted only where nothing else can reach the service — a loopback address or a unix socket — and %s=%q is neither", l.name, l.addr))
			}
		}
		s.Authenticator = auth.None()
	default:
		r.fail("PAYMENT_SERVICE_AUTH", fmt.Sprintf("must be none, mtls or bearer, got %q", s.Auth))
	}

	if s.Auth != authMTLS && (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		missing := "PAYMENT_TLS_KEY_FILE"
		if s.TLSCertFile == "" {
			missing = "PAYMENT_TLS_CERT_FILE"
		}
		r.fail(missing, "is required with its pair: a certificate needs its key and a key its certificate")
	}

	if len(r.errs) > 0 {
		return settings{}, fmt.Errorf("invalid configuration:\n%w", errors.Join(r.errs...))
	}
	s.Engine = cfg.WithDefaults()
	return s, nil
}

// envNames puts the engine's field names, which is what Config.Validate speaks, into the variables an operator sets.
var envNames = strings.NewReplacer(
	"BalanceRedisURL", "PAYMENT_BALANCE_REDIS_URL",
	"LedgerDSN", "PAYMENT_LEDGER_DSN",
	"realm ", "PAYMENT_REALM ",
	"shards must be", "PAYMENT_SHARDS must be",
	"Settlement", "PAYMENT_SETTLEMENT",
	"HotAckWait", "PAYMENT_HOT_ACK_WAIT",
	"LedgerGranularity", "PAYMENT_LEDGER_GRANULARITY",
	"AdmitFail", "PAYMENT_ADMIT_FAIL_POLICY",
	"NegativeBalance", "PAYMENT_NEGATIVE_BALANCE_POLICY",
	"CreditExpiry", "PAYMENT_CREDIT_EXPIRY",
	"DrainBatch", "PAYMENT_DRAIN_BATCH",
	"HotIdemTTL", "PAYMENT_HOT_IDEM_TTL",
	"UsageIdemWindow", "PAYMENT_USAGE_IDEM_WINDOW",
	"IdemSafetyMargin", "PAYMENT_IDEM_SAFETY_MARGIN",
	"ReconcileTolerance", "PAYMENT_RECONCILE_TOLERANCE",
	"MaxOperationAmount", "PAYMENT_MAX_OPERATION_AMOUNT",
	"BusMaxLen", "PAYMENT_BUS_MAX_LEN",
	"MaxAttempts", "PAYMENT_BUS_MAX_ATTEMPTS",
)

// private reports whether addr is reachable only from the host: a unix socket or a loopback address.
func private(addr string) bool {
	if strings.HasPrefix(addr, "unix:") {
		return true
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// reader reads typed values from the environment and collects what is wrong with them.
type reader struct {
	getenv func(string) string
	errs   []error
}

func (r *reader) add(err error)         { r.errs = append(r.errs, err) }
func (r *reader) fail(name, msg string) { r.errs = append(r.errs, fmt.Errorf("%s %s", name, msg)) }

func (r *reader) str(name string) string { return strings.TrimSpace(r.getenv(name)) }

func (r *reader) strOr(name, def string) string {
	if v := r.str(name); v != "" {
		return v
	}
	return def
}

func (r *reader) duration(name string, dst *time.Duration) {
	v := r.str(name)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		r.fail(name, fmt.Sprintf("must be a duration like 250ms or 5m, got %q", v))
	case d < 0:
		r.fail(name, fmt.Sprintf("must not be negative, got %q", v))
	default:
		*dst = d
	}
}

func (r *reader) int64(name string, dst *int64) {
	v := r.str(name)
	if v == "" {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		r.fail(name, fmt.Sprintf("must be a whole number, got %q", v))
		return
	}
	*dst = n
}

func (r *reader) integer(name string, dst *int) {
	var n int64
	if r.str(name) == "" {
		return
	}
	before := len(r.errs)
	r.int64(name, &n)
	if len(r.errs) == before {
		*dst = int(n)
	}
}

func (r *reader) credits(name string, dst *domain.Credits) {
	var n int64
	if r.str(name) == "" {
		return
	}
	before := len(r.errs)
	r.int64(name, &n)
	if len(r.errs) == before {
		*dst = domain.Credits(n)
	}
}

func (r *reader) boolean(name string, dst *bool) {
	v := r.str(name)
	if v == "" {
		return
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail(name, fmt.Sprintf("must be true or false, got %q", v))
		return
	}
	*dst = b
}
