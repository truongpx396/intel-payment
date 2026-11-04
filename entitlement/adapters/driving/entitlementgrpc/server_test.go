package entitlementgrpc_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/truongpx396/intel-payment/api/entitlementv1"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementgrpc"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R105: the gRPC door answers from the same ports, through the same conversions and the same table of failures as the REST door.

type credential struct {
	none, noRecord bool
	realms         []md.Realm
}

func (c credential) Resolve(_ context.Context, s md.Scope) (md.Scope, error) {
	switch {
	case c.none:
		return md.Scope{}, entitlementwire.ErrUnauthenticated
	case c.noRecord:
		return md.Scope{}, entitlementwire.ErrForbidden
	case s.Realm == "" && len(c.realms) == 1:
		s.Realm = c.realms[0]
		return s, nil
	case s.Realm == "":
		return md.Scope{}, domain.ErrInvalid
	}
	for _, r := range c.realms {
		if r == s.Realm {
			return s, nil
		}
	}
	return md.Scope{}, entitlementwire.ErrRealmMismatch
}

var inT1 = credential{realms: []md.Realm{"t1"}}

var errBoom = errors.New("dial tcp 10.1.2.3:5432: connection refused")

type sink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *sink) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.Write(p) }
func (l *sink) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

type brokenChecker struct{ err error }

func (b brokenChecker) Check(context.Context, md.Scope, []domain.CheckRequest) ([]domain.CheckResult, error) {
	return nil, b.err
}

type rig struct {
	c      pb.EntitlementClient
	mem    *contracts.MemStore
	faults *contracts.Breakable
	log    *sink
}

func newRig(t *testing.T, cred entitlementwire.Caller, tweak ...func(*entitlementgrpc.Config)) *rig {
	t.Helper()
	mem := contracts.NewMemStore()
	contracts.SeedDemo(t, mem)
	faults := contracts.NewBreakable(mem)
	e, err := app.New(app.Deps{Store: faults, Clock: contracts.NewFakeClock(contracts.T0)})
	if err != nil {
		t.Fatal(err)
	}
	log := &sink{}
	cfg := entitlementgrpc.Config{Entitler: e, Checker: e, Caller: cred, Logger: slog.New(slog.NewTextHandler(log, nil))}
	for _, f := range tweak {
		f(&cfg)
	}
	srv, err := entitlementgrpc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	srv.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); gs.Stop(); _ = lis.Close() })
	return &rig{c: pb.NewEntitlementClient(conn), mem: mem, faults: faults, log: log}
}

func scope(realm string) *pb.Scope { return &pb.Scope{Realm: realm, Kind: "org", Id: "o1"} }

func q(key string) *pb.Query { return &pb.Query{Key: key} }

func i64(v int64) *int64 { return &v }

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func reason(t *testing.T, err error) string {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.Domain == entitlementgrpc.ErrorDomain {
			return info.Reason
		}
	}
	t.Fatalf("the status carries no ErrorInfo in the domain %q: %v", entitlementgrpc.ErrorDomain, err)
	return ""
}

func TestACheckAnswersEveryKeyInOrderWithTheValuesOfTheirKinds(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	res, err := rg.c.Check(ctx(t), &pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{
		q("support_tier"), q("no_such_capability"), q("sso"), {Key: "seats", Current: i64(25)}, q("seats"), q("export"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	d := res.GetDecisions()
	keys := []string{"support_tier", "no_such_capability", "sso", "seats", "seats", "export"}
	if len(d) != len(keys) {
		t.Fatalf("%d answers for %d questions", len(d), len(keys))
	}
	for i, k := range keys {
		if d[i].GetKey() != k {
			t.Errorf("answer %d is for %q, want %q: request order", i, d[i].GetKey(), k)
		}
	}
	if d[0].GetKind() != "enum" || !d[0].GetAllowed() || d[0].GetValue().GetEnum() != "silver" {
		t.Errorf("support_tier: %v", d[0])
	}
	if d[1].GetAllowed() || d[1].GetKind() != "" || d[1].GetValue() != nil || !strings.Contains(d[1].GetReason(), "no_such_capability") {
		t.Errorf("an unknown key: %v; want denied inside the batch with a reason that names it", d[1])
	}
	if d[2].GetAllowed() || d[2].GetUpgradePath() != "pro" || d[2].GetReason() == "" {
		t.Errorf("sso on team: %v", d[2])
	}
	if d[3].GetAllowed() || d[3].Limit == nil || d[3].GetLimit() != 25 || d[3].Current == nil || d[3].GetCurrent() != 25 || d[3].Remaining == nil || d[3].GetRemaining() != 0 {
		t.Errorf("the 26th seat: %v", d[3])
	}
	// Present and zero are different from absent: an unmeasured quota has no current and no remaining.
	if !d[4].GetAllowed() || d[4].GetLimit() != 25 || d[4].Current != nil || d[4].Remaining != nil || d[4].GetValue().GetQuota() != 25 {
		t.Errorf("seats with no count: %v", d[4])
	}
	if !d[5].GetAllowed() || !d[5].GetValue().GetFlag() {
		t.Errorf("export (an override): %v", d[5])
	}
}

func TestUnlimitedIsMinusOneAndAValue(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	g := domain.Grant{Scope: contracts.DemoScope, Key: "seats", Value: domain.UnlimitedQuota(), Source: domain.FromOverride, Reason: "negotiated"}
	if err := rg.mem.PutOverride(t.Context(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	res, err := rg.c.Check(ctx(t), &pb.CheckRequest{Scope: scope("t1"), Checks: []*pb.Query{q("seats"), {Key: "seats", Current: i64(1_000_000)}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.GetDecisions() {
		if !d.GetAllowed() || d.GetValue().GetQuota() != -1 || d.Limit == nil || d.GetLimit() != -1 {
			t.Errorf("unlimited: %v; want allowed, and -1 as a value in the value and the limit", d)
		}
	}
	if r := res.GetDecisions()[1]; r.Remaining == nil || r.GetRemaining() != -1 {
		t.Errorf("unlimited leaves unlimited room: %v", r)
	}
}

func TestResolveListsEveryGrantWithItsProvenance(t *testing.T) {
	t.Parallel()
	res, err := newRig(t, inT1).c.Resolve(ctx(t), &pb.ResolveRequest{Scope: scope("")})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range res.GetGrants() {
		got = append(got, g.GetKey()+"/"+g.GetSource())
	}
	if want := "daily_user_credits/subscription export/override seats/subscription support_tier/subscription"; strings.Join(got, " ") != want {
		t.Errorf("grants = %v, want ordered by key: %s", got, want)
	}
	export := res.GetGrants()[1]
	if export.GetReason() != "launch partner" || export.ExpiresAt == nil || !export.GetExpiresAt().AsTime().Equal(contracts.T0.Add(30*24*time.Hour)) || !export.GetValue().GetFlag() {
		t.Errorf("an override's reason and end survive: %v", export)
	}
	if sub := res.GetGrants()[2]; sub.ExpiresAt != nil || sub.GetReason() != "" || sub.GetValue().GetQuota() != 25 {
		t.Errorf("a subscription's grant has no end and no reason: %v", sub)
	}
}

func TestEachFailureHasTheCodeOfItsRowAndAReasonARestClientWouldRecognise(t *testing.T) {
	t.Parallel()
	many := make([]*pb.Query, domain.MaxChecks+1)
	for i := range many {
		many[i] = q("sso")
	}
	rg := newRig(t, inT1)
	for name, c := range map[string]struct {
		req    *pb.CheckRequest
		code   codes.Code
		reason string
	}{
		"no scope":         {&pb.CheckRequest{Checks: []*pb.Query{q("sso")}}, codes.InvalidArgument, "invalid_request"},
		"no checks":        {&pb.CheckRequest{Scope: scope("")}, codes.InvalidArgument, "invalid_request"},
		"too many":         {&pb.CheckRequest{Scope: scope(""), Checks: many}, codes.InvalidArgument, "invalid_request"},
		"an empty key":     {&pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{q("")}}, codes.InvalidArgument, "invalid_request"},
		"a negative count": {&pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{{Key: "seats", Current: i64(-1)}}}, codes.InvalidArgument, "invalid_request"},
		"a delta alone":    {&pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{{Key: "seats", Delta: i64(1)}}}, codes.InvalidArgument, "invalid_request"},
		"another realm":    {&pb.CheckRequest{Scope: scope("other"), Checks: []*pb.Query{q("sso")}}, codes.PermissionDenied, "realm_mismatch"},
	} {
		_, err := rg.c.Check(ctx(t), c.req)
		if status.Code(err) != c.code || reason(t, err) != c.reason {
			t.Errorf("%s: %v; want %s / %s", name, err, c.code, c.reason)
		}
	}
	for name, c := range map[string]struct {
		cred   credential
		code   codes.Code
		reason string
	}{
		"no credential":               {credential{none: true}, codes.Unauthenticated, "unauthenticated"},
		"a credential without record": {credential{noRecord: true}, codes.PermissionDenied, "forbidden"},
		"two realms and none named":   {credential{realms: []md.Realm{"t1", "other"}}, codes.InvalidArgument, "invalid_request"},
	} {
		_, err := newRig(t, c.cred).c.Check(ctx(t), &pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{q("sso")}})
		if status.Code(err) != c.code || reason(t, err) != c.reason {
			t.Errorf("%s: %v; want %s / %s", name, err, c.code, c.reason)
		}
		if _, err := newRig(t, c.cred).c.Resolve(ctx(t), &pb.ResolveRequest{Scope: scope("")}); status.Code(err) != c.code {
			t.Errorf("%s (Resolve): %v; want %s", name, err, c.code)
		}
	}
}

// Fail closed and say nothing that names the topology: UNAVAILABLE with fixed text and no decisions; anything unnamed is INTERNAL with
// fixed text; the cause goes to the operator's log.
func TestAnOutageIsUnavailableWithFixedTextAndTheCauseIsLogged(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	rg.faults.BreakWith(errBoom)
	res, err := rg.c.Check(ctx(t), &pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{q("seats"), q("sso")}})
	if status.Code(err) != codes.Unavailable || reason(t, err) != "unavailable" || res != nil {
		t.Fatalf("Check = %v, %v; want UNAVAILABLE and no decisions", res, err)
	}
	if strings.Contains(err.Error(), "10.1.2.3") || strings.Contains(err.Error(), "refused") {
		t.Errorf("the status names the topology: %v", err)
	}
	if _, err := rg.c.Resolve(ctx(t), &pb.ResolveRequest{Scope: scope("")}); status.Code(err) != codes.Unavailable {
		t.Errorf("Resolve: %v", err)
	}
	if !strings.Contains(rg.log.String(), "10.1.2.3") {
		t.Errorf("the operator's log lacks the cause: %q", rg.log.String())
	}

	unknown := newRig(t, inT1, func(c *entitlementgrpc.Config) { c.Checker = brokenChecker{errBoom} })
	_, err = unknown.c.Check(ctx(t), &pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{q("sso")}})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" || strings.Contains(err.Error(), "10.1.2.3") {
		t.Errorf("an error the door does not name: %v; want INTERNAL with fixed text", err)
	}
	if !strings.Contains(unknown.log.String(), "10.1.2.3") {
		t.Errorf("the cause of an INTERNAL is not in the log: %q", unknown.log.String())
	}
}

func TestACallerWhoGivesUpIsCancelledNotInternal(t *testing.T) {
	t.Parallel()
	rg := newRig(t, inT1)
	c, cancel := context.WithCancel(ctx(t))
	cancel()
	_, err := rg.c.Check(c, &pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{q("sso")}})
	if status.Code(err) != codes.Canceled {
		t.Errorf("a cancelled call: %v; want CANCELED", err)
	}
}

// The cancellation of the call wins over whatever the work made of it. A client cancelling is cut off by gRPC before the server is
// reached, so this calls the handler with a context that is already done — what a server sees when the deadline passes in flight.
func TestACallThatIsCancelledInFlightIsCanceledAndNotInternal(t *testing.T) {
	t.Parallel()
	mem := contracts.NewMemStore()
	e, _ := app.New(app.Deps{Store: mem, Clock: contracts.NewFakeClock(contracts.T0)})
	log := &sink{}
	srv, err := entitlementgrpc.New(entitlementgrpc.Config{Entitler: e, Checker: brokenChecker{errBoom}, Caller: inT1, Logger: slog.New(slog.NewTextHandler(log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = srv.Check(c, &pb.CheckRequest{Scope: scope(""), Checks: []*pb.Query{q("sso")}})
	if status.Code(err) != codes.Canceled || reason(t, err) != "cancelled" {
		t.Errorf("a call cancelled in flight: %v; want CANCELED", err)
	}
	if strings.Contains(log.String(), "10.1.2.3") {
		t.Error("a caller going away was logged as the service failing")
	}
}

func TestTheServerNeedsEverythingItAnswersFrom(t *testing.T) {
	t.Parallel()
	mem := contracts.NewMemStore()
	e, _ := app.New(app.Deps{Store: mem, Clock: contracts.NewFakeClock(contracts.T0)})
	for name, cfg := range map[string]entitlementgrpc.Config{
		"no caller":   {Entitler: e, Checker: e},
		"no entitler": {Checker: e, Caller: inT1},
		"no checker":  {Entitler: e, Caller: inT1},
	} {
		if _, err := entitlementgrpc.New(cfg); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}
