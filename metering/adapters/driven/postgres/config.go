package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// ConfigChannel is the Postgres channel the database's audit trigger notifies on, in the same
// transaction as every configuration write (migrations/0004). A change reaches every process at
// once, with no deploy and no TTL wait (invariant 18).
const ConfigChannel = "intelpay_config"

// ConfigStore serves per-realm CONFIGURATION DATA — limits, pools, rate cards — from a cache that
// LISTEN keeps current. It implements ports.LimitStore, ports.PoolStore and ports.RateCardStore.
//
// Correctness never depends on the notification: an entry also expires after TTL, and after any
// listener reconnect everything is dropped, since a notification may have been missed while the
// connection was down.
type ConfigStore struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	now  func() time.Time

	mu     sync.RWMutex
	limits map[domain.Realm]entry[[]domain.Limit]
	pools  map[domain.Realm]entry[[]domain.PoolDef]
	active map[domain.Realm]entry[domain.RateCard]
	cards  map[cardKey]domain.RateCard // versions are immutable: cached forever

	sf     singleflight.Group
	cancel context.CancelFunc
	done   chan struct{}
	// Invalidations counts notifications applied (for tests and metrics).
	invalidations counter
}

type entry[T any] struct {
	v  T
	at time.Time
}

type cardKey struct {
	realm   domain.Realm
	version string
}

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

var (
	_ ports.LimitStore    = (*ConfigStore)(nil)
	_ ports.PoolStore     = (*ConfigStore)(nil)
	_ ports.RateCardStore = (*ConfigStore)(nil)
)

// ConfigOptions tune the cache.
type ConfigOptions struct {
	// TTL is the safety-net lifetime of a cached entry; 0 means 5 minutes.
	TTL time.Duration
	// Now is the clock; nil means the system clock. (Adapters own the real clock.)
	Now func() time.Time
}

// NewConfigStore builds the store and starts its listener. Close stops it.
func NewConfigStore(ctx context.Context, pool *pgxpool.Pool, o ConfigOptions) (*ConfigStore, error) {
	if pool == nil {
		return nil, errors.New("postgres: a pool is required")
	}
	if o.TTL == 0 {
		o.TTL = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &ConfigStore{
		pool: pool, ttl: o.TTL, now: o.Now,
		limits: map[domain.Realm]entry[[]domain.Limit]{}, pools: map[domain.Realm]entry[[]domain.PoolDef]{},
		active: map[domain.Realm]entry[domain.RateCard]{}, cards: map[cardKey]domain.RateCard{},
		done: make(chan struct{}),
	}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	ready := make(chan error, 1)
	go s.listen(lctx, ready)
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-s.done
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		<-s.done
		return nil, ctx.Err()
	}
	return s, nil
}

// Close stops the listener and waits for it.
func (s *ConfigStore) Close() {
	s.cancel()
	<-s.done
}

// Invalidations reports how many notifications have been applied.
func (s *ConfigStore) Invalidations() int { return s.invalidations.get() }

func (s *ConfigStore) fresh(at time.Time) bool { return s.now().Sub(at) < s.ttl }

// ---------------------------------------------------------------- listener --

type notice struct {
	Table string `json:"table"`
	Realm string `json:"realm"`
}

func (s *ConfigStore) listen(ctx context.Context, ready chan<- error) {
	defer close(s.done)
	first := true
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN "+ConfigChannel)
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close(context.WithoutCancel(ctx))
			}
			if first {
				ready <- fmt.Errorf("postgres: listen %s: %w", ConfigChannel, err)
				return
			}
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		// Only now is a notification guaranteed to reach us, so only now may a cache entry be trusted:
		// anything loaded before LISTEN could have missed a change.
		s.invalidate(notice{})
		if first {
			first = false
			ready <- nil
		}
		backoff = 100 * time.Millisecond
		for ctx.Err() == nil {
			n, err := conn.WaitForNotification(ctx)
			if err != nil {
				break
			}
			var nt notice
			if json.Unmarshal([]byte(n.Payload), &nt) != nil {
				nt = notice{} // an unreadable payload invalidates everything: safe, never stale
			}
			s.invalidate(nt)
		}
		_ = conn.Close(context.WithoutCancel(ctx))
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// invalidate drops the entries a change could have affected. A zero notice, a realm-less table
// (hot_config, provider_accounts) or "(global)" drops everything.
func (s *ConfigStore) invalidate(n notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	realm := domain.Realm(n.Realm)
	all := n.Table == "" || n.Realm == "" || n.Realm == "(global)"
	switch {
	case all:
		clear(s.limits)
		clear(s.pools)
		clear(s.active)
		clear(s.cards)
	case n.Table == "limits":
		delete(s.limits, realm)
	case n.Table == "credit_pools":
		delete(s.pools, realm)
	case n.Table == "rate_cards" || n.Table == "rate_card_entries":
		delete(s.active, realm)
		// Versions are immutable, so cached versions stay valid — but a card's params or entries
		// arriving after a read must not be missed if the version was read half-published.
		for k := range s.cards {
			if k.realm == realm {
				delete(s.cards, k)
			}
		}
	default:
		// A table this cache does not read (plans, entitlements…): nothing to do.
		return
	}
	s.invalidations.inc()
}

// ------------------------------------------------------------------ limits --

// Limits returns the realm's active limits in configured order.
func (s *ConfigStore) Limits(ctx context.Context, realm domain.Realm) ([]domain.Limit, error) {
	realm = realm.Or()
	s.mu.RLock()
	e, ok := s.limits[realm]
	s.mu.RUnlock()
	if ok && s.fresh(e.at) {
		return e.v, nil
	}
	v, err, _ := s.sf.Do("limits/"+string(realm), func() (any, error) {
		ls, err := s.loadLimits(ctx, realm)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.limits[realm] = entry[[]domain.Limit]{ls, s.now()}
		s.mu.Unlock()
		return ls, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]domain.Limit), nil
}

func (s *ConfigStore) loadLimits(ctx context.Context, realm domain.Realm) ([]domain.Limit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, subject_kind, unit, max, max_entitlement, window_kind, dur_seconds, tz,
		       warn_at::float8, deny_code, resource
		FROM limits WHERE realm = $1 AND active ORDER BY sort_order, name`, string(realm))
	if err != nil {
		return nil, fmt.Errorf("postgres: load limits: %w", err)
	}
	defer rows.Close()
	var out []domain.Limit
	for rows.Next() {
		var (
			l                         domain.Limit
			subject, maxEnt, resource *string
			dur                       *int32
			kind, unit                string
			warn                      float64
		)
		if err := rows.Scan(&l.Name, &subject, &unit, &l.Max, &maxEnt, &kind, &dur, &l.TZ, &warn, &l.DenyCode, &resource); err != nil {
			return nil, fmt.Errorf("postgres: scan limit: %w", err)
		}
		w, err := domain.ParseWindow(kind)
		if err != nil {
			return nil, err
		}
		l.Window, l.Unit, l.WarnAt = w, domain.Unit(unit), warn
		l.Subject, l.MaxEntitlement, l.Resource = deref(subject), deref(maxEnt), deref(resource)
		if dur != nil {
			l.Dur = time.Duration(*dur) * time.Second
		}
		if err := l.Validate(); err != nil {
			return nil, fmt.Errorf("postgres: limit %q in realm %q is invalid: %w", l.Name, realm, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ------------------------------------------------------------------- pools --

// Pools returns the realm's configured pools. `general` is implicit and not returned.
func (s *ConfigStore) Pools(ctx context.Context, realm domain.Realm) ([]domain.PoolDef, error) {
	realm = realm.Or()
	s.mu.RLock()
	e, ok := s.pools[realm]
	s.mu.RUnlock()
	if ok && s.fresh(e.at) {
		return e.v, nil
	}
	v, err, _ := s.sf.Do("pools/"+string(realm), func() (any, error) {
		rows, err := s.pool.Query(ctx, `SELECT name, priority, applies_to FROM credit_pools WHERE realm = $1 ORDER BY priority, name`, string(realm))
		if err != nil {
			return nil, fmt.Errorf("postgres: load pools: %w", err)
		}
		defer rows.Close()
		var out []domain.PoolDef
		for rows.Next() {
			var d domain.PoolDef
			var name string
			if err := rows.Scan(&name, &d.Priority, &d.AppliesTo); err != nil {
				return nil, fmt.Errorf("postgres: scan pool: %w", err)
			}
			d.Name = domain.Pool(name)
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.pools[realm] = entry[[]domain.PoolDef]{out, s.now()}
		s.mu.Unlock()
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]domain.PoolDef), nil
}

// -------------------------------------------------------------- rate cards --

// Active returns the realm's one active rate card. A realm with none cannot price, and that fails
// closed (ErrUnpriceable).
func (s *ConfigStore) Active(ctx context.Context, realm domain.Realm) (domain.RateCard, error) {
	realm = realm.Or()
	s.mu.RLock()
	e, ok := s.active[realm]
	s.mu.RUnlock()
	if ok && s.fresh(e.at) {
		return e.v, nil
	}
	v, err, _ := s.sf.Do("card/"+string(realm), func() (any, error) {
		var version string
		err := s.pool.QueryRow(ctx, `SELECT version FROM rate_cards WHERE realm = $1 AND retired_at IS NULL`, string(realm)).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: realm %q has no active rate card", domain.ErrUnpriceable, realm)
		}
		if err != nil {
			return nil, fmt.Errorf("postgres: active card: %w", err)
		}
		card, err := s.loadCard(ctx, realm, version)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.active[realm] = entry[domain.RateCard]{card, s.now()}
		s.mu.Unlock()
		return card, nil
	})
	if err != nil {
		return domain.RateCard{}, err
	}
	return v.(domain.RateCard), nil
}

// Get returns a specific version, active or retired: replay and audit re-price under it. Versions
// are immutable, so a loaded one is cached.
func (s *ConfigStore) Get(ctx context.Context, realm domain.Realm, version string) (domain.RateCard, error) {
	realm = realm.Or()
	k := cardKey{realm, version}
	s.mu.RLock()
	c, ok := s.cards[k]
	s.mu.RUnlock()
	if ok {
		return c, nil
	}
	card, err := s.loadCard(ctx, realm, version)
	if err != nil {
		return domain.RateCard{}, err
	}
	s.mu.Lock()
	s.cards[k] = card
	s.mu.Unlock()
	return card, nil
}

func (s *ConfigStore) loadCard(ctx context.Context, realm domain.Realm, version string) (domain.RateCard, error) {
	var pricer string
	var params []byte
	err := s.pool.QueryRow(ctx, `SELECT pricer, params FROM rate_cards WHERE realm = $1 AND version = $2`, string(realm), version).Scan(&pricer, &params)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RateCard{}, fmt.Errorf("%w: no rate card %q in realm %q", domain.ErrUnpriceable, version, realm)
	}
	if err != nil {
		return domain.RateCard{}, fmt.Errorf("postgres: load card: %w", err)
	}
	var p map[string]string
	if len(params) > 0 && strings.TrimSpace(string(params)) != "{}" {
		if err := json.Unmarshal(params, &p); err != nil {
			return domain.RateCard{}, fmt.Errorf("postgres: card %q params: %w", version, err)
		}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT rate_key, unit, credits_per_block, block_size, coalesce(cost_micros_per_block, 0)
		FROM rate_card_entries WHERE realm = $1 AND version = $2 ORDER BY rate_key, unit`, string(realm), version)
	if err != nil {
		return domain.RateCard{}, fmt.Errorf("postgres: load card entries: %w", err)
	}
	defer rows.Close()
	var entries []domain.RateEntry
	for rows.Next() {
		var e domain.RateEntry
		var unit string
		if err := rows.Scan(&e.RateKey, &unit, &e.CreditsPerBlock, &e.BlockSize, &e.CostMicrosPerBlock); err != nil {
			return domain.RateCard{}, fmt.Errorf("postgres: scan card entry: %w", err)
		}
		e.Unit = domain.Unit(unit)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return domain.RateCard{}, err
	}
	return domain.NewRateCard(realm, version, pricer, entries, p)
}
