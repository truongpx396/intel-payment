package main

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
)

// R067: paymentd reads its environment in cmd/ only, and refuses to start on every setting the configuration
// refuses — each refusal naming the variable an operator has to fix, all of them at once, and never echoing a
// secret. The settings are the whole of what the process is told: a typo that silently fell back to a default
// would be a ceiling or a policy that silently means something else.

const (
	dsnWithSecret   = "postgres://payment:s3cret-dsn@postgres:5432/payment?sslmode=disable"
	redisWithSecret = "redis://payment:s3cret-redis@redis:6379/0"
	plainToken      = "plaintext-token-do-not-echo"
)

// base is the least a bearer-mode paymentd needs.
func base() map[string]string {
	return map[string]string{
		"PAYMENT_BALANCE_REDIS_URL": redisWithSecret,
		"PAYMENT_LEDGER_DSN":        dsnWithSecret,
		"PAYMENT_SERVICE_AUTH":      "bearer",
		"PAYMENT_SERVICE_TOKENS":    "billing:" + auth.HashToken("a-token") + ":acme:record+grant",
	}
}

func with(kv ...string) map[string]string {
	m := base()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
			continue
		}
		m[kv[i]] = kv[i+1]
	}
	return m
}

func getenv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTheDocumentedDefaultsApplyToTheLeastAnOperatorMustSay(t *testing.T) {
	t.Parallel()
	s, err := parseSettings(getenv(base()))
	if err != nil {
		t.Fatal(err)
	}
	want := metering.Config{BalanceRedisURL: redisWithSecret, LedgerDSN: dsnWithSecret}.WithDefaults()
	if s.Engine != want {
		t.Errorf("the engine config is not the documented defaults:\n got %+v\nwant %+v", s.Engine, want)
	}
	if s.HTTPAddr != ":8080" || s.GRPCAddr != ":9090" {
		t.Errorf("listeners = %q and %q, documented defaults are :8080 and :9090", s.HTTPAddr, s.GRPCAddr)
	}
	if s.Auth != "bearer" {
		t.Errorf("auth = %q, want bearer", s.Auth)
	}
}

// Every variable lands in the field it is documented to set, and in no other: the expected value is built by hand,
// every field different from its default, so a variable read into the wrong field or not read at all fails here.
func TestEveryVariableLandsInItsField(t *testing.T) {
	t.Parallel()
	env := with(
		"PAYMENT_REALM", "acme",
		"PAYMENT_SUBJECT_PREFIX", "acme-billing",
		"PAYMENT_SHARDS", "64",
		"PAYMENT_SETTLEMENT", "journal",
		"PAYMENT_HOT_ACK_WAIT", "aof_local",
		"PAYMENT_LEDGER_GRANULARITY", "rollup",
		"PAYMENT_DRAIN_BATCH", "100",
		"PAYMENT_HOT_IDEM_TTL", "12h",
		"PAYMENT_USAGE_IDEM_WINDOW", "48h",
		"PAYMENT_IDEM_SAFETY_MARGIN", "30m",
		"PAYMENT_MAX_CLOCK_SKEW", "1m",
		"PAYMENT_RECONCILE_INTERVAL", "10m",
		"PAYMENT_RECONCILE_FULL_SWEEP_INTERVAL", "12h",
		"PAYMENT_RECONCILE_TOLERANCE", "3",
		"PAYMENT_GAP_TIMEOUT", "90s",
		"PAYMENT_TRANSFER_TRANSIT_ALARM", "2m",
		"PAYMENT_ADMIT_TIMEOUT", "100ms",
		"PAYMENT_RECORD_TIMEOUT", "1s",
		"PAYMENT_ADMIT_FAIL_POLICY", "fail_open",
		"PAYMENT_REQUIRE_MAX_COST", "true",
		"PAYMENT_MAX_OPERATION_AMOUNT", "1000000",
		"PAYMENT_NEGATIVE_BALANCE_POLICY", "allow_debt",
		"PAYMENT_CREDIT_EXPIRY", "lots_fefo",
		"PAYMENT_BUS_MAX_LEN", "5000",
		"PAYMENT_BUS_ACK_WAIT", "10s",
		"PAYMENT_BUS_MAX_ATTEMPTS", "3",
	)
	s, err := parseSettings(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	want := metering.Config{
		Realm: "acme", BalanceRedisURL: redisWithSecret, LedgerDSN: dsnWithSecret, SubjectPrefix: "acme-billing", Shards: 64,
		Settlement: metering.SettlementJournal, HotAckWait: metering.HotAckAOFLocal, LedgerGranularity: metering.GranularityRollup, DrainBatch: 100,
		HotIdemTTL: 12 * time.Hour, UsageIdemWindow: 48 * time.Hour, IdemSafetyMargin: 30 * time.Minute, MaxClockSkew: time.Minute,
		ReconcileInterval: 10 * time.Minute, ReconcileFullSweepInterval: 12 * time.Hour, ReconcileTolerance: 3,
		GapTimeout: 90 * time.Second, TransferTransitAlarm: 2 * time.Minute,
		AdmitTimeout: 100 * time.Millisecond, RecordTimeout: time.Second, AdmitFail: metering.FailOpen, RequireMaxCost: true,
		MaxOperationAmount: 1_000_000, NegativeBalance: metering.AllowDebt, CreditExpiry: metering.ExpiryLotsFEFO,
		BusMaxLen: 5000, AckWait: 10 * time.Second, MaxAttempts: 3,
	}.WithDefaults()
	if s.Engine != want {
		t.Errorf("a variable is read into the wrong field, or not read:\n got %+v\nwant %+v", s.Engine, want)
	}

	tr := with(
		"PAYMENT_HTTP_ADDR", "127.0.0.1:9000", "PAYMENT_GRPC_ADDR", "127.0.0.1:9001", "PAYMENT_LEGACY_429_FOR_LIMITS", "true",
		"PAYMENT_BUS", "nats_jetstream", "PAYMENT_BUS_URL", "nats://nats:4222", "PAYMENT_BUS_RETENTION", "72h",
	)
	s, err = parseSettings(getenv(tr))
	if err != nil {
		t.Fatal(err)
	}
	if s.HTTPAddr != "127.0.0.1:9000" || s.GRPCAddr != "127.0.0.1:9001" || !s.Legacy429 {
		t.Errorf("transport settings = %q %q legacy429=%v", s.HTTPAddr, s.GRPCAddr, s.Legacy429)
	}
	if s.Bus != "nats_jetstream" || s.BusURL != "nats://nats:4222" || s.BusRetention != 72*time.Hour {
		t.Errorf("bus settings = %q %q %v", s.Bus, s.BusURL, s.BusRetention)
	}
}

// One row per refusal. Each names the variable the operator must fix, which is the point of the row.
func TestEveryInvalidSettingIsRefusedAndNamesItsVariable(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		env  map[string]string
		want []string // every one must appear in the refusal
	}{
		// what Config.Validate refuses
		{"no hot store", with("PAYMENT_BALANCE_REDIS_URL", "-"), []string{"PAYMENT_BALANCE_REDIS_URL"}},
		{"no ledger", with("PAYMENT_LEDGER_DSN", "-"), []string{"PAYMENT_LEDGER_DSN"}},
		{"a realm that is not a slug", with("PAYMENT_REALM", "Not A Realm!"), []string{"PAYMENT_REALM"}},
		{"too few shards", with("PAYMENT_SHARDS", "-1"), []string{"PAYMENT_SHARDS"}},
		{"too many shards", with("PAYMENT_SHARDS", "20000"), []string{"PAYMENT_SHARDS"}},
		{"unknown settlement", with("PAYMENT_SETTLEMENT", "eventually"), []string{"PAYMENT_SETTLEMENT"}},
		{"unknown hot ack wait", with("PAYMENT_HOT_ACK_WAIT", "fsync"), []string{"PAYMENT_HOT_ACK_WAIT"}},
		{"unknown ledger granularity", with("PAYMENT_LEDGER_GRANULARITY", "daily"), []string{"PAYMENT_LEDGER_GRANULARITY"}},
		{"unknown admit fail policy", with("PAYMENT_ADMIT_FAIL_POLICY", "fail_sideways"), []string{"PAYMENT_ADMIT_FAIL_POLICY"}},
		{"unknown negative balance policy", with("PAYMENT_NEGATIVE_BALANCE_POLICY", "forgive"), []string{"PAYMENT_NEGATIVE_BALANCE_POLICY"}},
		{"unknown credit expiry", with("PAYMENT_CREDIT_EXPIRY", "lots_lifo"), []string{"PAYMENT_CREDIT_EXPIRY"}},
		{"the retired lots_fifo", with("PAYMENT_CREDIT_EXPIRY", "lots_fifo"), []string{"PAYMENT_CREDIT_EXPIRY", "lots_fefo"}},
		{"a drain batch below one", with("PAYMENT_DRAIN_BATCH", "-5"), []string{"PAYMENT_DRAIN_BATCH"}},
		{"a hot guard that outlives the durable one", with("PAYMENT_HOT_IDEM_TTL", "100h", "PAYMENT_USAGE_IDEM_WINDOW", "72h"), []string{"PAYMENT_HOT_IDEM_TTL", "PAYMENT_USAGE_IDEM_WINDOW"}},
		{"a safety margin that swallows the window", with("PAYMENT_IDEM_SAFETY_MARGIN", "72h"), []string{"PAYMENT_IDEM_SAFETY_MARGIN", "PAYMENT_USAGE_IDEM_WINDOW"}},
		{"a negative drift tolerance", with("PAYMENT_RECONCILE_TOLERANCE", "-1"), []string{"PAYMENT_RECONCILE_TOLERANCE"}},
		{"an operation amount past exact arithmetic", with("PAYMENT_MAX_OPERATION_AMOUNT", "9007199254740993"), []string{"PAYMENT_MAX_OPERATION_AMOUNT"}},
		{"a negative operation amount", with("PAYMENT_MAX_OPERATION_AMOUNT", "-1"), []string{"PAYMENT_MAX_OPERATION_AMOUNT"}},
		{"a bus cap below one", with("PAYMENT_BUS_MAX_LEN", "-1"), []string{"PAYMENT_BUS_MAX_LEN"}},
		{"fewer than one delivery attempt", with("PAYMENT_BUS_MAX_ATTEMPTS", "-1"), []string{"PAYMENT_BUS_MAX_ATTEMPTS"}},

		// a value that is not even the right kind
		{"a duration that is not one", with("PAYMENT_ADMIT_TIMEOUT", "soon"), []string{"PAYMENT_ADMIT_TIMEOUT"}},
		{"a negative duration", with("PAYMENT_RECORD_TIMEOUT", "-1s"), []string{"PAYMENT_RECORD_TIMEOUT"}},
		{"an integer that is not one", with("PAYMENT_DRAIN_BATCH", "ten"), []string{"PAYMENT_DRAIN_BATCH"}},
		{"a fractional integer", with("PAYMENT_MAX_OPERATION_AMOUNT", "1.5"), []string{"PAYMENT_MAX_OPERATION_AMOUNT"}},
		{"a boolean that is not one", with("PAYMENT_REQUIRE_MAX_COST", "maybe"), []string{"PAYMENT_REQUIRE_MAX_COST"}},
		{"a legacy-429 flag that is not a boolean", with("PAYMENT_LEGACY_429_FOR_LIMITS", "yes please"), []string{"PAYMENT_LEGACY_429_FOR_LIMITS"}},
		{"a log level that is not one", with("PAYMENT_LOG_LEVEL", "chatty"), []string{"PAYMENT_LOG_LEVEL"}},

		// the bus
		{"an unknown bus", with("PAYMENT_BUS", "kafka"), []string{"PAYMENT_BUS"}},
		{"jetstream without a url", with("PAYMENT_BUS", "nats_jetstream"), []string{"PAYMENT_BUS_URL"}},

		// the transports and who may call them
		{"an unknown auth mode", with("PAYMENT_SERVICE_AUTH", "basic"), []string{"PAYMENT_SERVICE_AUTH"}},
		{"auth none on every interface", with("PAYMENT_SERVICE_AUTH", "none", "PAYMENT_SERVICE_TOKENS", "-"), []string{"PAYMENT_SERVICE_AUTH", "PAYMENT_HTTP_ADDR", "loopback"}},
		{"auth none with only the REST listener on loopback", with("PAYMENT_SERVICE_AUTH", "none", "PAYMENT_SERVICE_TOKENS", "-", "PAYMENT_HTTP_ADDR", "127.0.0.1:8080"), []string{"PAYMENT_GRPC_ADDR", "loopback"}},
		{"auth none on a private network address", with("PAYMENT_SERVICE_AUTH", "none", "PAYMENT_SERVICE_TOKENS", "-", "PAYMENT_HTTP_ADDR", "10.0.0.5:8080", "PAYMENT_GRPC_ADDR", "127.0.0.1:9090"), []string{"PAYMENT_HTTP_ADDR"}},
		{"a token list with a plaintext token in it", with("PAYMENT_SERVICE_TOKENS", "billing:"+plainToken+":acme:record"), []string{"PAYMENT_SERVICE_TOKENS"}},
		{"a token list with a wildcard realm", with("PAYMENT_SERVICE_TOKENS", "billing:"+auth.HashToken("t")+":*:record"), []string{"PAYMENT_SERVICE_TOKENS"}},
		{"a token list with an unknown privilege", with("PAYMENT_SERVICE_TOKENS", "billing:"+auth.HashToken("t")+":acme:superuser"), []string{"PAYMENT_SERVICE_TOKENS"}},
		{"mtls without a certificate", mtls("PAYMENT_TLS_CERT_FILE", "-"), []string{"PAYMENT_TLS_CERT_FILE"}},
		{"mtls without a key", mtls("PAYMENT_TLS_KEY_FILE", "-"), []string{"PAYMENT_TLS_KEY_FILE"}},
		{"mtls without the CA that verifies callers", mtls("PAYMENT_TLS_CLIENT_CA_FILE", "-"), []string{"PAYMENT_TLS_CLIENT_CA_FILE"}},
		{"mtls with a malformed caller list", mtls("PAYMENT_SERVICE_CERTS", "billing"), []string{"PAYMENT_SERVICE_CERTS"}},
		{"a certificate without its key", with("PAYMENT_TLS_CERT_FILE", "/tls/cert.pem"), []string{"PAYMENT_TLS_KEY_FILE"}},
		{"a key without its certificate", with("PAYMENT_TLS_KEY_FILE", "/tls/key.pem"), []string{"PAYMENT_TLS_CERT_FILE"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSettings(getenv(r.env))
			if err == nil {
				t.Fatal("the settings were accepted; paymentd would start with them")
			}
			for _, w := range r.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal does not mention %q, so an operator cannot tell what to fix:\n%v", w, err)
				}
			}
		})
	}
}

func mtls(kv ...string) map[string]string {
	m := with("PAYMENT_SERVICE_AUTH", "mtls", "PAYMENT_SERVICE_TOKENS", "-",
		"PAYMENT_SERVICE_CERTS", "billing:billing.internal:acme:record+grant",
		"PAYMENT_TLS_CERT_FILE", "/tls/server.pem", "PAYMENT_TLS_KEY_FILE", "/tls/server-key.pem", "PAYMENT_TLS_CLIENT_CA_FILE", "/tls/ca.pem")
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func TestEverythingWrongIsReportedAtOnce(t *testing.T) {
	t.Parallel()
	_, err := parseSettings(getenv(with("PAYMENT_SETTLEMENT", "eventually", "PAYMENT_DRAIN_BATCH", "-5", "PAYMENT_ADMIT_TIMEOUT", "soon", "PAYMENT_LEDGER_DSN", "-")))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, v := range []string{"PAYMENT_SETTLEMENT", "PAYMENT_DRAIN_BATCH", "PAYMENT_ADMIT_TIMEOUT", "PAYMENT_LEDGER_DSN"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("the refusal leaves out %s, so an operator fixes one thing per restart:\n%v", v, err)
		}
	}
}

// A refusal is logged, shipped and pasted into tickets. The credentials it is about must not be in it.
func TestARefusalNeverEchoesASecret(t *testing.T) {
	t.Parallel()
	bad := []map[string]string{
		with("PAYMENT_LEDGER_DSN", "not a url "+dsnWithSecret, "PAYMENT_SETTLEMENT", "eventually"),
		with("PAYMENT_BALANCE_REDIS_URL", "redis://payment:s3cret-redis@[bad"),
		with("PAYMENT_SERVICE_TOKENS", "billing:"+plainToken+":acme:record"),
		with("PAYMENT_SERVICE_TOKENS", "billing:"+auth.HashToken("t")+":acme:superuser,"+plainToken),
		with("PAYMENT_BUS", "nats_jetstream", "PAYMENT_BUS_URL", "nats://user:s3cret-bus@nats:4222 bad"),
	}
	for i, env := range bad {
		_, err := parseSettings(getenv(env))
		if err == nil {
			t.Errorf("case %d accepted", i)
			continue
		}
		for _, secret := range []string{"s3cret-dsn", "s3cret-redis", "s3cret-bus", plainToken} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("case %d: the refusal echoes a secret (%q):\n%v", i, secret, err)
			}
		}
	}
}

// "none" accepts any caller, which is only safe where only the host itself can reach the listener.
func TestAuthNoneIsPermittedOnlyWhereNothingCanReachIt(t *testing.T) {
	t.Parallel()
	ok := []struct{ http, grpc string }{
		{"127.0.0.1:8080", "127.0.0.1:9090"},
		{"localhost:8080", "localhost:9090"},
		{"[::1]:8080", "[::1]:9090"},
		{"unix:/run/paymentd/http.sock", "unix:/run/paymentd/grpc.sock"},
		{"127.0.0.1:0", "127.0.0.1:0"},
	}
	for _, a := range ok {
		env := with("PAYMENT_SERVICE_AUTH", "none", "PAYMENT_SERVICE_TOKENS", "-", "PAYMENT_HTTP_ADDR", a.http, "PAYMENT_GRPC_ADDR", a.grpc)
		if _, err := parseSettings(getenv(env)); err != nil {
			t.Errorf("auth none on %s and %s is unreachable from outside the host, but was refused: %v", a.http, a.grpc, err)
		}
	}
	refused := []struct{ http, grpc string }{
		{":8080", ":9090"},
		{"0.0.0.0:8080", "127.0.0.1:9090"},
		{"127.0.0.1:8080", "0.0.0.0:9090"},
		{"[::]:8080", "127.0.0.1:9090"},
		{"192.168.1.10:8080", "127.0.0.1:9090"},
		{"example.com:8080", "127.0.0.1:9090"},
	}
	for _, a := range refused {
		env := with("PAYMENT_SERVICE_AUTH", "none", "PAYMENT_SERVICE_TOKENS", "-", "PAYMENT_HTTP_ADDR", a.http, "PAYMENT_GRPC_ADDR", a.grpc)
		if _, err := parseSettings(getenv(env)); err == nil {
			t.Errorf("auth none on %s and %s lets anyone who can route to it mint credits, but was accepted", a.http, a.grpc)
		}
	}
}
