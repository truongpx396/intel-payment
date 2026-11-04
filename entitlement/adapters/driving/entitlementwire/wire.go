// Package entitlementwire is what the two doors of the entitlement layer — REST and gRPC — share: the conversion between the
// domain and the wire contract (api/proto/entitlement/v1), the way a call learns whose realm it is for, and the one table of what
// each failure is called. A decision is built here once, so the two doors cannot answer the same question in two ways.
package entitlementwire

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/truongpx396/intel-payment/api/entitlementv1"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// The three ways a credential fails a door.
var (
	// ErrUnauthenticated is a call no credential reached. The transports authenticate every call before a handler runs, so it is
	// a wiring slip — and a slip refuses, it never serves.
	ErrUnauthenticated = errors.New("missing or invalid service credential")
	// ErrRealmMismatch is a scope whose realm the credential does not cover.
	ErrRealmMismatch = errors.New("the credential does not cover this realm")
	// ErrForbidden is a credential without the privilege the call needs.
	ErrForbidden = errors.New("the credential does not hold the privilege this call needs")
)

// Caller says whose call this is. The transport authenticated it; this is how a door learns what that means for a scope, without
// importing the transport. Both doors only read, so the one privilege they need is `record`.
type Caller interface {
	// Resolve gives s the realm the credential means: its own when it has one and s names none, s's when the credential covers
	// it. A credential that spans several realms and a scope that names none is domain.ErrInvalid — a realm is never guessed —
	// a realm the credential does not cover is ErrRealmMismatch, a credential without the privilege is ErrForbidden, and a call
	// no credential reached is ErrUnauthenticated.
	Resolve(ctx context.Context, s md.Scope) (md.Scope, error)
}

// ScopeFromProto reads a wire scope. A nil scope is the empty one, which the Caller and then Validate refuse.
func ScopeFromProto(s *pb.Scope) md.Scope {
	if s == nil {
		return md.Scope{}
	}
	return md.Scope{Realm: md.Realm(s.GetRealm()), Kind: s.GetKind(), ID: s.GetId()}
}

// ResolveScope binds s to the credential.
func ResolveScope(ctx context.Context, c Caller, s md.Scope) (md.Scope, error) {
	scope, err := c.Resolve(ctx, s)
	if err != nil {
		return md.Scope{}, err
	}
	return scope.Normalized(), nil // whether it is a scope at all is the Entitler's to say, once, for every door
}

// ChecksFromProto reads a batch of questions.
func ChecksFromProto(qs []*pb.Query) []domain.CheckRequest {
	out := make([]domain.CheckRequest, len(qs))
	for i, q := range qs {
		out[i] = domain.CheckRequest{Key: domain.Key(q.GetKey())}
		if q.Current != nil {
			v := q.GetCurrent()
			out[i].Current = &v
		}
		if q.Delta != nil {
			v := q.GetDelta()
			out[i].Delta = &v
		}
	}
	return out
}

// ValueToProto is a value on the wire: absent when unset, and -1 kept as -1.
func ValueToProto(v domain.Value) *pb.Value {
	switch v.Kind {
	case domain.Flag:
		return &pb.Value{Kind: &pb.Value_Flag{Flag: v.Bool}}
	case domain.Quota:
		return &pb.Value{Kind: &pb.Value_Quota{Quota: v.Int}}
	case domain.Enum:
		return &pb.Value{Kind: &pb.Value_Enum{Enum: v.Text}}
	}
	return nil
}

// DecisionsToProto is a batch's answers, in order.
func DecisionsToProto(rs []domain.CheckResult) *pb.CheckResponse {
	out := &pb.CheckResponse{Decisions: make([]*pb.Decision, len(rs))}
	for i, r := range rs {
		out.Decisions[i] = &pb.Decision{
			Key: string(r.Key), Kind: string(r.Kind), Allowed: r.Allowed, Value: ValueToProto(r.Value),
			Limit: r.Limit, Current: r.Current, Remaining: r.Remaining, Reason: r.Reason, UpgradePath: r.UpgradePath,
		}
	}
	return out
}

// GrantsToProto is a scope's effective grants, in the order given (the Entitler orders them by key).
func GrantsToProto(gs []domain.Grant) *pb.ResolveResponse {
	out := &pb.ResolveResponse{Grants: make([]*pb.Grant, len(gs))}
	for i, g := range gs {
		out.Grants[i] = &pb.Grant{Key: string(g.Key), Kind: string(g.Value.Kind), Value: ValueToProto(g.Value), Source: string(g.Source), Reason: g.Reason}
		if g.ExpiresAt != nil {
			out.Grants[i].ExpiresAt = timestamppb.New(g.ExpiresAt.UTC())
		}
	}
	return out
}

// Condition is what a failure is called on each door.
type Condition struct {
	HTTP int
	Code string // the REST error code
	GRPC codes.Code
	// Message is what the caller is told. Verbatim keeps the whole error because the caller can act on it; otherwise it is the
	// sentinel's own text, because the chain names the topology (a dial address, a DSN) and belongs in the operator's log.
	Message string
}

// Classify says what an error is called. ok is false for what no row names — the service failing — and the door answers Internal
// with fixed text and logs the cause.
func Classify(err error) (c Condition, ok bool) {
	switch {
	case errors.Is(err, context.Canceled):
		return Condition{499, "cancelled", codes.Canceled, "the caller went away"}, true
	case errors.Is(err, context.DeadlineExceeded):
		return Condition{http.StatusGatewayTimeout, "deadline_exceeded", codes.DeadlineExceeded, "the deadline passed"}, true
	case errors.Is(err, ErrUnauthenticated):
		return Condition{http.StatusUnauthorized, "unauthenticated", codes.Unauthenticated, ErrUnauthenticated.Error()}, true
	case errors.Is(err, ErrRealmMismatch):
		return Condition{http.StatusForbidden, "realm_mismatch", codes.PermissionDenied, ErrRealmMismatch.Error()}, true
	case errors.Is(err, ErrForbidden):
		return Condition{http.StatusForbidden, "forbidden", codes.PermissionDenied, ErrForbidden.Error()}, true
	case errors.Is(err, domain.ErrInvalid):
		return Condition{http.StatusBadRequest, "invalid_request", codes.InvalidArgument, err.Error()}, true
	case errors.Is(err, domain.ErrUnavailable):
		return Condition{http.StatusServiceUnavailable, "unavailable", codes.Unavailable, "entitlements are unavailable"}, true
	}
	return Condition{}, false
}

// Unavailable is the gRPC code of entitlements that could not be resolved.
const Unavailable = codes.Unavailable

// Internal is the condition of a failure no row names.
var Internal = Condition{http.StatusInternalServerError, "internal", codes.Internal, "internal error"}

// Invalid is a request that is not what was expected.
func Invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalid, fmt.Sprintf(format, a...))
}
