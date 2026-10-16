// Package envcfg reads the PAYMENT_* environment into typed settings for the binaries under cmd/. It is the one
// place a variable name maps to a field, so paymentd and payment-worker cannot disagree about what one means, and
// the one place a refusal is worded: every problem at once, each naming the variable to fix, none echoing a
// credential (a refusal is logged, shipped and pasted into tickets).
//
// The environment is read through the lookup a caller passes in — only cmd/ main functions touch os.Getenv.
package envcfg

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
	"github.com/truongpx396/intel-payment/metering/domain"
)

// Reader reads typed values and collects what is wrong with them.
type Reader struct {
	getenv func(string) string
	errs   []error
}

// NewReader reads through getenv.
func NewReader(getenv func(string) string) *Reader { return &Reader{getenv: getenv} }

// Err is every problem found, joined, or nil.
func (r *Reader) Err() error {
	if len(r.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n%w", errors.Join(r.errs...))
}

// Failed reports whether anything is wrong so far.
func (r *Reader) Failed() bool { return len(r.errs) > 0 }

// Fail records that a variable is wrong. msg completes the sentence "<name> …".
func (r *Reader) Fail(name, msg string) { r.errs = append(r.errs, fmt.Errorf("%s %s", name, msg)) }

// Add records a problem that is not about one variable.
func (r *Reader) Add(err error) { r.errs = append(r.errs, err) }

// Str is the variable, trimmed; "" when unset.
func (r *Reader) Str(name string) string { return strings.TrimSpace(r.getenv(name)) }

// StrOr is the variable, or def when unset.
func (r *Reader) StrOr(name, def string) string {
	if v := r.Str(name); v != "" {
		return v
	}
	return def
}

// Duration reads a non-negative duration into dst, leaving it alone when the variable is unset.
func (r *Reader) Duration(name string, dst *time.Duration) {
	v := r.Str(name)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		r.Fail(name, fmt.Sprintf("must be a duration like 250ms or 5m, got %q", v))
	case d < 0:
		r.Fail(name, fmt.Sprintf("must not be negative, got %q", v))
	default:
		*dst = d
	}
}

// PositiveDuration is Duration for a setting where zero would be a bug: an interval that spins, a timeout that
// waits for nothing.
func (r *Reader) PositiveDuration(name string, dst *time.Duration, why string) {
	before := len(r.errs)
	r.Duration(name, dst)
	if len(r.errs) == before && r.Str(name) != "" && *dst == 0 {
		r.Fail(name, "must be positive: "+why)
	}
}

// Int64 reads a whole number into dst.
func (r *Reader) Int64(name string, dst *int64) {
	v := r.Str(name)
	if v == "" {
		return
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		r.Fail(name, fmt.Sprintf("must be a whole number, got %q", v))
		return
	}
	*dst = n
}

// Int reads a whole number into dst.
func (r *Reader) Int(name string, dst *int) {
	var n int64
	before := len(r.errs)
	r.Int64(name, &n)
	if len(r.errs) == before && r.Str(name) != "" {
		*dst = int(n)
	}
}

// Credits reads a whole number of credits into dst.
func (r *Reader) Credits(name string, dst *domain.Credits) {
	var n int64
	before := len(r.errs)
	r.Int64(name, &n)
	if len(r.errs) == before && r.Str(name) != "" {
		*dst = domain.Credits(n)
	}
}

// Bool reads true or false into dst.
func (r *Reader) Bool(name string, dst *bool) {
	v := r.Str(name)
	if v == "" {
		return
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.Fail(name, fmt.Sprintf("must be true or false, got %q", v))
		return
	}
	*dst = b
}

// LogLevel reads PAYMENT_LOG_LEVEL (default info).
func (r *Reader) LogLevel() slog.Level {
	var l slog.Level
	if v := r.Str("PAYMENT_LOG_LEVEL"); v != "" {
		if err := l.UnmarshalText([]byte(v)); err != nil {
			r.Fail("PAYMENT_LOG_LEVEL", fmt.Sprintf("must be debug, info, warn or error, got %q", v))
		}
	}
	return l
}

// Engine reads the settings of the metering engine, checks them as metering.Config does — with the engine's field
// names put back into the operator's variables — and checks the shape of both store URLs without dialling them.
// The config returned has no defaults applied; metering.Config.WithDefaults does that.
func (r *Reader) Engine() metering.Config {
	var cfg metering.Config
	cfg.Realm = domain.Realm(r.Str("PAYMENT_REALM"))
	cfg.SubjectPrefix = r.Str("PAYMENT_SUBJECT_PREFIX")
	cfg.BalanceRedisURL = r.Str("PAYMENT_BALANCE_REDIS_URL")
	cfg.LedgerDSN = r.Str("PAYMENT_LEDGER_DSN")
	r.Int("PAYMENT_SHARDS", &cfg.Shards)
	cfg.Settlement = metering.SettlementDurability(r.Str("PAYMENT_SETTLEMENT"))
	cfg.HotAckWait = metering.HotAckWait(r.Str("PAYMENT_HOT_ACK_WAIT"))
	cfg.LedgerGranularity = metering.LedgerGranularity(r.Str("PAYMENT_LEDGER_GRANULARITY"))
	r.Int("PAYMENT_DRAIN_BATCH", &cfg.DrainBatch)
	r.Duration("PAYMENT_HOT_IDEM_TTL", &cfg.HotIdemTTL)
	r.Duration("PAYMENT_USAGE_IDEM_WINDOW", &cfg.UsageIdemWindow)
	r.Duration("PAYMENT_IDEM_SAFETY_MARGIN", &cfg.IdemSafetyMargin)
	r.Duration("PAYMENT_MAX_CLOCK_SKEW", &cfg.MaxClockSkew)
	r.Duration("PAYMENT_RECONCILE_INTERVAL", &cfg.ReconcileInterval)
	r.Duration("PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", &cfg.ReconcileFullSweepInterval)
	r.Credits("PAYMENT_RECONCILE_TOLERANCE", &cfg.ReconcileTolerance)
	r.Duration("PAYMENT_GAP_TIMEOUT", &cfg.GapTimeout)
	r.Duration("PAYMENT_TRANSFER_TRANSIT_ALARM", &cfg.TransferTransitAlarm)
	r.Duration("PAYMENT_ADMIT_TIMEOUT", &cfg.AdmitTimeout)
	r.Duration("PAYMENT_RECORD_TIMEOUT", &cfg.RecordTimeout)
	cfg.AdmitFail = metering.AdmitFailPolicy(r.Str("PAYMENT_ADMIT_FAIL_POLICY"))
	r.Bool("PAYMENT_REQUIRE_MAX_COST", &cfg.RequireMaxCost)
	r.Credits("PAYMENT_MAX_OPERATION_AMOUNT", &cfg.MaxOperationAmount)
	cfg.NegativeBalance = metering.NegativeBalancePolicy(r.Str("PAYMENT_NEGATIVE_BALANCE_POLICY"))
	cfg.CreditExpiry = metering.CreditExpiry(r.Str("PAYMENT_CREDIT_EXPIRY"))
	r.Int64("PAYMENT_BUS_MAX_LEN", &cfg.BusMaxLen)
	r.Duration("PAYMENT_BUS_ACK_WAIT", &cfg.AckWait)
	r.Int("PAYMENT_BUS_MAX_ATTEMPTS", &cfg.MaxAttempts)

	if err := cfg.Validate(); err != nil {
		r.Add(errors.New(engineNames.Replace(err.Error())))
	}
	// A URL is checked for its shape without being dialled, and the parser's complaint is dropped: it quotes the URL.
	if cfg.BalanceRedisURL != "" {
		if c, err := hotredis.Connect(cfg.BalanceRedisURL); err != nil {
			r.Fail("PAYMENT_BALANCE_REDIS_URL", "is not a redis:// or redis+cluster:// URL")
		} else {
			_ = c.Close()
		}
	}
	if cfg.LedgerDSN != "" {
		if _, err := pgconn.ParseConfig(cfg.LedgerDSN); err != nil {
			r.Fail("PAYMENT_LEDGER_DSN", "is not a PostgreSQL connection string")
		}
	}
	return cfg
}

// engineNames puts the engine's field names, which is what Config.Validate speaks, into the variables an operator sets.
var engineNames = strings.NewReplacer(
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

// The buses PAYMENT_BUS names.
const (
	BusStreams   = "redis_streams"
	BusJetStream = "nats_jetstream"
)

// Bus is the choice of bus and where it is.
type Bus struct {
	Kind      string
	URL       string
	Retention time.Duration
}

// Bus reads PAYMENT_BUS, PAYMENT_BUS_URL and PAYMENT_BUS_RETENTION.
func (r *Reader) Bus() Bus {
	b := Bus{Kind: r.Str("PAYMENT_BUS"), URL: r.Str("PAYMENT_BUS_URL"), Retention: 7 * 24 * time.Hour}
	switch b.Kind {
	case "":
		b.Kind = BusStreams
	case BusStreams, BusJetStream:
	default:
		r.Fail("PAYMENT_BUS", fmt.Sprintf("must be %s or %s, got %q", BusStreams, BusJetStream, b.Kind))
	}
	r.Duration("PAYMENT_BUS_RETENTION", &b.Retention)
	if b.Kind == BusJetStream {
		if b.URL == "" {
			r.Fail("PAYMENT_BUS_URL", "is required for "+BusJetStream)
		} else if u, err := url.Parse(b.URL); err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Host == "" {
			r.Fail("PAYMENT_BUS_URL", "is not a nats:// or tls:// URL")
		}
	}
	return b
}

// Private reports whether addr is reachable only from the host: a unix socket ("unix:/path") or a loopback address.
func Private(addr string) bool {
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

// Payments is what the payment layer is told: the binaries share one reader, one set of names and one set of refusals.
type Payments struct {
	WebhookTolerance time.Duration // PAYMENT_WEBHOOK_TOLERANCE: how far a signature's timestamp may be from when the delivery arrived
	DunningGrace     time.Duration // PAYMENT_DUNNING_GRACE_DAYS: how long entitlements hold after a failed payment
	WebhookTick      time.Duration // PAYMENT_WEBHOOK_TICK_INTERVAL: how often the webhook inbox is worked
	EventRetention   time.Duration // PAYMENT_PAYMENT_EVENT_RETENTION: how long a processed webhook event is kept
}

// The bounds of Payments, each with the reason it is there.
const (
	maxWebhookTolerance = time.Hour          // a wider window makes a captured delivery replayable for that long
	maxWebhookTick      = time.Minute        // a customer who paid waits up to this long for credits
	minEventRetention   = 7 * 24 * time.Hour // a provider redelivers for days; the inbox is what makes a redelivery a no-op
	maxGraceDays        = 365
)

// Payments reads the payment layer's settings. An unusable value is reported, never replaced by a default nobody chose.
func (r *Reader) Payments() Payments {
	p := Payments{WebhookTolerance: 5 * time.Minute, DunningGrace: 7 * 24 * time.Hour, WebhookTick: 5 * time.Second, EventRetention: 90 * 24 * time.Hour}

	before := len(r.errs)
	r.PositiveDuration("PAYMENT_WEBHOOK_TOLERANCE", &p.WebhookTolerance, "a tolerance of nothing rejects every delivery")
	if len(r.errs) == before && p.WebhookTolerance > maxWebhookTolerance {
		r.Fail("PAYMENT_WEBHOOK_TOLERANCE", "must be at most 1h: a wider window makes a captured delivery replayable for that long")
	}

	if v := r.Str("PAYMENT_DUNNING_GRACE_DAYS"); v != "" {
		var days int
		before = len(r.errs)
		r.Int("PAYMENT_DUNNING_GRACE_DAYS", &days)
		switch {
		case len(r.errs) > before:
		case days < 1 || days > maxGraceDays:
			r.Fail("PAYMENT_DUNNING_GRACE_DAYS", fmt.Sprintf("must be a whole number of days from 1 to %d, got %q", maxGraceDays, v))
		default:
			p.DunningGrace = time.Duration(days) * 24 * time.Hour
		}
	}

	before = len(r.errs)
	r.PositiveDuration("PAYMENT_WEBHOOK_TICK_INTERVAL", &p.WebhookTick, "an interval of nothing spins on an idle inbox")
	if len(r.errs) == before && p.WebhookTick > maxWebhookTick {
		r.Fail("PAYMENT_WEBHOOK_TICK_INTERVAL", "must be at most 1m: a customer who paid would wait that long for credits")
	}

	if v := r.Str("PAYMENT_PAYMENT_EVENT_RETENTION"); v != "" {
		d, err := parseRetention(v)
		switch {
		case err != nil:
			r.Fail("PAYMENT_PAYMENT_EVENT_RETENTION", fmt.Sprintf("must be a number of days like 90d or a duration like 2160h, got %q", v))
		case d < minEventRetention:
			r.Fail("PAYMENT_PAYMENT_EVENT_RETENTION", "must be at least 7d: it MUST exceed the longest provider replay window, and a provider redelivers for days")
		default:
			p.EventRetention = d
		}
	}
	return p
}

// parseRetention reads "90d" (whole days) or any Go duration.
func parseRetention(v string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 16)
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(v)
}
