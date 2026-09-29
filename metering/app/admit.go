package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Admit gates before expensive work. It resolves the realm's configured limits for the resource,
// sizes any that name a max_entitlement from the QuotaSource, merges the caller's (which can only
// tighten), and evaluates them against the hot account in ONE read (FR-004…006, FR-017, FR-017c).
// It reserves nothing (invariant 3).
func (m *meter) Admit(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (domain.Admission, error) {
	start := m.d.Clock.Now()
	adm, outcome, err := m.admit(ctx, s, req)
	m.met.Observe(ports.MetricAdmitLatency, m.d.Clock.Now().Sub(start).Seconds(),
		ports.Labels{"realm": string(s.Realm.Or()), "outcome": outcome})
	return adm, err
}

func (m *meter) admit(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (domain.Admission, string, error) {
	if err := s.Validate(); err != nil {
		return domain.Admission{}, "invalid", err
	}
	if err := req.Subjects.Validate(); err != nil {
		return domain.Admission{}, "invalid", err
	}
	if req.MaxCost < 0 {
		return domain.Admission{}, "invalid", fmt.Errorf("%w: max_cost must be >= 0", domain.ErrInvalid)
	}
	s = s.Normalized()

	// MaxCost is what makes the overshoot bound (invariant 3) real; count every caller that omits it.
	if req.MaxCost == 0 {
		if m.cfg.RequireMaxCost {
			return domain.Admission{}, "invalid", fmt.Errorf("%w: max_cost is required by this deployment", domain.ErrInvalid)
		}
		m.met.Count(ports.MetricAdmitUncapped, 1, realmLabel(s))
	}

	limits, failedOpen, err := m.resolveLimits(ctx, s, req)
	if err != nil {
		return domain.Admission{}, "error", err
	}
	if err := domain.CheckSubjects(limits, req.Resource, req.Subjects); err != nil {
		return domain.Admission{}, "invalid", err
	}

	final := req
	final.Limits = limits
	var adm domain.Admission
	err = m.reh.heal(ctx, s, func() (e error) { adm, e = m.d.Balance.Admit(ctx, s, final); return })
	switch {
	case err == nil:
	case domain.IsHotUnavailable(err) || errors.Is(err, domain.ErrColdScope):
		return m.admitOutage(s, err)
	default:
		return domain.Admission{}, "error", err
	}

	outcome := "allowed"
	if !adm.Allowed {
		outcome = "denied"
		m.recordDenial(ctx, s, adm)
	} else if adm.Warning != nil {
		m.publishWarning(ctx, s, req, *adm.Warning)
	}
	if failedOpen {
		outcome = "failopen_quota"
	}
	return adm, outcome, nil
}

// admitOutage applies AdmitFailPolicy when the hot tier cannot answer. Record never drops usage
// either way: it defers to the journal (invariant 16), so fail_open means served before it is
// metered, never served unmetered.
func (m *meter) admitOutage(s domain.Scope, cause error) (domain.Admission, string, error) {
	if m.cfg.AdmitFail == metering.FailOpen {
		m.met.Count(ports.MetricAdmitFailOpen, 1, realmLabel(s))
		// Headroom is unknown: report the largest operation, not zero — a caller sizing its work by
		// headroom must not be told it can afford nothing during an outage it is being served through.
		return domain.Admission{Allowed: true, Headroom: m.cfg.MaxOperationAmount}, "failopen", nil
	}
	if errors.Is(cause, domain.ErrHotStoreUnavailable) {
		return domain.Admission{}, "unavailable", cause
	}
	return domain.Admission{}, "unavailable", fmt.Errorf("%w: %w", domain.ErrHotStoreUnavailable, cause)
}

// resolveLimits returns the final limit set for one admission: the realm's, restricted to the
// resource, entitlement-sized, then merged with the caller's.
func (m *meter) resolveLimits(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (limits []domain.Limit, failedOpen bool, err error) {
	all, err := m.d.Limits.Limits(ctx, s.Realm)
	if err != nil {
		// Configuration could not be read. That is not "the hot store is down": serving without the
		// realm's ceilings would be serving unbounded, so it fails closed whatever AdmitFail says.
		return nil, false, fmt.Errorf("metering: realm limits: %w", err)
	}
	configured := make([]domain.Limit, 0, len(all))
	for _, l := range all {
		if !l.Applies(req.Resource) {
			continue
		}
		if l.MaxEntitlement != "" {
			size, keep, open, err := m.sizeFromEntitlement(ctx, s, l)
			if err != nil {
				return nil, false, err
			}
			failedOpen = failedOpen || open
			if !keep {
				continue // unlimited, or unresolved under fail_open
			}
			l.Max = size
		}
		configured = append(configured, l)
	}

	// A caller may tighten a configured limit or pass a per-run Job budget. A window limit that no
	// configured limit backs could never be counted at settlement, so it would silently never bind:
	// that is refused, never a no-op (invariant 17).
	names := make(map[string]bool, len(all))
	for _, l := range all {
		names[l.Name] = true
	}
	for _, c := range req.Limits {
		if err := c.Validate(); err != nil {
			return nil, false, err
		}
		switch c.Window {
		case domain.Daily, domain.Hourly, domain.Rolling:
			if !names[c.Name] {
				return nil, false, fmt.Errorf("%w: %q is not a configured limit, so it cannot be counted", domain.ErrUncountableLimit, c.Name)
			}
		}
	}
	return domain.MergeLimits(configured, req.Limits), failedOpen, nil
}

// sizeFromEntitlement resolves a limit's Max from the scope's entitlement quota.
// keep=false drops the limit: the quota is unlimited, or unresolved under fail_open.
func (m *meter) sizeFromEntitlement(ctx context.Context, s domain.Scope, l domain.Limit) (size int64, keep, failedOpen bool, err error) {
	if m.d.Quotas == nil {
		return m.quotaFailure(s, l, errors.New("no QuotaSource is wired"))
	}
	q, err := m.d.Quotas.Quota(ctx, s, l.MaxEntitlement)
	switch {
	case err == nil && q == ports.Unlimited:
		return 0, false, false, nil
	case err == nil && q >= 0:
		return q, true, false, nil
	case err == nil:
		return m.quotaFailure(s, l, fmt.Errorf("quota %d is not a size", q))
	case errors.Is(err, domain.ErrQuotaNotGranted):
		return l.Max, true, false, nil // nothing grants it: the row's own Max is the fallback
	}
	return m.quotaFailure(s, l, err)
}

// quotaFailure applies AdmitFailPolicy to a quota that could not be resolved — never "unlimited".
func (m *meter) quotaFailure(s domain.Scope, l domain.Limit, cause error) (int64, bool, bool, error) {
	if m.cfg.AdmitFail == metering.FailOpen {
		m.met.Count(ports.MetricAdmitFailOpen, 1, realmLabel(s))
		return 0, false, true, nil
	}
	return 0, false, false, fmt.Errorf("%w: limit %q needs entitlement %q: %w", domain.ErrQuotaUnavailable, l.Name, l.MaxEntitlement, cause)
}

func (m *meter) recordDenial(ctx context.Context, s domain.Scope, adm domain.Admission) {
	limit, code := "unknown", ""
	if adm.Exceeded != nil {
		limit, code = adm.Exceeded.Name, adm.Exceeded.DenyCode
	}
	m.met.Count(ports.MetricAdmitDenied, 1, ports.Labels{"realm": string(s.Realm), "limit": limit, "deny_code": code})
	if m.warn.allow("blocked|" + s.Tag() + "|" + limit) {
		m.publish(ctx, "blocked", s, map[string]any{"realm": string(s.Realm), "scope": s.Tag(), "limit_name": limit, "deny_code": code})
	}
}

func (m *meter) publishWarning(ctx context.Context, s domain.Scope, req domain.AdmitRequest, l domain.Limit) {
	sub := ""
	if l.Subject != "" {
		sub = req.Subjects[l.Subject]
	}
	if m.warn.allow("warn|" + s.Tag() + "|" + l.Name + "|" + sub) {
		m.publish(ctx, "warn", s, map[string]any{"realm": string(s.Realm), "scope": s.Tag(), "limit_name": l.Name,
			"subject": sub, "cap": l.Max, "window": l.Window.String()})
	}
}

// publish emits a coalesced notification. It is best effort by design: a failed notification must
// never fail or slow the admission it describes, so it has its own short deadline and its error is
// dropped.
func (m *meter) publish(ctx context.Context, kind string, s domain.Scope, body map[string]any) {
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
	defer cancel()
	_ = m.d.Bus.Publish(ctx, m.cfg.SubjectPrefix+"."+kind+"."+s.SubjectToken(), b)
}
