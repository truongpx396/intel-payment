package app

import (
	"sort"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// inputs is everything resolution reads: what the store held, and the instant it was asked.
type inputs struct {
	scope     md.Scope
	now       time.Time
	decls     []domain.KeyDecl
	defaults  []domain.Entitlement
	sub       *domain.Subscription // the scope's live subscription, if any
	plan      []domain.Entitlement // that plan's entitlements; read only when the subscription holds
	overrides []domain.Grant
}

// resolution is what a scope is entitled to at an instant.
type resolution struct {
	scope  md.Scope
	decls  map[domain.Key]domain.KeyDecl
	grants map[domain.Key]domain.Grant
	// plan is the plan that holds, or "" when the scope is on the free tier. Refusals name it.
	plan string
	// invalid counts the stored values skipped because their key cannot hold them, by source.
	invalid map[domain.Source]int
}

// candidate is one level's value for one key.
type candidate struct {
	value   domain.Value
	expires *time.Time
	reason  string
}

// resolve applies the one precedence: override > subscription > default. Within a level two values for one key are merged
// (flag OR, quota maximum with unlimited above every number, enum by the key's declared order), so what a migration between
// plans leaves behind is deterministic. A value its key cannot hold — an undeclared key, the wrong kind, an enum member nobody
// ranked — grants nothing and is counted, so the level below answers instead.
func resolve(in inputs) *resolution {
	r := &resolution{
		scope: in.scope, decls: make(map[domain.Key]domain.KeyDecl, len(in.decls)), grants: map[domain.Key]domain.Grant{},
		invalid: map[domain.Source]int{},
	}
	for _, d := range in.decls {
		r.decls[d.Key] = d
	}

	levels := map[domain.Source]map[domain.Key]candidate{
		domain.FromDefault:      r.collect(domain.FromDefault, entitlementItems(in.defaults, nil, ""), in.now),
		domain.FromSubscription: nil,
		domain.FromOverride:     nil,
	}
	if in.sub != nil && in.sub.Holds(in.now) {
		r.plan = in.sub.PlanCode
		var until *time.Time
		if in.sub.Status == domain.PastDue {
			until = in.sub.GraceUntil // held by grace: it ends when the window does
		}
		levels[domain.FromSubscription] = r.collect(domain.FromSubscription, entitlementItems(in.plan, until, ""), in.now)
	}
	levels[domain.FromOverride] = r.collect(domain.FromOverride, grantItems(in.overrides), in.now)

	for k := range r.decls {
		for _, src := range []domain.Source{domain.FromOverride, domain.FromSubscription, domain.FromDefault} {
			if c, ok := levels[src][k]; ok {
				r.grants[k] = domain.Grant{Scope: in.scope, Key: k, Value: c.value, Source: src, ExpiresAt: c.expires, Reason: c.reason}
				break
			}
		}
	}
	return r
}

type item struct {
	key     domain.Key
	value   domain.Value
	expires *time.Time
	reason  string
}

func entitlementItems(es []domain.Entitlement, until *time.Time, reason string) []item {
	out := make([]item, len(es))
	for i, e := range es {
		out[i] = item{key: e.Key, value: e.Value, expires: until, reason: reason}
	}
	return out
}

func grantItems(gs []domain.Grant) []item {
	out := make([]item, len(gs))
	for i, g := range gs {
		out[i] = item{key: g.Key, value: g.Value, expires: g.ExpiresAt, reason: g.Reason}
	}
	return out
}

// collect merges one level's items per key. An override that has expired is not an item at all.
func (r *resolution) collect(src domain.Source, items []item, now time.Time) map[domain.Key]candidate {
	out := map[domain.Key]candidate{}
	for _, it := range items {
		if it.expires != nil && src == domain.FromOverride && !now.Before(*it.expires) {
			continue
		}
		d, declared := r.decls[it.key]
		if !declared || d.Check(it.value) != nil {
			r.invalid[src]++
			continue
		}
		have, dup := out[it.key]
		if !dup {
			out[it.key] = candidate{value: it.value, expires: it.expires, reason: it.reason}
			continue
		}
		merged, err := d.Merge(have.value, it.value)
		if err != nil { // both were checked above; a failure here is a defect, and it grants nothing
			r.invalid[src]++
			delete(out, it.key)
			continue
		}
		if merged != have.value { // the newcomer won: its lifetime and its reason are the grant's
			have = candidate{expires: it.expires, reason: it.reason}
		}
		have.value = merged
		out[it.key] = have
	}
	return out
}

// sorted lists the grants ordered by key, so that a plan card is stable.
func (r *resolution) sorted() []domain.Grant {
	out := make([]domain.Grant, 0, len(r.grants))
	for _, g := range r.grants {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
