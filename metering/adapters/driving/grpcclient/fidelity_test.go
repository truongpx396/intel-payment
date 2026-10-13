package grpcclient_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/domain"
)

func TestEveryRequestFieldReachesTheWire(t *testing.T) {
	t.Parallel()

	t.Run("Admit, with every kind of limit", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{}
		_, err := dial(t, f).Admit(context.Background(), ws, domain.AdmitRequest{
			Resource: "llm.chat", Subjects: domain.Subjects{"user": "u1", "job": "r1"}, MaxCost: 50_000,
			Limits: []domain.Limit{
				{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: time.Hour, TZ: "Asia/Ho_Chi_Minh", WarnAt: 0.75, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"},
				{Name: "run_cap", Subject: "job", Unit: "credit", Max: 500, Window: domain.Job, Dur: time.Minute, DenyCode: "limit_reached"},
				{Name: "daily", Unit: "api_call", Max: 3, Window: domain.Daily, DenyCode: "limit_reached"},
				{Name: "hourly", Unit: "api_call", Max: 3, Window: domain.Hourly, DenyCode: "limit_reached"},
				{Name: "scope_balance", Unit: "credit", Window: domain.Balance, DenyCode: "payment_required"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.AdmitRequest{
			Scope: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w1"}, Resource: "llm.chat", Subjects: map[string]string{"user": "u1", "job": "r1"}, MaxCost: 50_000,
			Limits: []*meteringv1.Limit{
				{Name: "user_cap", Subject: "user", Resource: "llm.chat", Unit: "credit", Max: 1000, Window: meteringv1.Window_ROLLING, DurSeconds: 3600, Tz: "Asia/Ho_Chi_Minh", WarnAt: 0.75, DenyCode: "limit_reached", MaxEntitlement: "daily_user_credits"},
				{Name: "run_cap", Subject: "job", Unit: "credit", Max: 500, Window: meteringv1.Window_JOB, DurSeconds: 60, DenyCode: "limit_reached"},
				{Name: "daily", Unit: "api_call", Max: 3, Window: meteringv1.Window_DAILY, DenyCode: "limit_reached"},
				{Name: "hourly", Unit: "api_call", Max: 3, Window: meteringv1.Window_HOURLY, DenyCode: "limit_reached"},
				{Name: "scope_balance", Unit: "credit", Window: meteringv1.Window_BALANCE, DenyCode: "payment_required"},
			},
		}
		if !proto.Equal(f.gotAdmit, want) {
			t.Fatalf("got  %v\nwant %v", f.gotAdmit, want)
		}
	})

	t.Run("Record, to the nanosecond", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{}
		e := event()
		e.Subjects = domain.Subjects{"user": "u1"}
		e.Attributes = map[string]string{"trace_id": "t-1", "operation_type": "query"}
		if _, err := dial(t, f).Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.RecordRequest{Event: &meteringv1.Event{
			Scope: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w1"}, Resource: "llm.chat", RateKey: "gpt", IdemKey: "evt-1",
			OccurredAt: timestamppb.New(mustTime("2026-09-29T12:00:00.123456789Z")),
			Quantities: []*meteringv1.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}},
			Attributes: map[string]string{"trace_id": "t-1", "operation_type": "query"}, Subjects: map[string]string{"user": "u1"},
		}}
		if !proto.Equal(f.gotRec, want) {
			t.Fatalf("got  %v\nwant %v", f.gotRec, want)
		}
	})

	t.Run("a local-zone time is the same instant on the wire", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{}
		e := event()
		e.OccurredAt = e.OccurredAt.In(time.FixedZone("ICT", 7*3600))
		if _, err := dial(t, f).Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
		if got := f.gotRec.GetEvent().GetOccurredAt().AsTime(); !got.Equal(event().OccurredAt) {
			t.Fatalf("the instant moved: %v", got)
		}
	})

	t.Run("Grant: a clawback names its source, a positive grant its expiry", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{}
		cl := dial(t, f)
		if _, err := cl.Grant(context.Background(), domain.Grant{Scope: ws, Pool: "promo", Amount: math.MinInt64 + 1, IdemKey: "refund-1", Reason: "refund", SourceKey: "pay-1", Ref: map[string]string{"payment": "pi_1"}}); err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.GrantRequest{Scope: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w1"}, Pool: "promo", Amount: math.MinInt64 + 1, IdemKey: "refund-1", Reason: "refund", SourceKey: "pay-1", Ref: map[string]string{"payment": "pi_1"}}
		if !proto.Equal(f.gotGrant, want) {
			t.Fatalf("got  %v\nwant %v", f.gotGrant, want)
		}
		expires := mustTime("2027-01-01T00:00:00Z")
		if _, err := cl.Grant(context.Background(), domain.Grant{Scope: ws, Amount: 100, IdemKey: "pay-2", Reason: "purchase", ExpiresAt: &expires}); err != nil {
			t.Fatal(err)
		}
		if got := f.gotGrant.GetExpiresAt().AsTime(); !got.Equal(expires) || f.gotGrant.GetPool() != "" {
			t.Fatalf("the expiry arrives, and the default pool stays unnamed: %v", f.gotGrant)
		}
	})

	t.Run("Transfer and Balance", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{}
		cl := dial(t, f)
		to := domain.Scope{Realm: "acme", Kind: "workspace", ID: "w2"}
		if _, err := cl.Transfer(context.Background(), domain.Transfer{From: ws, To: to, FromPool: "promo", ToPool: "general", Amount: 2000, IdemKey: "alloc-1", Reason: "allocation", MaxDestBalance: 9000, Ref: map[string]string{"by": "ops"}}); err != nil {
			t.Fatal(err)
		}
		want := &meteringv1.TransferRequest{
			From: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w1"}, To: &meteringv1.Scope{Realm: "acme", Kind: "workspace", Id: "w2"},
			FromPool: "promo", ToPool: "general", Amount: 2000, IdemKey: "alloc-1", Reason: "allocation", MaxDestBalance: 9000, Ref: map[string]string{"by": "ops"},
		}
		if !proto.Equal(f.gotXfer, want) {
			t.Fatalf("got  %v\nwant %v", f.gotXfer, want)
		}
		if _, err := cl.Balance(context.Background(), ws); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(f.gotBal.GetScope(), want.GetFrom()) {
			t.Fatalf("balance scope: %v", f.gotBal)
		}
	})
}

// A value the wire cannot say is refused before it is sent, not rounded: a rolling window one second
// short is not the window the caller asked for.
func TestWhatTheWireCannotSayIsRefusedNotChanged(t *testing.T) {
	t.Parallel()
	bad := map[string]domain.Limit{
		"a fractional-second duration": {Name: "x", Unit: "credit", Max: 1, Window: domain.Rolling, Dur: 1500 * time.Millisecond, DenyCode: "limit_reached"},
		"a negative duration":          {Name: "x", Unit: "credit", Max: 1, Window: domain.Rolling, Dur: -time.Second, DenyCode: "limit_reached"},
		"a window nobody defined":      {Name: "x", Unit: "credit", Max: 1, Window: domain.Window(9), DenyCode: "limit_reached"},
	}
	for name, l := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeServer{}
			_, err := dial(t, f).Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", Limits: []domain.Limit{l}, MaxCost: 1})
			if !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if f.calls.Load() != 0 {
				t.Fatal("an unsayable request was sent")
			}
		})
	}
	t.Run("a time no timestamp can hold", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{}
		e := event()
		e.OccurredAt = time.Date(20000, 1, 1, 0, 0, 0, 0, time.UTC)
		_, err := dial(t, f).Record(context.Background(), e)
		if !errors.Is(err, domain.ErrInvalid) || f.calls.Load() != 0 {
			t.Fatalf("err=%v calls=%d", err, f.calls.Load())
		}
	})
}

func TestEveryResponseFieldReachesTheCaller(t *testing.T) {
	t.Parallel()

	t.Run("Receipt, writeoff included, int64 exact", func(t *testing.T) {
		t.Parallel()
		const hi = int64(1)<<53 + 1
		f := &fakeServer{grant: func(context.Context, *meteringv1.GrantRequest) (*meteringv1.Receipt, error) {
			return &meteringv1.Receipt{IdemKey: "k", Applied: true, Deferred: false, Delta: -hi, Balance: hi, Seq: hi, RateCardVersion: "2026-01-01", Writeoff: 90}, nil
		}}
		got, err := dial(t, f).Grant(context.Background(), domain.Grant{Scope: ws, Amount: 5, IdemKey: "k", Reason: "purchase"})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.Receipt{IdemKey: "k", Applied: true, Delta: domain.Credits(-hi), Balance: domain.Credits(hi), Seq: hi, RateCardVersion: "2026-01-01", Writeoff: 90}
		if got != want {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	t.Run("a deferred Record is a success that has not been applied", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{record: func(context.Context, *meteringv1.RecordRequest) (*meteringv1.Receipt, error) {
			return &meteringv1.Receipt{IdemKey: "evt-1", Applied: true, Deferred: true, RateCardVersion: "v1"}, nil
		}}
		got, err := dial(t, f).Record(context.Background(), event())
		if err != nil || !got.Deferred || !got.Applied || got.Seq != 0 || got.RateCardVersion != "v1" {
			t.Fatalf("%+v %v", got, err)
		}
	})

	t.Run("TransferReceipt", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{transfer: func(context.Context, *meteringv1.TransferRequest) (*meteringv1.TransferReply, error) {
			return &meteringv1.TransferReply{IdemKey: "alloc-1", Applied: true, Amount: 2000, FromBalance: 3000, Status: "in_transit"}, nil
		}}
		got, err := dial(t, f).Transfer(context.Background(), domain.Transfer{From: ws, To: domain.Scope{Realm: "acme", Kind: "workspace", ID: "w2"}, Amount: 2000, IdemKey: "alloc-1"})
		if err != nil {
			t.Fatal(err)
		}
		want := domain.TransferReceipt{IdemKey: "alloc-1", Applied: true, Amount: 2000, FromBalance: 3000, Status: domain.TransferInTransit}
		if got != want {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	t.Run("an allowed Admission with a warning", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{admit: func(context.Context, *meteringv1.AdmitRequest) (*meteringv1.Admission, error) {
			return &meteringv1.Admission{Allowed: true, Headroom: 340, Warning: &meteringv1.Limit{Name: "user_cap", Subject: "user", Unit: "credit", Max: 1000, Window: meteringv1.Window_ROLLING, DurSeconds: 5400, WarnAt: 0.8, DenyCode: "limit_reached"}}, nil
		}}
		got, err := dial(t, f).Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Allowed || got.Headroom != 340 || got.Exceeded != nil || got.Warning == nil ||
			*got.Warning != (domain.Limit{Name: "user_cap", Subject: "user", Unit: "credit", Max: 1000, Window: domain.Rolling, Dur: 90 * time.Minute, WarnAt: 0.8, DenyCode: "limit_reached"}) {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("an answer this client cannot read is an error, not a guess", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{admit: func(context.Context, *meteringv1.AdmitRequest) (*meteringv1.Admission, error) {
			return &meteringv1.Admission{Allowed: true, Warning: &meteringv1.Limit{Name: "x", Window: meteringv1.Window(99)}}, nil
		}}
		_, err := dial(t, f).Admit(context.Background(), ws, domain.AdmitRequest{Resource: "llm.chat", MaxCost: 1})
		if err == nil || errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("a server's bad answer is not the caller's invalid request: %v", err)
		}
	})

	t.Run("Balance", func(t *testing.T) {
		t.Parallel()
		f := &fakeServer{balance: func(context.Context, *meteringv1.BalanceRequest) (*meteringv1.BalanceReply, error) {
			return &meteringv1.BalanceReply{Pools: map[string]int64{"general": 700, "promo": 30, "debt": -50}, Total: 680}, nil
		}}
		got, err := dial(t, f).Balance(context.Background(), ws)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[domain.GeneralPool] != 700 || got["promo"] != 30 || got["debt"] != -50 {
			t.Fatalf("%v", got)
		}
	})
}
