package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// globalRealm is how the config relay announces a change to something that belongs to no realm (or that it could not place):
// it could affect any of them.
const globalRealm = "(global)"

// Watcher keeps an Entitler's cache current from the bus. Close ends it.
type Watcher struct {
	subs []mports.Subscription
	once sync.Once
	err  error
}

// Close ends both subscriptions. It is safe to call twice.
func (w *Watcher) Close() error {
	w.once.Do(func() {
		var errs []error
		for _, s := range w.subs {
			errs = append(errs, s.Close())
		}
		w.err = errors.Join(errs...)
	})
	return w.err
}

// Watch subscribes the cache to the two announcements that mean an entitlement may have changed (FR-035) — and to nothing else:
//
//	<prefix>.entitlement.<token>   evicts the one scope the token names
//	<prefix>.config.<realm>        evicts every scope of the realm; (global) evicts everything
//
// group names this REPLICA's subscription. It must differ from every other replica's — a shared group would hand each message to
// one replica — and be stable across restarts, since a bus stream is only trimmed up to what every group has acknowledged.
//
// A Watch that cannot join both subjects joins neither. An Entitler built without a cache has nothing to evict and refuses.
func (e *Entitler) Watch(ctx context.Context, bus mports.Bus, prefix, group string) (*Watcher, error) {
	switch {
	case e.cache == nil:
		return nil, fmt.Errorf("%w: this Entitler has no cache, so there is nothing to keep current", domain.ErrInvalid)
	case bus == nil:
		return nil, fmt.Errorf("%w: a Watch needs a bus", domain.ErrInvalid)
	case prefix == "" || strings.ContainsAny(prefix, "*> \t"):
		return nil, fmt.Errorf("%w: a Watch needs the deployment's subject prefix, got %q", domain.ErrInvalid, prefix)
	case group == "":
		return nil, fmt.Errorf("%w: a Watch needs this replica's group", domain.ErrInvalid)
	}
	w := &Watcher{}
	for _, sub := range []struct {
		pattern string
		h       mports.Handler
	}{
		{prefix + ".entitlement.>", e.onEntitlement(prefix + ".entitlement.")},
		{prefix + ".config.>", e.onConfig(prefix + ".config.")},
	} {
		s, err := bus.Subscribe(ctx, sub.pattern, group, sub.h)
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("entitlement: joining %s: %w", sub.pattern, err)
		}
		w.subs = append(w.subs, s)
	}
	return w, nil
}

func (e *Entitler) invalidated(source string) {
	e.count(mports.Labels{"source": source}, ports.MetricCacheInvalidations, 1)
}

// onEntitlement evicts the scope a <prefix>.entitlement.<token> subject names. The subject is the address: the body is not read.
// A message that is not ours — the wrong prefix, no token, more tokens than a subject has, a token that is not a scope's — is
// acknowledged and ignored, never an error: redelivering it for ever would help no one.
func (e *Entitler) onEntitlement(want string) mports.Handler {
	return func(_ context.Context, m mports.Message) error {
		token, ok := strings.CutPrefix(m.Subject, want)
		if !ok || token == "" || strings.ContainsAny(token, ".*> \t\r\n") {
			return nil
		}
		tag, err := url.PathUnescape(token)
		if err != nil {
			return nil
		}
		realm, rest, ok := strings.Cut(tag, "/")
		if !ok || rest == "" || realm == "" || md.Realm(realm).Validate() != nil {
			return nil
		}
		e.cache.evict(tag)
		e.invalidated("scope")
		return nil
	}
}

// onConfig evicts every scope of the realm a <prefix>.config.<realm> subject names, or everything for (global).
func (e *Entitler) onConfig(want string) mports.Handler {
	return func(_ context.Context, m mports.Message) error {
		realm, ok := strings.CutPrefix(m.Subject, want)
		if !ok || realm == "" || strings.ContainsAny(realm, ".*> \t\r\n") {
			return nil
		}
		if realm == globalRealm {
			e.cache.flush()
		} else {
			e.cache.evictRealm(md.Realm(realm)) // a realm nobody holds is a no-op
		}
		e.invalidated("realm")
		return nil
	}
}
