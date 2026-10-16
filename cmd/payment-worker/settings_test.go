package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/metering"
)

// R068: the worker reads the same engine settings paymentd does — one set of names, one set of refusals — and a
// few of its own, and stops before it dials anything when any is wrong.

func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(goleak.IgnoreAnyFunction("net/http.(*persistConn).readLoop"), goleak.IgnoreAnyFunction("net/http.(*persistConn).writeLoop")); err != nil {
			os.Stderr.WriteString("goleak: " + err.Error() + "\n")
			code = 1
		}
	}
	os.Exit(code)
}

const (
	dsnWithSecret   = "postgres://payment:s3cret-dsn@postgres:5432/payment?sslmode=disable"
	redisWithSecret = "redis://payment:s3cret-redis@redis:6379/0"
)

func base() map[string]string {
	return map[string]string{"PAYMENT_BALANCE_REDIS_URL": redisWithSecret, "PAYMENT_LEDGER_DSN": dsnWithSecret}
}

func with(kv ...string) map[string]string {
	m := base()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func getenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTheWorkersDefaultsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()
	s, err := parseSettings(getenv(base()))
	if err != nil {
		t.Fatal(err)
	}
	if want := (metering.Config{BalanceRedisURL: redisWithSecret, LedgerDSN: dsnWithSecret}).WithDefaults(); s.Engine != want {
		t.Errorf("the engine config is not the documented defaults:\n got %+v\nwant %+v", s.Engine, want)
	}
	if s.DrainInterval != 250*time.Millisecond || s.OwnershipRetry != 2*time.Second || s.TransferRedrive != 30*time.Second ||
		s.JournalReplayInterval != 5*time.Second || s.ShutdownTimeout != 30*time.Second || s.OpsAddr != ":9091" {
		t.Errorf("worker defaults = %+v", s)
	}
}

func TestEveryWorkerVariableLandsInItsField(t *testing.T) {
	t.Parallel()
	s, err := parseSettings(getenv(with(
		"PAYMENT_SHARDS", "64", "PAYMENT_DRAIN_BATCH", "50", "PAYMENT_SETTLEMENT", "journal",
		"PAYMENT_DRAIN_INTERVAL", "20ms", "PAYMENT_OWNERSHIP_RETRY", "700ms", "PAYMENT_TRANSFER_REDRIVE", "45s",
		"PAYMENT_JOURNAL_REPLAY_INTERVAL", "9s", "PAYMENT_SHUTDOWN_TIMEOUT", "12s", "PAYMENT_WORKER_ADDR", "127.0.0.1:9191",
	)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Engine.Shards != 64 || s.Engine.DrainBatch != 50 || s.Engine.Settlement != metering.SettlementJournal {
		t.Errorf("engine settings did not arrive: %+v", s.Engine)
	}
	if s.DrainInterval != 20*time.Millisecond || s.OwnershipRetry != 700*time.Millisecond || s.TransferRedrive != 45*time.Second ||
		s.JournalReplayInterval != 9*time.Second || s.ShutdownTimeout != 12*time.Second || s.OpsAddr != "127.0.0.1:9191" {
		t.Errorf("worker settings = %+v", s)
	}
}

func TestTheWorkerRefusesWhatPaymentdRefusesAndWhatOnlyItNeeds(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no ledger", with("PAYMENT_LEDGER_DSN", "-"), "PAYMENT_LEDGER_DSN"},
		{"no hot store", with("PAYMENT_BALANCE_REDIS_URL", "-"), "PAYMENT_BALANCE_REDIS_URL"},
		{"unknown settlement", with("PAYMENT_SETTLEMENT", "eventually"), "PAYMENT_SETTLEMENT"},
		{"a hot guard that outlives the durable one", with("PAYMENT_HOT_IDEM_TTL", "100h"), "PAYMENT_HOT_IDEM_TTL"},
		{"a drain interval that is not a duration", with("PAYMENT_DRAIN_INTERVAL", "often"), "PAYMENT_DRAIN_INTERVAL"},
		{"a drain interval of nothing, which would spin", with("PAYMENT_DRAIN_INTERVAL", "0s"), "PAYMENT_DRAIN_INTERVAL"},
		{"an ownership retry of nothing", with("PAYMENT_OWNERSHIP_RETRY", "0s"), "PAYMENT_OWNERSHIP_RETRY"},
		{"a journal replay interval that is not one", with("PAYMENT_JOURNAL_REPLAY_INTERVAL", "x"), "PAYMENT_JOURNAL_REPLAY_INTERVAL"},
		{"a redrive that is negative", with("PAYMENT_TRANSFER_REDRIVE", "-5s"), "PAYMENT_TRANSFER_REDRIVE"},
		{"a shutdown timeout of nothing", with("PAYMENT_SHUTDOWN_TIMEOUT", "0s"), "PAYMENT_SHUTDOWN_TIMEOUT"},
		{"an ops address that is not host:port", with("PAYMENT_WORKER_ADDR", "9091"), "PAYMENT_WORKER_ADDR"},
		{"the JetStream bus, which the worker does not read intents from yet", with("PAYMENT_BUS", "nats_jetstream", "PAYMENT_BUS_URL", "nats://nats:4222"), "PAYMENT_BUS"},
		{"an unknown bus", with("PAYMENT_BUS", "kafka"), "PAYMENT_BUS"},
		{"a log level that is not one", with("PAYMENT_LOG_LEVEL", "chatty"), "PAYMENT_LOG_LEVEL"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSettings(getenv(r.env))
			if err == nil {
				t.Fatal("accepted; the worker would start with it")
			}
			if !strings.Contains(err.Error(), r.want) {
				t.Errorf("the refusal does not name %s:\n%v", r.want, err)
			}
			for _, secret := range []string{"s3cret-dsn", "s3cret-redis"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal echoes a credential:\n%v", err)
				}
			}
		})
	}
}

func TestABadConfigurationStopsTheWorkerBeforeItDialsAnything(t *testing.T) {
	t.Parallel()
	env := with("PAYMENT_BALANCE_REDIS_URL", "redis://127.0.0.1:1/0", "PAYMENT_LEDGER_DSN", "postgres://x:y@127.0.0.1:1/z", "PAYMENT_SETTLEMENT", "eventually")
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	code := run(ctx, nil, getenv(env), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "PAYMENT_SETTLEMENT") {
		t.Errorf("exit %d, stderr:\n%s", code, errb.String())
	}
	if time.Since(start) > 3*time.Second {
		t.Error("the worker dialled something before it checked its settings")
	}
	if code := run(ctx, []string{"drain"}, getenv(base()), &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Errorf("an unknown command: exit %d, stderr:\n%s", code, errb.String())
	}
}

// R098: the worker is where payments are fulfilled, and it reads their settings like every other: documented defaults,
// each value in its field, and a refusal that names the variable.
func TestThePaymentSettingsOfTheWorkerAreReadAndChecked(t *testing.T) {
	t.Parallel()
	s, err := parseSettings(getenv(base()))
	if err != nil {
		t.Fatal(err)
	}
	want := envcfg.Payments{WebhookTolerance: 5 * time.Minute, DunningGrace: 7 * 24 * time.Hour, WebhookTick: 5 * time.Second, EventRetention: 90 * 24 * time.Hour}
	if s.Payments != want {
		t.Errorf("payment defaults %+v, want %+v", s.Payments, want)
	}
	s, err = parseSettings(getenv(with("PAYMENT_WEBHOOK_TOLERANCE", "2m", "PAYMENT_DUNNING_GRACE_DAYS", "3", "PAYMENT_WEBHOOK_TICK_INTERVAL", "1s", "PAYMENT_PAYMENT_EVENT_RETENTION", "30d")))
	if err != nil {
		t.Fatal(err)
	}
	if want := (envcfg.Payments{WebhookTolerance: 2 * time.Minute, DunningGrace: 3 * 24 * time.Hour, WebhookTick: time.Second, EventRetention: 30 * 24 * time.Hour}); s.Payments != want {
		t.Errorf("payment settings %+v, want %+v", s.Payments, want)
	}
	for name, bad := range map[string]string{
		"PAYMENT_WEBHOOK_TOLERANCE": "0", "PAYMENT_DUNNING_GRACE_DAYS": "0", "PAYMENT_WEBHOOK_TICK_INTERVAL": "10m", "PAYMENT_PAYMENT_EVENT_RETENTION": "1d",
	} {
		if _, err := parseSettings(getenv(with(name, bad))); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: %v, want a refusal naming the variable", name, bad, err)
		}
	}
}
