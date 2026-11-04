package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Deps is what the Entitler is built from.
type Deps struct {
	Store ports.EntitlementStore
	Clock mports.Clock
	// Metrics is optional: without it nothing is counted.
	Metrics mports.Metrics
	// CacheTTL bounds how long a resolution is kept. Zero means no cache: every check reads the store. A resolution is also kept
	// no longer than the grace window or the override expiry it was built from, and is evicted at once by Watch's announcements
	// (see the entitlement contract, "The cache").
	CacheTTL time.Duration
	// CacheMaxEntries bounds how many scopes are kept; 0 means 100 000. Past it an arbitrary entry is dropped, which costs a read
	// and never a wrong answer.
	CacheMaxEntries int
	// LoadTimeout bounds one read of the store on behalf of every caller waiting for it; 0 means 10 seconds. The read does not
	// belong to the caller that happened to start it, so that caller giving up does not fail the others.
	LoadTimeout time.Duration
}

const (
	defaultCacheMax    = 100_000
	defaultLoadTimeout = 10 * time.Second
)

// Entitler answers a host's questions. It implements ports.Entitler.
type Entitler struct {
	d     Deps
	cache *cache // nil: no cache
	loads singleflight.Group
}

var _ ports.Entitler = (*Entitler)(nil)

// New builds the Entitler.
func New(d Deps) (*Entitler, error) {
	if d.Store == nil || d.Clock == nil {
		return nil, errors.New("entitlement: an EntitlementStore and a Clock are required")
	}
	if d.CacheTTL < 0 || d.CacheMaxEntries < 0 || d.LoadTimeout < 0 {
		return nil, errors.New("entitlement: the cache TTL, its size and the load timeout are not negative")
	}
	if d.CacheMaxEntries == 0 {
		d.CacheMaxEntries = defaultCacheMax
	}
	if d.LoadTimeout == 0 {
		d.LoadTimeout = defaultLoadTimeout
	}
	e := &Entitler{d: d}
	if d.CacheTTL > 0 {
		e.cache = newCache(d.CacheMaxEntries)
	}
	return e, nil
}

// CacheLen is how many scopes the cache holds now.
func (e *Entitler) CacheLen() int {
	if e.cache == nil {
		return 0
	}
	return e.cache.len()
}

// resolve answers from the cache when it can and from the store when it cannot. Any failure to read is ErrUnavailable with the
// cause kept: the host denies (invariant 2).
func (e *Entitler) resolve(ctx context.Context, s md.Scope) (*resolution, error) {
	s = s.Normalized()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if e.cache == nil {
		return e.load(ctx, s)
	}
	tag := s.Tag()
	if r := e.cache.get(tag, e.d.Clock.Now()); r != nil {
		e.lookup("hit")
		return r, nil
	}
	e.lookup("miss")
	// One read for everyone who asks while it is in flight, made on its own deadline: the caller that started it may give up.
	ch := e.loads.DoChan(tag, func() (any, error) {
		gen := e.cache.generation()
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.d.LoadTimeout)
		defer cancel()
		r, err := e.load(lctx, s)
		if err != nil {
			return nil, err // never cached
		}
		until := r.at.Add(e.d.CacheTTL)
		if !r.until.IsZero() && r.until.Before(until) {
			until = r.until
		}
		e.cache.put(tag, s.Realm, r, until, gen)
		return r, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*resolution), nil //nolint:forcetypeassert // the load above returns nothing else
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", domain.ErrUnavailable, ctx.Err())
	}
}

// load reads the store and resolves the scope's entitlements at the clock's instant.
func (e *Entitler) load(ctx context.Context, s md.Scope) (*resolution, error) {
	unavailable := func(what string, err error) error {
		return fmt.Errorf("%w: reading %s: %w", domain.ErrUnavailable, what, err)
	}
	in := inputs{scope: s, now: e.d.Clock.Now()}
	var err error
	if in.decls, err = e.d.Store.Keys(ctx, s.Realm); err != nil {
		return nil, unavailable("the declared keys", err)
	}
	if in.defaults, err = e.d.Store.Defaults(ctx, s.Realm); err != nil {
		return nil, unavailable("the free tier", err)
	}
	sub, has, err := e.d.Store.Subscription(ctx, s)
	if err != nil {
		return nil, unavailable("the subscription", err)
	}
	if has {
		in.sub = &sub
		if sub.Holds(in.now) {
			if in.plan, err = e.d.Store.PlanEntitlements(ctx, s.Realm, sub.PlanCode); err != nil {
				return nil, unavailable("the plan's entitlements", err)
			}
		}
	}
	if in.overrides, err = e.d.Store.Overrides(ctx, s); err != nil {
		return nil, unavailable("the overrides", err)
	}
	r := resolve(in)
	for src, n := range r.invalid {
		e.count(mports.Labels{"realm": string(s.Realm), "source": string(src)}, ports.MetricInvalid, int64(n))
	}
	return r, nil
}

func (e *Entitler) count(l mports.Labels, name string, n int64) {
	if e.d.Metrics != nil {
		e.d.Metrics.Count(name, n, l)
	}
}

func (e *Entitler) lookup(result string) {
	e.count(mports.Labels{"result": result}, ports.MetricCacheLookups, 1)
}

func (e *Entitler) decided(s md.Scope, outcome string) { e.countDecisions(s, outcome, 1) }

func labelsFor(s md.Scope, outcome string) mports.Labels {
	return mports.Labels{"realm": string(s.Normalized().Realm), "outcome": outcome}
}

// check insists that k, if the realm declares it, is a key of the kind asked about. An undeclared key is not an error: nothing
// grants it, and the caller answers "no" (invariant 1).
func (r *resolution) check(k domain.Key, want domain.Kind) error {
	if d, ok := r.decls[k]; ok && d.Kind != want {
		return fmt.Errorf("%w: key %q is a %s, not a %s", domain.ErrInvalid, string(k), string(d.Kind), string(want))
	}
	return nil
}

// Allowed answers a flag check. An undeclared key is false with no error (invariant 1).
func (e *Entitler) Allowed(ctx context.Context, s md.Scope, k domain.Key) (bool, error) {
	r, err := e.resolve(ctx, s)
	if err != nil {
		e.decided(s, "error")
		return false, err
	}
	if err := r.check(k, domain.Flag); err != nil {
		return false, err
	}
	g, ok := r.grants[k] // only declared keys are ever granted
	if !ok || !g.Value.Bool {
		e.decided(s, "denied")
		return false, nil
	}
	e.decided(s, "allowed")
	return true, nil
}

// Quota returns the ceiling of a quota key. ok is false when nothing grants it.
func (e *Entitler) Quota(ctx context.Context, s md.Scope, k domain.Key) (int64, bool, error) {
	r, err := e.resolve(ctx, s)
	if err != nil {
		return 0, false, err
	}
	if err := r.check(k, domain.Quota); err != nil {
		return 0, false, err
	}
	g, ok := r.grants[k]
	if !ok {
		return 0, false, nil
	}
	return g.Value.Int, true, nil
}

// Choice returns an enum entitlement.
func (e *Entitler) Choice(ctx context.Context, s md.Scope, k domain.Key) (string, bool, error) {
	r, err := e.resolve(ctx, s)
	if err != nil {
		return "", false, err
	}
	if err := r.check(k, domain.Enum); err != nil {
		return "", false, err
	}
	g, ok := r.grants[k]
	if !ok {
		return "", false, nil
	}
	return g.Value.Text, true, nil
}

// Resolve returns every effective grant with its provenance, ordered by key.
func (e *Entitler) Resolve(ctx context.Context, s md.Scope) ([]domain.Grant, error) {
	r, err := e.resolve(ctx, s)
	if err != nil {
		return nil, err
	}
	return r.sorted(), nil
}

// Consume measures current+delta against the quota. The host owns its counts (invariant 4): nothing is stored here.
func (e *Entitler) Consume(ctx context.Context, s md.Scope, k domain.Key, current, delta int64) (domain.Decision, error) {
	if current < 0 || delta < 0 {
		return domain.Decision{}, fmt.Errorf("%w: counts are not negative (current %d, delta %d)", domain.ErrInvalid, current, delta)
	}
	r, err := e.resolve(ctx, s)
	if err != nil {
		e.decided(s, "error")
		return domain.Decision{}, err
	}
	if err := r.check(k, domain.Quota); err != nil {
		return domain.Decision{}, err
	}
	if _, declared := r.decls[k]; !declared {
		e.decided(s, "denied")
		return domain.Decision{Current: current, Reason: notACapability(k)}, nil
	}
	d, err := e.measure(ctx, r, &catalogue{}, k, current, delta)
	if err != nil {
		return domain.Decision{}, err
	}
	if d.Allowed {
		e.decided(s, "allowed")
	} else {
		e.decided(s, "denied")
	}
	return d, nil
}

// measure judges current+delta against the scope's quota for a key the realm declares as one. Consume and Check both ask it, so
// they cannot disagree about a quota.
func (e *Entitler) measure(ctx context.Context, r *resolution, cat *catalogue, k domain.Key, current, delta int64) (domain.Decision, error) {
	g, ok := r.grants[k]
	if !ok {
		d := domain.Decision{Current: current, Reason: notIncluded(r, nil, k)}
		d.UpgradePath = wayOut(cat.offers(ctx, e, r.scope.Realm), r.plan, k, fitsAQuota)
		return d, nil
	}
	d, err := g.Value.Admits(current, delta)
	if err != nil {
		return domain.Decision{}, err
	}
	if d.Allowed {
		return d, nil
	}
	d.Reason = refusal(g, r.plan, k, current, delta)
	if g.Source != domain.FromOverride { // an override outranks every plan: upgrading would not lift it
		need := current
		fits := func(v domain.Value) bool {
			a, err := v.Admits(need, delta)
			return err == nil && a.Allowed
		}
		d.UpgradePath = wayOut(cat.offers(ctx, e, r.scope.Realm), r.plan, k, fits)
	}
	return d, nil
}

// catalogue reads the realm's plans at most once for a call, and only when a refusal wants a way out. A store that cannot say
// leaves the refusal without one: the refusal itself stands.
type catalogue struct {
	read bool
	list []domain.Offer
}

func (c *catalogue) offers(ctx context.Context, e *Entitler, realm md.Realm) []domain.Offer {
	if !c.read {
		c.read = true
		c.list, _ = e.d.Store.Plans(ctx, realm)
	}
	return c.list
}

// wayOut is the first plan FURTHER UP the catalogue than the one held whose entitlement for k satisfies fits: a way out is a plan
// to move to, not a cheaper one to fall back to. When the plan held is no longer sold there is no position to measure from, and
// every plan on sale is a candidate.
func wayOut(offers []domain.Offer, held string, k domain.Key, fits func(domain.Value) bool) string {
	start := 0
	if held != "" {
		for i, o := range offers {
			if o.PlanCode == held {
				start = i + 1
				break
			}
		}
	}
	for _, o := range offers[start:] {
		for _, ent := range o.Entitlements {
			if ent.Key == k && fits(ent.Value) {
				return o.PlanCode
			}
		}
	}
	return ""
}

func fitsAQuota(v domain.Value) bool {
	d, err := v.Admits(0, 1)
	return err == nil && d.Allowed
}

func fitsAFlag(v domain.Value) bool { return v.Kind == domain.Flag && v.Bool }

func fitsAnEnum(v domain.Value) bool { return v.Kind == domain.Enum }

func notACapability(k domain.Key) string {
	return fmt.Sprintf("%q is not a capability this product offers.", string(k))
}

// notIncluded says why a key the realm declares is not granted: where the "no" comes from, if anywhere.
func notIncluded(r *resolution, g *domain.Grant, k domain.Key) string {
	what := words(k)
	switch {
	case g != nil && g.Source == domain.FromOverride:
		return fmt.Sprintf("%s is switched off for your account.", capitalised(what))
	case g != nil && g.Source == domain.FromDefault:
		return fmt.Sprintf("The free tier does not include %s.", what)
	case r.plan != "":
		return fmt.Sprintf("Your %q plan does not include %s.", r.plan, what)
	}
	return fmt.Sprintf("Your plan does not include %s.", what)
}

func capitalised(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// refusal explains a refused quota in words a person can act on (invariant 7): what they have, what they asked for, and
// where the limit comes from.
func refusal(g domain.Grant, plan string, k domain.Key, current, delta int64) string {
	what := words(k)
	var source string
	switch g.Source {
	case domain.FromSubscription:
		source = fmt.Sprintf("Your %q plan includes %d %s", plan, g.Value.Int, what)
	case domain.FromOverride:
		source = fmt.Sprintf("Your account is limited to %d %s", g.Value.Int, what)
	default:
		source = fmt.Sprintf("The free tier includes %d %s", g.Value.Int, what)
	}
	return fmt.Sprintf("%s; you have %d and asked for %d more.", source, current, delta)
}

// words reads a key aloud: daily_user_credits is "daily user credits".
func words(k domain.Key) string { return strings.ReplaceAll(string(k), "_", " ") }
