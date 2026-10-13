package grpcclient_test

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The compile-time half of FR-044: business code depends on ports.Meter, and cannot tell the client from
// the engine.
var (
	_ ports.Meter         = (*grpcclient.Client)(nil)
	_ ports.BalanceReader = (*grpcclient.Client)(nil)
)

// fakeServer is a metering.v1 server the TEST owns. It answers with statuses built by hand, from the
// contract, so the client is held to grpc-surface.md and not to the real server's table.
type fakeServer struct {
	meteringv1.UnimplementedMeteringServer
	calls atomic.Int64

	admit    func(context.Context, *meteringv1.AdmitRequest) (*meteringv1.Admission, error)
	record   func(context.Context, *meteringv1.RecordRequest) (*meteringv1.Receipt, error)
	grant    func(context.Context, *meteringv1.GrantRequest) (*meteringv1.Receipt, error)
	transfer func(context.Context, *meteringv1.TransferRequest) (*meteringv1.TransferReply, error)
	balance  func(context.Context, *meteringv1.BalanceRequest) (*meteringv1.BalanceReply, error)

	mu       sync.Mutex
	gotAdmit *meteringv1.AdmitRequest
	gotRec   *meteringv1.RecordRequest
	gotGrant *meteringv1.GrantRequest
	gotXfer  *meteringv1.TransferRequest
	gotBal   *meteringv1.BalanceRequest
	md       metadata.MD
}

func (f *fakeServer) saw(ctx context.Context) {
	f.calls.Add(1)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		f.mu.Lock()
		f.md = md
		f.mu.Unlock()
	}
}

func (f *fakeServer) Admit(ctx context.Context, r *meteringv1.AdmitRequest) (*meteringv1.Admission, error) {
	f.saw(ctx)
	f.mu.Lock()
	f.gotAdmit = proto.Clone(r).(*meteringv1.AdmitRequest)
	f.mu.Unlock()
	if f.admit == nil {
		return &meteringv1.Admission{Allowed: true}, nil
	}
	return f.admit(ctx, r)
}

func (f *fakeServer) Record(ctx context.Context, r *meteringv1.RecordRequest) (*meteringv1.Receipt, error) {
	f.saw(ctx)
	f.mu.Lock()
	f.gotRec = proto.Clone(r).(*meteringv1.RecordRequest)
	f.mu.Unlock()
	if f.record == nil {
		return &meteringv1.Receipt{Applied: true}, nil
	}
	return f.record(ctx, r)
}

func (f *fakeServer) Grant(ctx context.Context, r *meteringv1.GrantRequest) (*meteringv1.Receipt, error) {
	f.saw(ctx)
	f.mu.Lock()
	f.gotGrant = proto.Clone(r).(*meteringv1.GrantRequest)
	f.mu.Unlock()
	if f.grant == nil {
		return &meteringv1.Receipt{Applied: true}, nil
	}
	return f.grant(ctx, r)
}

func (f *fakeServer) Transfer(ctx context.Context, r *meteringv1.TransferRequest) (*meteringv1.TransferReply, error) {
	f.saw(ctx)
	f.mu.Lock()
	f.gotXfer = proto.Clone(r).(*meteringv1.TransferRequest)
	f.mu.Unlock()
	if f.transfer == nil {
		return &meteringv1.TransferReply{Applied: true}, nil
	}
	return f.transfer(ctx, r)
}

func (f *fakeServer) GetBalance(ctx context.Context, r *meteringv1.BalanceRequest) (*meteringv1.BalanceReply, error) {
	f.saw(ctx)
	f.mu.Lock()
	f.gotBal = proto.Clone(r).(*meteringv1.BalanceRequest)
	f.mu.Unlock()
	if f.balance == nil {
		return &meteringv1.BalanceReply{}, nil
	}
	return f.balance(ctx, r)
}

// dial serves f over an in-memory connection and returns a Client on it.
func dial(t *testing.T, f *fakeServer, opts ...grpc.DialOption) *grpcclient.Client {
	t.Helper()
	cl, err := tryDial(t, f, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// tryDial is dial for a connection that may legitimately be refused at creation.
func tryDial(t *testing.T, f *fakeServer, opts ...grpc.DialOption) (*grpcclient.Client, error) {
	t.Helper()
	g := grpc.NewServer()
	meteringv1.RegisterMeteringServer(g, f)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet", append([]grpc.DialOption{
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)...)
	t.Cleanup(func() {
		if conn != nil {
			_ = conn.Close()
		}
		g.Stop()
		_ = lis.Close()
	})
	if err != nil {
		return nil, err
	}
	return grpcclient.New(conn), nil
}

// wireStatus builds a status the way the contract says a server writes it: the code, and an ErrorInfo
// whose domain is metering.v1 and whose reason names the condition. Written out here so a client that
// agrees with a wrong server is still wrong.
func wireStatus(code codes.Code, reason, msg string, meta map[string]string, admission ...*meteringv1.Admission) error {
	st := status.New(code, msg)
	with, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "metering.v1", Metadata: meta})
	if err != nil {
		panic(err)
	}
	for _, a := range admission {
		if with, err = with.WithDetails(a); err != nil {
			panic(err)
		}
	}
	return with.Err()
}

var ws = domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}

func event() domain.Event {
	return domain.Event{
		Scope: ws, Resource: "llm.chat", RateKey: "gpt", IdemKey: "evt-1", OccurredAt: mustTime("2026-09-29T12:00:00.123456789Z"),
		Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}},
	}
}

func mustTime(rfc3339 string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		panic(err)
	}
	return t
}

// await receives from ch or fails the test: a client that never reaches the server must fail a test, not hang it.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
