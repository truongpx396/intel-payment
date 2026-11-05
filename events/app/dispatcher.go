package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Dispatcher defaults.
const (
	DefaultLease = 30 * time.Second // longer than a send (10 s) and the writes around it
	DefaultBatch = 50
	sendWorkers  = 8
)

// DispatcherDeps are what delivery needs.
type DispatcherDeps struct {
	Store   events.Store
	Sender  events.Sender
	Secrets events.SecretSource
	Clock   ports.Clock
	Metrics ports.Metrics // default: no-op
	// Jitter returns a uniform number in [0,1); default is math/rand. Tests choose it, so the schedule is exact.
	Jitter func() float64
	Lease  time.Duration // default DefaultLease
	Batch  int           // default DefaultBatch
}

// Dispatcher sends the deliveries the log owes (`billing.outbound.tick`). Every send is signed with the endpoint's secret; a 2xx is
// delivered; anything else — a timeout, a 4xx, a 5xx, a refused address — is retried on the schedule (1 m, 5 m, 30 m, 2 h, then every
// 6 h) for 72 h, then marked dead, counted and paged. Delivery is at-least-once: a worker killed after the send and before the record
// sends again when its lease lapses, to the same endpoint with the same event id, which is the host's dedup key.
type Dispatcher struct {
	d   DispatcherDeps
	met ports.Metrics
}

// NewDispatcher validates the dependencies.
func NewDispatcher(d DispatcherDeps) (*Dispatcher, error) {
	var errs []error
	if d.Store == nil || d.Sender == nil || d.Secrets == nil || d.Clock == nil {
		errs = append(errs, errors.New("a Store, a Sender, a SecretSource and a Clock are required"))
	}
	if d.Lease < 0 || d.Batch < 0 {
		errs = append(errs, errors.New("the lease and the batch cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("events dispatcher: %w", err)
	}
	if d.Lease == 0 {
		d.Lease = DefaultLease
	}
	if d.Batch == 0 {
		d.Batch = DefaultBatch
	}
	if d.Jitter == nil {
		d.Jitter = rand.Float64
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &Dispatcher{d: d, met: met}, nil
}

// TickResult is what one tick did.
type TickResult struct{ Delivered, Retried, Dead int }

// Tick ends the retries of what has outlived the window, then sends what is due. Two ticks at once send each delivery once: a claim
// is a lease, and the store gives a delivery to one claimant. A tick with nothing due writes nothing.
func (s *Dispatcher) Tick(ctx context.Context) (TickResult, error) {
	var res TickResult
	now := s.d.Clock.Now()
	expired, err := s.d.Store.ExpireStale(ctx, now)
	if err != nil {
		return res, fmt.Errorf("end the retries past the window: %w", err)
	}
	for _, e := range expired {
		res.Dead += int(e.N)
		s.met.Count(events.MetricDeliveryFailed, e.N, ports.Labels{"realm": e.Realm, "endpoint": e.Endpoint})
		s.met.Count(events.MetricDelivery, e.N, ports.Labels{"outcome": "dead"})
	}
	due, err := s.d.Store.Claim(ctx, now, s.d.Lease, s.d.Batch)
	if err != nil {
		return res, fmt.Errorf("claim the deliveries that are due: %w", err)
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
		sem  = make(chan struct{}, sendWorkers)
	)
	for _, dv := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			outcome, err := s.deliver(ctx, dv)
			mu.Lock()
			defer mu.Unlock()
			switch outcome {
			case "delivered":
				res.Delivered++
			case "retry":
				res.Retried++
			case "dead":
				res.Dead++
			}
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	return res, errors.Join(errs...)
}

// deliver makes one attempt and records it.
func (s *Dispatcher) deliver(ctx context.Context, dv events.Due) (string, error) {
	status, cause := s.attempt(ctx, dv)
	now := s.d.Clock.Now()
	if cause == nil {
		if err := s.d.Store.Delivered(ctx, dv.DeliveryID, status, now); err != nil {
			return "", fmt.Errorf("record the delivery of %s: %w", dv.Event.ID, err)
		}
		s.met.Count(events.MetricDelivery, 1, ports.Labels{"outcome": "delivered"})
		s.met.Observe(events.MetricDeliveryLag, now.Sub(dv.Event.CreatedAt).Seconds(), ports.Labels{"realm": dv.Event.Realm})
		return "delivered", nil
	}
	v := events.AfterFailure(dv.CreatedAt, now, dv.Attempts+1, s.d.Jitter())
	f := events.Failure{Status: status, Err: truncate(cause.Error(), 500), Next: v.Next, Dead: v.Dead}
	if err := s.d.Store.Failed(ctx, dv.DeliveryID, f); err != nil {
		return "", fmt.Errorf("record the failed delivery of %s: %w", dv.Event.ID, err)
	}
	if v.Dead {
		s.met.Count(events.MetricDeliveryFailed, 1, ports.Labels{"realm": dv.Event.Realm, "endpoint": dv.Endpoint.ID})
		s.met.Count(events.MetricDelivery, 1, ports.Labels{"outcome": "dead"})
		return "dead", nil
	}
	s.met.Count(events.MetricDelivery, 1, ports.Labels{"outcome": "retry"})
	return "retry", nil
}

// attempt signs and sends. It never sends unsigned: a secret that cannot be resolved is a failed attempt.
func (s *Dispatcher) attempt(ctx context.Context, dv events.Due) (int, error) {
	body, err := dv.Event.Body()
	if err != nil {
		return 0, err
	}
	var secrets [][]byte
	for _, ref := range strings.Split(dv.Endpoint.SecretRef, ",") {
		secret, err := s.d.Secrets.Resolve(ctx, strings.TrimSpace(ref))
		if err != nil {
			return 0, fmt.Errorf("cannot resolve the signing secret, so nothing is sent: %w", err)
		}
		secrets = append(secrets, secret)
	}
	header := map[string]string{
		"Content-Type":         "application/json",
		events.HeaderEventID:   dv.Event.ID,
		events.HeaderSignature: events.Sign(secrets, s.d.Clock.Now(), body),
	}
	status, err := s.d.Sender.Send(ctx, dv.Endpoint.URL, header, body)
	switch {
	case err != nil:
		return status, err
	case status < http.StatusOK || status >= http.StatusMultipleChoices:
		return status, fmt.Errorf("the endpoint answered %d", status)
	}
	return status, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Redeliver makes a dead delivery pending again, due now: the admin API's re-drive. The feed always has the event, so this is for a
// host that wants the webhook.
func (s *Dispatcher) Redeliver(ctx context.Context, deliveryID string) error {
	return s.d.Store.Redeliver(ctx, deliveryID, s.d.Clock.Now())
}

// ExportGauges publishes the dead deliveries nobody has re-driven, per realm, from the store.
func (s *Dispatcher) ExportGauges(ctx context.Context) error {
	dead, err := s.d.Store.DeadCounts(ctx)
	if err != nil {
		return err
	}
	for realm, n := range dead {
		s.met.Gauge(events.MetricDeadDeliveries, n, ports.Labels{"realm": realm})
	}
	return nil
}
