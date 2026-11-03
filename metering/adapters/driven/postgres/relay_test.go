//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R103: the contract names a publisher for <prefix>.config.<realm> — "the config relay (Postgres LISTEN intelpay_config)" — and
// until now there was none. Every process that caches configuration, the entitlement cache among them, is told of a change by
// this, so a change made by an operator, the admin API or plain SQL reaches it at once and not at its TTL.

type relayBus struct {
	mu   sync.Mutex
	msgs []relayMsg
	fail error
}

type relayMsg struct {
	Subject string
	Payload []byte
}

func (b *relayBus) Publish(_ context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.msgs = append(b.msgs, relayMsg{subject, append([]byte(nil), payload...)})
	return nil
}

func (b *relayBus) Subscribe(context.Context, string, string, ports.Handler) (ports.Subscription, error) {
	return nil, errors.New("not used")
}
func (b *relayBus) Pending(context.Context, string, string) (int64, time.Duration, error) {
	return 0, 0, nil
}
func (b *relayBus) Trim(context.Context, string, ports.RetentionPolicy) error { return nil }

func (b *relayBus) on(subject string) []relayMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []relayMsg
	for _, m := range b.msgs {
		if m.Subject == subject {
			out = append(out, m)
		}
	}
	return out
}

func (b *relayBus) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.msgs) }

// newRelay starts a relay on a database of its own and returns the pool the test writes through.
func newRelay(t *testing.T, bus ports.Bus, prefix string, onErr func(error)) (*pgadapter.ConfigRelay, *pgxpool.Pool) {
	t.Helper()
	p := db(t)
	r, err := pgadapter.NewConfigRelay(context.Background(), p, bus, prefix, pgadapter.RelayOptions{OnError: onErr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, p
}

func TestAConfigurationChangeIsRelayedOntoTheBusUnderItsRealm(t *testing.T) {
	t.Parallel()
	bus := &relayBus{}
	_, p := newRelay(t, bus, "acme", nil)
	exec(t, p, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('t1','sso','flag')`)
	eventually(t, 5*time.Second, "the notification", func() bool { return len(bus.on("acme.config.t1")) == 1 })
	var n struct{ Table, Realm string }
	if err := json.Unmarshal(bus.on("acme.config.t1")[0].Payload, &n); err != nil || n.Table != "entitlement_keys" || n.Realm != "t1" {
		t.Errorf("payload = %s (%v); want {table entitlement_keys, realm t1}", bus.on("acme.config.t1")[0].Payload, err)
	}
	// Another realm is another subject; a table that belongs to no realm is (global), which every cache reads as "everything".
	exec(t, p, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('other','sso','flag')`)
	eventually(t, 5*time.Second, "the other realm's notification", func() bool { return len(bus.on("acme.config.other")) == 1 })
	exec(t, p, `INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref) VALUES ('a1','stripe',ARRAY['t1'],'test','env:K','env:W')`)
	eventually(t, 5*time.Second, "the global notification", func() bool { return len(bus.on("acme.config.(global)")) == 1 })
	if len(bus.on("billing.config.t1")) != 0 {
		t.Error("the relay published under a prefix it was not given")
	}
}

// A notice it cannot read is a change it cannot place, and is announced as one that could be anywhere.
func TestAnUnreadableNotificationIsRelayedAsGlobal(t *testing.T) {
	t.Parallel()
	bus := &relayBus{}
	_, p := newRelay(t, bus, "billing", nil)
	exec(t, p, `SELECT pg_notify('intelpay_config', 'not json')`)
	eventually(t, 5*time.Second, "the global notification", func() bool { return len(bus.on("billing.config.(global)")) == 1 })
}

// A realm is a slug; a notification that names something else — a dot would split the subject, a star would be a wildcard — is
// announced as global rather than as a subject it was never meant to be.
func TestARealmThatCannotSitInASubjectIsRelayedAsGlobal(t *testing.T) {
	t.Parallel()
	bus := &relayBus{}
	_, p := newRelay(t, bus, "billing", nil)
	for _, realm := range []string{"a.b", "a*", "a>", "a b", ""} {
		body, _ := json.Marshal(map[string]string{"table": "x", "realm": realm})
		exec(t, p, `SELECT pg_notify('intelpay_config', $1)`, string(body))
	}
	eventually(t, 5*time.Second, "all five to be relayed", func() bool { return len(bus.on("billing.config.(global)")) == 5 })
	for _, m := range bus.msgs {
		if m.Subject != "billing.config.(global)" {
			t.Errorf("published on %q", m.Subject)
		}
	}
}

func TestARelayThatCannotPublishReportsItAndKeepsRelaying(t *testing.T) {
	t.Parallel()
	bus := &relayBus{fail: errors.New("bus: down")}
	var mu sync.Mutex
	var errs []error
	_, p := newRelay(t, bus, "billing", func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() })
	exec(t, p, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('t1','sso','flag')`)
	eventually(t, 5*time.Second, "the failure to be reported", func() bool { mu.Lock(); defer mu.Unlock(); return len(errs) == 1 })
	bus.mu.Lock()
	bus.fail = nil
	bus.mu.Unlock()
	exec(t, p, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('t1','export','flag')`)
	eventually(t, 5*time.Second, "the next change to be relayed", func() bool { return len(bus.on("billing.config.t1")) == 1 })
}

func TestARelayKeepsRelayingAfterItsConnectionIsLost(t *testing.T) {
	t.Parallel()
	bus := &relayBus{}
	_, p := newRelay(t, bus, "billing", nil)
	exec(t, p, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN intelpay_config%' AND pid <> pg_backend_pid() AND datname = current_database()`)
	// Whatever was missed while the connection was down could be anywhere, so the relay says so: caches drop everything.
	eventually(t, 15*time.Second, "the relay to announce that it may have missed something", func() bool { return len(bus.on("billing.config.(global)")) >= 1 })
	eventually(t, 15*time.Second, "a change after the reconnect to be relayed", func() bool {
		exec(t, p, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('t1', 'k'||floor(random()*1e9)::text, 'flag')`)
		time.Sleep(200 * time.Millisecond)
		return len(bus.on("billing.config.t1")) > 0
	})
}

func TestAClosedRelayRelaysNothingAndClosesTwice(t *testing.T) {
	t.Parallel()
	bus := &relayBus{}
	r, p := newRelay(t, bus, "billing", nil)
	r.Close()
	r.Close()
	exec(t, p, `INSERT INTO entitlement_keys (realm, key, kind) VALUES ('t1','sso','flag')`)
	time.Sleep(300 * time.Millisecond)
	if bus.count() != 0 {
		t.Errorf("a closed relay published %d message(s)", bus.count())
	}
}

func TestARelayNeedsAPoolABusAndAPrefix(t *testing.T) {
	t.Parallel()
	p := db(t)
	if _, err := pgadapter.NewConfigRelay(context.Background(), nil, &relayBus{}, "billing", pgadapter.RelayOptions{}); err == nil {
		t.Error("a relay with no pool")
	}
	if _, err := pgadapter.NewConfigRelay(context.Background(), p, nil, "billing", pgadapter.RelayOptions{}); err == nil {
		t.Error("a relay with no bus")
	}
	if _, err := pgadapter.NewConfigRelay(context.Background(), p, &relayBus{}, "", pgadapter.RelayOptions{}); err == nil {
		t.Error("a relay with no prefix")
	}
}
