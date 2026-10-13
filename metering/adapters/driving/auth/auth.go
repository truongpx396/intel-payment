// Package auth is who is calling: a Principal — a caller id, the realms it may address and the
// privileges it holds — and the two checks every transport makes before a request reaches the engine.
//
// The service authenticates callers, not end users (docs/security.md). A credential is realm-bound and
// carries any of three privileges, separated at the RPC and the route so a compromised spend producer
// can debit with idempotency but never mint, move or reprice:
//
//	record  admit, record, read
//	grant   mint, remove and transfer credits
//	admin   configuration and operations
//
// They are independent: none implies another. How a credential becomes a Principal (a bearer token, a
// client certificate) belongs to the transport; what a Principal may do is decided here, once.
package auth

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Privilege is a bit; a Principal's privileges are their union.
type Privilege uint8

// The three privileges.
const (
	Record Privilege = 1 << iota
	Grant
	Admin
)

var names = []struct {
	p    Privilege
	name string
}{{Record, "record"}, {Grant, "grant"}, {Admin, "admin"}}

// Has reports whether p holds every privilege in q. The empty privilege is held by no one: a check that
// needs nothing is a check nobody wrote.
func (p Privilege) Has(q Privilege) bool { return q != 0 && p&q == q }

// String is the privileges joined with '+', as PAYMENT_SERVICE_TOKENS spells them.
func (p Privilege) String() string {
	var parts []string
	for _, n := range names {
		if p&n.p != 0 {
			parts = append(parts, n.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// ParsePrivileges reads "record", "grant+record", … An unknown name is an error, not an ignored word: a
// misspelt privilege must not quietly become a narrower — or, worse, a different — credential.
func ParsePrivileges(s string) (Privilege, error) {
	var out Privilege
	for _, word := range strings.Split(s, "+") {
		found := false
		for _, n := range names {
			if n.name == word {
				out, found = out|n.p, true
			}
		}
		if !found {
			return 0, fmt.Errorf("unknown privilege %q (want record, grant or admin, joined with '+')", word)
		}
	}
	return out, nil
}

// The ways a call is refused before it reaches the engine. Each maps to its own status in the wire
// package (UNAUTHENTICATED / 401, PERMISSION_DENIED forbidden / 403, PERMISSION_DENIED realm_mismatch / 403).
var (
	ErrUnauthenticated = errors.New("missing or invalid service credential")
	ErrForbidden       = errors.New("the credential lacks the privilege this call needs")
	ErrRealmMismatch   = errors.New("the credential does not cover this realm")
)

// Principal is an authenticated caller.
type Principal struct {
	CallerID string
	// Realms are the realms the credential may address. Empty covers none: a credential with no realm
	// can do nothing, never everything.
	Realms []domain.Realm
	// AnyRealm covers every realm. It exists for PAYMENT_SERVICE_AUTH=none, which the service allows only
	// on a loopback address or a Unix socket, where the network is the credential.
	AnyRealm   bool
	Privileges Privilege
}

// Require reports whether p holds need.
func (p Principal) Require(need Privilege) error {
	if !p.Privileges.Has(need) {
		return fmt.Errorf("%w: %s needs %s, the credential holds %s", ErrForbidden, p.CallerID, need, p.Privileges)
	}
	return nil
}

// Covers reports whether p may address realm r. The empty realm is the default realm.
func (p Principal) Covers(r domain.Realm) bool {
	if p.AnyRealm {
		return true
	}
	r = r.Or()
	for _, have := range p.Realms {
		if have.Or() == r {
			return true
		}
	}
	return false
}

// Resolve returns s with its realm bound to the credential. The realm is authenticated, never asserted:
// one outside the credential's set is ErrRealmMismatch, and one left out is the credential's realm when
// it has exactly one — and an error when it has several, because guessing which product to charge is how
// one product's spend lands on another's books.
func (p Principal) Resolve(s domain.Scope) (domain.Scope, error) {
	if s.Realm == "" {
		switch {
		case p.AnyRealm:
			s.Realm = domain.DefaultRealm
		case len(p.Realms) == 1:
			s.Realm = p.Realms[0].Or()
		default:
			return domain.Scope{}, fmt.Errorf("%w: the credential spans %d realms, so the scope must name one", domain.ErrInvalid, len(p.Realms))
		}
		return s, nil
	}
	if !p.Covers(s.Realm) {
		return domain.Scope{}, fmt.Errorf("%w: %q", ErrRealmMismatch, s.Realm)
	}
	return s, nil
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p. Only an authenticator calls it.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the caller the authenticator established. There is none for an unauthenticated
// request, and a handler treats that as ErrUnauthenticated — never as an allowance.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Caller is FromContext that fails closed.
func Caller(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

// Credential is what a transport lifts off a request for an Authenticator, whatever the transport: the
// bearer token of an Authorization header or gRPC metadata, and the client certificates of a verified TLS
// handshake. Verifying the TLS chain itself is the listener's job; an Authenticator maps a verified
// certificate to a Principal.
type Credential struct {
	Bearer           string
	PeerCertificates []*x509.Certificate
}

// Authenticator turns a Credential into a Principal. A credential it does not recognise is
// ErrUnauthenticated; any other error means the credential could not be checked (a store is down), which a
// transport reports as the service's own failure and never as a stranger's access.
type Authenticator interface {
	Authenticate(ctx context.Context, c Credential) (Principal, error)
}

// AuthenticatorFunc is an Authenticator in a function.
type AuthenticatorFunc func(ctx context.Context, c Credential) (Principal, error)

func (f AuthenticatorFunc) Authenticate(ctx context.Context, c Credential) (Principal, error) {
	return f(ctx, c)
}
