package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	"github.com/truongpx396/intel-payment/cmd/internal/payments"
	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/events/adapters/driven/httpsend"
	"github.com/truongpx396/intel-payment/events/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/events/app"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// WorkerCommon is what payment-worker gives outbound delivery.
type WorkerCommon struct {
	Pool     *pgxpool.Pool
	Clock    mports.Clock
	Metrics  mports.Metrics
	Bus      mports.Bus
	Log      *slog.Logger
	Getenv   func(string) string
	Settings envcfg.Events
	Prefix   string // PAYMENT_SUBJECT_PREFIX
}

// Worker is the worker's half of outbound delivery: it records what the engine announces, sends what is owed, deletes what has been kept
// long enough, and publishes the dead-delivery gauge.
type Worker struct {
	c        WorkerCommon
	store    events.Store
	recorder *app.Recorder
	dispatch *app.Dispatcher
}

// secretBytes adapts the payment layer's reference resolver (env:, file:) to what a signature needs.
type secretBytes struct{ s *payments.Secrets }

func (b secretBytes) Resolve(ctx context.Context, ref string) ([]byte, error) {
	v, err := b.s.Resolve(ctx, ref)
	return []byte(v), err
}

// NewWorker builds the recorder and the dispatcher over the database.
func NewWorker(c WorkerCommon) (*Worker, error) {
	store := postgres.NewStore(c.Pool, c.Clock)
	rec, err := app.NewRecorder(app.RecorderDeps{Store: store, Bus: c.Bus, Metrics: c.Metrics, Prefix: c.Prefix})
	if err != nil {
		return nil, err
	}
	sender := httpsend.New(httpsend.Options{Guard: events.Guard{AllowPrivate: c.Settings.AllowPrivate}})
	disp, err := app.NewDispatcher(app.DispatcherDeps{
		Store: store, Sender: sender, Secrets: secretBytes{payments.NewSecrets(c.Getenv)}, Clock: c.Clock, Metrics: c.Metrics,
	})
	if err != nil {
		return nil, err
	}
	if c.Settings.AllowPrivate {
		c.Log.Warn("webhooks may be sent to localhost: PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS is for development, and in production it lets an endpoint reach this service's own network")
	}
	return &Worker{c: c, store: store, recorder: rec, dispatch: disp}, nil
}

// Subscribe joins the announcements the recorder turns into events. Not being able to is the caller's failure to report.
func (w *Worker) Subscribe(ctx context.Context) (mports.Subscription, error) {
	return w.recorder.Subscribe(ctx)
}

// Dispatcher is the delivery use case, for the admin door that re-drives a dead delivery.
func (w *Worker) Dispatcher() *app.Dispatcher { return w.dispatch }

// Store is the log, for the tick handlers that purge it.
func (w *Worker) Store() events.Store { return w.store }

// Run sends what is owed every TickInterval, deletes what has outlived its retention hourly, and publishes the dead-delivery gauge, until
// ctx is done. Each stage runs one at a time and again after the last run ended, so a slow one is never run over itself. A failure is
// logged and tried again: a stage that fails is the database being down, which the next run finds out.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, st := range []struct {
		name  string
		every time.Duration
		run   func(context.Context) error
	}{
		{"outbound deliveries", w.c.Settings.TickInterval, w.tick},
		{"event retention", time.Hour, w.purge},
		{"dead delivery gauge", 15 * time.Second, w.dispatch.ExportGauges},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, st.name, st.every, st.run)
		}()
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, name string, every time.Duration, run func(context.Context) error) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := run(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			w.c.Log.Error("events: a stage failed and will run again", "stage", name, "error", err.Error())
		}
		t.Reset(every)
	}
}

func (w *Worker) tick(ctx context.Context) error {
	res, err := w.dispatch.Tick(ctx)
	if res.Dead > 0 {
		w.c.Log.Error("events: deliveries were marked dead and a page has fired", "dead", res.Dead)
	}
	return err
}

func (w *Worker) purge(ctx context.Context) error {
	n, err := w.store.Purge(ctx, w.c.Clock.Now().Add(-w.c.Settings.Retention))
	if err != nil {
		return fmt.Errorf("purge events past retention: %w", err)
	}
	if n > 0 {
		w.c.Log.Info("events: events past retention were deleted", "count", n)
	}
	return nil
}
