package grpcclient_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// The inverse of the server's mapping: a status, as grpc-surface.md writes it, is the typed error the
// in-process Meter would have returned — errors.Is holds, and no text is matched.
var statusRows = []struct {
	name   string
	code   codes.Code
	reason string
	want   error // nil: no sentinel exists for the row, and the code alone is what survives
}{
	{"unauthenticated", codes.Unauthenticated, "unauthenticated", auth.ErrUnauthenticated},
	{"forbidden", codes.PermissionDenied, "forbidden", auth.ErrForbidden},
	{"realm mismatch", codes.PermissionDenied, "realm_mismatch", auth.ErrRealmMismatch},
	{"invalid request", codes.InvalidArgument, "invalid_request", domain.ErrInvalid},
	{"missing subject", codes.InvalidArgument, "missing_subject", domain.ErrMissingSubject},
	{"uncountable limit", codes.InvalidArgument, "uncountable_limit", domain.ErrUncountableLimit},
	{"amount out of range", codes.InvalidArgument, "amount_out_of_range", domain.ErrAmountOutOfRange},
	{"unpriceable", codes.FailedPrecondition, "unpriceable", domain.ErrUnpriceable},
	{"stale event", codes.FailedPrecondition, "stale_event", domain.ErrStaleEvent},
	{"destination balance cap", codes.FailedPrecondition, "dest_balance_cap", domain.ErrDestBalanceCap},
	{"insufficient balance", codes.FailedPrecondition, "insufficient_balance", domain.ErrInsufficient},
	{"idempotency conflict", codes.AlreadyExists, "idempotency_conflict", domain.ErrIdemConflict},
	{"cross realm", codes.PermissionDenied, "cross_realm", domain.ErrCrossRealm},
	{"hot store unavailable", codes.Unavailable, "hot_store_unavailable", domain.ErrHotStoreUnavailable},
	{"cold shard", codes.Unavailable, "hot_shard_cold", domain.ErrColdShard},
	{"frozen shard", codes.Unavailable, "hot_shard_frozen", domain.ErrShardFrozen},
	{"outbox backpressure", codes.Unavailable, "outbox_backpressure", domain.ErrBackpressure},
	{"cold scope", codes.Unavailable, "hot_account_cold", domain.ErrColdScope},
	{"quota unavailable", codes.Unavailable, "quota_unavailable", domain.ErrQuotaUnavailable},
	{"shard count mismatch", codes.Internal, "shard_count_mismatch", domain.ErrShardCountMismatch},
	{"quota not granted", codes.Internal, "quota_not_granted", domain.ErrQuotaNotGranted},
	{"cancelled", codes.Canceled, "cancelled", context.Canceled},
	{"deadline exceeded", codes.DeadlineExceeded, "deadline_exceeded", context.DeadlineExceeded},
	{"anything else", codes.Internal, "internal", nil},
}

// call is one RPC through the client, by name.
type call struct {
	name string
	do   func(context.Context, ports.Meter, ports.BalanceReader) error
}

var allCalls = []call{
	{"Admit", func(ctx context.Context, m ports.Meter, _ ports.BalanceReader) error {
		_, err := m.Admit(ctx, ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		return err
	}},
	{"Record", func(ctx context.Context, m ports.Meter, _ ports.BalanceReader) error {
		_, err := m.Record(ctx, event())
		return err
	}},
	{"Grant", func(ctx context.Context, m ports.Meter, _ ports.BalanceReader) error {
		_, err := m.Grant(ctx, domain.Grant{Scope: ws, Amount: 5, IdemKey: "g", Reason: "purchase"})
		return err
	}},
	{"Transfer", func(ctx context.Context, m ports.Meter, _ ports.BalanceReader) error {
		_, err := m.Transfer(ctx, domain.Transfer{From: ws, To: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w2"}, Amount: 5, IdemKey: "t"})
		return err
	}},
	{"Balance", func(ctx context.Context, _ ports.Meter, b ports.BalanceReader) error {
		_, err := b.Balance(ctx, ws)
		return err
	}},
}

// failing is a fake whose every RPC answers with err.
func failing(err error) *fakeServer {
	return &fakeServer{
		admit:    func(context.Context, *meteringv1.AdmitRequest) (*meteringv1.Admission, error) { return nil, err },
		record:   func(context.Context, *meteringv1.RecordRequest) (*meteringv1.Receipt, error) { return nil, err },
		grant:    func(context.Context, *meteringv1.GrantRequest) (*meteringv1.Receipt, error) { return nil, err },
		transfer: func(context.Context, *meteringv1.TransferRequest) (*meteringv1.TransferReply, error) { return nil, err },
		balance:  func(context.Context, *meteringv1.BalanceRequest) (*meteringv1.BalanceReply, error) { return nil, err },
	}
}

func TestEveryStatusBecomesItsTypedErrorOnEveryCall(t *testing.T) {
	t.Parallel()
	for _, row := range statusRows {
		for _, c := range allCalls {
			t.Run(row.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				msg := "the server's explanation, " + row.reason
				f := failing(wireStatus(row.code, row.reason, msg, nil))
				cl := dial(t, f)
				err := c.do(context.Background(), cl, cl)
				if err == nil {
					t.Fatal("a failed call returned no error")
				}
				if row.want != nil && !errors.Is(err, row.want) {
					t.Fatalf("errors.Is(err, %v) = false for %v", row.want, err)
				}
				// Identity is the sentinel's, not a lookalike's: no other row's error matches.
				for _, other := range statusRows {
					if other.want != nil && other.want != row.want && errors.Is(err, other.want) {
						t.Fatalf("a %q status also satisfies errors.Is(err, %v)", row.reason, other.want)
					}
				}
				if got := status.Code(err); got != row.code && row.want != context.Canceled && row.want != context.DeadlineExceeded {
					t.Fatalf("the status code is lost: %v, want %v", got, row.code)
				}
				if !strings.Contains(err.Error(), "the server's explanation") && row.want != context.Canceled && row.want != context.DeadlineExceeded {
					t.Fatalf("the server's message is lost: %q", err.Error())
				}
			})
		}
	}
}

// A refused Admit is an answer: (Admission{Allowed: false, …}, nil), exactly as the in-process Meter returns it.
func TestADeniedAdmitComesBackAsAnAdmissionNotAnError(t *testing.T) {
	t.Parallel()
	balanceLimit := &meteringv1.Limit{Name: "scope_balance", Unit: "credit", Window: meteringv1.Window_BALANCE, DenyCode: "payment_required"}
	userCap := &meteringv1.Limit{Name: "user_cap", Subject: "user", Unit: "credit", Max: 1000, Window: meteringv1.Window_ROLLING, DurSeconds: 3600, WarnAt: 0.5, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"}
	rows := []struct {
		name string
		err  error
		want domain.Admission
	}{
		{"a Balance limit", wireStatus(codes.ResourceExhausted, "payment_required", "limit \"scope_balance\" is exhausted", map[string]string{"limit": "scope_balance"},
			&meteringv1.Admission{Exceeded: balanceLimit, Headroom: 0}),
			domain.Admission{Exceeded: &domain.Limit{Name: "scope_balance", Unit: "credit", Window: domain.Balance, DenyCode: "payment_required"}}},
		{"a windowed limit, in full", wireStatus(codes.ResourceExhausted, "limit_reached", "limit \"user_cap\" is exhausted", map[string]string{"limit": "user_cap"},
			&meteringv1.Admission{Exceeded: userCap}),
			domain.Admission{Exceeded: &domain.Limit{Name: "user_cap", Subject: "user", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: time.Hour, WarnAt: 0.5, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"}}},
		{"a held account", wireStatus(codes.PermissionDenied, "account_blocked", "the account is held by an operator", nil,
			&meteringv1.Admission{Blocked: true, Exceeded: &meteringv1.Limit{Name: "account_blocked", DenyCode: "account_blocked"}}),
			domain.Admission{Blocked: true, Exceeded: &domain.Limit{Name: "account_blocked", DenyCode: "account_blocked"}}},
		{"a refusal from a server that sends no Admission rebuilds it from the status", wireStatus(codes.ResourceExhausted, "limit_reached", "x", map[string]string{"limit": "user_cap"}),
			domain.Admission{Exceeded: &domain.Limit{Name: "user_cap", DenyCode: "limit_reached"}}},
		{"a held account from a server that sends no Admission", wireStatus(codes.PermissionDenied, "account_blocked", "x", nil),
			domain.Admission{Blocked: true, Exceeded: &domain.Limit{DenyCode: "account_blocked"}}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cl := dial(t, failing(row.err))
			got, err := cl.Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
			if err != nil {
				t.Fatalf("a refusal is a decision, not a failure: %v", err)
			}
			if got.Allowed || got.Blocked != row.want.Blocked || got.Headroom != row.want.Headroom || got.Warning != nil {
				t.Fatalf("got %+v, want %+v", got, row.want)
			}
			if got.Exceeded == nil || *got.Exceeded != *row.want.Exceeded {
				t.Fatalf("the ceiling that refused, in full: got %+v, want %+v", got.Exceeded, row.want.Exceeded)
			}
		})
	}

	// Only what this service wrote is read as a refusal: a proxy's own RESOURCE_EXHAUSTED is an error.
	for name, err := range map[string]error{
		"a bare RESOURCE_EXHAUSTED":                  status.Error(codes.ResourceExhausted, "too many requests"),
		"a bare PERMISSION_DENIED":                   status.Error(codes.PermissionDenied, "forbidden by the mesh"),
		"a refusal reason from another domain":       foreignDomain(codes.ResourceExhausted, "payment_required"),
		"a privilege refusal is not a decision":      wireStatus(codes.PermissionDenied, "forbidden", "no", nil),
		"a realm refusal is not a decision":          wireStatus(codes.PermissionDenied, "realm_mismatch", "no", nil),
		"a resource-exhausted reason we do not know": wireStatus(codes.ResourceExhausted, "tenant_quota_v2", "no", nil),
	} {
		t.Run("not a refusal: "+name, func(t *testing.T) {
			t.Parallel()
			_, got := dial(t, failing(err)).Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
			if got == nil {
				t.Fatal("a status this service did not write as a refusal was read as one")
			}
		})
	}
}

// What the client does not recognise it does not guess at: it keeps the status.
func TestAStatusItDoesNotRecogniseIsKeptNotGuessedInto(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
	}{
		"a dead connection (UNAVAILABLE, no detail)": {status.Error(codes.Unavailable, "connection refused"), codes.Unavailable},
		"a newer server's reason":                    {wireStatus(codes.FailedPrecondition, "plan_frozen_v2", "frozen", nil), codes.FailedPrecondition},
		"another domain's reason that we also use":   {foreignDomain(codes.AlreadyExists, "idempotency_conflict"), codes.AlreadyExists},
		"unimplemented":                              {status.Error(codes.Unimplemented, "no such method"), codes.Unimplemented},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cl := dial(t, failing(tc.err))
			for _, c := range allCalls {
				err := c.do(context.Background(), cl, cl)
				if status.Code(err) != tc.code {
					t.Fatalf("%s: the status is lost: %v", c.name, err)
				}
				for _, r := range statusRows {
					if r.want != nil && errors.Is(err, r.want) {
						t.Fatalf("%s: an unrecognised status became %v", c.name, r.want)
					}
				}
			}
		})
	}
}

func foreignDomain(code codes.Code, reason string) error {
	st := status.New(code, "from a proxy")
	with, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "mesh.example"})
	if err != nil {
		panic(err)
	}
	return with.Err()
}
