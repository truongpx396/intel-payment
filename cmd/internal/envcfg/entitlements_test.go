package envcfg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
)

// R105: what the entitlement layer is told. Two knobs: how long a resolution may be kept (the bound on a lost announcement), and
// the identity of this replica, which names its subscription to the announcements. Each is checked as every other is: an unusable
// value stops the service at start, naming the variable, and never becomes a default nobody chose.

func readEnt(env map[string]string) (envcfg.Entitlements, error) {
	r := envcfg.NewReader(func(k string) string { return env[k] })
	e := r.Entitlements()
	return e, r.Err()
}

func TestTheEntitlementDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	e, err := readEnt(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (envcfg.Entitlements{CacheTTL: 30 * time.Second}); e != want {
		t.Fatalf("defaults %+v, want %+v", e, want)
	}
}

func TestEveryEntitlementVariableLandsInItsField(t *testing.T) {
	t.Parallel()
	e, err := readEnt(map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "2m", "PAYMENT_INSTANCE_ID": "paymentd-2.eu_west"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (envcfg.Entitlements{CacheTTL: 2 * time.Minute, InstanceID: "paymentd-2.eu_west"}); e != want {
		t.Fatalf("%+v, want %+v", e, want)
	}
	// Zero turns the cache off: every check reads the store. It is a decision, so it is allowed to be made.
	off, err := readEnt(map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "0s"})
	if err != nil || off.CacheTTL != 0 {
		t.Fatalf("a TTL of 0s: %+v, %v; want the cache off", off, err)
	}
	// The longest there is.
	if _, err := readEnt(map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "10m"}); err != nil {
		t.Errorf("10m, the longest allowed: %v", err)
	}
}

func TestAnUnusableEntitlementValueStopsTheServiceNamingTheVariable(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env   map[string]string
		names string
	}{
		"a TTL that is not a duration":  {map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "soon"}, "PAYMENT_ENTITLEMENT_CACHE_TTL"},
		"a negative TTL":                {map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "-1s"}, "PAYMENT_ENTITLEMENT_CACHE_TTL"},
		"a TTL past ten minutes":        {map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "10m1s"}, "PAYMENT_ENTITLEMENT_CACHE_TTL"},
		"a TTL of an hour":              {map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "1h"}, "PAYMENT_ENTITLEMENT_CACHE_TTL"},
		"an instance with a space":      {map[string]string{"PAYMENT_INSTANCE_ID": "pod 1"}, "PAYMENT_INSTANCE_ID"},
		"an instance with a wildcard":   {map[string]string{"PAYMENT_INSTANCE_ID": "pod-*"}, "PAYMENT_INSTANCE_ID"},
		"an instance that starts badly": {map[string]string{"PAYMENT_INSTANCE_ID": "-pod"}, "PAYMENT_INSTANCE_ID"},
		"an instance too long":          {map[string]string{"PAYMENT_INSTANCE_ID": strings.Repeat("a", 64)}, "PAYMENT_INSTANCE_ID"},
	} {
		_, err := readEnt(c.env)
		if err == nil || !strings.Contains(err.Error(), c.names) {
			t.Errorf("%s: %v; want an error naming %s", name, err, c.names)
		}
	}
}

func TestEveryBadEntitlementValueIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	_, err := readEnt(map[string]string{"PAYMENT_ENTITLEMENT_CACHE_TTL": "never", "PAYMENT_INSTANCE_ID": "a b"})
	if err == nil || !strings.Contains(err.Error(), "PAYMENT_ENTITLEMENT_CACHE_TTL") || !strings.Contains(err.Error(), "PAYMENT_INSTANCE_ID") {
		t.Fatalf("want both reported together: %v", err)
	}
}
