package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// Credentials are configured as a comma-separated list of
//
//	<caller-id>:<proof>:<realms>:<privileges>
//
// where the proof is the SHA-256 of the bearer token (PAYMENT_SERVICE_TOKENS) or the common name of a client
// certificate (PAYMENT_SERVICE_CERTS), realms are joined with '+', and privileges are record, grant and admin
// joined with '+'. A mistake stops the service at start-up: a credential that quietly means something other
// than what was written is a credential nobody reviewed.

type entry struct {
	callerID string
	proof    string // the digest (decoded) or the subject, per kind
	digest   [sha256.Size]byte
	realms   []domain.Realm
	privs    Privilege
}

func (e entry) principal() Principal {
	return Principal{CallerID: e.callerID, Realms: append([]domain.Realm(nil), e.realms...), Privileges: e.privs}
}

// parseEntries reads the list, calling proof on each entry's second field.
func parseEntries(spec, what string, proof func(*entry, string) error) ([]entry, error) {
	var out []entry
	seenCaller := map[string]bool{}
	seenProof := map[string]string{}
	for i, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		f := strings.Split(raw, ":")
		if len(f) != 4 {
			// By position, never by content: an entry that is not shaped like one may be a bare token, and a
			// refusal is logged and pasted into tickets.
			return nil, fmt.Errorf("%s: entry %d needs four fields, <caller-id>:<proof>:<realms>:<privileges>", what, i+1)
		}
		e := entry{callerID: f[0]}
		if e.callerID == "" || strings.ContainsAny(e.callerID, " \t") {
			return nil, fmt.Errorf("%s: a caller id is required and has no whitespace, got %q", what, f[0])
		}
		if seenCaller[e.callerID] {
			return nil, fmt.Errorf("%s: caller %q is listed twice", what, e.callerID)
		}
		seenCaller[e.callerID] = true
		if err := proof(&e, f[1]); err != nil {
			return nil, fmt.Errorf("%s: caller %q: %w", what, e.callerID, err)
		}
		if other, dup := seenProof[e.proof]; dup {
			return nil, fmt.Errorf("%s: callers %q and %q share one credential, so which of them is calling could not be told", what, other, e.callerID)
		}
		seenProof[e.proof] = e.callerID
		if f[2] == "" {
			return nil, fmt.Errorf("%s: caller %q names no realm, so it could address none", what, e.callerID)
		}
		for _, r := range strings.Split(f[2], "+") {
			if r == "" || r == "*" || domain.Realm(r).Validate() != nil {
				return nil, fmt.Errorf("%s: caller %q: %q is not a realm slug (a credential names the realms it may address, never a wildcard)", what, e.callerID, r)
			}
			e.realms = append(e.realms, domain.Realm(r))
		}
		privs, err := ParsePrivileges(f[3])
		if err != nil {
			return nil, fmt.Errorf("%s: caller %q: %w", what, e.callerID, err)
		}
		e.privs = privs
		out = append(out, e)
	}
	return out, nil
}

// HashToken is the stored form of a bearer token: its SHA-256 as lowercase hex. The configuration holds
// this, never the token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type tokens struct{ entries []entry }

// ParseTokens reads PAYMENT_SERVICE_TOKENS. An empty list is valid and refuses everyone: a service with no
// callers is closed, not open.
func ParseTokens(spec string) (Authenticator, error) {
	es, err := parseEntries(spec, "service tokens", func(e *entry, field string) error {
		raw, err := hex.DecodeString(field)
		if err != nil || len(raw) != sha256.Size {
			return fmt.Errorf("the second field must be the token's SHA-256 as 64 hex digits (is the token itself in the configuration?)")
		}
		copy(e.digest[:], raw)
		e.proof = hex.EncodeToString(raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &tokens{entries: es}, nil
}

// Authenticate compares the token's digest with every stored one, in constant time and without stopping at
// the first match, so how long it takes says nothing about which entries exist.
func (t *tokens) Authenticate(_ context.Context, c Credential) (Principal, error) {
	if c.Bearer == "" {
		return Principal{}, ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(c.Bearer))
	var found *entry
	for i := range t.entries {
		if subtle.ConstantTimeCompare(sum[:], t.entries[i].digest[:]) == 1 {
			found = &t.entries[i]
		}
	}
	if found == nil {
		return Principal{}, ErrUnauthenticated
	}
	return found.principal(), nil
}

type certs struct{ entries []entry }

// ParseCertSubjects reads PAYMENT_SERVICE_CERTS. The listener verifies the certificate chain against the
// private CA; this maps the verified leaf's subject common name to a caller.
func ParseCertSubjects(spec string) (Authenticator, error) {
	es, err := parseEntries(spec, "service certificates", func(e *entry, subject string) error {
		if subject == "" {
			return fmt.Errorf("the second field must be the certificate subject's common name")
		}
		e.proof = subject
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &certs{entries: es}, nil
}

func (c *certs) Authenticate(_ context.Context, cred Credential) (Principal, error) {
	if len(cred.PeerCertificates) == 0 {
		return Principal{}, ErrUnauthenticated
	}
	cn := cred.PeerCertificates[0].Subject.CommonName // the leaf is first
	for i := range c.entries {
		if c.entries[i].proof == cn {
			return c.entries[i].principal(), nil
		}
	}
	return Principal{}, ErrUnauthenticated
}

// None is no authentication: every caller is everyone. It is permitted only where the network is the
// credential — a loopback address or a Unix socket — and the service refuses to start with it on a routable one.
func None() Authenticator {
	return AuthenticatorFunc(func(context.Context, Credential) (Principal, error) {
		return Principal{CallerID: "anonymous", AnyRealm: true, Privileges: Record | Grant | Admin}, nil
	})
}
