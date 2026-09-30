package domain

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Scope is the opaque metering/billing subject WITHIN a realm. The HOST decides what it means
// (workspace, organization, user, project, api_key); the kernel treats it only as an identity.
// Parameterizing this is what makes a re-anchor a config change, not a migration (invariant 10).
type Scope struct {
	Realm Realm  // defaults to DefaultRealm when empty
	Kind  string // host-defined: "workspace" | "organization" | "user" | "project" | …
	ID    string // opaque, stable
}

// Shard is fnv1a64(Scope.Tag()) mod Shards — the unit of Redis Cluster placement and of writer
// ownership. The count is recorded in hot_config and changed only by a reshard.
type Shard int

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

const maxScopeIDLen = 256

// Tag is the scope's IDENTITY — the stable name used in keys, intents and subjects. It is not
// its PLACEMENT: every Redis key of a scope is hash-tagged by its shard (see Shard), because a
// Redis Cluster script may touch only one slot. The realm is inside the tag, so two products never
// share a balance, an intent or an idempotency guard.
func (s Scope) Tag() string {
	return string(s.Realm.Or()) + "/" + s.Kind + ":" + s.ID
}

// SubjectToken is Tag made safe to sit inside a bus subject: the characters a subject reserves (its
// token separator, its wildcards, whitespace) and the escape itself are percent-encoded, so a scope id
// with a dot or a space in it can never split into two tokens or be read as a wildcard. It is what
// subjects such as billing.warn.<token> are built from; the Tag stays the scope's identity.
func (s Scope) SubjectToken() string {
	tag := s.Tag()
	var b strings.Builder
	b.Grow(len(tag))
	for i := 0; i < len(tag); i++ {
		switch c := tag[i]; c {
		case '.', '*', '>', '%', ' ', '\t', '\n', '\r':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Normalized returns s with the realm defaulted.
func (s Scope) Normalized() Scope {
	s.Realm = s.Realm.Or()
	return s
}

// Shard places s among n shards. n must be positive.
func (s Scope) Shard(n int) Shard {
	if n < 1 {
		panic("domain: Shard needs a positive shard count")
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s.Tag()))
	return Shard(h.Sum64() % uint64(n)) //nolint:gosec // n >= 1 above, and the result is < n so it fits an int
}

// Validate refuses a scope whose Tag would be ambiguous. The Kind carries neither ':' nor '/', so
// {kind "a:b", id "c"} can never collide with {kind "a", id "b:c"} — two hosts, one balance.
func (s Scope) Validate() error {
	if err := s.Realm.Validate(); err != nil {
		return err
	}
	if !kindPattern.MatchString(s.Kind) {
		return fmt.Errorf("%w: scope kind %q must match %s", ErrInvalid, s.Kind, kindPattern)
	}
	if s.ID == "" {
		return fmt.Errorf("%w: scope id is required", ErrInvalid)
	}
	if len(s.ID) > maxScopeIDLen || !utf8.ValidString(s.ID) {
		return fmt.Errorf("%w: scope id must be valid UTF-8 of at most %d bytes", ErrInvalid, maxScopeIDLen)
	}
	for _, r := range s.ID {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: scope id must not contain control characters", ErrInvalid)
		}
	}
	return nil
}
