package metering

import (
	"errors"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// The domain vocabulary a host needs to fill in a Config, re-exported so a host imports one
// package: `metering.Config{Settlement: metering.SettlementJournal}`.
type (
	Realm                 = domain.Realm
	Credits               = domain.Credits
	SettlementDurability  = domain.SettlementDurability
	HotAckWait            = domain.HotAckWait
	LedgerGranularity     = domain.LedgerGranularity
	NegativeBalancePolicy = domain.NegativeBalancePolicy
	CreditExpiry          = domain.CreditExpiry
	AdmitFailPolicy       = domain.AdmitFailPolicy
)

const (
	DefaultRealm = domain.DefaultRealm

	SettlementOutbox  = domain.SettlementOutbox
	SettlementJournal = domain.SettlementJournal

	HotAckNone       = domain.HotAckNone
	HotAckAOFLocal   = domain.HotAckAOFLocal
	HotAckAOFReplica = domain.HotAckAOFReplica

	GranularityEvent  = domain.GranularityEvent
	GranularityRollup = domain.GranularityRollup

	AllowDebt    = domain.AllowDebt
	ClampToZero  = domain.ClampToZero
	BlockAndFlag = domain.BlockAndFlag

	ExpiryOff      = domain.ExpiryOff
	ExpiryLotsFIFO = domain.ExpiryLotsFIFO

	FailClosed = domain.FailClosed
	FailOpen   = domain.FailOpen
)

// Config is the ENTIRE configuration surface of the metering module. The host passes one value in
// at wire time; the core reads no env or global config, so the config travels with the module.
type Config struct {
	Realm Realm // the host product this configuration serves; default DefaultRealm

	// Stores — driven adapters dial these; the core never does.
	BalanceRedisURL string // hot accounts + outbox streams + guards (noeviction + AOF; cluster or single)
	LedgerDSN       string // durable Postgres

	SubjectPrefix string // default "billing" → billing.balance.low.<tag> …

	// Sharding: the Redis Cluster placement unit. Recorded in hot_config; a process whose count
	// disagrees refuses to start. Changed only by a reshard (hot-path-consistency.md §8).
	Shards int // default 256

	// Settlement.
	Settlement        SettlementDurability // default SettlementOutbox
	HotAckWait        HotAckWait           // default HotAckNone
	LedgerGranularity LedgerGranularity    // default GranularityEvent
	DrainBatch        int                  // intents booked per transaction; default 500

	// Idempotency (hot-path-consistency.md §6).
	HotIdemTTL       time.Duration // hot guard lifetime; default 24h; MUST be <= UsageIdemWindow
	UsageIdemWindow  time.Duration // durable usage dedup window; default 72h; > every producer's retry horizon
	IdemSafetyMargin time.Duration // Record refuses OccurredAt older than window − margin; default 1h
	MaxClockSkew     time.Duration // Record refuses OccurredAt this far in the future; default 5m

	// Reconcile and recovery.
	ReconcileInterval          time.Duration // incremental run; default 5m
	ReconcileFullSweepInterval time.Duration // every scope; default 24h
	ReconcileTolerance         Credits       // |drift| at equal seq above this PAGES; default 0
	GapTimeout                 time.Duration // a sequence gap older than this pages; default 2m
	TransferTransitAlarm       time.Duration // a transfer in transit longer than this pages; default 5m

	// Hot-path safety.
	AdmitTimeout       time.Duration   // default 250ms
	RecordTimeout      time.Duration   // default 500ms
	AdmitFail          AdmitFailPolicy // default FailClosed
	RequireMaxCost     bool            // reject an Admit with no MaxCost once every caller supplies one
	MaxOperationAmount Credits         // largest single operation; default 2^50 (hot-path-consistency.md §2)
	DefaultWarnAt      float64         // default 0.8

	// Money policies.
	NegativeBalance NegativeBalancePolicy // default ClampToZero
	CreditExpiry    CreditExpiry          // default ExpiryOff

	// Bus (the writer's stream). Not in the original contract's Config: the outbox's cap and the
	// claim threshold are properties of the deployment, and the contract's own text names them
	// (BusMaxLen in hot-path-consistency.md §2, AckWait in §3).
	BusMaxLen   int64         // outbox length at which the hot functions refuse with BACKPRESSURE; default 1,000,000
	AckWait     time.Duration // an un-acked entry idle this long is reclaimed; default 30s
	MaxAttempts int           // deliveries before an intent is parked; default 5
}

// WithDefaults returns a copy of c with every zero value replaced by its documented default. The
// caller's value is never mutated.
func (c Config) WithDefaults() Config {
	c.withDefaults()
	return c
}

func (c *Config) withDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.Realm == "" {
		c.Realm = DefaultRealm
	}
	if c.SubjectPrefix == "" {
		c.SubjectPrefix = "billing"
	}
	if c.Shards == 0 {
		c.Shards = 256
	}
	if c.Settlement == "" {
		c.Settlement = SettlementOutbox
	}
	if c.HotAckWait == "" {
		c.HotAckWait = HotAckNone
	}
	if c.LedgerGranularity == "" {
		c.LedgerGranularity = GranularityEvent
	}
	if c.DrainBatch == 0 {
		c.DrainBatch = 500
	}
	def(&c.HotIdemTTL, 24*time.Hour)
	def(&c.UsageIdemWindow, 72*time.Hour)
	def(&c.IdemSafetyMargin, time.Hour)
	def(&c.MaxClockSkew, 5*time.Minute)
	def(&c.ReconcileInterval, 5*time.Minute)
	def(&c.ReconcileFullSweepInterval, 24*time.Hour)
	def(&c.GapTimeout, 2*time.Minute)
	def(&c.TransferTransitAlarm, 5*time.Minute)
	def(&c.AdmitTimeout, 250*time.Millisecond)
	def(&c.RecordTimeout, 500*time.Millisecond)
	def(&c.AckWait, 30*time.Second)
	if c.AdmitFail == "" {
		c.AdmitFail = FailClosed
	}
	if c.MaxOperationAmount == 0 {
		c.MaxOperationAmount = domain.MaxOperationAmount
	}
	if c.DefaultWarnAt == 0 {
		c.DefaultWarnAt = 0.8
	}
	if c.NegativeBalance == "" {
		c.NegativeBalance = ClampToZero
	}
	if c.CreditExpiry == "" {
		c.CreditExpiry = ExpiryOff
	}
	if c.BusMaxLen == 0 {
		c.BusMaxLen = 1_000_000
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
}

// Validate checks the EFFECTIVE config (defaults applied to a copy; the caller is not mutated).
func (c Config) Validate() error {
	c.withDefaults()
	var errs []error
	if c.BalanceRedisURL == "" {
		errs = append(errs, errors.New("BalanceRedisURL is required"))
	}
	if c.LedgerDSN == "" {
		errs = append(errs, errors.New("LedgerDSN is required"))
	}
	if err := c.Realm.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Shards < 1 || c.Shards > 16384 {
		errs = append(errs, fmt.Errorf("shards must be in [1,16384], got %d", c.Shards))
	}
	for _, v := range []interface{ Validate() error }{
		c.Settlement, c.HotAckWait, c.LedgerGranularity, c.AdmitFail, c.NegativeBalance, c.CreditExpiry,
	} {
		if err := v.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.DrainBatch < 1 {
		errs = append(errs, fmt.Errorf("DrainBatch must be >= 1, got %d", c.DrainBatch))
	}
	if c.HotIdemTTL > c.UsageIdemWindow {
		errs = append(errs, errors.New("HotIdemTTL must not exceed UsageIdemWindow: a hot guard may never outlive the durable one"))
	}
	if c.IdemSafetyMargin >= c.UsageIdemWindow {
		errs = append(errs, errors.New("IdemSafetyMargin must be smaller than UsageIdemWindow"))
	}
	if c.ReconcileTolerance < 0 {
		errs = append(errs, errors.New("ReconcileTolerance must be >= 0"))
	}
	if c.MaxOperationAmount <= 0 || c.MaxOperationAmount > domain.MaxHotAmount {
		errs = append(errs, errors.New("MaxOperationAmount must be in (0, 2^52]: hot-tier arithmetic is exact only within ±2^53"))
	}
	if c.DefaultWarnAt < 0 || c.DefaultWarnAt > 1 {
		errs = append(errs, fmt.Errorf("DefaultWarnAt must be in [0,1], got %v", c.DefaultWarnAt))
	}
	if c.BusMaxLen < 1 {
		errs = append(errs, fmt.Errorf("BusMaxLen must be >= 1, got %d", c.BusMaxLen))
	}
	if c.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("MaxAttempts must be >= 1, got %d", c.MaxAttempts))
	}
	return errors.Join(errs...)
}
