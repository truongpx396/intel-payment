package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/cmd/internal/wiring"
	"github.com/truongpx396/intel-payment/metering"
	pgarchive "github.com/truongpx396/intel-payment/metering/adapters/driven/archive/postgres"
	otelmetrics "github.com/truongpx396/intel-payment/metering/adapters/driven/otel"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/grpcserver"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// serve parses the settings, wires the engine to its stores and runs both transports until ctx is done.
func serve(ctx context.Context, getenv func(string) string, _, stderr io.Writer) int {
	s, err := parseSettings(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "paymentd %s: %v\n", version, err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: s.LogLevel})).With("version", version)
	if err := runService(ctx, s, log); err != nil {
		log.Error("paymentd stopped", "error", err.Error())
		return 1
	}
	log.Info("stopped")
	return 0
}

// service is the wired engine and the things that must be closed with it.
type service struct {
	http    *http.Server
	grpc    *grpc.Server
	closers []func()
}

func (s *service) close() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
}

func runService(ctx context.Context, s settings, log *slog.Logger) error {
	svc, err := wire(ctx, s, log)
	if err != nil {
		return err
	}
	defer svc.close()

	httpLn, err := listen(s.HTTPAddr)
	if err != nil {
		return fmt.Errorf("PAYMENT_HTTP_ADDR: %w", err)
	}
	grpcLn, err := listen(s.GRPCAddr)
	if err != nil {
		_ = httpLn.Close()
		return fmt.Errorf("PAYMENT_GRPC_ADDR: %w", err)
	}
	secure := s.TLSCertFile != ""
	log.Info("listening", "transport", "http", "addr", describe(httpLn), "tls", secure)
	log.Info("listening", "transport", "grpc", "addr", describe(grpcLn), "tls", secure)

	errc := make(chan error, 2)
	go func() {
		var err error
		if secure {
			err = svc.http.ServeTLS(httpLn, "", "")
		} else {
			err = svc.http.Serve(httpLn)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http: %w", err)
		}
	}()
	go func() {
		if err := svc.grpc.Serve(grpcLn); err != nil {
			errc <- fmt.Errorf("grpc: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("stopping", "timeout", s.ShutdownTimeout.String())
		return svc.drain(s.ShutdownTimeout)
	case err := <-errc:
		_ = svc.http.Close()
		svc.grpc.Stop()
		return err
	}
}

// drain stops the service the way a deploy needs: both listeners stop taking new calls at once, then the calls
// already taken are answered — a Record in flight settles, or the engine journals it, and is never dropped. A
// stop that waited for ever would hold a deploy hostage, so past timeout it cuts what is left and says so; the
// caller of a cut call got an error and retries with the same Idempotency-Key, which is booked once.
func (s *service) drain(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan struct{}, 2)
	go func() { _ = s.http.Shutdown(ctx); done <- struct{}{} }()
	go func() { s.grpc.GracefulStop(); done <- struct{}{} }()
	for range 2 {
		select {
		case <-done:
		case <-ctx.Done():
			_ = s.http.Close()
			s.grpc.Stop()
			return fmt.Errorf("calls were still in flight after %v and were cut: raise PAYMENT_SHUTDOWN_TIMEOUT if deploys outlast it", timeout)
		}
	}
	return nil
}

// wire connects the stores, checks what must be true before money is served, and builds both transports.
func wire(ctx context.Context, s settings, log *slog.Logger) (*service, error) {
	cfg := s.Engine
	svc := &service{}
	ok := false
	defer func() {
		if !ok {
			svc.close()
		}
	}()

	st, err := wiring.Open(ctx, cfg, 0)
	if err != nil {
		return nil, err
	}
	svc.closers = append(svc.closers, st.Close)
	bus, closeBus, err := wiring.NewBus(ctx, envcfg.Bus{Kind: s.Bus, URL: s.BusURL, Retention: s.BusRetention}, cfg, st.Redis)
	if err != nil {
		return nil, err
	}
	svc.closers = append(svc.closers, closeBus)

	metrics, metricsHandler, err := otelmetrics.NewPrometheus()
	if err != nil {
		return nil, err
	}
	pricers := wiring.Pricers(cfg.MaxOperationAmount)
	deps := app.Deps{
		Pricers: pricers, Balance: st.Hot, Books: st.Books, Limits: st.Config, Pools: st.Config, RateCards: st.Config, Journal: st.Journal, Bus: bus,
		Usage: st.Hot, Ledger: st.Books, Clock: system.Clock{}, IDs: &system.IDs{}, Metrics: metrics,
	}
	if cfg.LedgerGranularity == metering.GranularityRollup {
		deps.Archive = pgarchive.New(st.Pool)
	}
	engine, err := app.NewService(cfg, deps)
	if err != nil {
		return nil, err
	}
	var writer ports.ConfigWriter = pgadapter.NewConfigWriter(st.Pool) // the engine is given the port, never the adapter
	admin, err := app.NewAdmin(cfg, pricers, writer)
	if err != nil {
		return nil, err
	}

	handler, err := resthandler.New(resthandler.Config{
		Meter: engine, Balance: engine, Credits: engine, Ledger: st.Books, Transfers: st.Books, Admin: admin,
		Auth: s.Authenticator, Clock: system.Clock{}, Logger: log, Legacy429ForLimits: s.Legacy429,
		Ready: readiness{db: st.DB, ms: st.Migrations, books: st.Books, shards: cfg.Shards}, Metrics: metricsHandler,
	})
	if err != nil {
		return nil, err
	}
	rpc, err := grpcserver.New(engine, engine)
	if err != nil {
		return nil, err
	}

	tlsCfg, err := serverTLS(s)
	if err != nil {
		return nil, err
	}
	svc.http = &http.Server{
		Handler: handler, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	opts := []grpc.ServerOption{grpc.UnaryInterceptor(grpcserver.AuthInterceptor(s.Authenticator, log))}
	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	svc.grpc = grpc.NewServer(opts...)
	rpc.Register(svc.grpc)
	ok = true
	return svc, nil
}

// serverTLS is the listeners' TLS configuration, or nil when none is configured. Callers are asked for a
// certificate and one that is offered is verified against the private CA; whether a call needs one is the
// authenticator's decision, so a probe with none can still reach /healthz and /readyz.
func serverTLS(s settings) (*tls.Config, error) {
	if s.TLSCertFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(s.TLSCertFile, s.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("PAYMENT_TLS_CERT_FILE / PAYMENT_TLS_KEY_FILE: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if s.Auth == authMTLS {
		pem, err := os.ReadFile(s.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("PAYMENT_TLS_CLIENT_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("PAYMENT_TLS_CLIENT_CA_FILE holds no PEM certificate")
		}
		cfg.ClientCAs, cfg.ClientAuth = pool, tls.VerifyClientCertIfGiven
	}
	return cfg, nil
}

// listen opens a TCP address, or a unix socket for "unix:/path".
func listen(addr string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		_ = os.Remove(path) // a socket left by a process that did not stop cleanly
		return (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	}
	return (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
}

func describe(ln net.Listener) string {
	if ln.Addr().Network() == "unix" {
		return "unix:" + ln.Addr().String()
	}
	return ln.Addr().String()
}
