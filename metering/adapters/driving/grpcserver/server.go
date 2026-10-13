// Package grpcserver serves ports.Meter over gRPC: the same engine, behind the metering.v1 wire contract
// (grpc-surface.md). It is a thin shell and decides nothing about money. For each call it makes the three
// checks only a transport can make — who is calling, what they may do, which realm they may address —
// refuses what the contract says is invalid before the engine sees it, converts, and reports the outcome
// as the fixed status of the wire package.
package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/wire"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Server implements meteringv1.MeteringServer.
type Server struct {
	meteringv1.UnimplementedMeteringServer
	meter   ports.Meter
	balance ports.BalanceReader
	log     *slog.Logger
}

// Option configures a Server.
type Option func(*Server)

// WithLogger sets where the operator's side of a failure goes. The default is slog.Default().
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// New returns a server in front of meter. Both are required: a server that could not read a balance, or
// could not meter, is a server that answers some questions and fails the rest at run time.
func New(meter ports.Meter, balance ports.BalanceReader, opts ...Option) (*Server, error) {
	if meter == nil || balance == nil {
		return nil, errors.New("grpcserver: a Meter and a BalanceReader are required")
	}
	s := &Server{meter: meter, balance: balance, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Register attaches the service to a gRPC server.
func (s *Server) Register(r grpc.ServiceRegistrar) { meteringv1.RegisterMeteringServer(r, s) }

var _ meteringv1.MeteringServer = (*Server)(nil)

// Admit gates before expensive work. A refusal is an answer, so it travels as the contract's status with
// the whole Admission beside it; a client turns it back into the (Admission, nil) the in-process Meter returns.
func (s *Server) Admit(ctx context.Context, req *meteringv1.AdmitRequest) (_ *meteringv1.Admission, err error) {
	defer s.guard("Admit", &err)
	p, err := s.caller(ctx, auth.Record)
	if err != nil {
		return nil, s.fail(ctx, "Admit", err)
	}
	if req.GetScope() == nil {
		return nil, s.fail(ctx, "Admit", required("scope"))
	}
	ar, err := wire.AdmitRequestFromProto(req)
	if err != nil {
		return nil, s.fail(ctx, "Admit", err)
	}
	scope, err := p.Resolve(wire.ScopeFromProto(req.GetScope()))
	if err != nil {
		return nil, s.fail(ctx, "Admit", err)
	}
	adm, err := s.meter.Admit(ctx, scope, ar)
	if err != nil {
		return nil, s.fail(ctx, "Admit", err)
	}
	if adm.Allowed {
		return wire.AdmissionToProto(adm), nil
	}
	st, ok := wire.DenialStatus(adm)
	if !ok {
		return nil, s.fail(ctx, "Admit", fmt.Errorf("the engine refused an admission without naming the ceiling: %+v", adm))
	}
	return nil, st.Err()
}

// Record prices and settles. A deferred Record is OK: it succeeded, and has not been applied yet.
func (s *Server) Record(ctx context.Context, req *meteringv1.RecordRequest) (_ *meteringv1.Receipt, err error) {
	defer s.guard("Record", &err)
	p, err := s.caller(ctx, auth.Record)
	if err != nil {
		return nil, s.fail(ctx, "Record", err)
	}
	ev := req.GetEvent()
	switch {
	case ev == nil:
		return nil, s.fail(ctx, "Record", required("event"))
	case ev.GetScope() == nil:
		return nil, s.fail(ctx, "Record", required("event.scope"))
	// A server-generated key makes every retry a new charge, and a defaulted time makes a missing one a
	// very stale event: both are refused here, by name, whatever the engine would do.
	case ev.GetIdemKey() == "":
		return nil, s.fail(ctx, "Record", required("event.idem_key"))
	case ev.GetOccurredAt() == nil:
		return nil, s.fail(ctx, "Record", required("event.occurred_at"))
	}
	e, err := wire.EventFromProto(ev)
	if err != nil {
		return nil, s.fail(ctx, "Record", err)
	}
	if e.Scope, err = p.Resolve(e.Scope); err != nil {
		return nil, s.fail(ctx, "Record", err)
	}
	r, err := s.meter.Record(ctx, e)
	if err != nil {
		return nil, s.fail(ctx, "Record", err)
	}
	return wire.ReceiptToProto(r), nil
}

// GetBalance reads the hot balance per pool.
func (s *Server) GetBalance(ctx context.Context, req *meteringv1.BalanceRequest) (_ *meteringv1.BalanceReply, err error) {
	defer s.guard("GetBalance", &err)
	p, err := s.caller(ctx, auth.Record)
	if err != nil {
		return nil, s.fail(ctx, "GetBalance", err)
	}
	if req.GetScope() == nil {
		return nil, s.fail(ctx, "GetBalance", required("scope"))
	}
	scope, err := p.Resolve(wire.ScopeFromProto(req.GetScope()))
	if err != nil {
		return nil, s.fail(ctx, "GetBalance", err)
	}
	pools, err := s.balance.Balance(ctx, scope)
	if err != nil {
		return nil, s.fail(ctx, "GetBalance", err)
	}
	out := &meteringv1.BalanceReply{Pools: make(map[string]int64, len(pools))}
	var total domain.Credits
	for pool, v := range pools {
		out.Pools[string(pool)] = int64(v)
		// Money is never allowed to wrap: a total that would is the server's fault, and an error.
		if total, err = total.Add(v); err != nil {
			return nil, s.fail(ctx, "GetBalance", fmt.Errorf("balance total: %v", err)) //nolint:errorlint // deliberately not wrapped: ErrAmountOutOfRange would blame the caller
		}
	}
	out.Total = int64(total)
	return out, nil
}

// Grant mints or removes credits. PRIVILEGED: a credential that holds only `record` is refused here.
func (s *Server) Grant(ctx context.Context, req *meteringv1.GrantRequest) (_ *meteringv1.Receipt, err error) {
	defer s.guard("Grant", &err)
	p, err := s.caller(ctx, auth.Grant)
	if err != nil {
		return nil, s.fail(ctx, "Grant", err)
	}
	switch {
	case req.GetScope() == nil:
		return nil, s.fail(ctx, "Grant", required("scope"))
	case req.GetIdemKey() == "":
		return nil, s.fail(ctx, "Grant", required("idem_key"))
	}
	g, err := wire.GrantFromProto(req)
	if err != nil {
		return nil, s.fail(ctx, "Grant", err)
	}
	if g.Scope, err = p.Resolve(g.Scope); err != nil {
		return nil, s.fail(ctx, "Grant", err)
	}
	r, err := s.meter.Grant(ctx, g)
	if err != nil {
		return nil, s.fail(ctx, "Grant", err)
	}
	return wire.ReceiptToProto(r), nil
}

// Transfer moves credits pool → allocation. PRIVILEGED, and both ends must be in a realm the credential covers.
func (s *Server) Transfer(ctx context.Context, req *meteringv1.TransferRequest) (_ *meteringv1.TransferReply, err error) {
	defer s.guard("Transfer", &err)
	p, err := s.caller(ctx, auth.Grant)
	if err != nil {
		return nil, s.fail(ctx, "Transfer", err)
	}
	switch {
	case req.GetFrom() == nil:
		return nil, s.fail(ctx, "Transfer", required("from"))
	case req.GetTo() == nil:
		return nil, s.fail(ctx, "Transfer", required("to"))
	case req.GetIdemKey() == "":
		return nil, s.fail(ctx, "Transfer", required("idem_key"))
	}
	t := wire.TransferFromProto(req)
	if t.From, err = p.Resolve(t.From); err != nil {
		return nil, s.fail(ctx, "Transfer", err)
	}
	if t.To, err = p.Resolve(t.To); err != nil {
		return nil, s.fail(ctx, "Transfer", err)
	}
	r, err := s.meter.Transfer(ctx, t)
	if err != nil {
		return nil, s.fail(ctx, "Transfer", err)
	}
	return wire.TransferReceiptToProto(r), nil
}

var errPanicked = errors.New("handler panicked")

func required(field string) error { return fmt.Errorf("%w: %s is required", domain.ErrInvalid, field) }

// caller authenticates and authorises in one step: no principal is unauthenticated, and a principal without
// the privilege is forbidden — before anything about the request is read, so a stranger learns nothing of it.
func (s *Server) caller(ctx context.Context, need auth.Privilege) (auth.Principal, error) {
	p, err := auth.Caller(ctx)
	if err != nil {
		return auth.Principal{}, err
	}
	return p, p.Require(need)
}

// fail turns an error into the call's status. The caller's own cancellation wins over whatever the engine
// made of it; everything an operator would want is logged here, whole, and none of it is put on the wire
// unless the contract says the caller may read it.
func (s *Server) fail(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	st := wire.Status(err)
	switch st.Code() { //nolint:exhaustive // only the codes an operator must see are logged
	case codes.Internal, codes.Unavailable:
		level := slog.LevelWarn
		if st.Code() == codes.Internal {
			level = slog.LevelError
		}
		s.log.Log(ctx, level, "metering call failed", "op", op, "code", st.Code().String(), "error", err)
	}
	return st.Err()
}

// guard turns a panic in a handler into one INTERNAL call. gRPC does not recover for us, and a Tier-0
// service that dies on one bad request has turned that request into an outage.
func (s *Server) guard(op string, err *error) {
	if r := recover(); r != nil {
		s.log.Error("metering call panicked", "op", op, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		*err = wire.Status(errPanicked).Err()
	}
}
