package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
}

// Entitler answers a host's questions. It implements ports.Entitler.
type Entitler struct{ d Deps }

var _ ports.Entitler = (*Entitler)(nil)

// New builds the Entitler.
func New(d Deps) (*Entitler, error) {
	if d.Store == nil || d.Clock == nil {
		return nil, errors.New("entitlement: an EntitlementStore and a Clock are required")
	}
	return &Entitler{d: d}, nil
}

// resolve reads the store and resolves the scope's entitlements at the clock's instant. Any failure to read is
// ErrUnavailable with the cause kept: the host denies (invariant 2).
func (e *Entitler) resolve(ctx context.Context, s md.Scope) (*resolution, error) {
	s = s.Normalized()
	if err := s.Validate(); err != nil {
		return nil, err
	}
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

func (e *Entitler) decided(s md.Scope, outcome string) {
	e.count(mports.Labels{"realm": string(s.Normalized().Realm), "outcome": outcome}, ports.MetricDecisions, 1)
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
		return domain.Decision{Current: current, Reason: fmt.Sprintf("%q is not a capability this product offers.", string(k))}, nil
	}
	g, ok := r.grants[k]
	if !ok {
		e.decided(s, "denied")
		d := domain.Decision{Current: current, Reason: fmt.Sprintf("Your plan does not include %s.", words(k))}
		d.UpgradePath = e.upgradePath(ctx, r, k, current, delta)
		return d, nil
	}
	d, err := g.Value.Admits(current, delta)
	if err != nil {
		return domain.Decision{}, err
	}
	if d.Allowed {
		e.decided(s, "allowed")
		return d, nil
	}
	e.decided(s, "denied")
	d.Reason = refusal(g, r.plan, k, current, delta)
	if g.Source != domain.FromOverride { // an override outranks every plan: upgrading would not lift it
		d.UpgradePath = e.upgradePath(ctx, r, k, current, delta)
	}
	return d, nil
}

// upgradePath is the first plan in the catalogue whose quota for k fits the request. A
// store that cannot say leaves the refusal without a path: the refusal itself stands.
func (e *Entitler) upgradePath(ctx context.Context, r *resolution, k domain.Key, current, delta int64) string {
	offers, err := e.d.Store.Plans(ctx, r.scope.Realm)
	if err != nil {
		return ""
	}
	for _, o := range offers { // the plan the scope holds is not skipped: it just refused this, so it cannot fit it
		for _, ent := range o.Entitlements {
			if ent.Key != k || ent.Value.Kind != domain.Quota {
				continue
			}
			if d, err := ent.Value.Admits(current, delta); err == nil && d.Allowed {
				return o.PlanCode
			}
		}
	}
	return ""
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
