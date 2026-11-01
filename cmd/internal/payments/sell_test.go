package payments_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
)

// R099b: paymentd sells. The door that serves the host's /v1 payment routes is built from the same books and the same
// provider registry the webhook door verifies with, under the selling settings — and a setting that cannot work stops it
// being built rather than selling under a policy nobody chose.

func common(t *testing.T) payments.Common {
	t.Helper()
	h := contracts.NewHarness()
	return payments.Common{Books: payments.OpenBooks(nil, h.Clock), Getenv: env(nil), Clock: h.Clock, Settings: envcfg.Payments{WebhookTolerance: 5 * time.Minute}}
}

func TestPaymentdServesEveryPaymentRouteOfTheContract(t *testing.T) {
	t.Parallel()
	c := common(t)
	sell, err := payments.NewSell(c, envcfg.Selling{PlanChangePolicy: domain.AtPeriodEnd, TaxStrategy: domain.TaxProviderManaged})
	if err != nil {
		t.Fatal(err)
	}
	door, err := payments.NewDoor(c, sell)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range door.Routes() {
		got = append(got, r.Method+" "+r.Path)
	}
	sort.Strings(got)
	want := []string{
		"GET /v1/auto-recharge", "GET /v1/fulfilment", "GET /v1/payments", "GET /v1/plans", "GET /v1/portal", "GET /v1/subscription",
		"POST /v1/checkout", "POST /v1/subscription/cancel", "POST /v1/subscription/change", "PUT /v1/auto-recharge",
		"GET /v1/admin/customers", "GET /v1/admin/plans", "GET /v1/admin/plans/{code}", "GET /v1/admin/provider-accounts",
		"GET /v1/admin/provider-accounts/{id}", "PATCH /v1/admin/plans/{code}", "POST /v1/admin/plans", "PUT /v1/admin/customers",
		"PUT /v1/admin/plans/{code}/provider-prices/{account}/{currency}", "PUT /v1/admin/provider-accounts/{id}",
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("serves %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSellingIsRefusedUnderASettingThatCannotWork(t *testing.T) {
	t.Parallel()
	c := common(t)
	if _, err := payments.NewSell(c, envcfg.Selling{PlanChangePolicy: "whenever", TaxStrategy: domain.TaxProviderManaged}); err == nil {
		t.Error("a plan change policy nobody defined was accepted")
	}
	if _, err := payments.NewSell(c, envcfg.Selling{PlanChangePolicy: domain.AtPeriodEnd, TaxStrategy: domain.TaxExternalHook}); err == nil {
		t.Error("external_hook was accepted with no tax service to call: tax would silently be nobody's")
	}
	if _, err := payments.NewSell(c, envcfg.Selling{PlanChangePolicy: domain.AtPeriodEnd}); err == nil {
		t.Error("no tax strategy was accepted: tax must be somebody's")
	}
}

// The admin accepts an account only if this deployment can read its secrets, so what it accepts is held equal to what the
// resolver understands: a scheme listed that Resolve refuses is an account whose deliveries are answered 500 for ever.
func TestTheSchemesTheAdminAcceptsAreExactlyTheOnesTheResolverReads(t *testing.T) {
	t.Parallel()
	r := payments.NewSecrets(env(map[string]string{"A_KEY": "k"}))
	for _, scheme := range payments.SecretSchemes {
		ref := scheme + ":/nonexistent/secret"
		if scheme == "env" {
			ref = "env:A_KEY"
		}
		if _, err := r.Resolve(context.Background(), ref); err != nil && strings.Contains(err.Error(), "is not one this build resolves") {
			t.Errorf("the admin accepts %s: but the resolver does not read it", scheme)
		}
	}
	if _, err := r.Resolve(context.Background(), "vault:secret/x"); err == nil || !strings.Contains(err.Error(), "is not one this build resolves") {
		t.Errorf("the resolver reads vault:, which the admin would refuse: %v", err)
	}
}
