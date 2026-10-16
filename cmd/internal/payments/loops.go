package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// RechargeGroup is the consumer group of the auto top-up subscription. It is part of the deployment: a group that changed
// between releases would hand every message to the old group and the new one, and a customer would be charged by both.
const RechargeGroup = "payment-autorecharge"

// defaultPurgeEvery is how often processed events past retention are deleted: they are not urgent, and a purge is a scan.
const defaultPurgeEvery = time.Hour

// Recharger is the auto top-up handler.
type Recharger interface {
	Handle(ctx context.Context, subject string, payload []byte) error
}

// Purger deletes the inbox's processed rows older than a cutoff.
type Purger interface {
	Purge(ctx context.Context, olderThan time.Time) (int, error)
}

// LoopsDeps are what payment-worker runs for payments.
type LoopsDeps struct {
	Processor ports.WebhookProcessor
	Ingress   ports.WebhookIngress
	Inbox     Purger
	Recharge  Recharger
	Bus       mports.Bus
	Clock     mports.Clock
	Log       *slog.Logger

	SubjectPrefix string        // default app.DefaultSubjectPrefix (PAYMENT_SUBJECT_PREFIX)
	Tick          time.Duration // how often the inbox is worked (PAYMENT_WEBHOOK_TICK_INTERVAL)
	Retention     time.Duration // how long a processed event is kept (PAYMENT_PAYMENT_EVENT_RETENTION)
	PurgeEvery    time.Duration // default hourly
}

// Loops is the payment half of payment-worker: it works the webhook inbox, verifies parked deliveries again, deletes what
// has been kept long enough, and tops up the balances that ran low.
type Loops struct{ d LoopsDeps }

// NewLoops validates the dependencies.
func NewLoops(d LoopsDeps) (*Loops, error) {
	var errs []error
	need := func(ok bool, what string) {
		if !ok {
			errs = append(errs, errors.New(what+" is required"))
		}
	}
	need(d.Processor != nil, "the processor")
	need(d.Ingress != nil, "the ingress")
	need(d.Inbox != nil, "the inbox")
	need(d.Recharge != nil, "the auto top-up handler")
	need(d.Bus != nil, "the bus")
	need(d.Clock != nil, "a clock")
	need(d.Log != nil, "a logger")
	if d.Tick <= 0 || d.Retention <= 0 {
		errs = append(errs, errors.New("a positive tick interval and retention are required"))
	}
	if d.PurgeEvery < 0 {
		errs = append(errs, errors.New("the purge interval cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("payments: loops: %w", err)
	}
	if d.SubjectPrefix == "" {
		d.SubjectPrefix = app.DefaultSubjectPrefix
	}
	if d.PurgeEvery == 0 {
		d.PurgeEvery = defaultPurgeEvery
	}
	return &Loops{d: d}, nil
}

// Run joins the low-balance subscription, then runs every stage until ctx is done and returns once each has stopped. Not
// being able to join is the only failure: a worker that runs without auto top-up is one nobody knows is not topping up.
func (l *Loops) Run(ctx context.Context) error {
	subject := l.d.SubjectPrefix + ".balance.low.>"
	sub, err := l.d.Bus.Subscribe(ctx, subject, RechargeGroup, func(ctx context.Context, m mports.Message) error {
		return l.d.Recharge.Handle(ctx, m.Subject, m.Payload)
	})
	if err != nil {
		return fmt.Errorf("payments: join %s as %s: %w", subject, RechargeGroup, err)
	}
	defer func() { _ = sub.Close() }()

	var wg sync.WaitGroup
	for _, st := range []struct {
		name  string
		every time.Duration
		run   func(context.Context) error
	}{
		{"webhook inbox", l.d.Tick, l.d.Processor.Tick},
		{"parked deliveries", l.d.Tick, l.d.Ingress.Retry},
		{"inbox retention", l.d.PurgeEvery, l.purge},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.loop(ctx, st.name, st.every, st.run)
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// loop runs a stage at once and then every interval after the last run ended, so a slow stage is never run over itself.
// A failure is logged and tried again: a stage that fails is the database being down, which the next run finds out.
func (l *Loops) loop(ctx context.Context, name string, every time.Duration, run func(context.Context) error) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := run(ctx); err != nil && ctx.Err() == nil {
			l.d.Log.Error("payments: a stage failed and will run again", "stage", name, "error", err.Error())
		}
		t.Reset(every)
	}
}

func (l *Loops) purge(ctx context.Context) error {
	n, err := l.d.Inbox.Purge(ctx, l.d.Clock.Now().Add(-l.d.Retention))
	if err != nil {
		return fmt.Errorf("purge processed webhook events: %w", err)
	}
	if n > 0 {
		l.d.Log.Info("payments: processed webhook events past retention were deleted", "count", n)
	}
	return nil
}
