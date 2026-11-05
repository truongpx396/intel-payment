package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// TickNames are the scheduled ticks (bus-subjects.md § Scheduled ticks), in the order of its table. A tick is published as
// `<prefix>.<name>.tick` by whatever schedules deployments' periodic work — a cron, a CronJob, `payment-worker tick` — and delivered to
// one replica of the worker group.
var TickNames = []string{
	"reconcile", "audit", "transfer", "journal", "webhook", "outbound", "expiry", "dunning", "subdrift",
	"partitions", "idem.purge", "events.purge", "trim",
}

// TickGroup is the consumer group of every tick handler: each tick is delivered to one replica. It is part of the deployment, as the
// other groups are.
const TickGroup = "worker"

// TickSubject is the subject a tick is published on.
func TickSubject(prefix, name string) string { return prefix + "." + name + ".tick" }

// ErrTickSkipped is what a handler returns, wrapped, for work that is not this process's to do (a shard another worker owns): the tick is
// counted skipped and acknowledged, not failed.
var ErrTickSkipped = errors.New("tick skipped")

// TickHandler is the work behind one tick.
type TickHandler struct {
	Name string
	// Key says what the claim is on: two ticks with the same key are the same work and run one at a time. A payload it cannot read is an
	// error — a tick that means nothing must not be taken to mean everything — and so is a bus redelivery of it, until the bus parks it.
	Key func(payload []byte) (string, error)
	// Run does the work and says how much it did; 0 means nothing was due, and a run that found nothing due wrote nothing. It must be
	// idempotent: a run killed midway is finished by the next, which starts again from what the books say is left.
	Run func(ctx context.Context, payload []byte) (work int, err error)
}

// TickOutcome is what a tick came to.
type TickOutcome string

// The outcomes, as `metering_tick_total` labels them.
const (
	TickRan     TickOutcome = "ran"     // work was done
	TickIdle    TickOutcome = "idle"    // nothing was due, and nothing was written
	TickSkipped TickOutcome = "skipped" // another run held the claim, or the work is not this process's
	TickFailed  TickOutcome = "failed"  // the bus will deliver it again, then park it
)

// TicksDeps are what the tick handlers run under.
type TicksDeps struct {
	Claims   ports.Claimer
	Metrics  ports.Metrics // default: no-op
	Prefix   string        // the subject prefix
	Handlers []TickHandler
}

// Ticks runs the scheduled ticks. Every handler is an idempotent claim, so a duplicated tick has no second effect: two runs of one tick
// are one, a run killed midway is completed by the next, and a tick with nothing due writes nothing.
type Ticks struct {
	d      TicksDeps
	met    ports.Metrics
	byName map[string]TickHandler
	order  []string
}

// NewTicks validates the handlers: each is one of the known ticks, once, with a key and a run.
func NewTicks(d TicksDeps) (*Ticks, error) {
	var errs []error
	if d.Claims == nil {
		errs = append(errs, errors.New("a Claimer is required"))
	}
	if strings.TrimSpace(d.Prefix) == "" {
		errs = append(errs, errors.New("a subject prefix is required"))
	}
	t := &Ticks{d: d, byName: map[string]TickHandler{}}
	for _, h := range d.Handlers {
		switch {
		case !slices.Contains(TickNames, h.Name):
			errs = append(errs, fmt.Errorf("%q is not a tick (they are %s)", h.Name, strings.Join(TickNames, ", ")))
		case h.Key == nil || h.Run == nil:
			errs = append(errs, fmt.Errorf("the %s tick needs a Key and a Run", h.Name))
		default:
			if _, dup := t.byName[h.Name]; dup {
				errs = append(errs, fmt.Errorf("the %s tick has two handlers", h.Name))
				continue
			}
			t.byName[h.Name] = h
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metering ticks: %w", err)
	}
	for _, n := range TickNames {
		if _, ok := t.byName[n]; ok {
			t.order = append(t.order, n)
		}
	}
	t.met = d.Metrics
	if t.met == nil {
		t.met = nopMetrics{}
	}
	return t, nil
}

// Served is the ticks this process has a handler for, in table order.
func (t *Ticks) Served() []string { return slices.Clone(t.order) }

// Subscribe joins the subject of every handler under TickGroup. A tick nobody serves is not subscribed to, so it is acknowledged by
// every group and trimmed, and not left to pile up. Not being able to join is the caller's failure to report.
func (t *Ticks) Subscribe(ctx context.Context, bus ports.Bus) (ports.Subscription, error) {
	var subs closers
	for _, n := range t.order {
		sub, err := bus.Subscribe(ctx, TickSubject(t.d.Prefix, n), TickGroup, t.Handle)
		if err != nil {
			_ = subs.Close()
			return nil, fmt.Errorf("join %s: %w", TickSubject(t.d.Prefix, n), err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

type closers []ports.Subscription

func (c closers) Close() error {
	var errs []error
	for _, s := range c {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// Handle is the bus handler: nil acknowledges the tick, an error has it delivered again.
func (t *Ticks) Handle(ctx context.Context, m ports.Message) error {
	name, ok := strings.CutSuffix(strings.TrimPrefix(m.Subject, t.d.Prefix+"."), ".tick")
	if !ok || !strings.HasPrefix(m.Subject, t.d.Prefix+".") {
		return fmt.Errorf("%w: %q is not a tick subject of %q", domain.ErrInvalid, m.Subject, t.d.Prefix)
	}
	_, err := t.Do(ctx, name, m.Payload)
	return err
}

// Do runs one tick: claim, run, release. It is also how a process runs a tick from a timer of its own, under the same claim.
func (t *Ticks) Do(ctx context.Context, name string, payload []byte) (TickOutcome, error) {
	h, ok := t.byName[name]
	if !ok {
		return TickFailed, fmt.Errorf("%w: this process serves no %q tick (it serves %s)", domain.ErrInvalid, name, strings.Join(t.order, ", "))
	}
	count := func(o TickOutcome) {
		t.met.Count(ports.MetricTick, 1, ports.Labels{"tick": name, "outcome": string(o)})
	}
	key, err := h.Key(payload)
	if err != nil {
		count(TickFailed)
		return TickFailed, fmt.Errorf("the %s tick: %w", name, err)
	}
	release, claimed, err := t.d.Claims.TryClaim(ctx, name+"|"+key)
	if err != nil {
		count(TickFailed) // cannot say whether anyone holds it: not a reason to believe nobody does
		return TickFailed, fmt.Errorf("claim the %s tick: %w", name, err)
	}
	if !claimed {
		count(TickSkipped)
		return TickSkipped, nil
	}
	defer release()
	work, err := h.Run(ctx, payload)
	switch {
	case errors.Is(err, ErrTickSkipped):
		count(TickSkipped)
		return TickSkipped, nil
	case err != nil:
		count(TickFailed)
		return TickFailed, fmt.Errorf("the %s tick: %w", name, err)
	case work == 0:
		count(TickIdle)
		return TickIdle, nil
	}
	count(TickRan)
	return TickRan, nil
}

// NoPayload is the Key of a tick that is the same work whoever publishes it and with whatever it carries: the claim is on the tick alone.
func NoPayload([]byte) (string, error) { return "", nil }

// ShardPayload reads the payload of a per-shard tick, `{"shard": 3, "kind": "full"}`: which shard, and for a reconcile whether the sweep is
// full (the default is incremental). A tick that names no shard is refused: guessing "all of them" is how one worker would be handed
// every shard's work.
type ShardPayload struct {
	Shard *int   `json:"shard"`
	Kind  string `json:"kind"`
}

// ParseShard reads and checks a per-shard payload against the number of shards.
func ParseShard(payload []byte, shards int) (ShardPayload, error) {
	var p ShardPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return p, fmt.Errorf("%w: the payload is not JSON: %v", domain.ErrInvalid, err)
	}
	switch {
	case p.Shard == nil:
		return p, fmt.Errorf("%w: a per-shard tick names its shard: {\"shard\": N}", domain.ErrInvalid)
	case *p.Shard < 0 || *p.Shard >= shards:
		return p, fmt.Errorf("%w: shard %d is not one of this deployment's %d", domain.ErrInvalid, *p.Shard, shards)
	}
	if p.Kind == "" {
		p.Kind = string(domain.ReconcileIncremental)
	}
	return p, nil
}
