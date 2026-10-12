//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/truongpx396/intel-payment/metering"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcclient"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// R067 / T067: cmd/paymentd is the whole service — the engine wired to the real stores and fronted by REST and
// gRPC behind a service credential. These tests run THE BINARY, as a process, with nothing but PAYMENT_*
// variables in its environment, against the real Postgres and Redis: what is under test is the wiring, which
// no package test can see.

var (
	paymentdOnce sync.Once
	paymentdPath string
	paymentdDir  string
	paymentdErr  error
)

func paymentdBinary(t *testing.T) string {
	t.Helper()
	paymentdOnce.Do(func() {
		paymentdDir, paymentdErr = os.MkdirTemp("", "paymentd-bin-")
		if paymentdErr != nil {
			return
		}
		paymentdPath = filepath.Join(paymentdDir, "paymentd")
		out, err := exec.Command("go", "build", "-o", paymentdPath, "../cmd/paymentd").CombinedOutput()
		if err != nil {
			paymentdErr = fmt.Errorf("go build ./cmd/paymentd: %w\n%s", err, out)
		}
	})
	if paymentdErr != nil {
		t.Fatal(paymentdErr)
	}
	return paymentdPath
}

func cleanupPaymentd() {
	if paymentdDir != "" {
		_ = os.RemoveAll(paymentdDir)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// daemon is one running paymentd.
type daemon struct {
	cmd    *exec.Cmd
	logs   *lockedBuffer
	exited chan struct{}
	err    error // the Wait error, valid once exited is closed
	env    map[string]string
}

const serviceToken = "integration-service-token"

// paymentdEnv is the environment of a paymentd serving one realm from one database.
func paymentdEnv(realm, dsn string, kv ...string) map[string]string {
	m := map[string]string{
		"PAYMENT_REALM":             realm,
		"PAYMENT_BALANCE_REDIS_URL": "redis://" + redis.Addr + "/0",
		"PAYMENT_LEDGER_DSN":        dsn,
		"PAYMENT_HTTP_ADDR":         "127.0.0.1:0",
		"PAYMENT_GRPC_ADDR":         "127.0.0.1:0",
		"PAYMENT_SERVICE_AUTH":      "bearer",
		"PAYMENT_SERVICE_TOKENS":    "integration:" + auth.HashToken(serviceToken) + ":" + realm + ":record+grant+admin",
		"PAYMENT_LOG_LEVEL":         "debug",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func startPaymentd(t *testing.T, env map[string]string, args ...string) *daemon {
	t.Helper()
	cmd := exec.Command(paymentdBinary(t), args...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v) // nothing else: the process reads PAYMENT_* and no ambient variable
	}
	d := &daemon{cmd: cmd, logs: &lockedBuffer{}, exited: make(chan struct{}), env: env}
	cmd.Stdout, cmd.Stderr = d.logs, d.logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.err = cmd.Wait(); close(d.exited) }()
	t.Cleanup(func() {
		select {
		case <-d.exited:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-d.exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-d.exited
			t.Errorf("paymentd did not stop within 15s of SIGTERM; logs:\n%s", d.logs.String())
		}
	})
	return d
}

// waitExit returns the process's exit code, failing if it is still running after within.
func (d *daemon) waitExit(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case <-d.exited:
		if d.cmd.ProcessState == nil {
			return -1
		}
		return d.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		t.Fatalf("paymentd is still running %v after it should have refused to start; logs:\n%s", within, d.logs.String())
		return 0
	}
}

// listening returns the address a transport reports having bound, from the structured log.
func (d *daemon) listening(t *testing.T, transport string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sc := bufio.NewScanner(strings.NewReader(d.logs.String()))
		for sc.Scan() {
			var line struct {
				Msg       string `json:"msg"`
				Transport string `json:"transport"`
				Addr      string `json:"addr"`
			}
			if json.Unmarshal(sc.Bytes(), &line) == nil && line.Msg == "listening" && line.Transport == transport {
				return line.Addr
			}
		}
		select {
		case <-d.exited:
			t.Fatalf("paymentd exited (%v) before it was listening on %s; logs:\n%s", d.err, transport, d.logs.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("paymentd never reported listening on %s; logs:\n%s", transport, d.logs.String())
	return ""
}

func (d *daemon) httpURL(t *testing.T) string { t.Helper(); return "http://" + d.listening(t, "http") }

func httpDo(t *testing.T, c *http.Client, method, url, token, body string, hdr ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	must(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func awaitReady(t *testing.T, d *daemon, c *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := httpDo(t, c, "GET", base+"/readyz", "", ""); code == 200 {
			return
		}
		select {
		case <-d.exited:
			t.Fatalf("paymentd exited (%v) before it was ready; logs:\n%s", d.err, d.logs.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("paymentd never became ready; logs:\n%s", d.logs.String())
}

// seededRealm is a migrated database holding one realm's configuration.
func seededRealm(t *testing.T) (realm, dsn string) {
	t.Helper()
	realm = fmt.Sprintf("pd%d", seq.Add(1))
	dsn = pg.NewDB(t)
	pool, err := pgadapter.Connect(t.Context(), dsn)
	must(t, err)
	t.Cleanup(pool.Close)
	seedRealmSQL(func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}, realm)
	return realm, dsn
}

// scopeStore is the hot store and the books a test reads and arranges state through, beside the service under test.
func scopeStore(t *testing.T, addr, realm, dsn string) (*hotredis.Store, *pgadapter.Books, metering.Config) {
	t.Helper()
	ctx := t.Context()
	pool, err := pgadapter.Connect(ctx, dsn)
	must(t, err)
	t.Cleanup(pool.Close)
	cfgStore, err := pgadapter.NewConfigStore(ctx, pool, pgadapter.ConfigOptions{})
	must(t, err)
	t.Cleanup(cfgStore.Close)
	cfg := metering.Config{Realm: domain.Realm(realm), BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}.WithDefaults()
	rdb := hotredisClient(addr)
	t.Cleanup(func() { _ = rdb.Close() })
	store, err := hotredis.New(rdb, hotredis.Options{Config: cfg, Pools: cfgStore, Limits: cfgStore, Clock: system.Clock{}})
	must(t, err)
	return store, pgadapter.NewBooks(pool), cfg
}

// hotBalance is the scope's balance across pools, read straight from Redis.
func hotBalance(t *testing.T, addr, realm, dsn string, sc domain.Scope) domain.Credits {
	t.Helper()
	store, _, _ := scopeStore(t, addr, realm, dsn)
	acct, err := store.Snapshot(t.Context(), sc)
	must(t, err)
	var sum domain.Credits
	for _, v := range acct.Pools {
		sum += v
	}
	return sum
}

// openScope opens the scope's shard in Redis the way the worker does at start, so the service has a hot account to serve.
func openScope(t *testing.T, addr, realm, dsn string, sc domain.Scope) {
	t.Helper()
	ctx := t.Context()
	store, books, cfg := scopeStore(t, addr, realm, dsn)
	must(t, books.EnsureHotConfig(ctx, cfg.Shards))
	sh := sc.Normalized().Shard(cfg.Shards)
	rec, err := books.EnsureShard(ctx, sh)
	must(t, err)
	must(t, store.OpenShard(ctx, sh, rec.Gen))
}

func TestPaymentdServesThePlatformEndpoints(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	d := startPaymentd(t, paymentdEnv(realm, dsn))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)

	if code, body := httpDo(t, c, "GET", base+"/healthz", "", ""); code != 200 || !strings.Contains(body, `"ok"`) {
		t.Errorf("/healthz = %d %s", code, body)
	}
	code, body := httpDo(t, c, "GET", base+"/readyz", "", "")
	var ready struct {
		Status, Migrations string
		Shards             int
	}
	must(t, json.Unmarshal([]byte(body), &ready))
	if code != 200 || ready.Status != "ready" || ready.Migrations != "head" || ready.Shards != 256 {
		t.Errorf("/readyz = %d %s, want ready at head with the 256 shards of the default", code, body)
	}
	code, body = httpDo(t, c, "GET", base+"/metrics", "", "")
	if code != 200 || !strings.Contains(body, "# TYPE metering_admit_denied_total counter") {
		t.Errorf("/metrics = %d and does not declare the catalogue:\n%.400s", code, body)
	}
	// The process says what it is listening on, and the binary's `healthcheck` agrees it is ready.
	hc := exec.Command(paymentdBinary(t), "healthcheck")
	hc.Env = []string{"PAYMENT_HTTP_ADDR=" + d.listening(t, "http")}
	if out, err := hc.CombinedOutput(); err != nil {
		t.Errorf("paymentd healthcheck: %v\n%s", err, out)
	}
}

func TestPaymentdServesMoneyThroughBothTransportsBehindACredential(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	openScope(t, redis.Addr, realm, dsn, sc)
	d := startPaymentd(t, paymentdEnv(realm, dsn))
	base := d.httpURL(t)
	c := &http.Client{Timeout: 10 * time.Second}
	awaitReady(t, d, c, base)
	scopeJSON := fmt.Sprintf(`{"kind":%q,"id":%q}`, sc.Kind, sc.ID)
	q := fmt.Sprintf("?scope_kind=%s&scope_id=%s", sc.Kind, sc.ID)

	// No credential, no service: every /v1 route is closed to a stranger.
	if code, body := httpDo(t, c, "POST", base+"/v1/admit", "", `{"scope":`+scopeJSON+`}`); code != 401 {
		t.Errorf("admit without a credential = %d %s, want 401", code, body)
	}
	if code, _ := httpDo(t, c, "GET", base+"/v1/balance"+q, "not-the-token", ""); code != 401 {
		t.Errorf("balance with a wrong token = %d, want 401", code)
	}

	// A fresh scope has no credits: refused before any work.
	if code, body := httpDo(t, c, "POST", base+"/v1/admit", serviceToken, `{"scope":`+scopeJSON+`,"resource":"llm.chat"}`); code != 402 || !strings.Contains(body, "payment_required") {
		t.Errorf("admit on an empty scope = %d %s, want 402 payment_required", code, body)
	}
	// Grant, then spend, once.
	if code, body := httpDo(t, c, "POST", base+"/v1/grant", serviceToken, `{"scope":`+scopeJSON+`,"amount":1000,"reason":"purchase"}`, "Idempotency-Key", "g-1"); code != 200 {
		t.Fatalf("grant = %d %s", code, body)
	}
	if code, body := httpDo(t, c, "POST", base+"/v1/admit", serviceToken, `{"scope":`+scopeJSON+`,"resource":"llm.chat"}`); code != 200 || !strings.Contains(body, `"allowed":true`) {
		t.Errorf("admit after a grant = %d %s, want 200 allowed", code, body)
	}
	rec := `{"scope":` + scopeJSON + `,"resource":"llm.chat","rate_key":"gpt","occurred_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","quantities":[{"unit":"in","amount":1200},{"unit":"out","amount":800}]}`
	code, first := httpDo(t, c, "POST", base+"/v1/record", serviceToken, rec, "Idempotency-Key", "r-1")
	if code != 200 {
		t.Fatalf("record = %d %s", code, first)
	}
	var r1 struct {
		Applied        bool
		Delta, Balance int64
	}
	must(t, json.Unmarshal([]byte(first), &r1))
	if !r1.Applied || r1.Delta >= 0 || r1.Balance != 1000+r1.Delta {
		t.Errorf("the record's receipt does not debit the grant: %s", first)
	}
	// The same operation again is the same answer and no second charge.
	if code, second := httpDo(t, c, "POST", base+"/v1/record", serviceToken, rec, "Idempotency-Key", "r-1"); code != 200 {
		t.Errorf("replayed record = %d %s", code, second)
	}
	code, bal := httpDo(t, c, "GET", base+"/v1/balance"+q, serviceToken, "")
	var b struct{ Total int64 }
	must(t, json.Unmarshal([]byte(bal), &b))
	if code != 200 || b.Total != r1.Balance {
		t.Errorf("balance after a record and its replay = %d %s, want total %d (charged once)", code, bal, r1.Balance)
	}

	// The same service, through gRPC.
	g, err := grpc.NewClient(d.listening(t, "grpc"), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcclient.WithBearer(serviceToken, grpcclient.OverInsecureTransport()))
	must(t, err)
	t.Cleanup(func() { _ = g.Close() })
	pools, err := grpcclient.New(g).Balance(t.Context(), sc)
	must(t, err)
	var sum domain.Credits
	for _, v := range pools {
		sum += v
	}
	if int64(sum) != r1.Balance {
		t.Errorf("gRPC balance = %d, REST balance = %d: one service, two answers", sum, r1.Balance)
	}
	stranger, err := grpc.NewClient(d.listening(t, "grpc"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(t, err)
	t.Cleanup(func() { _ = stranger.Close() })
	if _, err := grpcclient.New(stranger).Balance(t.Context(), sc); status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC without a credential = %v, want Unauthenticated", err)
	}
}

func TestPaymentdRefusesToStartWhenTheShardCountDisagreesWithTheBooks(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	pool, err := pgadapter.Connect(t.Context(), dsn)
	must(t, err)
	t.Cleanup(pool.Close)
	must(t, pgadapter.NewBooks(pool).EnsureHotConfig(t.Context(), 64)) // the books were written under 64 shards

	d := startPaymentd(t, paymentdEnv(realm, dsn, "PAYMENT_SHARDS", "128"))
	if code := d.waitExit(t, 30*time.Second); code == 0 {
		t.Fatalf("paymentd exited 0 with the wrong shard count; logs:\n%s", d.logs.String())
	}
	if out := d.logs.String(); !strings.Contains(strings.ToLower(out), "shard") || !strings.Contains(out, "64") || !strings.Contains(out, "128") {
		t.Errorf("the refusal does not say which shard counts disagree:\n%s", out)
	}
}

func TestPaymentdRefusesToStartAgainstASchemaThatIsNotAtHead(t *testing.T) {
	t.Parallel()
	admin, err := sql.Open("pgx", pg.DSN("postgres"))
	must(t, err)
	defer func() { _ = admin.Close() }()
	name := fmt.Sprintf("empty%d_%d", time.Now().UnixNano(), seq.Add(1))
	_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
	must(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })

	d := startPaymentd(t, paymentdEnv("default", pg.DSN(name)))
	if code := d.waitExit(t, 30*time.Second); code == 0 {
		t.Fatalf("paymentd exited 0 against an unmigrated database; logs:\n%s", d.logs.String())
	}
	if out := d.logs.String(); !strings.Contains(out, "payment-migrate") {
		t.Errorf("the refusal does not say how to fix it (run payment-migrate up):\n%s", out)
	}
}

func TestPaymentdWithMutualTLSVerifiesTheCallersCertificate(t *testing.T) {
	t.Parallel()
	pki := newPKI(t)
	realm, dsn := seededRealm(t)
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	openScope(t, redis.Addr, realm, dsn, sc)
	d := startPaymentd(t, paymentdEnv(realm, dsn,
		"PAYMENT_SERVICE_AUTH", "mtls", "PAYMENT_SERVICE_TOKENS", "-",
		"PAYMENT_SERVICE_CERTS", "billing:billing.internal:"+realm+":record+grant",
		"PAYMENT_TLS_CERT_FILE", pki.serverCert, "PAYMENT_TLS_KEY_FILE", pki.serverKey, "PAYMENT_TLS_CLIENT_CA_FILE", pki.caFile))
	base := "https://" + d.listening(t, "http")
	q := fmt.Sprintf("/v1/balance?scope_kind=%s&scope_id=%s", sc.Kind, sc.ID)

	client := func(cert *tls.Certificate) *http.Client {
		cfg := &tls.Config{RootCAs: pki.pool, MinVersion: tls.VersionTLS12}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	}
	// A probe holds no client certificate and must still be able to ask whether the process is ready.
	awaitReady(t, d, client(nil), base)

	if code, body := httpDo(t, client(&pki.good), "GET", base+q, "", ""); code != 200 {
		t.Errorf("a certificate from the private CA, mapped to a caller: %d %s, want 200", code, body)
	}
	if code, body := httpDo(t, client(nil), "GET", base+q, "", ""); code != 401 {
		t.Errorf("no client certificate: %d %s, want 401", code, body)
	}
	// A certificate the private CA did not sign is not a credential: the handshake fails, or the client — which
	// offers a certificate only if the server's list of acceptable CAs names its issuer — arrives with none. Either
	// way it is not let in.
	if resp, err := client(&pki.rogue).Get(base + q); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a certificate from a CA the service does not trust: %d, want a refused handshake or 401", resp.StatusCode)
		}
	}
	// A bearer token is not a credential in this mode.
	if code, _ := httpDo(t, client(nil), "GET", base+q, serviceToken, ""); code != 401 {
		t.Errorf("a bearer token in mtls mode: %d, want 401", code)
	}
	// And a healthcheck from inside the container reaches a TLS listener too.
	hc := exec.Command(paymentdBinary(t), "healthcheck")
	hc.Env = []string{"PAYMENT_HTTP_ADDR=" + d.listening(t, "http"), "PAYMENT_TLS_CERT_FILE=" + pki.serverCert, "PAYMENT_TLS_KEY_FILE=" + pki.serverKey}
	if out, err := hc.CombinedOutput(); err != nil {
		t.Errorf("paymentd healthcheck against a TLS listener: %v\n%s", err, out)
	}
}

// pki is a private CA, a server certificate for 127.0.0.1, a client certificate it issued (subject billing.internal)
// and one issued by a CA the service has never heard of.
type pki struct {
	caFile, serverCert, serverKey string
	pool                          *x509.CertPool
	good, rogue                   tls.Certificate
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	dir := t.TempDir()
	ca, caKey, caPEM := newCA(t, "intel-payment test CA")
	rogue, rogueKey, _ := newCA(t, "someone else's CA")
	p := &pki{pool: x509.NewCertPool(), caFile: filepath.Join(dir, "ca.pem")}
	p.pool.AddCert(ca)
	must(t, os.WriteFile(p.caFile, caPEM, 0o600))

	srvPEM, srvKeyPEM, _ := issue(t, ca, caKey, "paymentd", []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	p.serverCert, p.serverKey = filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	must(t, os.WriteFile(p.serverCert, srvPEM, 0o600))
	must(t, os.WriteFile(p.serverKey, srvKeyPEM, 0o600))

	goodPEM, goodKey, _ := issue(t, ca, caKey, "billing.internal", nil, x509.ExtKeyUsageClientAuth)
	var err error
	p.good, err = tls.X509KeyPair(goodPEM, goodKey)
	must(t, err)
	roguePEM, rogueKeyPEM, _ := issue(t, rogue, rogueKey, "billing.internal", nil, x509.ExtKeyUsageClientAuth)
	p.rogue, err = tls.X509KeyPair(roguePEM, rogueKeyPEM)
	must(t, err)
	return p
}

func newCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	cert, err := x509.ParseCertificate(der)
	must(t, err)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, ips []net.IP, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte, _ *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, IPAddresses: ips,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	must(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	leaf, err := x509.ParseCertificate(der)
	must(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), leaf
}

// The bus is a choice (PAYMENT_BUS), and the one that is not the default has to be wired too.
func TestPaymentdStartsOnTheJetStreamBusAndRefusesOneItCannotReach(t *testing.T) {
	t.Parallel()
	realm, dsn := seededRealm(t)
	prefix := fmt.Sprintf("pdjs%d", seq.Add(1))
	d := startPaymentd(t, paymentdEnv(realm, dsn, "PAYMENT_BUS", "nats_jetstream", "PAYMENT_BUS_URL", natsSrv.URL, "PAYMENT_SUBJECT_PREFIX", prefix))
	base := d.httpURL(t)
	awaitReady(t, d, &http.Client{Timeout: 10 * time.Second}, base)

	r2, dsn2 := seededRealm(t)
	bad := startPaymentd(t, paymentdEnv(r2, dsn2, "PAYMENT_BUS", "nats_jetstream", "PAYMENT_BUS_URL", "nats://127.0.0.1:1"))
	if code := bad.waitExit(t, 30*time.Second); code == 0 {
		t.Fatalf("paymentd exited 0 with a bus it cannot reach; logs:\n%s", bad.logs.String())
	}
	if out := bad.logs.String(); !strings.Contains(out, "PAYMENT_BUS_URL") {
		t.Errorf("the refusal does not name the variable:\n%s", out)
	}
}
