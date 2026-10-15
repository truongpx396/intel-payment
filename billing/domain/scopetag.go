package domain

import (
	"fmt"
	"strings"

	md "github.com/truongpx396/intel-payment/metering/domain"
)

// ParseScopeTag reads a scope back from its Tag — <realm>/<kind>:<id> — which is how a bus message names one. The realm has no
// slash and the kind no colon, so the first slash and the first colon after it are the seams, and the id is everything left.
// It accepts only what Tag would write: a valid scope, with its realm spelled out.
func ParseScopeTag(tag string) (Scope, error) {
	realm, rest, ok := strings.Cut(tag, "/")
	if !ok {
		return Scope{}, fmt.Errorf("%w: %q is not a scope tag (<realm>/<kind>:<id>)", md.ErrInvalid, tag)
	}
	kind, id, ok := strings.Cut(rest, ":")
	if !ok {
		return Scope{}, fmt.Errorf("%w: %q is not a scope tag (<realm>/<kind>:<id>)", md.ErrInvalid, tag)
	}
	s := Scope{Realm: Realm(realm), Kind: kind, ID: id}
	if err := s.Validate(); err != nil {
		return Scope{}, err
	}
	if s.Tag() != tag {
		return Scope{}, fmt.Errorf("%w: %q is not the tag of the scope it names", md.ErrInvalid, tag)
	}
	return s, nil
}
