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

	"github.com/truongpx396/intel-payment/metering/ports"
)

// globalRealm is the realm a change is announced under when it belongs to none, or could not be placed: it could affect any.
const globalRealm = "(global)"

// RelayOptions tune a ConfigRelay.
type RelayOptions struct {
	// OnError receives what the relay could not do — a publication the bus refused, a connection it lost — and is told once per
	// failure. The relay carries on: a change it could not announce reaches caches at their TTL. Nil discards.
	OnError func(error)
}

// ConfigRelay forwards the database's configuration notifications onto the bus as <prefix>.config.<realm> with {table, realm},
// the announcement every process that caches configuration listens for (bus-subjects.md). The database's audit trigger
// NOTIFYs in the same transaction as the write, so a change made by SQL reaches every cache as surely as one made through the
// API.
//
// One relay per deployment is enough (payment-worker runs it): the announcement is idempotent, and a duplicate costs a cache a
// read. A notification lost while the relay's connection was down is not recoverable, so after a reconnect it announces
// "(global)" — everything — which is safe and cheap.
type ConfigRelay struct {
	pool   *pgxpool.Pool
	bus    ports.Bus
	prefix string
	onErr  func(error)

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewConfigRelay starts the relay. It returns once it is listening, so a change made after it returns is not missed; Close stops
// it.
func NewConfigRelay(ctx context.Context, pool *pgxpool.Pool, bus ports.Bus, prefix string, o RelayOptions) (*ConfigRelay, error) {
	switch {
	case pool == nil:
		return nil, errors.New("postgres: a relay needs a pool")
	case bus == nil:
		return nil, errors.New("postgres: a relay needs a bus")
	case prefix == "" || strings.ContainsAny(prefix, "*> \t"):
		return nil, fmt.Errorf("postgres: a relay needs the deployment's subject prefix, got %q", prefix)
	}
	r := &ConfigRelay{pool: pool, bus: bus, prefix: prefix, onErr: o.OnError, done: make(chan struct{})}
	lctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancel = cancel
	ready := make(chan error, 1)
	go r.run(lctx, ready)
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-r.done
			return nil, err
		}
	case <-ctx.Done():
		cancel()
		<-r.done
		return nil, ctx.Err()
	}
	return r, nil
}

// Close stops the relay and waits for it. It is safe to call twice.
func (r *ConfigRelay) Close() {
	r.once.Do(func() {
		r.cancel()
		<-r.done
	})
}

func (r *ConfigRelay) report(err error) {
	if r.onErr != nil && err != nil {
		r.onErr(err)
	}
}

func (r *ConfigRelay) run(ctx context.Context, ready chan<- error) {
	defer close(r.done)
	first := true
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, r.pool.Config().ConnConfig)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN "+ConfigChannel)
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close(context.WithoutCancel(ctx))
			}
			if first {
				ready <- fmt.Errorf("postgres: relay listen %s: %w", ConfigChannel, err)
				return
			}
			r.report(fmt.Errorf("postgres: relay reconnect: %w", err))
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		if first {
			first = false
			ready <- nil
		} else {
			r.announce(ctx, notice{}) // whatever was missed while we were away could be anywhere
		}
		backoff = 100 * time.Millisecond
		for ctx.Err() == nil {
			n, err := conn.WaitForNotification(ctx)
			if err != nil {
				break
			}
			var nt notice
			if json.Unmarshal([]byte(n.Payload), &nt) != nil {
				nt = notice{}
			}
			r.announce(ctx, nt)
		}
		_ = conn.Close(context.WithoutCancel(ctx))
	}
}

// announce publishes a notice. A realm that is empty, "(global)", or not safe to put in a subject is announced as global: the
// failure direction is fresh.
func (r *ConfigRelay) announce(ctx context.Context, n notice) {
	realm := n.Realm
	if realm == "" || strings.ContainsAny(realm, ".*> \t\r\n") {
		realm = globalRealm
	}
	body, _ := json.Marshal(notice{Table: n.Table, Realm: realm})
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.bus.Publish(pctx, r.prefix+".config."+realm, body); err != nil {
		r.report(fmt.Errorf("postgres: relay publish %s.config.%s: %w", r.prefix, realm, err))
	}
}
