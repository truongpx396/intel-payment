package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	billingports "github.com/truongpx396/intel-payment/billing/ports"
	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
	"github.com/truongpx396/intel-payment/cmd/internal/wiring"
	"github.com/truongpx396/intel-payment/metering"
	pgarchive "github.com/truongpx396/intel-payment/metering/adapters/driven/archive/postgres"
	otelmetrics "github.com/truongpx396/intel-payment/metering/adapters/driven/otel"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/redisstreams"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/app"
)

// serve parses the settings, wires the writer to its stores and runs it until ctx is done.
func serve(ctx context.Context, getenv func(string) string, stderr io.Writer) int {
	s, err := parseSettings(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "payment-worker %s: %v\n", version, err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: s.LogLevel})).With("version", version)
	if err := runWorker(ctx, s, log, getenv); err != nil {
		log.Error("payment-worker stopped", "error", err.Error())
		return 1
	}
	log.Info("stopped")
	return 0
}

func runWorker(ctx context.Context, s settings, log *slog.Logger, getenv func(string) string) error {
	cfg := s.Engine
	// A worker holds one connection per shard it owns, plus those its batches use.
	st, err := wiring.Open(ctx, cfg, cfg.Shards+16)
	if err != nil {
		return err
	}
	defer st.Close()
	bus, closeBus, err := wiring.NewBus(ctx, envcfg.Bus{Kind: envcfg.BusStreams}, cfg, st.Redis)
	if err != nil {
		return err
	}
	defer closeBus()
	metrics, metricsHandler, err := otelmetrics.NewPrometheus(otelmetrics.WithDefs(billingports.MetricCatalog...))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	stream, err := redisstreams.NewStream(st.Redis, redisstreams.IntentOptions{Consumer: fmt.Sprintf("worker-%s-%d", host, os.Getpid()), Clock: system.Clock{}})
	if err != nil {
		return err
	}
	deps := app.Deps{
		Pricers: wiring.Pricers(cfg.MaxOperationAmount), Balance: st.Hot, Books: st.Books, Limits: st.Config, Pools: st.Config, RateCards: st.Config,
		Journal: st.Journal, Bus: bus, Stream: stream, Clock: system.Clock{}, IDs: &system.IDs{}, Metrics: metrics,
	}
	if cfg.LedgerGranularity == metering.GranularityRollup {
		deps.Archive = pgarchive.New(st.Pool)
	}
	writer, err := app.NewWriter(cfg, deps)
	if err != nil {
		return err
	}
	replayer, err := app.NewJournalReplayer(cfg, deps)
	if err != nil {
		return err
	}
	expirer, err := app.NewExpirer(cfg, deps)
	if err != nil {
		return err
	}

	// Payments: stage 2 of the webhook flow mints credits through the engine, the same one paymentd fronts, and auto top-up
	// reads a balance back through it.
	engine, err := app.NewService(cfg, deps)
	if err != nil {
		return err
	}
	pay, err := payments.NewWorker(payments.Common{
		Books: payments.OpenBooks(st.Pool, system.Clock{}), Getenv: getenv, Clock: system.Clock{}, Metrics: metrics, Bus: bus, Log: log,
		Settings: s.Payments, SubjectPrefix: cfg.SubjectPrefix,
	}, payments.Money{Meter: engine, Lots: st.Books, Balances: engine})
	if err != nil {
		return err
	}

	ops, err := serveOps(s.OpsAddr, metricsHandler, log)
	if err != nil {
		return err
	}
	defer func() { _ = ops.Close() }()

	// The payment loops stop with the worker, and the worker stops with them: one that cannot join the low-balance
	// subscription is a worker that is not topping anyone up, which must be seen as a failure and not as a quiet process.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	paid := make(chan error, 1)
	go func() {
		err := pay.Run(runCtx)
		if err != nil {
			stop()
		}
		paid <- err
	}()

	r := &runner{s: s, log: log, books: st.Books, writer: writer, replayer: replayer, expirer: expirer}
	err = r.run(runCtx)
	stop()
	if perr := <-paid; perr != nil && err == nil {
		err = perr
	}
	return err
}

// serveOps serves /healthz and /metrics for the process. The worker takes no calls; this is for the platform.
func serveOps(addr string, metrics http.Handler, log *slog.Logger) (*http.Server, error) {
	var ln net.Listener
	var err error
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		_ = os.Remove(path)
		ln, err = (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	} else {
		ln, err = (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("PAYMENT_WORKER_ADDR: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
	})
	mux.Handle("GET /metrics", metrics)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	shown := ln.Addr().String()
	if ln.Addr().Network() == "unix" {
		shown = "unix:" + shown
	}
	log.Info("listening", "transport", "ops", "addr", shown)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("ops listener failed", "error", err.Error())
		}
	}()
	return srv, nil
}
