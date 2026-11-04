package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/truongpx396/intel-payment/api/entitlementv1"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementgrpc"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementhttp"
	"github.com/truongpx396/intel-payment/entitlement/adapters/driving/entitlementwire"
	entapp "github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	entdomain "github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R105: REST and gRPC return byte-identical decisions for the same input. They share an Entitler and a conversion, so this is not
// a hope; it is checked — for the cases that matter, and for a seeded random run of batches, which finds the case nobody wrote.
// "Byte-identical" is the deterministic protobuf encoding of the decision each door returns, the REST body read back into the
// message it renders. A failure is held to the same rule: the same message, and the status each door gives it.

type realms struct {
	none, noRecord bool
	set            []domain.Realm
}

func (c realms) Resolve(_ context.Context, s domain.Scope) (domain.Scope, error) {
	switch {
	case c.none:
		return domain.Scope{}, entitlementwire.ErrUnauthenticated
	case c.noRecord:
		return domain.Scope{}, entitlementwire.ErrForbidden
	case s.Realm == "" && len(c.set) == 1:
		s.Realm = c.set[0]
		return s, nil
	case s.Realm == "":
		return domain.Scope{}, entdomain.ErrInvalid
	}
	for _, r := range c.set {
		if r == s.Realm {
			return s, nil
		}
	}
	return domain.Scope{}, entitlementwire.ErrRealmMismatch
}

type transports struct {
	rest http.Handler
	grpc pb.EntitlementClient
	mem  *contracts.MemStore
	down *contracts.Breakable
}

func newTransports(t *testing.T, cred entitlementwire.Caller) *transports {
	t.Helper()
	mem := contracts.NewMemStore()
	contracts.SeedDemo(t, mem)
	down := contracts.NewBreakable(mem)
	e, err := entapp.New(entapp.Deps{Store: down, Clock: contracts.NewFakeClock(contracts.T0)})
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	door, err := entitlementhttp.New(entitlementhttp.Config{Entitler: e, Checker: e, Caller: cred, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, r := range door.Routes() {
		mux.HandleFunc(r.Method+" "+r.Path, r.Handler)
	}
	srv, err := entitlementgrpc.New(entitlementgrpc.Config{Entitler: e, Checker: e, Caller: cred, Logger: quiet})
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
	return &transports{rest: mux, grpc: pb.NewEntitlementClient(conn), mem: mem, down: down}
}

// ---- the same input to both doors ------------------------------------------------------------------------------------------

func checkJSON(req *pb.CheckRequest) string {
	type q struct {
		Key     string `json:"key"`
		Current *int64 `json:"current,omitempty"`
		Delta   *int64 `json:"delta,omitempty"`
	}
	var body struct {
		Scope  map[string]string `json:"scope"`
		Checks []q               `json:"checks"`
	}
	if s := req.GetScope(); s != nil {
		body.Scope = map[string]string{"kind": s.GetKind(), "id": s.GetId()}
		if s.GetRealm() != "" {
			body.Scope["realm"] = s.GetRealm()
		}
	}
	body.Checks = []q{}
	for _, c := range req.GetChecks() {
		body.Checks = append(body.Checks, q{c.GetKey(), c.Current, c.Delta})
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func (tr *transports) viaREST(t *testing.T, method, target, body string) (code int, raw string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	tr.rest.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// decisionsFromREST reads the body back into the message it renders.
func decisionsFromREST(t *testing.T, raw string) *pb.CheckResponse {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber() // a quota of -1 or 9007199254740993 must not pass through a float64
	var body struct {
		Data []struct {
			Key, Kind, Reason string
			UpgradePath       string `json:"upgrade_path"`
			Allowed           bool
			Value             any
			Limit             *int64
			Current           *int64
			Remaining         *int64
		}
	}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("the body is not decisions: %v\n%s", err, raw)
	}
	out := &pb.CheckResponse{}
	for _, d := range body.Data {
		out.Decisions = append(out.Decisions, &pb.Decision{
			Key: d.Key, Kind: d.Kind, Allowed: d.Allowed, Value: valueFromJSON(t, d.Value), Limit: d.Limit, Current: d.Current, Remaining: d.Remaining,
			Reason: d.Reason, UpgradePath: d.UpgradePath,
		})
	}
	return out
}

func valueFromJSON(t *testing.T, v any) *pb.Value {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		return &pb.Value{Kind: &pb.Value_Flag{Flag: x}}
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			t.Fatalf("a quota %s is not an integer: %v", x, err)
		}
		return &pb.Value{Kind: &pb.Value_Quota{Quota: n}}
	case string:
		return &pb.Value{Kind: &pb.Value_Enum{Enum: x}}
	}
	t.Fatalf("unexpected value %T", v)
	return nil
}

func grantsFromREST(t *testing.T, raw string) *pb.ResolveResponse {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var body struct {
		Data []struct {
			Key, Kind, Source, Reason string
			Value                     any
			ExpiresAt                 *string `json:"expires_at"`
		}
	}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("the body is not grants: %v\n%s", err, raw)
	}
	out := &pb.ResolveResponse{}
	for _, g := range body.Data {
		pg := &pb.Grant{Key: g.Key, Kind: g.Kind, Value: valueFromJSON(t, g.Value), Source: g.Source, Reason: g.Reason}
		if g.ExpiresAt != nil {
			ts, err := time.Parse(time.RFC3339Nano, *g.ExpiresAt)
			if err != nil {
				t.Fatalf("expires_at %q: %v", *g.ExpiresAt, err)
			}
			pg.ExpiresAt = timestamppb.New(ts)
		}
		out.Grants = append(out.Grants, pg)
	}
	return out
}

func bytesOf(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// bothCheck asks both doors the same question and requires the same answer, or the same refusal: for REST the status and the error
// body, for gRPC the code and the ErrorInfo.
func (tr *transports) bothCheck(t *testing.T, req *pb.CheckRequest, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	code, raw := tr.viaREST(t, http.MethodPost, "/v1/entitlements/check", checkJSON(req))
	res, gerr := tr.grpc.Check(ctx, req)

	switch {
	case gerr == nil && code == http.StatusOK:
		got := decisionsFromREST(t, raw)
		if !bytes.Equal(bytesOf(t, got), bytesOf(t, res)) {
			t.Fatalf("%s: the doors answered differently\n  REST: %v\n  gRPC: %v\n  body: %s", label, got, res, raw)
		}
	case gerr != nil && code != http.StatusOK:
		var e struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("%s: REST said %d with a body that is not an error: %s", label, code, raw)
		}
		st := status.Convert(gerr)
		var reason string
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reason = info.Reason
			}
		}
		if reason != e.Error.Code || st.Message() != e.Error.Message {
			t.Fatalf("%s: the doors refused differently\n  REST: %d %s %q\n  gRPC: %s %s %q", label, code, e.Error.Code, e.Error.Message, st.Code(), reason, st.Message())
		}
		if want := map[int]string{400: "InvalidArgument", 401: "Unauthenticated", 403: "PermissionDenied", 503: "Unavailable"}[code]; want != st.Code().String() {
			t.Fatalf("%s: REST %d and gRPC %s are not the same refusal", label, code, st.Code())
		}
	default:
		t.Fatalf("%s: one door answered and the other refused: REST %d %s; gRPC %v, %v", label, code, raw, res, gerr)
	}
}

func (tr *transports) bothResolve(t *testing.T, s *pb.Scope, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	target := fmt.Sprintf("/v1/entitlements?scope_kind=%s&scope_id=%s", s.GetKind(), s.GetId())
	if s.GetRealm() != "" {
		target += "&scope_realm=" + s.GetRealm()
	}
	code, raw := tr.viaREST(t, http.MethodGet, target, "")
	res, gerr := tr.grpc.Resolve(ctx, &pb.ResolveRequest{Scope: s})
	if (gerr == nil) != (code == http.StatusOK) {
		t.Fatalf("%s: one door answered and the other refused: REST %d %s; gRPC %v, %v", label, code, raw, res, gerr)
	}
	if gerr == nil {
		if got := grantsFromREST(t, raw); !bytes.Equal(bytesOf(t, got), bytesOf(t, res)) {
			t.Fatalf("%s: the doors listed different grants\n  REST: %v\n  gRPC: %v", label, got, res)
		}
	}
}

func scopeOf(realm string) *pb.Scope { return &pb.Scope{Realm: realm, Kind: "org", Id: "o1"} }

func qq(key string, current, delta *int64) *pb.Query {
	return &pb.Query{Key: key, Current: current, Delta: delta}
}

func n(v int64) *int64 { return &v }

func TestRESTAndGRPCReturnTheSameDecisionsForTheSameInput(t *testing.T) {
	t.Parallel()
	tr := newTransports(t, realms{set: []domain.Realm{"t1"}})
	many := make([]*pb.Query, entdomain.MaxChecks+1)
	for i := range many {
		many[i] = qq("sso", nil, nil)
	}
	for label, req := range map[string]*pb.CheckRequest{
		"every kind, a typo, a quota counted and not": {Scope: scopeOf(""), Checks: []*pb.Query{
			qq("support_tier", nil, nil), qq("no_such_capability", nil, nil), qq("sso", nil, nil), qq("seats", n(25), nil), qq("seats", nil, nil), qq("export", nil, nil),
		}},
		"a typo among the rest":          {Scope: scopeOf("t1"), Checks: []*pb.Query{qq("sso", nil, nil), qq("SSO", nil, nil), qq("seats", nil, nil)}},
		"the edges of a quota":           {Scope: scopeOf(""), Checks: []*pb.Query{qq("seats", n(24), n(1)), qq("seats", n(25), n(0)), qq("seats", n(26), n(0)), qq("seats", n(0), n(26)), qq("seats", n(0), n(25))}},
		"counts at the int64 edge":       {Scope: scopeOf(""), Checks: []*pb.Query{qq("seats", n(9223372036854775807), n(9223372036854775807)), qq("seats", n(1), n(9223372036854775807))}},
		"the most there may be":          {Scope: scopeOf(""), Checks: many[:entdomain.MaxChecks]},
		"one too many":                   {Scope: scopeOf(""), Checks: many},
		"nothing asked":                  {Scope: scopeOf("")},
		"an empty key":                   {Scope: scopeOf(""), Checks: []*pb.Query{qq("", nil, nil)}},
		"a negative count":               {Scope: scopeOf(""), Checks: []*pb.Query{qq("seats", n(-1), nil)}},
		"a delta with no count":          {Scope: scopeOf(""), Checks: []*pb.Query{qq("seats", nil, n(1))}},
		"a realm outside the credential": {Scope: scopeOf("other"), Checks: []*pb.Query{qq("sso", nil, nil)}},
		"a scope that is not valid":      {Scope: &pb.Scope{Kind: "org"}, Checks: []*pb.Query{qq("sso", nil, nil)}},
	} {
		tr.bothCheck(t, req, label)
	}
	for label, s := range map[string]*pb.Scope{
		"the demo scope": scopeOf(""), "naming its own realm": scopeOf("t1"), "a realm outside the credential": scopeOf("other"),
		"a scope with nothing": {Kind: "org", Id: "nobody"}, "a scope that is not valid": {Kind: "org"},
	} {
		tr.bothResolve(t, s, label)
	}
}

func TestTheDoorsAgreeAboutUnlimitedAndAboutWhoIsAsking(t *testing.T) {
	t.Parallel()
	tr := newTransports(t, realms{set: []domain.Realm{"t1"}})
	g := entdomain.Grant{Scope: contracts.DemoScope, Key: "seats", Value: entdomain.UnlimitedQuota(), Source: entdomain.FromOverride, Reason: "negotiated"}
	if err := tr.mem.PutOverride(t.Context(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	tr.bothCheck(t, &pb.CheckRequest{Scope: scopeOf(""), Checks: []*pb.Query{qq("seats", nil, nil), qq("seats", n(1_000_000), nil)}}, "unlimited")
	tr.bothResolve(t, scopeOf(""), "unlimited, listed")

	for label, cred := range map[string]realms{
		"no credential": {none: true}, "no privilege": {noRecord: true}, "two realms and none named": {set: []domain.Realm{"t1", "other"}},
	} {
		x := newTransports(t, cred)
		x.bothCheck(t, &pb.CheckRequest{Scope: scopeOf(""), Checks: []*pb.Query{qq("sso", nil, nil)}}, label)
		x.bothResolve(t, scopeOf(""), label)
	}
	both := newTransports(t, realms{set: []domain.Realm{"t1", "other"}})
	both.bothCheck(t, &pb.CheckRequest{Scope: scopeOf("other"), Checks: []*pb.Query{qq("seats", nil, nil)}}, "the second realm of two, named")
	both.bothResolve(t, scopeOf("other"), "the second realm's grants")
}

func TestTheDoorsRefuseAnOutageTheSameWay(t *testing.T) {
	t.Parallel()
	tr := newTransports(t, realms{set: []domain.Realm{"t1"}})
	tr.down.BreakWith(fmt.Errorf("dial tcp 10.1.2.3:5432: connection refused"))
	tr.bothCheck(t, &pb.CheckRequest{Scope: scopeOf(""), Checks: []*pb.Query{qq("seats", nil, nil), qq("sso", nil, nil)}}, "an outage")
	tr.bothResolve(t, scopeOf(""), "an outage, listed")
}

// A seeded random run: batches of every key and every awkward count, each asked of both doors. It finds the input nobody wrote.
func TestRandomBatchesGetTheSameDecisionsFromBothDoors(t *testing.T) {
	t.Parallel()
	tr := newTransports(t, realms{set: []domain.Realm{"t1", "other"}})
	keys := []string{"sso", "export", "seats", "support_tier", "daily_user_credits", "nope", "SSO", "seats ", "x"}
	counts := []int64{0, 1, 2, 3, 24, 25, 26, 99, 100, 101, 699, 700, 701, 1999, 2000, 2001, 1 << 40, 9223372036854775807}
	rng := rand.New(rand.NewPCG(20261104, 7))
	pick := func(xs []int64) *int64 {
		if rng.IntN(3) == 0 {
			return nil
		}
		v := xs[rng.IntN(len(xs))]
		return &v
	}
	for i := range 300 {
		req := &pb.CheckRequest{Scope: scopeOf([]string{"", "t1", "other"}[rng.IntN(3)])}
		if req.Scope.Realm == "" { // a credential for two realms must be told which
			req.Scope.Realm = "t1"
		}
		for range 1 + rng.IntN(12) {
			q := &pb.Query{Key: keys[rng.IntN(len(keys))], Current: pick(counts)}
			if q.Current != nil {
				q.Delta = pick(counts)
			}
			req.Checks = append(req.Checks, q)
		}
		tr.bothCheck(t, req, fmt.Sprintf("random batch %d", i))
	}
}
