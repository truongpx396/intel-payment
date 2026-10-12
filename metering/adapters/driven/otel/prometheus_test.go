package otelmetrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	otelmetrics "github.com/truongpx396/intel-payment/metering/adapters/driven/otel"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// An operator writes alerts against metric names before ever reading the code (constitution XII). So /metrics
// declares EVERY name of the catalogue from the first scrape — with its type and help — and not only the ones
// that have seen traffic: an alert on a counter that has not moved yet must not be an alert on a name that
// does not exist.

func scrape(t *testing.T, h http.Handler) (string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, body)
	}
	return string(body), rec.Header()
}

func newProm(t *testing.T) (*otelmetrics.Metrics, http.Handler) {
	t.Helper()
	m, h, err := otelmetrics.NewPrometheus()
	if err != nil {
		t.Fatal(err)
	}
	return m, h
}

func TestEveryCataloguedMetricIsDeclaredOnTheFirstScrape(t *testing.T) {
	t.Parallel()
	_, h := newProm(t)
	body, hdr := scrape(t, h)
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Prometheus text exposition: %q", ct)
	}
	for _, d := range ports.Catalog {
		if !strings.Contains(body, "# TYPE "+d.Name+" "+string(d.Kind)+"\n") {
			t.Errorf("%s is not declared as a %s on the first scrape", d.Name, d.Kind)
		}
		if !strings.Contains(body, "# HELP "+d.Name+" ") {
			t.Errorf("%s has no help line", d.Name)
		}
	}
}

func TestTrafficAddsSamplesWithTheirLabels(t *testing.T) {
	t.Parallel()
	m, h := newProm(t)
	l := ports.Labels{"realm": "acme", "limit": "scope_balance", "deny_code": "payment_required"}
	m.Count(ports.MetricAdmitDenied, 2, l)
	m.Count(ports.MetricAdmitDenied, 3, l)
	m.Gauge(ports.MetricOutboxDepth, 17, ports.Labels{"shard": "4"})
	m.Gauge(ports.MetricOutboxDepth, 9, ports.Labels{"shard": "4"}) // a gauge keeps its last value
	for _, s := range []float64{0.001, 0.003, 0.004, 0.2} {
		m.Observe(ports.MetricAdmitLatency, s, ports.Labels{"realm": "acme", "outcome": "allowed"})
	}
	body, _ := scrape(t, h)

	for _, want := range []string{
		`metering_admit_denied_total{deny_code="payment_required",limit="scope_balance",realm="acme"} 5`,
		`metering_outbox_depth{shard="4"} 9`,
		`metering_admit_latency_seconds_count{outcome="allowed",realm="acme"} 4`,
	} {
		if !hasSample(body, want) {
			t.Errorf("no sample %q in:\n%s", want, grep(body, "metering_admit_denied_total", "metering_outbox_depth", "metering_admit_latency_seconds_count"))
		}
	}
	// "latency buckets resolve the 5 ms target": a bucket at 5 ms holds the three fast calls, not the slow one.
	if !hasSample(body, `metering_admit_latency_seconds_bucket{outcome="allowed",realm="acme",le="0.005"} 3`) {
		t.Errorf("the 5 ms bucket:\n%s", grep(body, "metering_admit_latency_seconds_bucket"))
	}
	if !hasSample(body, `metering_admit_latency_seconds_bucket{outcome="allowed",realm="acme",le="+Inf"} 4`) {
		t.Errorf("the +Inf bucket:\n%s", grep(body, "metering_admit_latency_seconds_bucket"))
	}
}

func TestACounterOnlyRisesAcrossScrapes(t *testing.T) {
	t.Parallel()
	m, h := newProm(t)
	l := ports.Labels{"realm": "acme"}
	m.Count(ports.MetricRecordDeferred, 1, l)
	first, _ := scrape(t, h)
	m.Count(ports.MetricRecordDeferred, 4, l)
	second, _ := scrape(t, h)
	if !hasSample(first, `metering_record_deferred_total{realm="acme"} 1`) || !hasSample(second, `metering_record_deferred_total{realm="acme"} 5`) {
		t.Fatalf("\n%s\n--\n%s", grep(first, "metering_record_deferred_total"), grep(second, "metering_record_deferred_total"))
	}
}

func TestAMetricOutsideTheCatalogueIsExportedOnFirstUse(t *testing.T) {
	t.Parallel()
	m, h := newProm(t)
	m.Count("billing_webhook_verify_failed_total", 1, ports.Labels{"provider": "stripe"})
	body, _ := scrape(t, h)
	if !hasSample(body, `billing_webhook_verify_failed_total{provider="stripe"} 1`) {
		t.Fatalf("a later phase's metric appears when it is first used:\n%s", grep(body, "billing_"))
	}
}

func TestScrapingWhileRecordingIsSafe(t *testing.T) {
	t.Parallel()
	m, h := newProm(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Count(ports.MetricStaleEvent, 1, ports.Labels{"realm": "acme"})
				m.Observe(ports.MetricAdmitLatency, 0.001, ports.Labels{"realm": "acme", "outcome": "allowed"})
			}
		}()
	}
	for i := 0; i < 20; i++ {
		scrape(t, h)
	}
	wg.Wait()
	body, _ := scrape(t, h)
	if !hasSample(body, `metering_stale_event_total{realm="acme"} 1600`) {
		t.Fatalf("no increment is lost:\n%s", grep(body, "metering_stale_event_total"))
	}
}

func hasSample(body, line string) bool {
	return regexp.MustCompile("(?m)^" + regexp.QuoteMeta(line) + "$").MatchString(body)
}

func grep(body string, needles ...string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		for _, n := range needles {
			if strings.Contains(l, n) {
				out = append(out, l)
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
