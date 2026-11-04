package app

import (
	"context"
	"fmt"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

var _ ports.Checker = (*Entitler)(nil)

// Check answers a batch: every question it was given, in the order it was given, from ONE resolution — a consistent picture of the
// scope and one read of the store however many keys are asked about. A page render asks about a dozen capabilities at once.
//
// A key the realm never declared is denied inside the batch and is not an error (invariant 1): a typo must fail closed and not
// fail the page. A batch that cannot be answered — the store is down — is refused whole, with no answers, so no key is left
// permitted by an outage (invariant 2). A batch that is not a question — nothing asked, too many, a key that is empty, a negative
// count — is refused whole too.
func (e *Entitler) Check(ctx context.Context, s md.Scope, reqs []domain.CheckRequest) ([]domain.CheckResult, error) {
	if len(reqs) == 0 || len(reqs) > domain.MaxChecks {
		return nil, fmt.Errorf("%w: a batch asks about 1 to %d keys, got %d", domain.ErrInvalid, domain.MaxChecks, len(reqs))
	}
	for i, q := range reqs {
		if err := q.Validate(); err != nil {
			return nil, fmt.Errorf("check %d: %w", i, err)
		}
	}
	r, err := e.resolve(ctx, s)
	if err != nil {
		e.countDecisions(s, "error", int64(len(reqs)))
		return nil, err
	}
	cat := &catalogue{}
	out := make([]domain.CheckResult, len(reqs))
	var allowed, denied int64
	for i, q := range reqs {
		res, err := e.checkOne(ctx, r, cat, q)
		if err != nil {
			return nil, err
		}
		out[i] = res
		if res.Allowed {
			allowed++
		} else {
			denied++
		}
	}
	e.countDecisions(s, "allowed", allowed)
	e.countDecisions(s, "denied", denied)
	return out, nil
}

func (e *Entitler) countDecisions(s md.Scope, outcome string, n int64) {
	if n > 0 {
		e.count(labelsFor(s, outcome), ports.MetricDecisions, n)
	}
}

func (e *Entitler) checkOne(ctx context.Context, r *resolution, cat *catalogue, q domain.CheckRequest) (domain.CheckResult, error) {
	res := domain.CheckResult{Key: q.Key}
	decl, declared := r.decls[q.Key]
	if !declared {
		res.Reason = notACapability(q.Key)
		return res, nil
	}
	res.Kind = decl.Kind
	g, granted := r.grants[q.Key]
	if granted {
		res.Value = g.Value
	}
	var gp *domain.Grant
	if granted {
		gp = &g
	}
	offers := func() []domain.Offer { return cat.offers(ctx, e, r.scope.Realm) }

	switch decl.Kind {
	case domain.Flag:
		res.Allowed = granted && g.Value.Bool
		if !res.Allowed {
			res.Reason = notIncluded(r, gp, q.Key)
			if !granted || g.Source != domain.FromOverride {
				res.UpgradePath = wayOut(offers(), r.plan, q.Key, fitsAFlag)
			}
		}
	case domain.Enum:
		res.Allowed = granted
		if !res.Allowed {
			res.Reason = notIncluded(r, nil, q.Key)
			res.UpgradePath = wayOut(offers(), r.plan, q.Key, fitsAnEnum)
		}
	default: // Quota
		if q.Current != nil {
			delta := int64(1)
			if q.Delta != nil {
				delta = *q.Delta
			}
			d, err := e.measure(ctx, r, cat, q.Key, *q.Current, delta)
			if err != nil {
				return domain.CheckResult{}, err
			}
			res.Allowed, res.Reason, res.UpgradePath = d.Allowed, d.Reason, d.UpgradePath
			res.Limit, res.Current, res.Remaining = &d.Limit, &d.Current, &d.Remaining
			break
		}
		// Not measured against a count: is it granted at all, and how large?
		if !granted {
			res.Reason = notIncluded(r, nil, q.Key)
			res.UpgradePath = wayOut(offers(), r.plan, q.Key, fitsAQuota)
			break
		}
		limit := g.Value.Int
		res.Limit = &limit
		d, err := g.Value.Admits(0, 1)
		if err != nil {
			return domain.CheckResult{}, err
		}
		res.Allowed = d.Allowed
		if !res.Allowed {
			res.Reason = notIncluded(r, gp, q.Key)
			if g.Source != domain.FromOverride {
				res.UpgradePath = wayOut(offers(), r.plan, q.Key, fitsAQuota)
			}
		}
	}
	return res, nil
}
