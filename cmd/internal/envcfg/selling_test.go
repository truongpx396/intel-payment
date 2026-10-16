package envcfg_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
)

// R099: what paymentd is told about selling. A plan change, tax and where a buyer may be sent are decisions about a
// customer's money, so an unusable value stops the service at start, naming the variable — it never becomes a default
// nobody chose, and a mode that cannot work here (a tax service the binary has no way to call) is refused rather than
// silently becoming "nobody computes tax".

func readSelling(env map[string]string) (envcfg.Selling, error) {
	r := envcfg.NewReader(func(k string) string { return env[k] })
	s := r.Selling()
	return s, r.Err()
}

func TestSellingDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	s, err := readSelling(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.PlanChangePolicy != domain.AtPeriodEnd || s.TaxStrategy != domain.TaxProviderManaged || len(s.ReturnHosts) != 0 {
		t.Fatalf("defaults %+v, want at_period_end, provider_managed and no host list", s)
	}
}

func TestEverySellingVariableLandsInItsField(t *testing.T) {
	t.Parallel()
	s, err := readSelling(map[string]string{
		"PAYMENT_PLAN_CHANGE_POLICY": "immediate_prorate", "PAYMENT_TAX_STRATEGY": "none",
		"PAYMENT_CHECKOUT_RETURN_HOSTS": " App.Example.com , billing.example.com:8443 ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.PlanChangePolicy != domain.ImmediateProrate || s.TaxStrategy != domain.TaxNone {
		t.Errorf("policy %q tax %q", s.PlanChangePolicy, s.TaxStrategy)
	}
	if want := []string{"app.example.com", "billing.example.com:8443"}; !reflect.DeepEqual(s.ReturnHosts, want) {
		t.Errorf("hosts %q, want %q (trimmed and lower-cased: a URL's host is compared case-insensitively)", s.ReturnHosts, want)
	}
}

func TestAnUnusableSellingValueStopsTheServiceNamingTheVariable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value string }{
		{"PAYMENT_PLAN_CHANGE_POLICY", "sometimes"}, {"PAYMENT_PLAN_CHANGE_POLICY", "IMMEDIATE_PRORATE"}, {"PAYMENT_PLAN_CHANGE_POLICY", "now"},
		{"PAYMENT_TAX_STRATEGY", "compute_here"}, {"PAYMENT_TAX_STRATEGY", "Provider_Managed"},
		// external_hook is a real mode, but it needs the host's tax service, and the binary has no way to be given one:
		// accepting it would run with nobody computing tax.
		{"PAYMENT_TAX_STRATEGY", "external_hook"},
		// A host is a host or host:port — not a URL, not a path, not an empty entry that would match nothing or everything.
		{"PAYMENT_CHECKOUT_RETURN_HOSTS", "https://app.example.com"}, {"PAYMENT_CHECKOUT_RETURN_HOSTS", "app.example.com/back"},
		{"PAYMENT_CHECKOUT_RETURN_HOSTS", "app.example.com,,billing.example.com"}, {"PAYMENT_CHECKOUT_RETURN_HOSTS", "user@app.example.com"},
		{"PAYMENT_CHECKOUT_RETURN_HOSTS", "app example.com"}, {"PAYMENT_CHECKOUT_RETURN_HOSTS", "app.example.com:"}, {"PAYMENT_CHECKOUT_RETURN_HOSTS", "*.example.com"},
	} {
		_, err := readSelling(map[string]string{tc.name: tc.value})
		if err == nil {
			t.Errorf("%s=%q was accepted", tc.name, tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s=%q: the error does not name the variable: %v", tc.name, tc.value, err)
		}
	}
}
