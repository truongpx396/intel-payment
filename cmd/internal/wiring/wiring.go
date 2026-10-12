// Package wiring connects the engine to its stores for the binaries under cmd/, and checks what must be true
// before any of them touches money: the schema is at head, and the books were written under the shard count this
// process is configured with. paymentd and payment-worker open the same stores the same way.
package wiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	goredis "github.com/redis/go-redis/v9"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/natsjetstream"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/llmtoken"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/seat"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/storagebyte"
	pricingtable "github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
	"github.com/truongpx396/intel-payment/migrate"
)

// Stores are the engine's connections, and the function that closes them all.
type Stores struct {
	Pool       *pgxpool.Pool
	DB         *sql.DB // the same pool, for the migrator
	Migrations []migrate.Migration
	Books      *pgadapter.Books
	Journal    *pgadapter.Journal
	Config     *pgadapter.ConfigStore
	Redis      goredis.UniversalClient
	Hot        *hotredis.Store

	closers []func()
}

// Close releases everything Open acquired, newest first.
func (s *Stores) Close() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

// Defer registers something to close with the stores.
func (s *Stores) Defer(f func()) { s.closers = append(s.closers, f) }

// Open connects Postgres and Redis. It refuses a schema that is not at head and a shard count that disagrees
// with the books. maxConns sizes the Postgres pool (0 leaves pgx's default): a worker holds one connection per
// shard it owns.
func Open(ctx context.Context, cfg metering.Config, maxConns int) (_ *Stores, err error) {
	s := &Stores{}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	dsn := cfg.LedgerDSN
	if maxConns > 0 {
		dsn = WithMaxConns(dsn, maxConns)
	}
	if s.Pool, err = pgadapter.Connect(ctx, dsn); err != nil {
		return nil, fmt.Errorf("the ledger database is not reachable (PAYMENT_LEDGER_DSN): %w", err)
	}
	s.Defer(s.Pool.Close)

	if s.Migrations, err = migrate.Embedded(); err != nil {
		return nil, err
	}
	s.DB = stdlib.OpenDBFromPool(s.Pool)
	s.Defer(func() { _ = s.DB.Close() })
	if st, cerr := migrate.Check(ctx, s.DB, s.Migrations); cerr != nil {
		return nil, fmt.Errorf("the schema is not at head (at %04d, head is %04d): run payment-migrate up before starting: %w", st.Current, st.Head, cerr)
	}
	s.Books, s.Journal = pgadapter.NewBooks(s.Pool), pgadapter.NewJournal(s.Pool)
	if err = s.Books.EnsureHotConfig(ctx, cfg.Shards); err != nil {
		return nil, err
	}

	if s.Config, err = pgadapter.NewConfigStore(ctx, s.Pool, pgadapter.ConfigOptions{}); err != nil {
		return nil, fmt.Errorf("configuration cache: %w", err)
	}
	s.Defer(s.Config.Close)

	if s.Redis, err = hotredis.Connect(cfg.BalanceRedisURL); err != nil {
		return nil, errors.New("PAYMENT_BALANCE_REDIS_URL is not a redis:// or redis+cluster:// URL")
	}
	s.Defer(func() { _ = s.Redis.Close() })
	if s.Hot, err = hotredis.New(s.Redis, hotredis.Options{Config: cfg, Pools: s.Config, Limits: s.Config, Clock: system.Clock{}}); err != nil {
		return nil, err
	}
	return s, nil
}

// WithMaxConns sets pgx's pool_max_conns on a DSN that does not name one, in either of PostgreSQL's two forms.
func WithMaxConns(dsn string, n int) string {
	if strings.Contains(dsn, "pool_max_conns") {
		return dsn
	}
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn // pgx will refuse it, naming nothing secret
		}
		q := u.Query()
		q.Set("pool_max_conns", fmt.Sprint(n))
		u.RawQuery = q.Encode()
		return u.String()
	}
	return fmt.Sprintf("%s pool_max_conns=%d", dsn, n)
}

// Pricers is the registry of the four reference pricers, each capped at the largest single operation.
func Pricers(limit domain.Credits) ports.PricerRegistry {
	return ports.PricerRegistry{
		"table": pricingtable.New(limit), llmtoken.Name: llmtoken.New(limit), seat.Name: seat.New(limit), storagebyte.Name: storagebyte.New(limit),
	}
}

// NewBus builds the bus warn/blocked events go to, and the function that closes it.
func NewBus(ctx context.Context, b envcfg.Bus, cfg metering.Config, rdb goredis.UniversalClient) (ports.Bus, func(), error) {
	switch b.Kind {
	case envcfg.BusJetStream:
		nc, err := nats.Connect(b.URL, nats.Name("intel-payment"), nats.Timeout(5*time.Second))
		if err != nil {
			return nil, nil, errors.New("PAYMENT_BUS_URL: the NATS server is not reachable")
		}
		bus, err := natsjetstream.NewBus(ctx, nc, natsjetstream.BusOptions{
			Clock: system.Clock{}, Prefix: cfg.SubjectPrefix, Retention: b.Retention, AckWait: cfg.AckWait, MaxAttempts: cfg.MaxAttempts,
		})
		if err != nil {
			nc.Close()
			return nil, nil, err
		}
		return bus, nc.Close, nil
	default:
		host, _ := os.Hostname()
		bus, err := redisstreams.NewBus(rdb, redisstreams.BusOptions{
			Clock: system.Clock{}, Prefix: cfg.SubjectPrefix, Consumer: fmt.Sprintf("%s-%d", host, os.Getpid()),
			AckWait: cfg.AckWait, MaxAttempts: cfg.MaxAttempts,
		})
		return bus, func() {}, err
	}
}
