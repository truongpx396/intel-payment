package payments_test

import (
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/billing/contracts"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
)

// R097b: a provider adapter that exists but is not in the binary's registry is a provider whose webhooks are answered 404
// "unknown account" in production while every test of it passes. The registry is held equal to the adapter directories.

func TestEveryProviderAdapterIsServedByTheBinaries(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	got, err := payments.Providers(payments.ProviderDeps{Secrets: payments.NewSecrets(env(nil)), Catalog: h.Catalog, Clock: h.Clock, Tolerance: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("../../../billing/adapters/driven")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "postgres" { // the books are not a provider
			want = append(want, e.Name())
		}
	}
	var have []string
	for name, p := range got {
		have = append(have, string(name))
		if p.Name() != name {
			t.Errorf("registered under %q but calls itself %q: the route would find one adapter and verify with another", name, p.Name())
		}
	}
	sort.Strings(want)
	sort.Strings(have)
	if !slices.Equal(want, have) {
		t.Fatalf("the binaries serve %v and the adapters are %v", have, want)
	}
}

func TestTheRegistryIsRefusedWithoutWhatTheAdaptersNeed(t *testing.T) {
	t.Parallel()
	h := contracts.NewHarness()
	ok := payments.ProviderDeps{Secrets: payments.NewSecrets(env(nil)), Catalog: h.Catalog, Clock: h.Clock, Tolerance: time.Minute}
	if _, err := payments.Providers(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*payments.ProviderDeps){
		"no secrets":        func(d *payments.ProviderDeps) { d.Secrets = nil },
		"no catalogue":      func(d *payments.ProviderDeps) { d.Catalog = nil },
		"no clock":          func(d *payments.ProviderDeps) { d.Clock = nil },
		"a negative window": func(d *payments.ProviderDeps) { d.Tolerance = -time.Second },
	} {
		d := ok
		mutate(&d)
		if _, err := payments.Providers(d); err == nil {
			t.Errorf("%s: the registry was built", name)
		}
	}
}
