package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// OverrideDeps is what Overrides is built from.
type OverrideDeps struct {
	Store ports.EntitlementStore
	// Bus, with the deployment's SubjectPrefix, announces each write on <prefix>.entitlement.<tag> so that caches evict the scope
	// now and not at their TTL (FR-035). Optional: without it nothing is announced, which is right only for a process that
	// caches nothing.
	Bus           mports.Bus
	SubjectPrefix string
}

// Overrides writes a scope's exceptions: a sales deal, a trial extension, a comp account. It is the use case an admin API calls,
// and it refuses at WRITE time what the Entitler would otherwise have to skip at read time — an override that says neither who
// nor why (invariant 5), or that gives a key a value it cannot hold — because a bad value that reaches the table is a bad value
// in every cache until someone finds it.
type Overrides struct{ d OverrideDeps }

// NewOverrides builds the use case.
func NewOverrides(d OverrideDeps) (*Overrides, error) {
	if d.Store == nil {
		return nil, errors.New("entitlement: an EntitlementStore is required")
	}
	if d.Bus != nil && d.SubjectPrefix == "" {
		return nil, errors.New("entitlement: announcing overrides needs the deployment's subject prefix")
	}
	return &Overrides{d: d}, nil
}

// Put writes g as actor's override. The key must be one the scope's realm declares, and the value one that key can hold.
func (o *Overrides) Put(ctx context.Context, g domain.Grant, actor string) error {
	g.Scope = g.Scope.Normalized()
	if err := g.ValidateOverride(actor); err != nil {
		return err
	}
	decls, err := o.d.Store.Keys(ctx, g.Scope.Realm)
	if err != nil {
		return fmt.Errorf("%w: reading the declared keys: %w", domain.ErrUnavailable, err)
	}
	var decl *domain.KeyDecl
	for i := range decls {
		if decls[i].Key == g.Key {
			decl = &decls[i]
		}
	}
	if decl == nil {
		return fmt.Errorf("%w: key %q is not declared in realm %q", domain.ErrInvalid, string(g.Key), string(g.Scope.Realm))
	}
	if err := decl.Check(g.Value); err != nil {
		return err
	}
	if err := o.d.Store.PutOverride(ctx, g, actor); err != nil {
		if errors.Is(err, domain.ErrInvalid) {
			return err
		}
		return fmt.Errorf("%w: writing the override: %w", domain.ErrUnavailable, err)
	}
	return o.announce(ctx, g.Scope)
}

// overrideNote is the body of <prefix>.entitlement.<tag>, in the shape the payment layer publishes it.
type overrideNote struct {
	Realm  string   `json:"realm"`
	Scope  scopeRef `json:"scope"`
	Reason string   `json:"reason"`
}

type scopeRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// announce tells every cache the scope's entitlements changed. The write is the truth and the announcement is the hurry: when
// it cannot be made the caller is told — the write is repeatable, and repeating it announces — while the override is already in
// the store and caches pick it up at their TTL.
func (o *Overrides) announce(ctx context.Context, s md.Scope) error {
	if o.d.Bus == nil {
		return nil
	}
	body, err := json.Marshal(overrideNote{Realm: string(s.Realm), Scope: scopeRef{s.Kind, s.ID}, Reason: "override"})
	if err != nil {
		return fmt.Errorf("%w: encoding the announcement: %w", domain.ErrUnavailable, err)
	}
	if err := o.d.Bus.Publish(ctx, o.d.SubjectPrefix+".entitlement."+s.SubjectToken(), body); err != nil {
		return fmt.Errorf("%w: the override is written but its announcement failed (caches pick it up at their TTL; repeating the write announces it): %w", domain.ErrUnavailable, err)
	}
	return nil
}
