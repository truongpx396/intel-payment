package paymenthttp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/domain"
)

func paymentsDoor(t *testing.T, list func(domain.Scope, string, int) ([]domain.Payment, string, error)) *door {
	t.Helper()
	return newDoor(t, paymenthttp.Config{Payments: paymentsStub{list: func(_ context.Context, s domain.Scope, c string, n int) ([]domain.Payment, string, error) {
		return list(s, c, n)
	}}})
}

// The history is the scope's, a page at a time, with what the provider did since: a refund's cumulative figure and a
// dispute's state. It names no idempotency key and no provider account — those join the books and the deployment's wiring,
// and a client has no use for them.
func TestPaymentsAreTheScopesHistoryWithRefundAndDisputeState(t *testing.T) {
	t.Parallel()
	var scope domain.Scope
	var cursor string
	var limit int
	tax := domain.Money{MinorUnits: 80, Currency: "USD"}
	d := paymentsDoor(t, func(s domain.Scope, c string, n int) ([]domain.Payment, string, error) {
		scope, cursor, limit = s, c, n
		return []domain.Payment{{
			Scope: s, Account: "stripe-test", ProviderPaymentID: "pi_123", PlanCode: "pack_10", Kind: "one_time",
			Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, TaxAmount: &tax, CreditsGranted: 1000, CreditsRevoked: 500, Pool: "general",
			RefundedMinor: 500, Status: domain.PartiallyRefunded, DisputeID: "dp_9", DisputeStatus: "open", ReceiptURL: "https://r.example/1",
			IdemKey: "pay:stripe-test/pi_123", CreatedAt: time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC),
		}, {
			Scope: s, Account: "stripe-test", ProviderPaymentID: "pi_124", Kind: "auto_recharge", Amount: domain.Money{MinorUnits: 500, Currency: "USD"},
			Status: domain.Failed, FailureReason: "card_declined", CreatedAt: time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC),
		}}, "next-token", nil
	})
	got := d.call(t, "GET", "/v1/payments?scope_kind=user&scope_id=u1&limit=2&cursor=abc", "")
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", got.Status, got.Body)
	}
	if scope != (domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}) || cursor != "abc" || limit != 2 {
		t.Errorf("listed %+v cursor %q limit %d", scope, cursor, limit)
	}
	var out struct {
		Data []struct {
			ID       string `json:"id"`
			PlanCode string `json:"plan_code"`
			Kind     string `json:"kind"`
			Amount   struct {
				MinorUnits int64  `json:"minor_units"`
				Currency   string `json:"currency"`
			} `json:"amount"`
			TaxAmount *struct {
				MinorUnits int64 `json:"minor_units"`
			} `json:"tax_amount"`
			CreditsGranted int64  `json:"credits_granted"`
			CreditsRevoked int64  `json:"credits_revoked"`
			Pool           string `json:"pool"`
			RefundedMinor  int64  `json:"refunded_minor"`
			Status         string `json:"status"`
			DisputeStatus  string `json:"dispute_status"`
			ReceiptURL     string `json:"receipt_url"`
			FailureReason  string `json:"failure_reason"`
			CreatedAt      string `json:"created_at"`
		} `json:"data"`
		Next *string `json:"next"`
	}
	got.JSON(t, &out)
	if len(out.Data) != 2 || out.Next == nil || *out.Next != "next-token" {
		t.Fatalf("got %d payments, next %v: %s", len(out.Data), out.Next, got.Body)
	}
	p := out.Data[0]
	if p.ID != "pi_123" || p.PlanCode != "pack_10" || p.Kind != "one_time" || p.Amount.MinorUnits != 1000 || p.Amount.Currency != "USD" ||
		p.TaxAmount == nil || p.TaxAmount.MinorUnits != 80 || p.CreditsGranted != 1000 || p.CreditsRevoked != 500 || p.Pool != "general" ||
		p.RefundedMinor != 500 || p.Status != "partially_refunded" || p.DisputeStatus != "open" || p.ReceiptURL != "https://r.example/1" ||
		p.CreatedAt != "2026-10-17T09:30:00Z" {
		t.Errorf("the first payment came out as %+v", p)
	}
	if f := out.Data[1]; f.Status != "failed" || f.FailureReason != "card_declined" || f.TaxAmount != nil || f.Kind != "auto_recharge" {
		t.Errorf("the failed payment came out as %+v", f)
	}
	for _, leak := range []string{"pay:stripe-test", "stripe-test", "idem"} {
		if strings.Contains(string(got.Body), leak) {
			t.Errorf("the history names %q: that is the deployment's wiring, not the client's", leak)
		}
	}
}

func TestPaymentsTheLastPageHasANullCursorAndAnEmptyHistoryIsAnEmptyList(t *testing.T) {
	t.Parallel()
	d := paymentsDoor(t, func(domain.Scope, string, int) ([]domain.Payment, string, error) { return nil, "", nil })
	got := d.call(t, "GET", "/v1/payments?scope_kind=user&scope_id=u1", "")
	if got.Status != http.StatusOK || strings.TrimSpace(string(got.Body)) != `{"data":[],"next":null}` {
		t.Errorf("%d %s, want 200 {\"data\":[],\"next\":null}", got.Status, got.Body)
	}
}

func TestPaymentsTheLimitIsCheckedNotGuessedAt(t *testing.T) {
	t.Parallel()
	var limits []int
	d := paymentsDoor(t, func(_ domain.Scope, _ string, n int) ([]domain.Payment, string, error) {
		limits = append(limits, n)
		return nil, "", nil
	})
	for _, bad := range []string{"abc", "-1", "0", "1.5", "201", "99999999999999999999"} {
		if got := d.call(t, "GET", "/v1/payments?scope_kind=user&scope_id=u1&limit="+bad, ""); got.Status != http.StatusBadRequest || got.ErrorCode(t) != "invalid_request" {
			t.Errorf("limit=%s: %d %s, want 400 invalid_request", bad, got.Status, got.Body)
		}
	}
	if got := d.call(t, "GET", "/v1/payments?scope_kind=user&scope_id=u1&limit=200", ""); got.Status != http.StatusOK {
		t.Errorf("limit=200: %d %s, want 200", got.Status, got.Body)
	}
	if got := d.call(t, "GET", "/v1/payments?scope_kind=user&scope_id=u1", ""); got.Status != http.StatusOK {
		t.Errorf("no limit: %d %s, want 200", got.Status, got.Body)
	}
	if len(limits) != 2 || limits[0] != 200 || limits[1] != 0 {
		t.Errorf("the store was asked for limits %v, want [200 0] (0 is its default)", limits)
	}
}

// ---------------------------------------------------------------- auto top-up --

func rechargeRow(s domain.Scope) domain.AutoRecharge {
	failed := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	return domain.AutoRecharge{
		Scope: s, Enabled: false, Pool: "general", Threshold: 500, PlanCode: "pack_10", Currency: "USD", Account: "stripe-test", PaymentMethodRef: "pm_secret_ref",
		MaxPerDay: 3, ConsecutiveFailed: 3, DisabledReason: "charge_declined", LastTriggeredAt: &failed, Day: "2026-10-17", ChargesToday: 3, InflightN: 2,
	}
}

func rechargeDoor(t *testing.T, get func(domain.Scope) (domain.AutoRecharge, bool, error), put func(domain.AutoRecharge) error) *door {
	t.Helper()
	return newDoor(t, paymenthttp.Config{Recharges: rechargesStub{
		get: func(_ context.Context, s domain.Scope) (domain.AutoRecharge, bool, error) { return get(s) },
		put: func(_ context.Context, a domain.AutoRecharge) error { return put(a) },
	}})
}

func TestAutoRechargeNoInstructionIsNull(t *testing.T) {
	t.Parallel()
	d := rechargeDoor(t, func(domain.Scope) (domain.AutoRecharge, bool, error) { return domain.AutoRecharge{}, false, nil }, nil)
	got := d.call(t, "GET", "/v1/auto-recharge?scope_kind=user&scope_id=u1", "")
	if got.Status != http.StatusOK || strings.TrimSpace(string(got.Body)) != "null" {
		t.Errorf("= %d %s, want 200 null", got.Status, got.Body)
	}
}

// The instruction is shown without its saved method's reference: the client learns that one is saved, not which — the
// reference is the provider's, and the host that wrote it has no need to read it back.
func TestAutoRechargeIsShownWithItsStateAndWithoutTheMethodsReference(t *testing.T) {
	t.Parallel()
	d := rechargeDoor(t, func(s domain.Scope) (domain.AutoRecharge, bool, error) { return rechargeRow(s), true, nil }, nil)
	got := d.call(t, "GET", "/v1/auto-recharge?scope_kind=user&scope_id=u1", "")
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", got.Status, got.Body)
	}
	var out struct {
		Enabled          bool   `json:"enabled"`
		Pool             string `json:"pool"`
		Threshold        int64  `json:"threshold"`
		PlanCode         string `json:"plan_code"`
		Currency         string `json:"currency"`
		Account          string `json:"account"`
		MaxPerDay        int    `json:"max_per_day"`
		HasPaymentMethod bool   `json:"has_payment_method"`
		DisabledReason   string `json:"disabled_reason"`
	}
	got.JSON(t, &out)
	if out.Enabled || out.Pool != "general" || out.Threshold != 500 || out.PlanCode != "pack_10" || out.Currency != "USD" || out.Account != "stripe-test" ||
		out.MaxPerDay != 3 || !out.HasPaymentMethod || out.DisabledReason != "charge_declined" {
		t.Errorf("the instruction came out as %+v", out)
	}
	if strings.Contains(string(got.Body), "pm_secret_ref") {
		t.Errorf("the saved method's reference is on the wire: %s", got.Body)
	}
	for _, internal := range []string{"charges_today", "inflight", "consecutive", `"day"`, "last_triggered"} {
		if strings.Contains(string(got.Body), internal) {
			t.Errorf("the day's ledger leaks (%q): %s", internal, got.Body)
		}
	}
}

const putBody = `{"scope":{"kind":"user","id":"u1"},"enabled":true,"pool":"general","threshold":500,"plan_code":"pack_10","currency":"USD",` +
	`"account":"stripe-test","payment_method_ref":"pm_123","max_per_day":3}`

func TestAutoRechargePutWritesTheInstructionForTheCredentialsRealmAndOnlyTheInstruction(t *testing.T) {
	t.Parallel()
	var wrote []domain.AutoRecharge
	d := rechargeDoor(t, func(s domain.Scope) (domain.AutoRecharge, bool, error) { return rechargeRow(s), true, nil },
		func(a domain.AutoRecharge) error { wrote = append(wrote, a); return nil })
	got := d.call(t, "PUT", "/v1/auto-recharge", putBody)
	if got.Status != http.StatusOK {
		t.Fatalf("status = %d: %s", got.Status, got.Body)
	}
	want := domain.AutoRecharge{
		Scope: domain.Scope{Realm: "acme", Kind: "user", ID: "u1"}, Enabled: true, Pool: "general", Threshold: 500, PlanCode: "pack_10", Currency: "USD",
		Account: "stripe-test", PaymentMethodRef: "pm_123", MaxPerDay: 3,
	}
	if len(wrote) != 1 || wrote[0] != want {
		t.Errorf("wrote %+v, want exactly %+v: the day's ledger and the failure count are the store's, never the caller's", wrote, want)
	}
}

// Editing an instruction must not hand out more charges today: none of the ledger's fields can be sent.
func TestAutoRechargePutCannotSetTheDaysLedgerOrTheFailureCount(t *testing.T) {
	t.Parallel()
	var wrote int
	d := rechargeDoor(t, nil, func(domain.AutoRecharge) error { wrote++; return nil })
	for _, field := range []string{`"charges_today":0`, `"inflight_n":0`, `"consecutive_failures":0`, `"consecutive_failed":0`, `"day":"2026-01-01"`, `"disabled_reason":""`, `"last_triggered_at":null`} {
		body := strings.Replace(putBody, `{`, `{`+field+`,`, 1)
		if got := d.call(t, "PUT", "/v1/auto-recharge", body); got.Status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", field, got.Status, got.Body)
		}
	}
	if wrote != 0 {
		t.Errorf("%d instructions were written from a request that set the day's ledger", wrote)
	}
}

func TestAutoRechargePutRefusesWhatCouldNotChargeSafelyBeforeTheStore(t *testing.T) {
	t.Parallel()
	var wrote int
	d := rechargeDoor(t, nil, func(domain.AutoRecharge) error { wrote++; return nil })
	for name, body := range map[string]string{
		"no threshold":       strings.Replace(putBody, `"threshold":500`, `"threshold":0`, 1),
		"no plan":            strings.Replace(putBody, `"plan_code":"pack_10"`, `"plan_code":""`, 1),
		"a lower-case code":  strings.Replace(putBody, `"USD"`, `"usd"`, 1),
		"no account":         strings.Replace(putBody, `"account":"stripe-test"`, `"account":""`, 1),
		"no saved method":    strings.Replace(putBody, `"payment_method_ref":"pm_123"`, `"payment_method_ref":""`, 1),
		"too many a day":     strings.Replace(putBody, `"max_per_day":3`, `"max_per_day":51`, 1),
		"none a day":         strings.Replace(putBody, `"max_per_day":3`, `"max_per_day":0`, 1),
		"a fractional limit": strings.Replace(putBody, `"threshold":500`, `"threshold":500.5`, 1),
		"no scope":           `{"enabled":true}`,
		"another realm":      strings.Replace(putBody, `"id":"u1"`, `"id":"u1","realm":"other"`, 1),
	} {
		if got := d.call(t, "PUT", "/v1/auto-recharge", body); got.Status < 400 || got.Status >= 500 {
			t.Errorf("%s: %d %s, want a 4xx", name, got.Status, got.Body)
		}
	}
	if wrote != 0 {
		t.Errorf("%d instructions that could not charge safely reached the store", wrote)
	}
}

func TestAutoRechargePutAnswersWhatIsNowStored(t *testing.T) {
	t.Parallel()
	stored := domain.AutoRecharge{}
	d := rechargeDoor(t, func(domain.Scope) (domain.AutoRecharge, bool, error) { return stored, true, nil },
		func(a domain.AutoRecharge) error { stored = a; stored.ConsecutiveFailed = 0; return nil })
	got := d.call(t, "PUT", "/v1/auto-recharge", putBody)
	if got.Status != http.StatusOK || !strings.Contains(string(got.Body), `"enabled":true`) || !strings.Contains(string(got.Body), `"has_payment_method":true`) {
		t.Errorf("= %d %s, want 200 with the instruction as stored", got.Status, got.Body)
	}
}
