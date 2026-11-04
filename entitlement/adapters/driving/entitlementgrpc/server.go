// Package entitlementgrpc is the gRPC door of the entitlement layer: entitlement.v1.Entitlement, Check and Resolve. It answers
// from the same ports as the REST door, through the same conversions and the same table of what each failure is called
// (entitlementwire), so a question asked of either gets the same decision. It learns whose realm a call is for through a Caller;
// the binary's gRPC server has already authenticated the call.
package entitlementgrpc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	pb "github.com/truongpx396/intel-payment/api/entitlementv1"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	"github.com/truongpx396/intel-payment/entitlement/ports"
)

// ErrorDomain is the domain of the ErrorInfo detail every failed call carries: its reason is the REST error code, so a client
// that speaks both finds one vocabulary.
const ErrorDomain = "entitlement.intel-payment"

// Config is what the server is built from.
type Config struct {
	Entitler ports.Entitler // Resolve
	Checker  ports.Checker  // Check
	// Caller resolves realms from the credential the gRPC server's interceptor established. Required.
	Caller entitlementwire.Caller
	// Logger receives the failures an operator must act on, with their cause. Default slog.Default().
	Logger *slog.Logger
}

// Server implements entitlement.v1.Entitlement.
type Server struct {
	pb.UnimplementedEntitlementServer
	cfg Config
}

var _ pb.EntitlementServer = (*Server)(nil)

// New builds the server.
func New(cfg Config) (*Server, error) {
	if cfg.Caller == nil || cfg.Entitler == nil || cfg.Checker == nil {
		return nil, errors.New("entitlementgrpc: an Entitler, a Checker and a Caller are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{cfg: cfg}, nil
}

// Register adds the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) { pb.RegisterEntitlementServer(r, s) }

// fail is the status of a failed call: the code of its row, an ErrorInfo whose reason is the REST error code, and a message that is
// the whole error where the caller can act on it. What no row names is Internal with fixed text, and the cause goes to the log.
func (s *Server) fail(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	c, ok := entitlementwire.Classify(err)
	if !ok {
		s.cfg.Logger.Log(ctx, slog.LevelError, "entitlement call failed", "op", op, "error", err)
		c = entitlementwire.Internal
	} else if c.GRPC == entitlementwire.Unavailable {
		s.cfg.Logger.Log(ctx, slog.LevelError, "entitlements could not be resolved", "op", op, "error", err)
	}
	st := status.New(c.GRPC, c.Message)
	// A detail that cannot be attached leaves a status with the right code, which is the part a caller cannot do without.
	if with, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: c.Code, Domain: ErrorDomain}); derr == nil {
		st = with
	}
	return st.Err()
}

// Check implements entitlement.v1.Entitlement.
func (s *Server) Check(ctx context.Context, req *pb.CheckRequest) (*pb.CheckResponse, error) {
	scope, err := entitlementwire.ResolveScope(ctx, s.cfg.Caller, entitlementwire.ScopeFromProto(req.GetScope()))
	if err != nil {
		return nil, s.fail(ctx, "check", err)
	}
	results, err := s.cfg.Checker.Check(ctx, scope, entitlementwire.ChecksFromProto(req.GetChecks()))
	if err != nil {
		return nil, s.fail(ctx, "check", err)
	}
	return entitlementwire.DecisionsToProto(results), nil
}

// Resolve implements entitlement.v1.Entitlement.
func (s *Server) Resolve(ctx context.Context, req *pb.ResolveRequest) (*pb.ResolveResponse, error) {
	scope, err := entitlementwire.ResolveScope(ctx, s.cfg.Caller, entitlementwire.ScopeFromProto(req.GetScope()))
	if err != nil {
		return nil, s.fail(ctx, "resolve", err)
	}
	grants, err := s.cfg.Entitler.Resolve(ctx, scope)
	if err != nil {
		return nil, s.fail(ctx, "resolve", err)
	}
	return entitlementwire.GrantsToProto(grants), nil
}
