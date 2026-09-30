package app

import (
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Deps are the driven ports the core needs. The host supplies them in cmd/. The only
// product-specific ones are the Pricers it registers (and the cards it publishes).
type Deps struct {
	Pricers   ports.PricerRegistry // MUST contain "table"; add compiled pricers by name
	Balance   ports.BalanceStore   // redis adapter: the hot functions
	Books     ports.BookStore      // postgres: read for a rehydrate; written only by the LedgerWriter
	Limits    ports.LimitStore     // per-realm Limit rows — CONFIG DATA
	Pools     ports.PoolStore      // per-realm pools — CONFIG DATA
	RateCards ports.RateCardStore  // versioned, immutable cards — CONFIG DATA
	Journal   ports.JournalStore   // Settlement=journal and the outage fallback
	Bus       ports.Bus            // warn/blocked events (redisstreams by default)
	Quotas    ports.QuotaSource    // optional: sizes limits that name a max_entitlement
	Archive   ports.UsageArchive   // required when LedgerGranularity=rollup
	Stream    ports.IntentStream   // the writer's view of the outbox (redisstreams); unused by the Meter
	Clock     ports.Clock          // required: the core reads no clock. adapters/driven/system.Clock{}
	IDs       ports.IDSource       // required by the writer; adapters/driven/system.IDs
	Metrics   ports.Metrics        // default: no-op. Invariant 15 needs a real one in production
}

// validate checks everything the Meter needs. The writer checks its own extras (IDs, Archive).
func (d Deps) validate(cfg metering.Config) error {
	var errs []error
	if d.Balance == nil || d.Books == nil || d.Bus == nil || d.Journal == nil {
		errs = append(errs, errors.New("the Balance, Books, Bus and Journal ports are required"))
	}
	if d.Pricers["table"] == nil {
		errs = append(errs, errors.New("the `table` pricer must be registered"))
	}
	if d.Limits == nil || d.RateCards == nil || d.Pools == nil {
		errs = append(errs, errors.New("the Limits, Pools and RateCards ports are required — they are config data, not code"))
	}
	if d.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: the core reads no wall clock (system.Clock{} in the reference wiring)"))
	}
	if cfg.LedgerGranularity == metering.GranularityRollup && d.Archive == nil {
		errs = append(errs, errors.New("rollup granularity needs a UsageArchive, or per-event detail is lost"))
	}
	return errors.Join(errs...)
}

// meter is the ports.Meter implementation.
type meter struct {
	cfg  metering.Config
	d    Deps
	met  ports.Metrics
	reh  *rehydrator
	warn *coalescer
}

var _ ports.Meter = (*meter)(nil)

// New validates config, assembles the core, and returns it as the ports.Meter interface.
func New(cfg metering.Config, d Deps) (ports.Meter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metering config: %w", err)
	}
	cfg = cfg.WithDefaults()
	if err := d.validate(cfg); err != nil {
		return nil, fmt.Errorf("metering: %w", err)
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &meter{cfg: cfg, d: d, met: met, reh: newRehydrator(cfg, d, met), warn: newCoalescer(d.Clock, coalesceWindow)}, nil
}

type nopMetrics struct{}

func (nopMetrics) Count(string, int64, ports.Labels)     {}
func (nopMetrics) Gauge(string, int64, ports.Labels)     {}
func (nopMetrics) Observe(string, float64, ports.Labels) {}

func realmLabel(s domain.Scope) ports.Labels { return ports.Labels{"realm": string(s.Realm.Or())} }
