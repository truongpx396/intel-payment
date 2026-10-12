package resthandler_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// The error table of rest-api.md, one row per (HTTP status, code), written out by hand so a wrong row in
// the handler's own table is a failing test and not a shared mistake. Every row on every route: the
// mapping is a property of the condition, not of the call.
var errorRows = []struct {
	name   string
	err    error
	status int
	code   string
}{
	{"invalid request", domain.ErrInvalid, 400, "invalid_request"},
	{"missing subject", domain.ErrMissingSubject, 400, "missing_subject"},
	{"uncountable limit", domain.ErrUncountableLimit, 400, "uncountable_limit"},
	{"cross realm", domain.ErrCrossRealm, 403, "cross_realm"},
	{"idempotency conflict", domain.ErrIdemConflict, 409, "idempotency_conflict"},
	{"already exists", domain.ErrAlreadyExists, 409, "already_exists"},
	{"not found", domain.ErrNotFound, 404, "not_found"},
	{"insufficient balance", domain.ErrInsufficient, 409, "insufficient_balance"},
	{"unpriceable", domain.ErrUnpriceable, 422, "unpriceable"},
	{"stale event", domain.ErrStaleEvent, 422, "stale_event"},
	{"amount out of range", domain.ErrAmountOutOfRange, 422, "amount_out_of_range"},
	{"destination balance cap", domain.ErrDestBalanceCap, 422, "dest_balance_cap"},
	{"hot store unavailable", domain.ErrHotStoreUnavailable, 503, "hot_store_unavailable"},
	{"cold shard", domain.ErrColdShard, 503, "hot_store_unavailable"},
	{"frozen shard", domain.ErrShardFrozen, 503, "hot_store_unavailable"},
	{"outbox backpressure", domain.ErrBackpressure, 503, "hot_store_unavailable"},
	{"cold account", domain.ErrColdScope, 503, "hot_store_unavailable"},
	{"quota unavailable", domain.ErrQuotaUnavailable, 503, "quota_unavailable"},
	{"shard count mismatch", domain.ErrShardCountMismatch, 500, "internal"},
	{"quota not granted", domain.ErrQuotaNotGranted, 500, "internal"},
	{"cancelled", context.Canceled, 499, "cancelled"},
	{"deadline exceeded", context.DeadlineExceeded, 504, "deadline_exceeded"},
	{"anything else", errors.New("pq: password authentication failed for user payment"), 500, "internal"},
}

func failing(err error) *stubMeter {
	return &stubMeter{
		admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{}, err
		},
		record: func(context.Context, domain.Event) (domain.Receipt, error) { return domain.Receipt{}, err },
		grant:  func(context.Context, domain.Grant) (domain.Receipt, error) { return domain.Receipt{}, err },
		transfer: func(context.Context, domain.Transfer) (domain.TransferReceipt, error) {
			return domain.TransferReceipt{}, err
		},
		balance: func(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) { return nil, err },
	}
}

func TestEveryConditionMapsToItsHTTPStatusAndCodeOnEveryRoute(t *testing.T) {
	t.Parallel()
	for _, row := range errorRows {
		for _, r := range allRoutes {
			t.Run(row.name+"/"+r.name, func(t *testing.T) {
				t.Parallel()
				wrapped := fmt.Errorf("meter: %w", fmt.Errorf("%w: and some detail", row.err))
				a := newAPI(t, failing(wrapped), apiOpts{})
				rec := a.do("all", r)
				if rec.Code != row.status {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, row.status, rec.Body.String())
				}
				if e := decodeErr(t, rec); e.Error.Code != row.code {
					t.Fatalf("code = %q, want %q", e.Error.Code, row.code)
				}
			})
		}
	}
}

// A refusal explains itself to the caller who can act on it, and tells nobody anything else.
func TestWhatTheCallerMayReadInAnErrorMessage(t *testing.T) {
	t.Parallel()
	t.Run("a refusal the caller can act on keeps its explanation", func(t *testing.T) {
		t.Parallel()
		a := newAPI(t, failing(fmt.Errorf("%w: occurred 100h0m0s ago, beyond 71h0m0s", domain.ErrStaleEvent)), apiOpts{})
		e := decodeErr(t, a.do("all", recordRoute))
		if !strings.Contains(e.Error.Message, "100h0m0s ago") {
			t.Fatalf("the caller needs the window to fix the event: %q", e.Error.Message)
		}
	})
	for name, err := range map[string]error{
		"an unavailable hot store": fmt.Errorf("%w: dial tcp 10.1.2.3:6379: connect: connection refused", domain.ErrHotStoreUnavailable),
		"an unavailable quota":     fmt.Errorf("%w: limit \"x\": postgres at db.internal:5432 refused", domain.ErrQuotaUnavailable),
		"an unclassified failure":  errors.New("pq: password authentication failed for user payment at db.internal:5432"),
	} {
		t.Run(name+" does not leak the topology", func(t *testing.T) {
			t.Parallel()
			a := newAPI(t, failing(err), apiOpts{})
			rec := a.do("all", admitRoute)
			for _, secret := range []string{"10.1.2.3", "db.internal", "password", "postgres"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatalf("the response leaks %q: %s", secret, rec.Body.String())
				}
			}
			// Fail loud: the operator has the whole error, in the server's log.
			if root := strings.SplitN(err.Error(), ":", 2)[0]; !strings.Contains(a.logged(), root) {
				t.Fatalf("the full error is logged for the operator; log = %q", a.logged())
			}
		})
	}
}

// A refused Admit is a decision, and over REST it is the status the error table gives it — with the error
// envelope AND the Admission's own fields, so a client reading `.error.code` and one reading `.allowed` both
// find what they look for.
func TestADeniedAdmitIsTheTableStatusWithTheEnvelopeAndTheAdmission(t *testing.T) {
	t.Parallel()
	bal := domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}
	rolling := domain.Limit{Name: "user_cap", Subject: "user", Unit: domain.CreditUnit, Max: 1000, Window: domain.Rolling, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
	daily := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 1000, Window: domain.Daily, DenyCode: domain.DenyLimitReached}
	dailyVN := domain.Limit{Name: "user_daily", Unit: domain.CreditUnit, Max: 1000, Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh", DenyCode: domain.DenyLimitReached}
	hourly := domain.Limit{Name: "api_hourly", Unit: "api_call", Max: 3, Window: domain.Hourly, DenyCode: domain.DenyLimitReached}
	upgrade := func(_ context.Context, s domain.Scope) string { return "https://app.example/upgrade?scope=" + s.ID }

	rows := []struct {
		name    string
		adm     domain.Admission
		legacy  bool
		upgrade bool
		status  int
		code    string
		details map[string]any
		absent  []string
	}{
		{"a Balance limit: 402, with the way out", domain.Admission{Exceeded: &bal}, false, true, 402, "payment_required",
			map[string]any{"limit": "scope_balance", "upgrade_url": "https://app.example/upgrade?scope=w1"}, nil},
		{"a Balance limit with no plan to offer carries no upgrade_url", domain.Admission{Exceeded: &bal}, false, false, 402, "payment_required",
			map[string]any{"limit": "scope_balance"}, []string{"upgrade_url"}},
		{"a daily limit: 409, with when it resets (UTC midnight)", domain.Admission{Exceeded: &daily}, false, true, 409, "limit_reached",
			map[string]any{"limit": "user_daily", "resets_at": "2026-09-30T00:00:00Z"}, []string{"upgrade_url"}},
		{"a daily limit resets at the limit's own midnight", domain.Admission{Exceeded: &dailyVN}, false, false, 409, "limit_reached",
			map[string]any{"resets_at": "2026-09-29T17:00:00Z"}, nil},
		{"an hourly limit", domain.Admission{Exceeded: &hourly}, false, false, 409, "limit_reached",
			map[string]any{"limit": "api_hourly", "resets_at": "2026-09-29T13:00:00Z"}, nil},
		{"a rolling limit has no single reset: it names its window", domain.Admission{Exceeded: &rolling}, false, false, 409, "limit_reached",
			map[string]any{"limit": "user_cap", "window": "rolling", "dur_seconds": float64(3600)}, []string{"resets_at"}},
		{"PAYMENT_LEGACY_429_FOR_LIMITS restores 429 for a spent window", domain.Admission{Exceeded: &daily}, true, false, 429, "limit_reached",
			map[string]any{"limit": "user_daily"}, nil},
		{"the legacy flag leaves 402 alone", domain.Admission{Exceeded: &bal}, true, false, 402, "payment_required", nil, nil},
		{"a held account: 403", domain.Admission{Blocked: true, Exceeded: &domain.Limit{Name: "account_blocked", DenyCode: domain.DenyAccountBlocked}}, false, true, 403, "account_blocked", nil, []string{"upgrade_url"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
				return row.adm, nil
			}}
			o := apiOpts{legacy429: row.legacy}
			if row.upgrade {
				o.upgrade = upgrade
			}
			rec := newAPI(t, stub, o).do("all", admitRoute)
			if rec.Code != row.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, row.status, rec.Body.String())
			}
			e := decodeErr(t, rec)
			if e.Error.Code != row.code {
				t.Fatalf("code = %q, want %q", e.Error.Code, row.code)
			}
			for k, want := range row.details {
				if got := e.Error.Details[k]; got != want {
					t.Fatalf("details.%s = %v, want %v (all: %v)", k, got, want, e.Error.Details)
				}
			}
			for _, k := range row.absent {
				if _, ok := e.Error.Details[k]; ok {
					t.Fatalf("details.%s must be absent: %v", k, e.Error.Details)
				}
			}
			// …and the Admission itself, beside the envelope.
			if e.Allowed == nil || *e.Allowed || e.Headroom == nil || e.Blocked == nil || *e.Blocked != row.adm.Blocked {
				t.Fatalf("the body carries the Admission too: %s", rec.Body.String())
			}
			if e.Exceeded["name"] != row.adm.Exceeded.Name || e.Exceeded["deny_code"] != row.adm.Exceeded.DenyCode {
				t.Fatalf("the ceiling that refused: %v", e.Exceeded)
			}
		})
	}

	t.Run("an allowed admission is 200, may warn, and has no error", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{Allowed: true, Warning: &rolling, Headroom: 340}, nil
		}}
		rec := newAPI(t, stub, apiOpts{}).do("all", admitRoute)
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Allowed  bool           `json:"allowed"`
			Headroom int64          `json:"headroom"`
			Warning  map[string]any `json:"warning"`
			Exceeded any            `json:"exceeded"`
			Error    any            `json:"error"`
		}
		mustDecode(t, rec.Body.Bytes(), &got)
		if !got.Allowed || got.Headroom != 340 || got.Warning["name"] != "user_cap" || got.Exceeded != nil || got.Error != nil {
			t.Fatalf("%s", rec.Body.String())
		}
	})

	t.Run("a refusal the engine did not classify is loud, not a guess", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{admit: func(context.Context, domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
			return domain.Admission{Allowed: false}, nil
		}}
		if rec := newAPI(t, stub, apiOpts{}).do("all", admitRoute); rec.Code != 500 {
			t.Fatalf("%d", rec.Code)
		}
	})
}

func TestADeferredRecordIs202AndAReplayIs200(t *testing.T) {
	t.Parallel()
	t.Run("deferred: it succeeded and has not been applied yet", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{record: func(_ context.Context, e domain.Event) (domain.Receipt, error) {
			return domain.Receipt{IdemKey: e.IdemKey, Applied: true, Deferred: true, RateCardVersion: "v1"}, nil
		}}
		rec := newAPI(t, stub, apiOpts{}).do("all", recordRoute)
		if rec.Code != 202 {
			t.Fatalf("an outage returns 202, not a failure: %d %s", rec.Code, rec.Body.String())
		}
		var got struct {
			Deferred bool   `json:"deferred"`
			Applied  bool   `json:"applied"`
			Version  string `json:"rate_card_version"`
		}
		mustDecode(t, rec.Body.Bytes(), &got)
		if !got.Deferred || !got.Applied || got.Version != "v1" {
			t.Fatalf("%s", rec.Body.String())
		}
	})
	t.Run("a replay: 200 with applied:false", func(t *testing.T) {
		t.Parallel()
		stub := &stubMeter{record: func(_ context.Context, e domain.Event) (domain.Receipt, error) {
			return domain.Receipt{IdemKey: e.IdemKey, Applied: false, Seq: 7}, nil
		}}
		rec := newAPI(t, stub, apiOpts{}).do("all", recordRoute)
		var got struct {
			Applied *bool `json:"applied"`
			Seq     int64 `json:"seq"`
		}
		mustDecode(t, rec.Body.Bytes(), &got)
		if rec.Code != 200 || got.Applied == nil || *got.Applied || got.Seq != 7 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
}

// The caller's cancellation reaches the engine and is the table's 499; the service adds no clock of its own.
func TestTheCallersCancellationReachesTheEngine(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	saw := make(chan error, 1)
	stub := &stubMeter{record: func(ctx context.Context, _ domain.Event) (domain.Receipt, error) {
		entered <- struct{}{}
		<-ctx.Done()
		saw <- ctx.Err()
		return domain.Receipt{}, ctx.Err()
	}}
	a := newAPI(t, stub, apiOpts{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		req := newRequest(ctx, recordRoute, "all")
		rec := serve(a, req)
		done <- rec.Code
	}()
	await(t, entered, "the request to reach the engine")
	cancel()
	if err := await(t, saw, "the engine to see the cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if code := await(t, done, "the response"); code != 499 {
		t.Fatalf("a cancelled call is 499: %d", code)
	}
}
