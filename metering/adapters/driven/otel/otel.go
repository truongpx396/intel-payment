// Package otelmetrics exports the metrics of the observability contract (FR-041) through the
// OpenTelemetry metrics API. It implements ports.Metrics and knows nothing of any backend: the host
// hands it a MeterProvider and chooses the exporter (Prometheus, OTLP, …).
//
// Counts and gauges are int64 and only durations are observed as float seconds, so money never passes
// through a float. Every metric in ports.Catalog gets an instrument of its declared kind, unit and
// description up front, and so does every metric a host adds with WithDefs — the billing_* metrics of the
// payment layer; a name outside both is created on first use from the method that reported it.
package otelmetrics

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// ScopeName is the instrumentation scope the meter is created under.
const ScopeName = "github.com/truongpx396/intel-payment/metering"

// latencyBuckets suit Admit, which is meant to answer in under 5 ms at p99.
var latencyBuckets = []float64{0.0005, 0.001, 0.002, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Metrics implements ports.Metrics.
type Metrics struct {
	meter metric.Meter

	mu       sync.RWMutex
	counters map[string]metric.Int64Counter
	gauges   map[string]metric.Int64Gauge
	hists    map[string]metric.Float64Histogram
	onError  func(error)
	defs     []ports.MetricDef // a host's own, after metering's
	all      []ports.MetricDef // metering's, then the host's: built once, read on every call
}

var _ ports.Metrics = (*Metrics)(nil)

// Option configures New.
type Option func(*Metrics)

// OnError sees an instrument that could not be created. The default drops it: telemetry must never
// break the call it describes.
func OnError(f func(error)) Option { return func(m *Metrics) { m.onError = f } }

// WithDefs adds a host's own metrics to the catalogue the exporter declares: the payment layer's, for a service that
// serves more than metering. Each is held to the rules of metering's own — a name an operator can write an alert
// against (lower snake case, a counter ending _total, a histogram in _seconds), a help line, a kind New knows — and none may
// repeat a name already taken.
func WithDefs(defs ...ports.MetricDef) Option {
	return func(m *Metrics) { m.defs = append(m.defs, defs...) }
}

var metricName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkDefs reports every way the host's definitions would make the catalogue ambiguous.
func checkDefs(defs []ports.MetricDef) error {
	taken := map[string]bool{}
	for _, d := range ports.Catalog {
		taken[d.Name] = true
	}
	var errs []error
	for _, d := range defs {
		bad := func(why string) { errs = append(errs, fmt.Errorf("metric %q: %s", d.Name, why)) }
		switch {
		case !metricName.MatchString(d.Name):
			bad("a name is lower snake case")
		case taken[d.Name]:
			bad("the name is already taken")
		case strings.TrimSpace(d.Help) == "":
			bad("a metric has a help line")
		case d.Kind == ports.KindCounter && !strings.HasSuffix(d.Name, "_total"):
			bad("a counter's name ends _total")
		case d.Kind == ports.KindHistogram && !strings.HasSuffix(d.Name, "_seconds"):
			bad("a histogram is in seconds and its name ends _seconds")
		}
		taken[d.Name] = true
	}
	return errors.Join(errs...)
}

// catalog is every metric this exporter declares: metering's, then the host's.
func (m *Metrics) catalog() []ports.MetricDef { return m.all }

// New registers every catalogued metric on the provider's meter.
func New(mp metric.MeterProvider, opts ...Option) (*Metrics, error) {
	if mp == nil {
		return nil, errors.New("otelmetrics: a MeterProvider is required")
	}
	m := &Metrics{
		meter:    mp.Meter(ScopeName),
		counters: map[string]metric.Int64Counter{}, gauges: map[string]metric.Int64Gauge{}, hists: map[string]metric.Float64Histogram{},
	}
	for _, o := range opts {
		o(m)
	}
	if err := checkDefs(m.defs); err != nil {
		return nil, fmt.Errorf("otelmetrics: %w", err)
	}
	m.all = append(append([]ports.MetricDef(nil), ports.Catalog...), m.defs...)
	var errs []error
	for _, d := range m.catalog() {
		var err error
		switch d.Kind {
		case ports.KindCounter:
			_, err = m.counter(d.Name, d.Unit, d.Help)
		case ports.KindGauge:
			_, err = m.gauge(d.Name, d.Unit, d.Help)
		case ports.KindHistogram:
			_, err = m.histogram(d.Name, d.Unit, d.Help)
		default:
			err = fmt.Errorf("unknown kind %q", d.Kind)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("otelmetrics: %w", err)
	}
	return m, nil
}

// def finds the catalogued unit and description of a name, if it has one.
func (m *Metrics) def(name string) (unit, help string) {
	for _, d := range m.catalog() {
		if d.Name == name {
			return d.Unit, d.Help
		}
	}
	return "1", ""
}

func (m *Metrics) counter(name, unit, help string) (metric.Int64Counter, error) {
	m.mu.RLock()
	c, ok := m.counters[name]
	m.mu.RUnlock()
	if ok {
		return c, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok = m.counters[name]; ok {
		return c, nil
	}
	c, err := m.meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(help))
	if err != nil {
		return nil, err
	}
	m.counters[name] = c
	return c, nil
}

func (m *Metrics) gauge(name, unit, help string) (metric.Int64Gauge, error) {
	m.mu.RLock()
	g, ok := m.gauges[name]
	m.mu.RUnlock()
	if ok {
		return g, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok = m.gauges[name]; ok {
		return g, nil
	}
	g, err := m.meter.Int64Gauge(name, metric.WithUnit(unit), metric.WithDescription(help))
	if err != nil {
		return nil, err
	}
	m.gauges[name] = g
	return g, nil
}

func (m *Metrics) histogram(name, unit, help string) (metric.Float64Histogram, error) {
	m.mu.RLock()
	h, ok := m.hists[name]
	m.mu.RUnlock()
	if ok {
		return h, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok = m.hists[name]; ok {
		return h, nil
	}
	h, err := m.meter.Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(help), metric.WithExplicitBucketBoundaries(latencyBuckets...))
	if err != nil {
		return nil, err
	}
	m.hists[name] = h
	return h, nil
}

// attrs turns labels into attributes in a stable order, so one label set is always one series.
func attrs(l ports.Labels) metric.MeasurementOption {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kv := make([]attribute.KeyValue, 0, len(keys))
	for _, k := range keys {
		kv = append(kv, attribute.String(k, l[k]))
	}
	return metric.WithAttributeSet(attribute.NewSet(kv...))
}

func (m *Metrics) fail(err error) {
	if m.onError != nil {
		m.onError(err)
	}
}

// Count adds n to a counter. A negative n is dropped: a counter only rises.
func (m *Metrics) Count(name string, n int64, l ports.Labels) {
	if n <= 0 {
		return
	}
	unit, help := m.def(name)
	c, err := m.counter(name, unit, help)
	if err != nil {
		m.fail(err)
		return
	}
	c.Add(context.Background(), n, attrs(l))
}

// Gauge records a gauge's current value.
func (m *Metrics) Gauge(name string, v int64, l ports.Labels) {
	unit, help := m.def(name)
	g, err := m.gauge(name, unit, help)
	if err != nil {
		m.fail(err)
		return
	}
	g.Record(context.Background(), v, attrs(l))
}

// Observe records one duration, in seconds.
func (m *Metrics) Observe(name string, seconds float64, l ports.Labels) {
	unit, help := m.def(name)
	if unit == "1" {
		unit = "s"
	}
	h, err := m.histogram(name, unit, help)
	if err != nil {
		m.fail(err)
		return
	}
	h.Record(context.Background(), seconds, attrs(l))
}
