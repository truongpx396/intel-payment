package otelmetrics_test

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/goleak"

	otelmetrics "github.com/truongpx396/intel-payment/metering/adapters/driven/otel"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func harness(t *testing.T) (*otelmetrics.Metrics, func() map[string]metricdata.Metrics) {
	t.Helper()
	reader := sdk.NewManualReader()
	mp := sdk.NewMeterProvider(sdk.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := otelmetrics.New(mp, otelmetrics.OnError(func(err error) { t.Errorf("instrument: %v", err) }))
	if err != nil {
		t.Fatal(err)
	}
	return m, func() map[string]metricdata.Metrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		out := map[string]metricdata.Metrics{}
		for _, sm := range rm.ScopeMetrics {
			for _, mt := range sm.Metrics {
				out[mt.Name] = mt
			}
		}
		return out
	}
}

func TestEveryCataloguedMetricGetsItsDeclaredKindAndUnit(t *testing.T) {
	t.Parallel()
	m, collect := harness(t)
	for _, d := range ports.Catalog {
		l := ports.Labels{}
		for _, k := range d.Labels {
			l[k] = "x"
		}
		switch d.Kind {
		case ports.KindCounter:
			m.Count(d.Name, 1, l)
		case ports.KindGauge:
			m.Gauge(d.Name, 1, l)
		case ports.KindHistogram:
			m.Observe(d.Name, 0.003, l)
		}
	}
	got := collect()
	for _, d := range ports.Catalog {
		mt, ok := got[d.Name]
		if !ok {
			t.Errorf("%s was not exported", d.Name)
			continue
		}
		if mt.Unit != d.Unit || mt.Description != d.Help {
			t.Errorf("%s: unit %q description %q, want %q %q", d.Name, mt.Unit, mt.Description, d.Unit, d.Help)
		}
		var kind ports.MetricKind
		switch mt.Data.(type) {
		case metricdata.Sum[int64]:
			kind = ports.KindCounter
		case metricdata.Gauge[int64]:
			kind = ports.KindGauge
		case metricdata.Histogram[float64]:
			kind = ports.KindHistogram
		}
		if kind != d.Kind {
			t.Errorf("%s exported as %q, declared %q", d.Name, kind, d.Kind)
		}
	}
}

func TestLabelsBecomeAttributesAndOneLabelSetIsOneSeries(t *testing.T) {
	t.Parallel()
	m, collect := harness(t)
	a := ports.Labels{"realm": "prod", "op": "usage"}
	m.Count(ports.MetricIdemConflict, 2, a)
	m.Count(ports.MetricIdemConflict, 3, ports.Labels{"op": "usage", "realm": "prod"}) // same set, other order
	m.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": "other", "op": "usage"})

	sum := collect()[ports.MetricIdemConflict].Data.(metricdata.Sum[int64])
	if !sum.IsMonotonic || len(sum.DataPoints) != 2 {
		t.Fatalf("%+v", sum)
	}
	byRealm := map[string]int64{}
	for _, dp := range sum.DataPoints {
		v, _ := dp.Attributes.Value(attribute.Key("realm"))
		byRealm[v.AsString()] = dp.Value
	}
	if byRealm["prod"] != 5 || byRealm["other"] != 1 {
		t.Fatalf("%v", byRealm)
	}
}

func TestACounterOnlyRisesAndAGaugeKeepsTheLastValue(t *testing.T) {
	t.Parallel()
	m, collect := harness(t)
	l := ports.Labels{"shard": "3"}
	m.Count(ports.MetricHotRegression, -5, l)
	m.Count(ports.MetricHotRegression, 0, l)
	m.Gauge(ports.MetricOutboxDepth, 10, l)
	m.Gauge(ports.MetricOutboxDepth, 4, l)
	m.Gauge(ports.MetricOutboxDepth, 7, l)

	got := collect()
	if mt, ok := got[ports.MetricHotRegression]; ok && len(mt.Data.(metricdata.Sum[int64]).DataPoints) != 0 {
		t.Fatalf("a counter cannot fall or add nothing: %+v", mt)
	}
	g := got[ports.MetricOutboxDepth].Data.(metricdata.Gauge[int64])
	if len(g.DataPoints) != 1 || g.DataPoints[0].Value != 7 {
		t.Fatalf("a gauge reports what it was last set to: %+v", g)
	}
}

func TestAMetricOutsideTheCatalogueIsCreatedOnFirstUse(t *testing.T) {
	t.Parallel()
	m, collect := harness(t)
	m.Count("billing_grant_applied_total", 1, ports.Labels{"realm": "r", "reason": "purchase"})
	m.Gauge("billing_webhook_inbox_age_seconds", 12, nil)
	m.Observe("billing_webhook_latency_seconds", 0.2, nil)
	got := collect()
	for _, n := range []string{"billing_grant_applied_total", "billing_webhook_inbox_age_seconds", "billing_webhook_latency_seconds"} {
		if _, ok := got[n]; !ok {
			t.Errorf("%s was not exported", n)
		}
	}
}

func TestLatencyIsBucketedForASubFiveMillisecondTarget(t *testing.T) {
	t.Parallel()
	m, collect := harness(t)
	for _, s := range []float64{0.0007, 0.003, 0.004, 0.2} {
		m.Observe(ports.MetricAdmitLatency, s, ports.Labels{"realm": "r", "outcome": "allowed"})
	}
	h := collect()[ports.MetricAdmitLatency].Data.(metricdata.Histogram[float64])
	dp := h.DataPoints[0]
	if dp.Count != 4 || len(dp.Bounds) < 6 {
		t.Fatalf("%+v", dp)
	}
	var under5ms uint64
	for i, b := range dp.Bounds {
		if b <= 0.005 {
			under5ms += dp.BucketCounts[i]
		}
	}
	if under5ms != 3 {
		t.Fatalf("three of four observations are within 5 ms, and the buckets must resolve that: %d", under5ms)
	}
}

func TestItIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	m, collect := harness(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Count(ports.MetricLateReplay, 1, ports.Labels{"realm": "r"})
				m.Gauge(ports.MetricSuspenseOpen, int64(j), ports.Labels{"realm": "r", "reason": "duplicate"})
				m.Observe(ports.MetricAdmitLatency, 0.001, ports.Labels{"realm": "r", "outcome": "allowed"})
				m.Count("brand_new_metric_total", 1, nil)
			}
		}()
	}
	wg.Wait()
	sum := collect()[ports.MetricLateReplay].Data.(metricdata.Sum[int64])
	if sum.DataPoints[0].Value != 16*200 {
		t.Fatalf("lost increments: %d", sum.DataPoints[0].Value)
	}
}

func TestNewRefusesNoProvider(t *testing.T) {
	t.Parallel()
	if _, err := otelmetrics.New(nil); err == nil {
		t.Fatal("a nil provider must be refused")
	}
}
