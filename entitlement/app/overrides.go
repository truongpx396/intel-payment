package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
)

// OverrideDeps is what Overrides is built from.
type OverrideDeps struct {
	Store ports.EntitlementStore
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
	return nil
}
