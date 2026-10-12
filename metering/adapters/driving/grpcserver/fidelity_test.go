package grpcserver_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// What the caller sent is what the engine receives, field by field: the wire is a pass-through, so a
// field that is dropped, defaulted or reinterpreted is a different request.

func TestEveryRequestFieldReachesTheEngine(t *testing.T) {
	t.Parallel()
	occurred := time.Date(2026, 9, 29, 12, 0, 0, 123_456_789, time.UTC)
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("Admit", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		_, err := newRig(t, stub, &acme).client.Admit(context.Background(), &meteringv1.AdmitRequest{
			Scope: okScope(), Resource: "llm.chat", Subjects: map[string]string{"user": "u1", "job": "r1"}, MaxCost: 50_000,
			Limits: []*meteringv1.Limit{
				{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: meteringv1.Window_ROLLING, DurSeconds: 3600, Tz: "Asia/Ho_Chi_Minh", WarnAt: 0.75, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"},
				{Name: "run_cap", Subject: "job", Unit: "credit", Max: 500, Window: meteringv1.Window_JOB, DurSeconds: 60, DenyCode: "limit_reached"},
				{Name: "daily", Unit: "api_call", Max: 3, Window: meteringv1.Window_DAILY, DenyCode: "limit_reached"},
				{Name: "hourly", Unit: "api_call", Max: 3, Window: meteringv1.Window_HOURLY, DenyCode: "limit_reached"},
				{Name: "scope_balance", Unit: "credit", Window: meteringv1.Window_BALANCE, DenyCode: "payment_required"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.AdmitRequest{
			Resource: "llm.chat", Subjects: domain.Subjects{"user": "u1", "job": "r1"}, MaxCost: 50_000,
			Limits: []domain.Limit{
				{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: time.Hour, TZ: "Asia/Ho_Chi_Minh", WarnAt: 0.75, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"},
				{Name: "run_cap", Subject: "job", Unit: "credit", Max: 500, Window: domain.Job, Dur: time.Minute, DenyCode: "limit_reached"},
				{Name: "daily", Unit: "api_call", Max: 3, Window: domain.Daily, DenyCode: "limit_reached"},
				{Name: "hourly", Unit: "api_call", Max: 3, Window: domain.Hourly, DenyCode: "limit_reached"},
				{Name: "scope_balance", Unit: "credit", Window: domain.Balance, DenyCode: "payment_required"},
			},
		}
		if !reflect.DeepEqual(stub.admitReq, want) {
			t.Fatalf("got  %+v\nwant %+v", stub.admitReq, want)
		}
		if stub.scope != (domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}) {
			t.Fatalf("scope = %+v", stub.scope)
		}
	})

	t.Run("Record", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		_, err := newRig(t, stub, &acme).client.Record(context.Background(), &meteringv1.RecordRequest{Event: &meteringv1.Event{
			Scope: okScope(), Resource: "llm.chat", RateKey: "gpt-4o", IdemKey: "call-abc",
			OccurredAt: timestamppb.New(occurred),
			Quantities: []*meteringv1.Quantity{{Unit: "llm_input_token", Amount: 1200}, {Unit: "llm_output_token", Amount: 800}},
			Attributes: map[string]string{"trace_id": "t-1", "operation_type": "query"},
			Subjects:   map[string]string{"user": "u1"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.Event{
			Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Resource: "llm.chat", RateKey: "gpt-4o", IdemKey: "call-abc",
			OccurredAt: occurred, // to the nanosecond: the fingerprint of a retry covers it
			Quantities: []domain.Quantity{{Unit: "llm_input_token", Amount: 1200}, {Unit: "llm_output_token", Amount: 800}},
			Attributes: map[string]string{"trace_id": "t-1", "operation_type": "query"},
			Subjects:   domain.Subjects{"user": "u1"},
		}
		if !reflect.DeepEqual(stub.event, want) || !stub.event.OccurredAt.Equal(occurred) {
			t.Fatalf("got  %+v\nwant %+v", stub.event, want)
		}
	})

	t.Run("Grant", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		_, err := newRig(t, stub, &acme).client.Grant(context.Background(), &meteringv1.GrantRequest{
			Scope: okScope(), Pool: "promo", Amount: -250, IdemKey: "refund-1", Reason: "refund",
			Ref: map[string]string{"payment": "pi_1"}, SourceKey: "pay-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.Grant{
			Scope: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, Pool: "promo", Amount: -250, IdemKey: "refund-1", Reason: "refund",
			Ref: map[string]string{"payment": "pi_1"}, SourceKey: "pay-1",
		}
		if !reflect.DeepEqual(stub.gotGrant, want) {
			t.Fatalf("got  %+v\nwant %+v", stub.gotGrant, want)
		}
		if stub.gotGrant.ExpiresAt != nil {
			t.Fatal("an absent expires_at is no expiry, not the epoch")
		}

		_, err = newRig(t, stub, &acme).client.Grant(context.Background(), &meteringv1.GrantRequest{
			Scope: okScope(), Amount: 100, IdemKey: "pay-2", Reason: "purchase", ExpiresAt: timestamppb.New(expires),
		})
		if err != nil {
			t.Fatal(err)
		}
		if g := stub.gotGrant; g.ExpiresAt == nil || !g.ExpiresAt.Equal(expires) || g.Pool != "" {
			t.Fatalf("a positive grant carries its expiry and the default pool stays empty: %+v", g)
		}
	})

	t.Run("Transfer", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{}
		_, err := newRig(t, stub, &acme).client.Transfer(context.Background(), &meteringv1.TransferRequest{
			From: okScope(), To: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w2"}, FromPool: "promo", ToPool: "general",
			Amount: 2000, IdemKey: "alloc-1", Reason: "allocation", MaxDestBalance: 9000, Ref: map[string]string{"by": "ops"},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.Transfer{
			From: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}, To: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w2"},
			FromPool: "promo", ToPool: "general", Amount: 2000, IdemKey: "alloc-1", Reason: "allocation", MaxDestBalance: 9000, Ref: map[string]string{"by": "ops"},
		}
		if !reflect.DeepEqual(stub.gotXfer, want) {
			t.Fatalf("got  %+v\nwant %+v", stub.gotXfer, want)
		}
	})
}

func TestEveryResponseFieldReachesTheCaller(t *testing.T) {
	t.Parallel()
	t.Run("Receipt", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{record: func(context.Context, domain.Event) (domain.Receipt, error) {
			return domain.Receipt{IdemKey: "k", Applied: true, Deferred: false, Delta: -660, Balance: 99_340, Seq: 7, RateCardVersion: "2026-01-01", Writeoff: 90}, nil
		}}
		got, err := newRig(t, stub, &acme).client.Record(context.Background(), okRecord())
		if err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.Receipt{IdemKey: "k", Applied: true, Delta: -660, Balance: 99_340, Seq: 7, RateCardVersion: "2026-01-01", Writeoff: 90}
		if !proto.Equal(got, want) {
			t.Fatalf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("TransferReply", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{transfer: func(context.Context, domain.Transfer) (domain.TransferReceipt, error) {
			return domain.TransferReceipt{IdemKey: "alloc-1", Applied: true, Amount: 2000, FromBalance: 3000, Status: domain.TransferInTransit}, nil
		}}
		got, err := newRig(t, stub, &acme).client.Transfer(context.Background(), okTransfer())
		if err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.TransferReply{IdemKey: "alloc-1", Applied: true, Amount: 2000, FromBalance: 3000, Status: "in_transit"}
		if !proto.Equal(got, want) {
			t.Fatalf("got  %v\nwant %v", got, want)
		}
	})

	t.Run("Admission, with its warning and every limit field", func(t *testing.T) {
		t.Parallel()
		warn := domain.Limit{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: 90 * time.Minute, TZ: "UTC", WarnAt: 0.8, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"}
		stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{Allowed: true, Warning: &warn, Headroom: 340}, nil
		}}
		got, err := newRig(t, stub, &acme).client.Admit(context.Background(), okAdmit())
		if err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.Admission{Allowed: true, Headroom: 340, Warning: &meteringv1.Limit{
			Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: meteringv1.Window_ROLLING,
			DurSeconds: 5400, Tz: "UTC", WarnAt: 0.8, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits",
		}}
		if !proto.Equal(got, want) {
			t.Fatalf("got  %v\nwant %v", got, want)
		}
	})
}
