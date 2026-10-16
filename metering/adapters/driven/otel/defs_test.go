package otelmetrics_test

import (
	"context"
	"strings"
	"testing"

	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	otelmetrics "github.com/truongpx396/intel-payment/metering/adapters/driven/otel"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R096: a host that serves more than metering — the payment layer is the first — has metrics of its own, and the
// operator's rule is the same for them (constitution XII): every name is DECLARED from the first scrape, with its
// type, unit and help, so an alert on a counter that has not moved yet is not an alert on a name that does not exist.

var hostDefs = []ports.MetricDef{
	{Name: "billing_webhook_received_total", Kind: ports.KindCounter, Labels: []string{"provider", "result"}, Unit: "{delivery}", Help: "Webhook deliveries by provider and result"},
	{Name: "billing_webhook_inbox_age_seconds", Kind: ports.KindGauge, Unit: "s", Help: "Age of the oldest unprocessed webhook event"},
	{Name: "billing_fulfilment_seconds", Kind: ports.KindHistogram, Labels: []string{"provider"}, Unit: "s", Help: "From delivery to credits"},
}

func TestAHostsMetricsAreDeclaredOnTheFirstScrapeWithTheirTypeAndHelp(t *testing.T) {
	t.Parallel()
	_, h, err := otelmetrics.NewPrometheus(otelmetrics.WithDefs(hostDefs...))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := scrape(t, h)
	for _, d := range hostDefs {
		if !strings.Contains(body, "# TYPE "+d.Name+" "+string(d.Kind)+"\n") {
			t.Errorf("%s is not declared as a %s on the first scrape", d.Name, d.Kind)
		}
		if !strings.Contains(body, "# HELP "+d.Name+" "+d.Help+"\n") {
			t.Errorf("%s has not the help the host gave it", d.Name)
		}
	}
	for _, d := range ports.Catalog {
		if !strings.Contains(body, "# TYPE "+d.Name+" ") {
			t.Errorf("metering's own %s is no longer declared once a host adds metrics", d.Name)
		}
	}
}

func TestAHostsMetricsKeepTheirUnitAndKindWhenTheyAreRecorded(t *testing.T) {
	t.Parallel()
	reader := sdk.NewManualReader()
	mp := sdk.NewMeterProvider(sdk.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	m, err := otelmetrics.New(mp, otelmetrics.WithDefs(hostDefs...), otelmetrics.OnError(func(err error) { t.Errorf("instrument: %v", err) }))
	if err != nil {
		t.Fatal(err)
	}
	m.Count("billing_webhook_received_total", 2, ports.Labels{"provider": "stripe", "result": "new"})
	m.Gauge("billing_webhook_inbox_age_seconds", 41, nil)
	m.Observe("billing_fulfilment_seconds", 0.4, ports.Labels{"provider": "stripe"})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			got[mt.Name] = mt
		}
	}
	for _, d := range hostDefs {
		mt, ok := got[d.Name]
		if !ok {
			t.Errorf("%s was not exported", d.Name)
			continue
		}
		if mt.Unit != d.Unit || mt.Description != d.Help {
			t.Errorf("%s: unit %q description %q, want %q %q: a metric the host declared is exported as if it had not", d.Name, mt.Unit, mt.Description, d.Unit, d.Help)
		}
	}
}

func TestAHostCannotRedefineOrRepeatAMetric(t *testing.T) {
	t.Parallel()
	for name, defs := range map[string][]ports.MetricDef{
		"one of metering's":     {{Name: ports.Catalog[0].Name, Kind: ports.KindCounter, Unit: "1", Help: "mine now"}},
		"the same name twice":   {hostDefs[0], hostDefs[0]},
		"an unknown kind":       {{Name: "billing_x_total", Kind: "summary", Unit: "1", Help: "h"}},
		"a name with no help":   {{Name: "billing_y_total", Kind: ports.KindCounter, Unit: "1"}},
		"a name that is empty":  {{Kind: ports.KindCounter, Unit: "1", Help: "h"}},
		"a name with a space":   {{Name: "billing z total", Kind: ports.KindCounter, Unit: "1", Help: "h"}},
		"a name in upper case":  {{Name: "Billing_Z_total", Kind: ports.KindCounter, Unit: "1", Help: "h"}},
		"a counter not _total":  {{Name: "billing_w", Kind: ports.KindCounter, Unit: "1", Help: "h"}},
		"a histogram not in _s": {{Name: "billing_v_ms", Kind: ports.KindHistogram, Unit: "ms", Help: "h"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := otelmetrics.NewPrometheus(otelmetrics.WithDefs(defs...)); err == nil {
				t.Fatalf("%s was accepted: the catalogue is what an operator writes alerts against, and it must not be ambiguous", name)
			}
		})
	}
}
