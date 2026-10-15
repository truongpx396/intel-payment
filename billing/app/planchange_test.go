package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// ---- plan change ----

// subscriptionsUnderTest builds the plan-change service over a harness's collaborators.
var subscriptionsUnderTest contracts.SubscriptionsFactory = func(t testing.TB, h *contracts.Harness, policy domain.PlanChangePolicy, guard ports.PlanChangeGuard) ports.Subscriptions {
	t.Helper()
	s, err := app.NewSubscriptions(app.SubscriptionsDeps{Providers: h.Providers(), Accounts: h.Accounts, Catalog: h.Catalog, Subs: h.Subs,
		Payments: h.Payments, Policy: policy, Guard: guard, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPlanChangeContract(t *testing.T) {
	t.Parallel()
	contracts.PlanChangeContract(t, subscriptionsUnderTest)
}

func TestPlanChangeContractRejectsEveryBrokenServiceByName(t *testing.T) {
	t.Parallel()
	const (
		policy = "a plan change follows the policy, and the guard decides whether it can be made now (FR-029)"
		status = "changing or canceling writes no status: the webhook does (12)"
	)
	broken := []struct {
		name     string
		build    contracts.SubscriptionsFactory
		caughtBy string
	}{
		{"a service that never asks the guard", func(t testing.TB, h *contracts.Harness, p domain.PlanChangePolicy, _ ports.PlanChangeGuard) ports.Subscriptions {
			return subscriptionsUnderTest(t, h, p, nil)
		}, policy},
		{"a service that applies one policy whatever is configured", func(t testing.TB, h *contracts.Harness, _ domain.PlanChangePolicy, g ports.PlanChangeGuard) ports.Subscriptions {
			return subscriptionsUnderTest(t, h, domain.AtPeriodEnd, g)
		}, policy},
		{"a service that writes the new status itself", func(t testing.TB, h *contracts.Harness, p domain.PlanChangePolicy, g ports.PlanChangeGuard) ports.Subscriptions {
			return writesStatus{subscriptionsUnderTest(t, h, p, g), h}
		}, status},
	}
	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			var rejected []string
			for _, c := range contracts.PlanChangeCases() {
				if failed, _ := contracts.Probe(10*time.Second, func(t testing.TB) { c.Run(t, b.build) }); failed {
					rejected = append(rejected, c.Name)
				}
			}
			if !slices.Contains(rejected, b.caughtBy) {
				t.Fatalf("the suite PASSED %s on %q (rejected only by %q)", b.name, b.caughtBy, rejected)
			}
		})
	}
}

func TestPlanChangeContractPassesEveryCaseOnTheServiceUnderTest(t *testing.T) {
	t.Parallel()
	for _, c := range contracts.PlanChangeCases() {
		if failed, said := contracts.Probe(10*time.Second, func(t testing.TB) { c.Run(t, subscriptionsUnderTest) }); failed {
			t.Errorf("%q: %v", c.Name, said)
		}
	}
}

type writesStatus struct {
	ports.Subscriptions
	h *contracts.Harness
}

func (w writesStatus) Change(ctx context.Context, s domain.Scope, plan string) error {
	err := w.Subscriptions.Change(ctx, s, plan)
	cur := w.h.Subs.Row("sub-1")
	cur.PlanCode = plan
	_, _ = w.h.Subs.ApplyIfNewer(ctx, cur, cur.LastEventAt.Add(time.Second))
	return errors.Join(err)
}

func TestASubscriptionServiceNeedsAValidPolicyAndItsPorts(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	ok := app.SubscriptionsDeps{Providers: h.Providers(), Accounts: h.Accounts, Catalog: h.Catalog, Subs: h.Subs, Payments: h.Payments, Policy: domain.AtPeriodEnd}
	if _, err := app.NewSubscriptions(ok); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*app.SubscriptionsDeps){
		"a policy nobody defined": func(d *app.SubscriptionsDeps) { d.Policy = "whenever" },
		"no policy at all":        func(d *app.SubscriptionsDeps) { d.Policy = "" },
		"no providers":            func(d *app.SubscriptionsDeps) { d.Providers = nil },
		"no catalogue":            func(d *app.SubscriptionsDeps) { d.Catalog = nil },
		"no subscriptions":        func(d *app.SubscriptionsDeps) { d.Subs = nil },
	} {
		d := ok
		mutate(&d)
		if _, err := app.NewSubscriptions(d); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
