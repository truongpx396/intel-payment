package contracts

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// AutoRecharger is the handler of a low-balance trigger, as the suite sees it: it is handed the message the writer publishes
// when a debit crosses a balance watch, and returns nil to acknowledge it or an error to have it delivered again.
type AutoRecharger interface {
	Handle(ctx context.Context, subject string, payload []byte) error
}

// RechargeWorld is the harness with a standing instruction for its first customer: when the general pool falls below 1000
// credits, charge the saved method for the pack, at most three times a day. The pool holds 400.
type RechargeWorld struct{ *Harness }

// NewRechargeWorld builds that world.
func NewRechargeWorld() *RechargeWorld {
	h := NewHarness()
	if err := h.Recharges.Put(Ctx(), domain.AutoRecharge{Scope: h.Scope, Enabled: true, Pool: md.GeneralPool, Threshold: 1000, PlanCode: PackCode,
		Currency: "USD", Account: AccountID, PaymentMethodRef: "pm_1", MaxPerDay: 3}); err != nil {
		panic("contracts: " + err.Error())
	}
	h.Balances.Set(h.Scope, md.GeneralPool, 400)
	h.Log.Reset()
	return &RechargeWorld{h}
}

// AutoRechargerFactory builds the handler over a world's collaborators.
type AutoRechargerFactory func(t testing.TB, w *RechargeWorld) AutoRecharger

const metricAutoRecharge = "billing_autorecharge_total"

// LowBalance is the message the writer publishes when a debit crosses a balance watch: the subject carries the scope, the body
// names it by its tag and says which watch (<pool>:<threshold>) was crossed.
func LowBalance(s md.Scope, pool md.Pool, threshold int64, seq int) (subject string, payload []byte) {
	return "billing.balance.low." + s.SubjectToken(),
		[]byte(fmt.Sprintf(`{"realm":%q,"scope":%q,"watch":%q,"seq":%d}`, string(s.Realm.Or()), s.Tag(), fmt.Sprintf("%s:%d", pool, threshold), seq))
}

// trigger fires the world's first customer's low-balance message at the handler.
func (w *RechargeWorld) trigger(h AutoRecharger) error {
	subject, payload := LowBalance(w.Scope, md.GeneralPool, 1000, 1)
	return h.Handle(Ctx(), subject, payload)
}

func (w *RechargeWorld) mustTrigger(t testing.TB, h AutoRecharger) {
	t.Helper()
	if err := w.trigger(h); err != nil {
		t.Fatalf("a trigger the handler should acknowledge: %v", err)
	}
}

// key is the idempotency key of the nth slot of the day the world starts on.
func (w *RechargeWorld) key(n int) string { return domain.RechargeKey(w.Scope, "2026-03-01", n) }

func (w *RechargeWorld) instruction(t testing.TB) domain.AutoRecharge {
	t.Helper()
	a, ok, err := w.Recharges.Get(Ctx(), w.Scope)
	if err != nil || !ok {
		t.Fatalf("the instruction: %v %v", ok, err)
	}
	return a
}

// settleInflight is the webhook of the charge in flight arriving: the slot is released with the outcome.
func (w *RechargeWorld) settleInflight(t testing.TB, ok bool) {
	t.Helper()
	a := w.instruction(t)
	if _, err := w.Recharges.Settle(Ctx(), w.Scope, ports.RechargeClaim{Day: a.Day, N: a.InflightN}, ok, "card_declined", w.Clock.Now()); err != nil {
		t.Fatal(err)
	}
}

// keysCharged are the distinct idempotency keys the provider was asked to charge under.
func (w *RechargeWorld) keysCharged() map[string]bool {
	keys := map[string]bool{}
	for _, c := range w.Provider.ChargeCalls() {
		keys[c.IdemKey] = true
	}
	return keys
}

func (w *RechargeWorld) count(result string) int64 {
	return w.Metrics.Counter(metricAutoRecharge, mports.Labels{"result": result})
}

// AutoRechargeHandlerCases are the subtests of AutoRechargeHandlerContract (payments invariant 18).
func AutoRechargeHandlerCases() []StoreCase[AutoRechargerFactory] {
	return []StoreCase[AutoRechargerFactory]{
		{"a low balance charges the saved method once, for the catalogue's price, under the slot's key, and grants nothing", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			w.mustTrigger(t, h)
			calls := w.Provider.ChargeCalls()
			if len(calls) != 1 {
				t.Fatalf("%d charges, want 1", len(calls))
			}
			c := calls[0]
			if c.CustomerID != "cus_1" || c.PaymentMethodRef != "pm_1" || c.Amount != (domain.Money{MinorUnits: 1000, Currency: "USD"}) || c.PlanCode != PackCode || c.IdemKey != w.key(1) {
				t.Fatalf("charge = %+v, want cus_1's pm_1 for $10.00 of the pack under %s", c, w.key(1))
			}
			if n := w.Meter.GrantCalls(); n != 0 {
				t.Fatalf("the handler made %d grants: credits arrive only when the verified payment is processed (invariants 3 and 18)", n)
			}
			id := w.Provider.Charges()[w.key(1)]
			pay, ok, err := w.Payments.Get(Ctx(), AccountID, id)
			if err != nil || !ok {
				t.Fatalf("the charge %q left no payment to settle: %v %v", id, ok, err)
			}
			if pay.Kind != "auto_recharge" || pay.Status != domain.Pending || pay.IdemKey != w.key(1) || pay.Scope != w.Scope.Normalized() || pay.PlanCode != PackCode ||
				pay.Amount != (domain.Money{MinorUnits: 1000, Currency: "USD"}) || pay.Pool != md.GeneralPool || pay.CreditsGranted != 0 {
				t.Fatalf("payment = %+v: it waits for its webhook, keyed on the slot, granting nothing yet", pay)
			}
			if a := w.instruction(t); a.InflightN != 1 || a.Day != "2026-03-01" {
				t.Fatalf("the slot is not in flight until its webhook settles it: %+v", a)
			}
			if got := w.count("charged"); got != 1 {
				t.Fatalf("%s{result=charged} = %d", metricAutoRecharge, got)
			}
		}},
		{"a duplicated trigger is the same charge, however many replicas see it", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			// Sixteen replicas see the one crossing at the same moment, and race to claim the day's first slot...
			const replicas = 16
			var wg sync.WaitGroup
			for range replicas {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := w.trigger(h); err != nil {
						t.Errorf("trigger: %v", err)
					}
				}()
			}
			wg.Wait()
			// ...and the provider redelivers it twice more afterwards.
			w.mustTrigger(t, h)
			w.mustTrigger(t, h)
			if keys := w.keysCharged(); len(keys) != 1 || !keys[w.key(1)] {
				t.Fatalf("%d triggers charged under %v: one key, or the provider charges twice", replicas+2, keys)
			}
			if a := w.instruction(t); a.ChargesToday != 1 {
				t.Fatalf("ChargesToday = %d: a duplicate was counted as a charge", a.ChargesToday)
			}
			if charged, resumed := w.count("charged"), w.count("resumed"); charged != 1 || charged+resumed != replicas+2 {
				t.Fatalf("charged = %d resumed = %d for %d triggers: exactly one is the charge and the rest are it again", charged, resumed, replicas+2)
			}
		}},
		{"the trigger is a hint: a balance that is not below the threshold charges nothing", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			for _, balance := range []md.Credits{1000, 1001, 50_000} {
				w.Balances.Set(w.Scope, md.GeneralPool, balance)
				w.mustTrigger(t, h)
			}
			if n := len(w.Provider.ChargeCalls()); n != 0 {
				t.Fatalf("%d charges for a balance at or above the threshold: a duplicate delivered after a recharge would charge again", n)
			}
			if a := w.instruction(t); a.ChargesToday != 0 || a.InflightN != 0 {
				t.Fatalf("a hint that was not acted on took a slot: %+v", a)
			}
			if got := w.count("ignored"); got != 3 {
				t.Fatalf("%s{result=ignored} = %d", metricAutoRecharge, got)
			}
			w.Balances.Set(w.Scope, md.GeneralPool, 999) // one below: that is a low balance
			w.mustTrigger(t, h)
			if n := len(w.Provider.ChargeCalls()); n != 1 {
				t.Fatalf("%d charges for a balance of 999 against a threshold of 1000", n)
			}
		}},
		{"the balance that matters is the pool the instruction names", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			a := w.instruction(t)
			a.Pool = "promo"
			if err := w.Recharges.Put(Ctx(), a); err != nil {
				t.Fatal(err)
			}
			subject, payload := LowBalance(w.Scope, "promo", 1000, 1)
			// The general pool is empty and the promo pool is fine: the customer is not low where the instruction looks.
			w.Balances.Set(w.Scope, md.GeneralPool, 0)
			w.Balances.Set(w.Scope, "promo", 5000)
			if err := h.Handle(Ctx(), subject, payload); err != nil {
				t.Fatal(err)
			}
			if n := len(w.Provider.ChargeCalls()); n != 0 {
				t.Fatalf("%d charges for a pool that is not low", n)
			}
			w.Balances.Set(w.Scope, md.GeneralPool, 5000)
			w.Balances.Set(w.Scope, "promo", 400)
			if err := h.Handle(Ctx(), subject, payload); err != nil {
				t.Fatal(err)
			}
			if n := len(w.Provider.ChargeCalls()); n != 1 {
				t.Fatalf("%d charges for a pool that is low", n)
			}
		}},
		{"a trigger about another pool, a scope with no instruction, or a disabled instruction charges nothing", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			subject, payload := LowBalance(w.Scope, "promo", 1000, 1) // the host's own watch on another pool
			if err := h.Handle(Ctx(), subject, payload); err != nil {
				t.Fatal(err)
			}
			subject, payload = LowBalance(w.Scope2, md.GeneralPool, 1000, 1) // cus_2 has no instruction
			if err := h.Handle(Ctx(), subject, payload); err != nil {
				t.Fatal(err)
			}
			subject, payload = LowBalance(md.Scope{Realm: "other", Kind: "workspace", ID: "w1"}, md.GeneralPool, 1000, 1) // another realm's scope of the same id
			if err := h.Handle(Ctx(), subject, payload); err != nil {
				t.Fatal(err)
			}
			off := w.instruction(t)
			off.Enabled, off.DisabledReason = false, "paused by the customer"
			if err := w.Recharges.Put(Ctx(), off); err != nil {
				t.Fatal(err)
			}
			w.mustTrigger(t, h)
			if n := len(w.Provider.ChargeCalls()); n != 0 {
				t.Fatalf("%d charges where there was nothing to charge", n)
			}
			if a := w.instruction(t); a.ChargesToday != 0 {
				t.Fatalf("a trigger that was not acted on took a slot: %+v", a)
			}
		}},
		{"a day is bounded by max_per_day however many triggers fire", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			for range 6 {
				w.mustTrigger(t, h)
				if a := w.instruction(t); a.InflightN > 0 {
					w.settleInflight(t, true) // the charge's webhook, which clears the failures and releases the slot
				}
			}
			if keys := w.keysCharged(); len(keys) != 3 || !keys[w.key(1)] || !keys[w.key(2)] || !keys[w.key(3)] {
				t.Fatalf("six triggers charged under %v: exactly the three slots of a day of three (invariant 18)", keys)
			}
			if got := w.count("refused"); got != 3 {
				t.Fatalf("%s{result=refused} = %d, want 3", metricAutoRecharge, got)
			}
		}},
		{"a charge the provider refuses counts, and the third in a row switches the instruction off and tells the customer", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			w.Provider.DeclineCharges(true)
			tag := "billing.payment." + w.Scope.SubjectToken()
			for i := 1; i <= 3; i++ {
				w.mustTrigger(t, h) // a refusal is known: acknowledged, not retried
				a := w.instruction(t)
				if a.ConsecutiveFailed != i || a.InflightN != 0 {
					t.Fatalf("refusal %d: %+v — the slot is released and the failure counted", i, a)
				}
				if told := len(w.Bus.On(tag)); (i < 3 && told != 0) || (i == 3 && told != 1) {
					t.Fatalf("refusal %d: %d announcements on %s, want one, when it is switched off", i, told, tag)
				}
			}
			a := w.instruction(t)
			if a.Enabled || a.DisabledReason == "" {
				t.Fatalf("after three refusals: %+v", a)
			}
			note := w.Bus.On(tag)[0]
			if note["event"] != "failed" || note["reason"] != a.DisabledReason || note["amount"] != float64(1000) || note["currency"] != "USD" {
				t.Fatalf("announcement = %v, want a failed payment of $10.00 whose reason is why the instruction was switched off", note)
			}
			if keys := w.keysCharged(); len(keys) != 3 {
				t.Fatalf("three refusals were tried under %v: a refused key stays refused, so each attempt is a slot of its own", keys)
			}
			w.mustTrigger(t, h)
			if n := len(w.Provider.ChargeCalls()); n != 3 {
				t.Fatalf("a switched-off instruction charged again (%d calls)", n)
			}
			if w.count("declined") != 3 || w.count("disabled") != 1 {
				t.Fatalf("declined = %d disabled = %d", w.count("declined"), w.count("disabled"))
			}
		}},
		{"an outcome that is not known is tried again under the same key, and does not count", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			w.Provider.FailCharges(errors.New("connection reset by peer"))
			if err := w.trigger(h); err == nil {
				t.Fatal("an outage was acknowledged: the message is lost and the customer is never charged")
			}
			a := w.instruction(t)
			if a.ConsecutiveFailed != 0 || a.InflightN != 1 || a.ChargesToday != 1 {
				t.Fatalf("after an outage: %+v — the slot stays held, and an outage is not a refusal", a)
			}
			w.Provider.FailCharges(nil)
			w.mustTrigger(t, h) // redelivered
			calls := w.Provider.ChargeCalls()
			if len(calls) != 2 || calls[0].IdemKey != calls[1].IdemKey || calls[0].IdemKey != w.key(1) {
				t.Fatalf("calls = %+v: the retry must name the same key, which charges at most once", calls)
			}
			if a = w.instruction(t); a.ChargesToday != 1 || a.ConsecutiveFailed != 0 {
				t.Fatalf("after the retry: %+v", a)
			}
			if w.count("error") != 1 {
				t.Fatalf("%s{result=error} = %d", metricAutoRecharge, w.count("error"))
			}
		}},
		{"the price is the catalogue's today, never the instruction's", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			plan, _, err := w.Catalog.Resolve(Ctx(), md.DefaultRealm, PackCode, "USD", AccountID)
			if err != nil {
				t.Fatal(err)
			}
			plan.Prices = map[string]domain.Money{"USD": {MinorUnits: 1250, Currency: "USD"}}
			w.Catalog.Add(plan, AccountID, "USD", PackPriceID)
			w.mustTrigger(t, h)
			if calls := w.Provider.ChargeCalls(); len(calls) != 1 || calls[0].Amount.MinorUnits != 1250 {
				t.Fatalf("calls = %+v, want the catalogue's $12.50", calls)
			}
		}},
		{"an instruction that cannot buy what it names fails its slot, for a reason it records, and charges nothing", func(t testing.TB, mk AutoRechargerFactory) {
			negotiated := domain.Plan{Realm: md.DefaultRealm, Code: "negotiated", Name: "Negotiated", Kind: domain.OneTime, Pool: md.GeneralPool, Active: true,
				Prices: map[string]domain.Money{"USD": {MinorUnits: 1000, Currency: "USD"}}} // a pack whose credits are agreed case by case: it has no allotment
			for name, tc := range map[string]struct {
				mutate func(*domain.AutoRecharge)
				reason string
			}{
				"a plan nobody sells":                     {func(a *domain.AutoRecharge) { a.PlanCode = "nope" }, "plan_not_sold"},
				"a plan not sold in the currency":         {func(a *domain.AutoRecharge) { a.Currency = "EUR" }, "plan_not_sold"},
				"a subscription, which is not a pack":     {func(a *domain.AutoRecharge) { a.PlanCode = ProCode }, "plan_is_not_a_credit_pack"},
				"a pack with no allotment of credits":     {func(a *domain.AutoRecharge) { a.PlanCode = "negotiated" }, "plan_is_not_a_credit_pack"},
				"a provider account that is not there":    {func(a *domain.AutoRecharge) { a.Account = "no-such-account" }, "provider_account_unavailable"},
				"a provider account that is switched off": {func(a *domain.AutoRecharge) { a.Account = InactiveID }, "provider_account_unavailable"},
				"a customer the account has never seen":   {func(a *domain.AutoRecharge) { a.Account = OtherAccountID }, "no_customer_on_the_account"},
				"a provider account of another realm":     {func(a *domain.AutoRecharge) { a.Account = "acct-elsewhere" }, "provider_account_unavailable"},
			} {
				w := NewRechargeWorld()
				h := mk(t, w)
				w.Catalog.Add(negotiated, AccountID, "USD", "price_negotiated")
				// The inactive account and the account that has never seen the customer sell the pack too, so the failure is the account's
				// or the customer's and not the price's.
				pack, _, _ := w.Catalog.Resolve(Ctx(), md.DefaultRealm, PackCode, "USD", AccountID)
				w.Catalog.Add(pack, OtherAccountID, "USD", "price_pack_other")
				w.Catalog.Add(pack, InactiveID, "USD", "price_pack_inactive")
				// An account that serves only another realm, and has this customer and sells this pack: the only thing wrong is the realm.
				const elsewhere = "acct-elsewhere"
				w.Accounts.Put(domain.ProviderAccount{ID: elsewhere, Provider: ProviderName, Realms: []domain.Realm{"other"}, Mode: "test", Active: true,
					SecretRef: "env:FAKE_SECRET_" + elsewhere, WebhookSecretRef: "env:FAKE_WEBHOOK_" + elsewhere})
				_ = w.Customers.Upsert(Ctx(), domain.Customer{Scope: w.Scope, Account: "acct-elsewhere", ProviderCustomerID: "cus_elsewhere"})
				w.Catalog.Add(pack, "acct-elsewhere", "USD", "price_pack_elsewhere")
				a := w.instruction(t)
				tc.mutate(&a)
				if err := w.Recharges.Put(Ctx(), a); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				// Two refusals already, so that this failure is the third and the reason it records is plain to see.
				w.Recharges.SetFailures(w.Scope, 2)
				if err := w.trigger(h); err != nil {
					t.Errorf("%s: %v — a configuration that cannot work is acknowledged and counted, not retried for ever", name, err)
				}
				if n := len(w.Provider.ChargeCalls()); n != 0 {
					t.Errorf("%s: %d charges", name, n)
				}
				if got := w.instruction(t); got.ConsecutiveFailed != 3 || got.InflightN != 0 || got.Enabled || got.DisabledReason != tc.reason {
					t.Errorf("%s: %+v, want the slot released, the third failure, and the instruction switched off because %q", name, got, tc.reason)
				}
			}
			w := NewRechargeWorld()
			h := mk(t, w)
			w.Provider.UnsupportedCharges(true)
			w.Recharges.SetFailures(w.Scope, 2)
			if err := w.trigger(h); err != nil {
				t.Errorf("a provider that cannot charge a saved method: %v", err)
			}
			if got := w.instruction(t); got.ConsecutiveFailed != 3 || got.InflightN != 0 || got.DisabledReason != "provider_cannot_charge_saved_methods" {
				t.Errorf("a provider that cannot charge a saved method: %+v", got)
			}
		}},
		{"a slot abandoned past the staleness window is replaced by the next trigger, and not a minute sooner", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			w.Provider.FailCharges(errors.New("connection reset by peer"))
			if err := w.trigger(h); err == nil {
				t.Fatal("an outage was acknowledged")
			}
			w.Clock.Advance(time.Minute)
			if err := w.trigger(h); err == nil { // still down, and still the same slot
				t.Fatal("an outage was acknowledged")
			}
			if keys := w.keysCharged(); len(keys) != 1 || !keys[w.key(1)] {
				t.Fatalf("a minute later the charge was tried under %v: still slot one", keys)
			}
			// Nothing ever settled slot one: the process that held it died, and no webhook is coming.
			w.Provider.FailCharges(nil)
			w.Clock.Advance(16 * time.Minute)
			w.mustTrigger(t, h)
			if keys := w.keysCharged(); len(keys) != 2 || !keys[w.key(2)] {
				t.Fatalf("keys = %v: past the window the next trigger must take a slot of its own (key %s), or a customer is never charged again", keys, w.key(2))
			}
			if a := w.instruction(t); a.ChargesToday != 2 || a.InflightN != 2 {
				t.Fatalf("instruction = %+v: the abandoned slot still counts", a)
			}
		}},
		{"a charge whose webhook beat the handler is settled at once", func(t testing.TB, mk AutoRechargerFactory) {
			for name, tc := range map[string]struct {
				status domain.PaymentStatus
				failed int
			}{"succeeded": {domain.Succeeded, 0}, "failed": {domain.Failed, 1}} {
				w := NewRechargeWorld()
				h := mk(t, w)
				// The processor met the payment first and recorded it as any purchase: the grant is already made, under the payment's own id.
				if err := w.Payments.Upsert(Ctx(), domain.Payment{Scope: w.Scope.Normalized(), Account: AccountID, ProviderPaymentID: "pi_1", PlanCode: PackCode, Kind: "one_time",
					Pool: md.GeneralPool, Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, CreditsGranted: PackCredits, Status: tc.status, IdemKey: "pi_1", CreatedAt: w.Clock.Now()}); err != nil {
					t.Fatal(err)
				}
				w.mustTrigger(t, h)
				pay, _, _ := w.Payments.Get(Ctx(), AccountID, "pi_1")
				if pay.Status != tc.status || pay.IdemKey != "pi_1" || pay.CreditsGranted != PackCredits {
					t.Fatalf("%s: the payment the processor made was changed: %+v", name, pay)
				}
				if a := w.instruction(t); a.InflightN != 0 || a.ConsecutiveFailed != tc.failed {
					t.Fatalf("%s: %+v, want the slot released and %d failures: nothing else will ever settle it", name, a, tc.failed)
				}
			}
		}},
		{"a trigger that cannot be read is an error, charges nothing and takes no slot", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			subject, good := LowBalance(w.Scope, md.GeneralPool, 1000, 1)
			for name, payload := range map[string]string{
				"not JSON":                                      `<html>`,
				"an empty object":                               `{}`,
				"a scope that is not a tag":                     `{"realm":"default","scope":"workspace-w1","watch":"general:1000","seq":1}`,
				"a scope with no kind":                          `{"realm":"default","scope":"default/:w1","watch":"general:1000","seq":1}`,
				"a realm that is not the scope's":               `{"realm":"other","scope":"default/workspace:w1","watch":"general:1000","seq":1}`,
				"a watch with no threshold":                     `{"realm":"default","scope":"default/workspace:w1","watch":"general","seq":1}`,
				"a watch with a threshold that is not a number": `{"realm":"default","scope":"default/workspace:w1","watch":"general:lots","seq":1}`,
				"a watch of no pool":                            `{"realm":"default","scope":"default/workspace:w1","watch":":1000","seq":1}`,
				"a watch with a threshold of zero":              `{"realm":"default","scope":"default/workspace:w1","watch":"general:0","seq":1}`,
				"a watch with a negative threshold":             `{"realm":"default","scope":"default/workspace:w1","watch":"general:-5","seq":1}`,
			} {
				if err := h.Handle(Ctx(), subject, []byte(payload)); !errors.Is(err, md.ErrInvalid) {
					t.Errorf("%s: err = %v, want ErrInvalid", name, err)
				}
			}
			// A message published for one scope's subject that names another scope is not read either.
			if err := h.Handle(Ctx(), "billing.balance.low."+w.Scope2.SubjectToken(), good); !errors.Is(err, md.ErrInvalid) {
				t.Errorf("a message on another scope's subject: err = %v, want ErrInvalid", err)
			}
			if err := h.Handle(Ctx(), subject, good); err != nil {
				t.Fatalf("the good trigger: %v", err)
			}
			if keys := w.keysCharged(); len(keys) != 1 {
				t.Fatalf("keys = %v: only the readable trigger charged", keys)
			}
			if a := w.instruction(t); a.ChargesToday != 1 {
				t.Fatalf("the unreadable triggers took slots: %+v", a)
			}
		}},
		{"a balance that cannot be read is tried again, and charges nothing", func(t testing.TB, mk AutoRechargerFactory) {
			w := NewRechargeWorld()
			h := mk(t, w)
			w.Balances.Fail(errors.New("hot tier unavailable"))
			if err := w.trigger(h); err == nil {
				t.Fatal("a trigger that could not be checked was acknowledged")
			}
			if n := len(w.Provider.ChargeCalls()); n != 0 {
				t.Fatalf("%d charges on a balance nobody could read", n)
			}
			if a := w.instruction(t); a.ChargesToday != 0 {
				t.Fatalf("a slot was taken before the balance was known: %+v", a)
			}
			w.Balances.Fail(nil)
			w.mustTrigger(t, h)
			if n := len(w.Provider.ChargeCalls()); n != 1 {
				t.Fatalf("the redelivery made %d charges", n)
			}
		}},
	}
}

// AutoRechargeHandlerContract is the specification of the handler of a low-balance trigger (payments invariant 18): it
// charges a saved method for the catalogue's price under a slot's key and grants nothing; a duplicated trigger is the same
// charge; a day is bounded; a refusal counts and the third in a row switches the instruction off; and an outcome that is not
// known is tried again under the same key.
func AutoRechargeHandlerContract(t *testing.T, mk AutoRechargerFactory) {
	t.Helper()
	runStoreCases(t, AutoRechargeHandlerCases(), mk)
}

// ------------------------------------------------------------ the processor's half --

// rechargeInFlight is what the handler leaves behind for the processor: the slot claimed, and the payment its charge made,
// pending, keyed on the slot.
func (w *RechargeWorld) rechargeInFlight(t testing.TB, paymentID string) (slot ports.RechargeClaim, key string) {
	t.Helper()
	slot, refusal, err := w.Recharges.Claim(Ctx(), w.Scope, w.Clock.Now(), 10*time.Minute)
	if err != nil || refusal != ports.RechargeOK {
		t.Fatalf("claim: %q %v", refusal, err)
	}
	key = domain.RechargeKey(w.Scope, slot.Day, slot.N)
	if err := w.Payments.Upsert(Ctx(), domain.Payment{Scope: w.Scope.Normalized(), Account: AccountID, ProviderPaymentID: paymentID, PlanCode: PackCode, Kind: "auto_recharge",
		Pool: md.GeneralPool, Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, Status: domain.Pending, IdemKey: key, CreatedAt: w.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	return slot, key
}

func (w *RechargeWorld) paymentFailed(eventID, paymentID string) FakeEvent {
	return FakeEvent{ID: eventID, Type: string(domain.PaymentFailed), Created: w.Now(), Customer: "cus_1", Payment: paymentID, Price: PackPriceID,
		Amount: 1000, Currency: "USD", Reason: "card_declined"}
}

// AutoRechargeSettlementCases are the subtests of the processor's half of auto top-up: the payment the handler's charge made is
// fulfilled like any purchase, as an auto top-up, and its outcome settles its slot — once, and only that slot.
func AutoRechargeSettlementCases() []ProcessorCase {
	return []ProcessorCase{
		{"an auto top-up that succeeds grants its credits as an auto_recharge, keyed on its slot, and settles the slot", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			w.Recharges.SetFailures(w.Scope, 2) // two earlier refusals: a confirmed good charge clears them
			_, key := w.rechargeInFlight(t, "pi_1")
			w.Persisted(w.PaymentSucceeded("evt_1", "pi_1"))
			w.Tick(t, p)
			g := w.Meter.LastGrant()
			if g.Reason != "auto_recharge" || g.IdemKey != key || g.Amount != PackCredits || g.Scope != w.Scope.Normalized() {
				t.Fatalf("grant = %+v, want %d credits as an auto_recharge under %s", g, PackCredits, key)
			}
			pay, _, _ := w.Payments.Get(Ctx(), AccountID, "pi_1")
			if pay.Status != domain.Succeeded || pay.Kind != "auto_recharge" || pay.IdemKey != key || pay.CreditsGranted != PackCredits {
				t.Fatalf("payment = %+v", pay)
			}
			if got := w.instruction(t); got.InflightN != 0 || got.ConsecutiveFailed != 0 {
				t.Fatalf("instruction = %+v: the slot is released and the failures cleared by a confirmed charge", got)
			}
		}},
		{"a success that arrives after its slot went stale does not release the slot that replaced it, and replaying it grants nothing more", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			_, key := w.rechargeInFlight(t, "pi_1") // slot one: the charge was accepted, its webhook has not come
			w.Clock.Advance(20 * time.Minute)       // so long that another trigger replaced the slot
			two, refusal, err := w.Recharges.Claim(Ctx(), w.Scope, w.Clock.Now(), 10*time.Minute)
			if err != nil || refusal != ports.RechargeOK || two.N != 2 || two.Resumed {
				t.Fatalf("slot two: %+v %q %v", two, refusal, err)
			}
			w.Persisted(w.PaymentSucceeded("evt_1", "pi_1")) // and now slot one's webhook arrives, late
			w.Tick(t, p)
			if got := w.instruction(t); got.InflightN != 2 {
				t.Fatalf("instruction = %+v: slot one's late webhook released slot two, and a duplicate trigger would charge a third time (invariant 18)", got)
			}
			w.Persisted(w.PaymentSucceeded("evt_2", "pi_1")) // the provider redelivers it
			w.Tick(t, p)
			if n := w.Meter.Grants(key); n != 1 {
				t.Fatalf("slot one's credits were granted %d times", n)
			}
			if got := w.instruction(t); got.InflightN != 2 {
				t.Fatalf("instruction = %+v after the replay", got)
			}
		}},
		{"an auto top-up that fails counts once, even replayed", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			w.rechargeInFlight(t, "pi_1")
			w.Persisted(w.paymentFailed("evt_1", "pi_1"))
			w.Tick(t, p)
			pay, _, _ := w.Payments.Get(Ctx(), AccountID, "pi_1")
			if pay.Status != domain.Failed || pay.Kind != "auto_recharge" {
				t.Fatalf("payment = %+v", pay)
			}
			if got := w.instruction(t); got.ConsecutiveFailed != 1 || got.InflightN != 0 {
				t.Fatalf("instruction = %+v, want one failure and the slot released", got)
			}
			w.Persisted(w.paymentFailed("evt_2", "pi_1"))
			w.Tick(t, p)
			if got := w.instruction(t); got.ConsecutiveFailed != 1 {
				t.Fatalf("a replayed failure counted again: %+v — three replays would switch a good card off", got)
			}
		}},
		{"the third failure switches the instruction off and tells the customer why", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			w.Recharges.SetFailures(w.Scope, 2)
			w.rechargeInFlight(t, "pi_1")
			w.Persisted(w.paymentFailed("evt_1", "pi_1"))
			w.Tick(t, p)
			got := w.instruction(t)
			if got.Enabled || got.DisabledReason != "card_declined" {
				t.Fatalf("instruction = %+v", got)
			}
			told := false
			for _, m := range w.Bus.On("billing.payment." + w.Scope.SubjectToken()) {
				told = told || (m["event"] == "failed" && m["reason"] == got.DisabledReason)
			}
			if !told {
				t.Fatalf("the customer was not told why: %v", w.Bus.On("billing.payment."+w.Scope.SubjectToken()))
			}
		}},
		{"a payment keyed on another scope's slot settles nothing", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			slot, _ := w.rechargeInFlight(t, "pi_1") // cus_1's slot one is in flight
			// A payment of cus_1's whose key names cus_2's slot one of the same day: whatever made it, it must not release cus_1's.
			foreign := domain.RechargeKey(w.Scope2, slot.Day, slot.N)
			pay, _, _ := w.Payments.Get(Ctx(), AccountID, "pi_1")
			pay.IdemKey = foreign
			if err := w.Payments.Upsert(Ctx(), pay); err != nil {
				t.Fatal(err)
			}
			w.Persisted(w.paymentFailed("evt_1", "pi_1"))
			w.Tick(t, p)
			if got := w.instruction(t); got.InflightN != slot.N || got.ConsecutiveFailed != 0 {
				t.Fatalf("instruction = %+v: a payment whose key is not its own scope's slot settled something", got)
			}
		}},
		{"a payment that is not an auto top-up never touches the instruction", func(t testing.TB, mk ProcessorFactory) {
			w := NewRechargeWorld()
			p := mk(t, w.Harness)
			w.rechargeInFlight(t, "pi_1") // a slot is in flight
			w.Persisted(w.PaymentSucceeded("evt_1", "pay-9"))
			w.Persisted(w.paymentFailed("evt_2", "pay-8"))
			w.Tick(t, p)
			if got := w.instruction(t); got.InflightN != 1 || got.ConsecutiveFailed != 0 {
				t.Fatalf("instruction = %+v: a customer's other purchases are not the top-up's outcome", got)
			}
			if g := w.Meter.LastGrant(); g.Reason != "purchase" {
				t.Fatalf("grant = %+v", g)
			}
		}},
	}
}

// ProcessorAutoRecharge runs the processor's half of auto top-up against a processor.
func ProcessorAutoRecharge(t *testing.T, mk ProcessorFactory) {
	t.Helper()
	runCases(t, AutoRechargeSettlementCases(), mk)
}
