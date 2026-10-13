package grpcserver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/domain"
)

func tspb(y int, mo time.Month, d, h, mi, s int) *timestamppb.Timestamp {
	return timestamppb.New(time.Date(y, mo, d, h, mi, s, 0, time.UTC))
}

// reasonOf is the ErrorInfo.reason of a status, "" when it carries none.
func reasonOf(st *status.Status) string {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// The fixed status mapping of grpc-surface.md, one row per condition, written out by hand: the table
// the server uses is not read here, so a wrong row in it is a failing test and not a shared mistake.
// A domain error that is not a row is the test below it.
var statusRows = []struct {
	name   string
	err    error
	code   codes.Code
	reason string
}{
	{"invalid request", domain.ErrInvalid, codes.InvalidArgument, "invalid_request"},
	{"missing subject", domain.ErrMissingSubject, codes.InvalidArgument, "missing_subject"},
	{"uncountable limit", domain.ErrUncountableLimit, codes.InvalidArgument, "uncountable_limit"},
	{"amount out of range", domain.ErrAmountOutOfRange, codes.InvalidArgument, "amount_out_of_range"},
	{"unpriceable", domain.ErrUnpriceable, codes.FailedPrecondition, "unpriceable"},
	{"stale event", domain.ErrStaleEvent, codes.FailedPrecondition, "stale_event"},
	{"destination balance cap", domain.ErrDestBalanceCap, codes.FailedPrecondition, "dest_balance_cap"},
	{"insufficient balance", domain.ErrInsufficient, codes.FailedPrecondition, "insufficient_balance"},
	{"idempotency conflict", domain.ErrIdemConflict, codes.AlreadyExists, "idempotency_conflict"},
	{"cross realm", domain.ErrCrossRealm, codes.PermissionDenied, "cross_realm"},
	{"hot store unavailable", domain.ErrHotStoreUnavailable, codes.Unavailable, "hot_store_unavailable"},
	{"cold shard", domain.ErrColdShard, codes.Unavailable, "hot_shard_cold"},
	{"frozen shard", domain.ErrShardFrozen, codes.Unavailable, "hot_shard_frozen"},
	{"outbox backpressure", domain.ErrBackpressure, codes.Unavailable, "outbox_backpressure"},
	{"cold scope", domain.ErrColdScope, codes.Unavailable, "hot_account_cold"},
	{"quota unavailable", domain.ErrQuotaUnavailable, codes.Unavailable, "quota_unavailable"},
	{"shard count mismatch", domain.ErrShardCountMismatch, codes.Internal, "shard_count_mismatch"},
	{"quota not granted", domain.ErrQuotaNotGranted, codes.Internal, "quota_not_granted"},
	{"cancelled", context.Canceled, codes.Canceled, "cancelled"},
	{"deadline exceeded", context.DeadlineExceeded, codes.DeadlineExceeded, "deadline_exceeded"},
	{"anything else", errors.New("pq: password authentication failed for user payment"), codes.Internal, "internal"},
}

// Every row, on every RPC: the mapping is a property of the condition, not of the call.
func TestEveryConditionMapsToItsFixedStatusOnEveryRPC(t *testing.T) {
	t.Parallel()
	for _, row := range statusRows {
		for _, call := range allRPCs {
			t.Run(row.name+"/"+call.name, func(t *testing.T) {
				t.Parallel()
				// The engine wraps with %w, often twice; the mapping must see through it.
				wrapped := fmt.Errorf("meter: %w", fmt.Errorf("%w: and some detail", row.err))
				stub := &stubMeter{
					admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
						return domain.Admission{}, wrapped
					},
					record: func(context.Context, domain.Event) (domain.Receipt, error) { return domain.Receipt{}, wrapped },
					grant:  func(context.Context, domain.Grant) (domain.Receipt, error) { return domain.Receipt{}, wrapped },
					transfer: func(context.Context, domain.Transfer) (domain.TransferReceipt, error) {
						return domain.TransferReceipt{}, wrapped
					},
					balance: func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) { return nil, wrapped },
				}
				r := newRig(t, stub, &acme)
				st := status.Convert(call.call(context.Background(), r.client))
				if st.Code() != row.code {
					t.Fatalf("code = %v, want %v (%v)", st.Code(), row.code, st.Message())
				}
				if got := reasonOf(st); got != row.reason {
					t.Fatalf("ErrorInfo.reason = %q, want %q", got, row.reason)
				}
			})
		}
	}
}

// A refusal explains itself to the caller who can act on it, and tells nobody anything else.
func TestWhatTheCallerMayReadInAStatusMessage(t *testing.T) {
	t.Parallel()
	t.Run("a refusal the caller can act on keeps its explanation", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{record: func(context.Context, domain.Event) (domain.Receipt, error) {
			return domain.Receipt{}, fmt.Errorf("%w: occurred 100h0m0s ago, beyond 71h0m0s", domain.ErrStaleEvent)
		}}
		st := status.Convert(rpcRecord.call(context.Background(), newRig(t, stub, &acme).client))
		if !strings.Contains(st.Message(), "100h0m0s ago") {
			t.Fatalf("the caller needs the window to fix the event: %q", st.Message())
		}
	})
	for name, err := range map[string]error{
		"an unavailable hot store": fmt.Errorf("%w: dial tcp 10.1.2.3:6379: connect: connection refused", domain.ErrHotStoreUnavailable),
		"an unavailable quota":     fmt.Errorf("%w: limit \"x\": postgres at db.internal:5432 refused", domain.ErrQuotaUnavailable),
		"an unclassified failure":  errors.New("pq: password authentication failed for user payment at db.internal:5432"),
	} {
		t.Run(name+" does not leak the topology", func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return domain.Admission{}, err
			}}
			r := newRig(t, stub, &acme)
			st := status.Convert(rpcAdmit.call(context.Background(), r.client))
			for _, secret := range []string{"10.1.2.3", "db.internal", "password", "postgres"} {
				if strings.Contains(st.Message(), secret) {
					t.Fatalf("the status message leaks %q: %q", secret, st.Message())
				}
			}
			// Fail loud: the operator has the whole error, in the server's log.
			if root := err.Error(); !strings.Contains(r.logged(), strings.SplitN(root, ":", 2)[0]) {
				t.Fatalf("the full error is logged for the operator; log = %q", r.logged())
			}
		})
	}
}

// A denied Admit is a decision, and it travels as the contract's status — with the whole Admission
// beside the reason, so a client can hand back exactly what the in-process Meter returns.
func TestADeniedAdmitIsAStatusThatCarriesTheAdmission(t *testing.T) {
	t.Parallel()
	balanceLimit := domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}
	userCap := domain.Limit{Name: "user_cap", Subject: "user", Unit: domain.CreditUnit, Max: 1000, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached, WarnAt: 0.5}
	rows := []struct {
		name   string
		adm    domain.Admission
		code   codes.Code
		reason string
	}{
		{"a Balance limit is exhausted", domain.Admission{Exceeded: &balanceLimit}, codes.ResourceExhausted, "payment_required"},
		{"a windowed limit is exhausted", domain.Admission{Exceeded: &userCap, Headroom: 0}, codes.ResourceExhausted, "limit_reached"},
		{"the account is held by block_and_flag", domain.Admission{Blocked: true, Exceeded: &domain.Limit{Name: domain.DenyAccountBlocked, DenyCode: domain.DenyAccountBlocked}}, codes.PermissionDenied, "account_blocked"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return row.adm, nil
			}}
			_, err := newRig(t, stub, &acme).client.Admit(context.Background(), okAdmit())
			st := status.Convert(err)
			if st.Code() != row.code || reasonOf(st) != row.reason {
				t.Fatalf("got %v / %q, want %v / %q", st.Code(), reasonOf(st), row.code, row.reason)
			}
			var carried *meteringv1.Admission
			for _, d := range st.Details() {
				if a, ok := d.(*meteringv1.Admission); ok {
					carried = a
				}
			}
			if carried == nil || carried.GetAllowed() || carried.GetBlocked() != row.adm.Blocked ||
				carried.GetExceeded().GetName() != row.adm.Exceeded.Name || carried.GetExceeded().GetDenyCode() != row.adm.Exceeded.DenyCode {
				t.Fatalf("the refusal carries the Admission that refused: %+v", carried)
			}
		})
	}

	t.Run("an allowed admission is OK and may warn", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{Allowed: true, Warning: &userCap, Headroom: 340}, nil
		}}
		got, err := newRig(t, stub, &acme).client.Admit(context.Background(), okAdmit())
		if err != nil {
			t.Fatal(err)
		}
		if !got.GetAllowed() || got.GetWarning().GetName() != "user_cap" || got.GetWarning().GetWarnAt() != 0.5 || got.GetHeadroom() != 340 {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("a refusal the engine did not classify is loud, not a guess", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{Allowed: false}, nil // refused, but by which ceiling?
		}}
		st := status.Convert(rpcAdmit.call(context.Background(), newRig(t, stub, &acme).client))
		if st.Code() != codes.Internal {
			t.Fatalf("got %v", st.Code())
		}
	})
}

// A deferred Record succeeded; it has not been applied yet.
func TestADeferredRecordIsOK(t *testing.T) {
	t.Parallel()
	stub := &stubMeter{record: func(_ context.Context, e domain.Event) (domain.Receipt, error) {
		return domain.Receipt{IdemKey: e.IdemKey, Applied: true, Deferred: true, RateCardVersion: "v1"}, nil
	}}
	got, err := newRig(t, stub, &acme).client.Record(context.Background(), okRecord())
	if err != nil {
		t.Fatalf("an outage must not fail usage: %v", err)
	}
	if !got.GetDeferred() || !got.GetApplied() || got.GetSeq() != 0 || got.GetRateCardVersion() != "v1" {
		t.Fatalf("%+v", got)
	}
}

// Credits arrive as int64 and leave as int64: nothing on the path is a float.
func TestMoneyCrossesAsExactInt64(t *testing.T) {
	t.Parallel()
	const hi = int64(1)<<53 + 1 // the first integer a float64 cannot hold
	stub := &stubMeter{
		grant: func(_ context.Context, g domain.Grant) (domain.Receipt, error) {
			return domain.Receipt{IdemKey: g.IdemKey, Applied: true, Delta: g.Amount, Balance: g.Amount, Seq: hi}, nil
		},
	}
	req := okGrant()
	req.Amount = hi
	got, err := newRig(t, stub, &acme).client.Grant(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDelta() != hi || got.GetBalance() != hi || got.GetSeq() != hi {
		t.Fatalf("2^53+1 must survive exactly: %+v", got)
	}
}

func TestGetBalanceTotalsThePoolsAndRefusesToWrap(t *testing.T) {
	t.Parallel()
	t.Run("the total is the sum of the pools", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{balance: func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) {
			return map[domain.Pool]domain.Credits{domain.GeneralPool: 700, "promo": 30, "debt": -50}, nil
		}}
		got, err := newRig(t, stub, &acme).client.GetBalance(context.Background(), okBalance())
		if err != nil {
			t.Fatal(err)
		}
		if got.GetTotal() != 680 || got.GetPools()["general"] != 700 || got.GetPools()["promo"] != 30 || got.GetPools()["debt"] != -50 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a total that would wrap is an error, not a wrong number", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{balance: func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) {
			return map[domain.Pool]domain.Credits{domain.GeneralPool: domain.Credits(1<<63 - 1), "promo": 1}, nil
		}}
		_, err := newRig(t, stub, &acme).client.GetBalance(context.Background(), okBalance())
		if status.Code(err) != codes.Internal {
			t.Fatalf("an overflowing sum is the server's fault: %v", err)
		}
	})
}

// The caller's deadline and cancellation reach the engine untouched: the server adds no clock of its own.
func TestTheCallersContextReachesTheMeter(t *testing.T) {
	t.Parallel()
	t.Run("the deadline", func(t *testing.T) {
		t.Parallel()
		got := make(chan time.Time, 1)
		stub := &stubMeter{admit: func(ctx context.Context, _ domain.Scope, _ domain.AdmitRequest) (domain.Admission, error) {
			d, ok := ctx.Deadline()
			if !ok {
				return domain.Admission{}, errors.New("no deadline reached the meter")
			}
			got <- d
			return domain.Admission{Allowed: true}, nil
		}}
		r := newRig(t, stub, &acme)
		want := time.Now().Add(3 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), want)
		defer cancel()
		if _, err := r.client.Admit(ctx, okAdmit()); err != nil {
			t.Fatal(err)
		}
		if d := <-got; d.After(want.Add(50*time.Millisecond)) || d.Before(want.Add(-1500*time.Millisecond)) {
			t.Fatalf("the meter's deadline %v is not the caller's %v", d, want)
		}
	})
	t.Run("a cancellation", func(t *testing.T) {
		t.Parallel()
		entered, saw := make(chan struct{}), make(chan error, 1)
		stub := &stubMeter{record: func(ctx context.Context, _ domain.Event) (domain.Receipt, error) {
			close(entered)
			<-ctx.Done()
			saw <- ctx.Err()
			return domain.Receipt{}, ctx.Err()
		}}
		r := newRig(t, stub, &acme)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := r.client.Record(ctx, okRecord()); done <- err }()
		<-entered
		cancel()
		if err := <-saw; !errors.Is(err, context.Canceled) {
			t.Fatalf("the meter must see the caller go: %v", err)
		}
		if st := status.Convert(<-done); st.Code() != codes.Canceled {
			t.Fatalf("got %v", st.Code())
		}
	})
}

// A panic in a handler is one failed call, not a dead Tier-0 service.
func TestAPanicInTheMeterIsOneInternalError(t *testing.T) {
	t.Parallel()
	stub := &stubMeter{record: func(context.Context, domain.Event) (domain.Receipt, error) { panic("boom: nil map in the pricer") }}
	r := newRig(t, stub, &acme)
	st := status.Convert(rpcRecord.call(context.Background(), r.client))
	if st.Code() != codes.Internal || strings.Contains(st.Message(), "boom") {
		t.Fatalf("got %v %q", st.Code(), st.Message())
	}
	if !strings.Contains(r.logged(), "boom") {
		t.Fatalf("and the operator is told: %q", r.logged())
	}
	// The server is still up.
	if _, err := r.client.Admit(context.Background(), okAdmit()); err != nil {
		t.Fatalf("the next call is served: %v", err)
	}
}
