package app_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// rechargerUnderTest builds the handler over a world's collaborators.
var rechargerUnderTest contracts.AutoRechargerFactory = func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
	t.Helper()
	h, err := app.NewAutoRecharge(app.AutoRechargeDeps{
		Instructions: w.Recharges, Accounts: w.Accounts, Providers: w.Providers(), Catalog: w.Catalog, Customers: w.Customers,
		Payments: w.Payments, Balances: w.Balances, Bus: w.Bus, Metrics: w.Metrics, Clock: w.Clock,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestAutoRechargeContract(t *testing.T) {
	t.Parallel()
	contracts.AutoRechargeHandlerContract(t, rechargerUnderTest)
}

func TestAutoRechargeIsBuiltOnlyWithWhatItNeeds(t *testing.T) {
	t.Parallel()
	w := contracts.NewRechargeWorld()
	ok := app.AutoRechargeDeps{Instructions: w.Recharges, Accounts: w.Accounts, Providers: w.Providers(), Catalog: w.Catalog, Customers: w.Customers,
		Payments: w.Payments, Balances: w.Balances, Metrics: w.Metrics, Clock: w.Clock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := app.NewAutoRecharge(ok); err != nil {
		t.Fatalf("a complete configuration (without the optional bus) was refused: %v", err)
	}
	for name, mutate := range map[string]func(*app.AutoRechargeDeps){
		"no instructions":      func(d *app.AutoRechargeDeps) { d.Instructions = nil },
		"no accounts":          func(d *app.AutoRechargeDeps) { d.Accounts = nil },
		"no providers":         func(d *app.AutoRechargeDeps) { d.Providers = nil },
		"no catalogue":         func(d *app.AutoRechargeDeps) { d.Catalog = nil },
		"no customers":         func(d *app.AutoRechargeDeps) { d.Customers = nil },
		"no payments":          func(d *app.AutoRechargeDeps) { d.Payments = nil },
		"no balance reader":    func(d *app.AutoRechargeDeps) { d.Balances = nil },
		"no metrics":           func(d *app.AutoRechargeDeps) { d.Metrics = nil },
		"no clock":             func(d *app.AutoRechargeDeps) { d.Clock = nil },
		"no logger":            func(d *app.AutoRechargeDeps) { d.Log = nil },
		"a negative staleness": func(d *app.AutoRechargeDeps) { d.StaleAfter = -time.Second },
	} {
		d := ok
		mutate(&d)
		if _, err := app.NewAutoRecharge(d); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The handler is held to the contract by showing the contract rejects a handler, or a collaborator, that breaks it: each
// variant is the handler under test with ONE defect, and each must be rejected by a subtest that names the invariant.
func TestAutoRechargeContractRejectsEveryBrokenVariantByName(t *testing.T) {
	t.Parallel()
	const (
		charges    = "a low balance charges the saved method once, for the catalogue's price, under the slot's key, and grants nothing"
		duplicated = "a duplicated trigger is the same charge, however many replicas see it"
		bounded    = "a day is bounded by max_per_day however many triggers fire"
		refused    = "a charge the provider refuses counts, and the third in a row switches the instruction off and tells the customer"
		unknown    = "an outcome that is not known is tried again under the same key, and does not count"
		hint       = "the trigger is a hint: a balance that is not below the threshold charges nothing"
	)
	broken := []struct {
		name     string
		build    contracts.AutoRechargerFactory
		caughtBy []string
	}{
		{"grants the credits at the door", func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			return grantsWhenItCharges{rechargerUnderTest(t, w), w}
		}, []string{charges}},
		{"charges every trigger under a key of its own", func(_ testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			return &chargesEveryTrigger{w: w}
		}, []string{duplicated, bounded}},
		{"over a store that forgets the ledger it computed", func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			w.Recharges.ForgetLedger = true
			return rechargerUnderTest(t, w)
		}, []string{duplicated, bounded}},
		{"over a store that claims without a lock", func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			w.Recharges.Racy = true
			return rechargerUnderTest(t, w)
		}, []string{duplicated}},
		{"over a store that never switches an instruction off", func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			w.Recharges.NeverDisables = true
			return rechargerUnderTest(t, w)
		}, []string{refused}},
		{"acknowledges an outage, so the message is lost", func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			return swallowsErrors{rechargerUnderTest(t, w)}
		}, []string{unknown}},
		{"charges on a hint without looking at the balance", func(t testing.TB, w *contracts.RechargeWorld) contracts.AutoRecharger {
			w.Balances.Set(w.Scope, md.GeneralPool, 400) // the world's balance is low; the defect is a handler that does not ask
			return ignoresTheBalance{rechargerUnderTest(t, w), w}
		}, []string{hint}},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.AutoRechargeHandlerCases() {
				if failed, _ := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, b.build) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			for _, want := range b.caughtBy {
				if !slices.Contains(rejected, want) {
					t.Errorf("the contract PASSED a handler that %s on %q (rejected only by %q)", b.name, want, rejected)
				}
			}
		})
	}
}

func TestEveryAutoRechargeHandlerCaseHasAUniqueNameAndTheHandlerUnderTestPassesThemAll(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range contracts.AutoRechargeHandlerCases() {
		if seen[c.Name] {
			t.Fatalf("two cases are called %q", c.Name)
		}
		seen[c.Name] = true
		if failed, said := contracts.Probe(30*time.Second, func(t testing.TB) { c.Run(t, rechargerUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

// ---- the defects, as wrappers around the handler under test ----

type grantsWhenItCharges struct {
	contracts.AutoRecharger
	w *contracts.RechargeWorld
}

func (g grantsWhenItCharges) Handle(ctx context.Context, subject string, payload []byte) error {
	err := g.AutoRecharger.Handle(ctx, subject, payload)
	_, _ = g.w.Meter.Grant(ctx, md.Grant{Scope: g.w.Scope, Pool: md.GeneralPool, Amount: contracts.PackCredits, IdemKey: "door", Reason: "auto_recharge"})
	return err
}

type chargesEveryTrigger struct {
	w *contracts.RechargeWorld
	n atomic.Int64
}

func (c *chargesEveryTrigger) Handle(ctx context.Context, _ string, _ []byte) error {
	acct, _, _ := c.w.Accounts.Account(ctx, contracts.AccountID)
	_, err := c.w.Provider.ChargeSaved(ctx, acct, ports.OffSessionInput{CustomerID: "cus_1", PaymentMethodRef: "pm_1",
		Amount: domain.Money{MinorUnits: 1000, Currency: "USD"}, PlanCode: contracts.PackCode, IdemKey: fmt.Sprintf("trigger-%d", c.n.Add(1))})
	return err
}

type swallowsErrors struct{ contracts.AutoRecharger }

func (s swallowsErrors) Handle(ctx context.Context, subject string, payload []byte) error {
	_ = s.AutoRecharger.Handle(ctx, subject, payload)
	return nil
}

type ignoresTheBalance struct {
	contracts.AutoRecharger
	w *contracts.RechargeWorld
}

func (i ignoresTheBalance) Handle(ctx context.Context, subject string, payload []byte) error {
	i.w.Balances.Set(i.w.Scope, md.GeneralPool, 0) // whatever the real balance, the handler acts as if it were empty
	return i.AutoRecharger.Handle(ctx, subject, payload)
}
