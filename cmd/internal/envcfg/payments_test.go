package envcfg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
)

// R097: what the payment layer is told. The knobs are few — how far a signature's timestamp may be from now, how long
// entitlements hold after a failed payment, how often the webhook inbox is worked, how long a processed event is kept —
// and each is checked the way every other is: an unusable value stops the service at start, naming the variable, and
// never becomes a default nobody chose.

func read(env map[string]string) (envcfg.Payments, error) {
	r := envcfg.NewReader(func(k string) string { return env[k] })
	p := r.Payments()
	return p, r.Err()
}

func TestThePaymentDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	p, err := read(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := envcfg.Payments{WebhookTolerance: 5 * time.Minute, DunningGrace: 7 * 24 * time.Hour, WebhookTick: 5 * time.Second, EventRetention: 90 * 24 * time.Hour}
	if p != want {
		t.Fatalf("defaults %+v, want %+v", p, want)
	}
}

func TestEveryPaymentVariableLandsInItsField(t *testing.T) {
	t.Parallel()
	p, err := read(map[string]string{
		"PAYMENT_WEBHOOK_TOLERANCE": "90s", "PAYMENT_DUNNING_GRACE_DAYS": "3", "PAYMENT_WEBHOOK_TICK_INTERVAL": "750ms",
		"PAYMENT_PAYMENT_EVENT_RETENTION": "30d",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := envcfg.Payments{WebhookTolerance: 90 * time.Second, DunningGrace: 3 * 24 * time.Hour, WebhookTick: 750 * time.Millisecond, EventRetention: 30 * 24 * time.Hour}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
}

func TestRetentionIsAcceptedInDaysAndInHours(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "90d": 90 * 24 * time.Hour, "365d": 365 * 24 * time.Hour, "168h": 168 * time.Hour, "2160h": 2160 * time.Hour} {
		p, err := read(map[string]string{"PAYMENT_PAYMENT_EVENT_RETENTION": in})
		if err != nil || p.EventRetention != want {
			t.Errorf("%q: got %v, %v; want %v", in, p.EventRetention, err, want)
		}
	}
}

func TestAnUnusablePaymentValueStopsTheServiceNamingTheVariable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, value string }{
		// A tolerance of nothing rejects every delivery; one past an hour makes a captured delivery replayable for that long.
		{"PAYMENT_WEBHOOK_TOLERANCE", "0"}, {"PAYMENT_WEBHOOK_TOLERANCE", "0s"}, {"PAYMENT_WEBHOOK_TOLERANCE", "-5m"},
		{"PAYMENT_WEBHOOK_TOLERANCE", "five minutes"}, {"PAYMENT_WEBHOOK_TOLERANCE", "61m"}, {"PAYMENT_WEBHOOK_TOLERANCE", "2h"},
		// A grace of zero would silently become the default; days are a whole number.
		{"PAYMENT_DUNNING_GRACE_DAYS", "0"}, {"PAYMENT_DUNNING_GRACE_DAYS", "-1"}, {"PAYMENT_DUNNING_GRACE_DAYS", "7d"},
		{"PAYMENT_DUNNING_GRACE_DAYS", "1.5"}, {"PAYMENT_DUNNING_GRACE_DAYS", "366"}, {"PAYMENT_DUNNING_GRACE_DAYS", "a week"},
		// A tick of nothing spins on an idle inbox.
		{"PAYMENT_WEBHOOK_TICK_INTERVAL", "0"}, {"PAYMENT_WEBHOOK_TICK_INTERVAL", "0ms"}, {"PAYMENT_WEBHOOK_TICK_INTERVAL", "-1s"},
		{"PAYMENT_WEBHOOK_TICK_INTERVAL", "often"}, {"PAYMENT_WEBHOOK_TICK_INTERVAL", "10m"},
		// Retention must outlast a provider's redelivery: a floor of a week, and a unit.
		{"PAYMENT_PAYMENT_EVENT_RETENTION", "6d"}, {"PAYMENT_PAYMENT_EVENT_RETENTION", "0d"}, {"PAYMENT_PAYMENT_EVENT_RETENTION", "0"},
		{"PAYMENT_PAYMENT_EVENT_RETENTION", "-30d"}, {"PAYMENT_PAYMENT_EVENT_RETENTION", "90"}, {"PAYMENT_PAYMENT_EVENT_RETENTION", "forever"},
		{"PAYMENT_PAYMENT_EVENT_RETENTION", "d"}, {"PAYMENT_PAYMENT_EVENT_RETENTION", "1.5d"}, {"PAYMENT_PAYMENT_EVENT_RETENTION", "167h"},
	} {
		_, err := read(map[string]string{tc.name: tc.value})
		if err == nil {
			t.Errorf("%s=%q was accepted", tc.name, tc.value)
			continue
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s=%q: the error does not name the variable: %v", tc.name, tc.value, err)
		}
	}
}

func TestEveryBadPaymentValueIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	_, err := read(map[string]string{
		"PAYMENT_WEBHOOK_TOLERANCE": "0", "PAYMENT_DUNNING_GRACE_DAYS": "0", "PAYMENT_WEBHOOK_TICK_INTERVAL": "0", "PAYMENT_PAYMENT_EVENT_RETENTION": "1d",
	})
	if err == nil {
		t.Fatal("four bad values were accepted")
	}
	for _, name := range []string{"PAYMENT_WEBHOOK_TOLERANCE", "PAYMENT_DUNNING_GRACE_DAYS", "PAYMENT_WEBHOOK_TICK_INTERVAL", "PAYMENT_PAYMENT_EVENT_RETENTION"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s is not named, so the operator fixes one and finds the next at the next start: %v", name, err)
		}
	}
}
