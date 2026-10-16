package otelmetrics

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// NewPrometheus is the metrics the service exports, and the handler that serves them in the Prometheus text
// exposition (/metrics). It owns its registry and its SDK, so a host embedding the engine as a library is not
// forced to share either.
//
// An operator writes alerts against metric names before ever reading the code (constitution XII), so every name
// of ports.Catalog is DECLARED — its type and its help — from the first scrape, not only once it has seen
// traffic: an alert on a counter that has not moved yet must not be an alert on a name that does not exist.
// The OTel exporter omits a metric with no samples, so the missing declarations are appended; the format allows
// a declaration with no samples after it.
func NewPrometheus(opts ...Option) (*Metrics, http.Handler, error) {
	reg := prometheus.NewRegistry()
	// The catalog's names already carry their suffixes (_total, _seconds), and an alert is written against
	// them: the exporter must not rename them.
	exp, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo(), otelprom.WithoutTargetInfo(),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes))
	if err != nil {
		return nil, nil, fmt.Errorf("otelmetrics: prometheus exporter: %w", err)
	}
	m, err := New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp)), opts...)
	if err != nil {
		return nil, nil, err
	}
	return m, declareCatalog(m.catalog(), promhttp.HandlerFor(reg, promhttp.HandlerOpts{})), nil
}

// declareCatalog serves next's exposition followed by the declaration of every catalogued metric it left out.
func declareCatalog(catalog []ports.MetricDef, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The declarations are appended to the body as text, so the body must be text: a compressed stream with
		// plain lines stuck on its end is one no scraper can read. The exposition is small and served on a
		// private network; it goes out uncompressed.
		plain := r.Clone(r.Context())
		plain.Header.Del("Accept-Encoding")
		buf := &bufferedResponse{header: http.Header{}, code: http.StatusOK}
		next.ServeHTTP(buf, plain)
		body := buf.body.String()
		if buf.code == http.StatusOK {
			var missing strings.Builder
			for _, d := range catalog {
				if !strings.Contains(body, "# TYPE "+d.Name+" ") {
					fmt.Fprintf(&missing, "# HELP %s %s\n# TYPE %s %s\n", d.Name, escapeHelp(d.Help), d.Name, d.Kind)
				}
			}
			body += missing.String()
		}
		for k, v := range buf.header {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(buf.code)
		_, _ = w.Write([]byte(body))
	})
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *bufferedResponse) WriteHeader(code int)        { b.code = code }
