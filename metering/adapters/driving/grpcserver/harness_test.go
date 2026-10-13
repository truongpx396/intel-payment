package grpcserver_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcserver"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// stubMeter is a programmable ports.Meter that counts every call: a refusal at the RPC must be
// provable as "the engine was never reached".
type stubMeter struct {
	calls atomic.Int64

	admit    func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error)
	record   func(context.Context, domain.Event) (domain.Receipt, error)
	grant    func(context.Context, domain.Grant) (domain.Receipt, error)
	transfer func(context.Context, domain.Transfer) (domain.TransferReceipt, error)
	balance  func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error)

	mu       sync.Mutex
	scope    domain.Scope // the scope of the last Admit / GetBalance
	admitReq domain.AdmitRequest
	event    domain.Event
	gotGrant domain.Grant
	gotXfer  domain.Transfer
}

var (
	_ ports.Meter         = (*stubMeter)(nil)
	_ ports.BalanceReader = (*stubMeter)(nil)
)

func (m *stubMeter) Admit(ctx context.Context, s domain.Scope, r domain.AdmitRequest) (domain.Admission, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.scope, m.admitReq = s, r
	m.mu.Unlock()
	if m.admit == nil {
		return domain.Admission{Allowed: true, Headroom: 7}, nil
	}
	return m.admit(ctx, s, r)
}

func (m *stubMeter) Record(ctx context.Context, e domain.Event) (domain.Receipt, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.event = e
	m.mu.Unlock()
	if m.record == nil {
		return domain.Receipt{IdemKey: e.IdemKey, Applied: true, Delta: -660, Seq: 1}, nil
	}
	return m.record(ctx, e)
}

func (m *stubMeter) Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.gotGrant = g
	m.mu.Unlock()
	if m.grant == nil {
		return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Delta: g.Amount, Seq: 1}, nil
	}
	return m.grant(ctx, g)
}

func (m *stubMeter) Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.gotXfer = t
	m.mu.Unlock()
	if m.transfer == nil {
		return domain.TransferReceipt{IdemKey: t.IdemKey, Applied: true, Amount: t.Amount, Status: domain.TransferInTransit}, nil
	}
	return m.transfer(ctx, t)
}

func (m *stubMeter) Balance(ctx context.Context, s domain.Scope) (map[domain.Pool]domain.Credits, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.scope = s
	m.mu.Unlock()
	if m.balance == nil {
		return map[domain.Pool]domain.Credits{domain.GeneralPool: 5}, nil
	}
	return m.balance(ctx, s)
}

// rig is a real gRPC server over an in-memory connection, in front of a stubMeter.
type rig struct {
	meter  *stubMeter
	client meteringv1.MeteringClient
	logs   *lockedWriter
}

var acme = auth.Principal{CallerID: "host-a", Realms: []domain.Realm{"acme"}, Privileges: auth.Record | auth.Grant}

// newRig serves stub as caller p. A nil p is an unauthenticated connection: no principal reaches the handler.
func newRig(t *testing.T, stub *stubMeter, p *auth.Principal) *rig {
	t.Helper()
	logs := &lockedWriter{}
	srv, err := grpcserver.New(stub, stub, grpcserver.WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	// What authenticates a caller is not under test here (T065): the interceptor stands in for it.
	g := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if p != nil {
			ctx = auth.WithPrincipal(ctx, *p)
		}
		return h(ctx, req)
	}))
	srv.Register(g)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = g.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); g.Stop(); _ = lis.Close() })
	return &rig{meter: stub, client: meteringv1.NewMeteringClient(conn), logs: logs}
}

// lockedWriter is a log sink the server goroutine writes and the test reads.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (r *rig) logged() string { return r.logs.String() }

// valid requests, one per RPC, that the engine would accept.
func okScope() *meteringv1.Scope {
	return &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w1"}
}

func okAdmit() *meteringv1.AdmitRequest {
	return &meteringv1.AdmitRequest{Scope: okScope(), Resource: "llm.chat", MaxCost: 10}
}

func okRecord() *meteringv1.RecordRequest {
	return &meteringv1.RecordRequest{Event: &meteringv1.Event{
		Scope: okScope(), Resource: "llm.chat", RateKey: "gpt", IdemKey: "evt-1",
		OccurredAt: tspb(2026, 9, 29, 12, 0, 0),
		Quantities: []*meteringv1.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}},
	}}
}

func okGrant() *meteringv1.GrantRequest {
	return &meteringv1.GrantRequest{Scope: okScope(), Amount: 100, IdemKey: "pay-1", Reason: "purchase"}
}

func okTransfer() *meteringv1.TransferRequest {
	return &meteringv1.TransferRequest{
		From: okScope(), To: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w2"}, Amount: 5, IdemKey: "alloc-1", Reason: "allocation",
	}
}

func okBalance() *meteringv1.BalanceRequest { return &meteringv1.BalanceRequest{Scope: okScope()} }

// rpc is one call by name, so a rule that holds for every RPC is a loop and not five copies.
type rpc struct {
	name string
	call func(context.Context, meteringv1.MeteringClient) error
}

var (
	rpcAdmit = rpc{"Admit", func(ctx context.Context, c meteringv1.MeteringClient) error {
		_, err := c.Admit(ctx, okAdmit())
		return err
	}}
	rpcRecord = rpc{"Record", func(ctx context.Context, c meteringv1.MeteringClient) error {
		_, err := c.Record(ctx, okRecord())
		return err
	}}
	rpcGrant = rpc{"Grant", func(ctx context.Context, c meteringv1.MeteringClient) error {
		_, err := c.Grant(ctx, okGrant())
		return err
	}}
	rpcTransfer = rpc{"Transfer", func(ctx context.Context, c meteringv1.MeteringClient) error {
		_, err := c.Transfer(ctx, okTransfer())
		return err
	}}
	rpcBalance = rpc{"GetBalance", func(ctx context.Context, c meteringv1.MeteringClient) error {
		_, err := c.GetBalance(ctx, okBalance())
		return err
	}}

	allRPCs = []rpc{rpcAdmit, rpcRecord, rpcGrant, rpcTransfer, rpcBalance}
)
