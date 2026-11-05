package envcfg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
)

// R115: what outbound delivery is told. Each value is checked as every other is: an unusable one stops the service at start, naming the
// variable, and never becomes a default nobody chose.

func readEvents(env map[string]string) (envcfg.Events, error) {
	r := envcfg.NewReader(func(k string) string { return env[k] })
	e := r.Events()
	return e, r.Err()
}

func TestTheEventDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	e, err := readEvents(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (envcfg.Events{Retention: 30 * 24 * time.Hour, TickInterval: 5 * time.Second}); e != want {
		t.Fatalf("defaults %+v, want %+v", e, want)
	}
}

func TestEveryEventVariableLandsInItsField(t *testing.T) {
	t.Parallel()
	if e, err := readEvents(map[string]string{"PAYMENT_EVENT_RETENTION": "30d"}); err != nil || e.Retention != 30*24*time.Hour {
		t.Fatalf("the documented form, 30d: %+v, %v", e, err)
	}
	e, err := readEvents(map[string]string{"PAYMENT_EVENT_RETENTION": "72h", "PAYMENT_EVENTS_TICK_INTERVAL": "1m", "PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (envcfg.Events{Retention: 72 * time.Hour, TickInterval: time.Minute, AllowPrivate: true}); e != want {
		t.Fatalf("%+v, want %+v", e, want)
	}
}

func TestAnUnusableEventSettingStopsTheServiceNamingTheVariable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ env, value, want string }{
		"retention below the retry window": {"PAYMENT_EVENT_RETENTION", "71h", "at least 3d (72h)"},
		"retention of two days":            {"PAYMENT_EVENT_RETENTION", "2d", "at least 3d (72h)"},
		"retention of nothing":             {"PAYMENT_EVENT_RETENTION", "0s", "at least 3d (72h)"},
		"retention that is not a duration": {"PAYMENT_EVENT_RETENTION", "a month", "number of days like 30d"},
		"retention of fractional days":     {"PAYMENT_EVENT_RETENTION", "1.5d", "number of days like 30d"},
		"a tick of nothing":                {"PAYMENT_EVENTS_TICK_INTERVAL", "0s", "must be positive"},
		"a tick longer than a minute":      {"PAYMENT_EVENTS_TICK_INTERVAL", "61s", "at most 1m0s"},
		"a tick that is negative":          {"PAYMENT_EVENTS_TICK_INTERVAL", "-5s", "must not be negative"},
		"private endpoints that is not":    {"PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS", "sometimes", "must be true or false"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := readEvents(map[string]string{tc.env: tc.value})
			if err == nil || !strings.Contains(err.Error(), tc.env) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to name %s and say %q", err, tc.env, tc.want)
			}
		})
	}
}
