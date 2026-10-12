package resthandler

import (
	"context"
	"net/http"
	"time"

	"github.com/truongpx396/intel-payment/metering/ports"
)

const defaultReadyTimeout = 2 * time.Second

// platform registers the platform's three routes: open, uncached, and never behind a credential or CORS.
func (h *handler) platform(reg *Registry) {
	reg.Add(Route{Method: http.MethodGet, Path: "/healthz", Surface: Platform, Handler: h.healthz})
	if h.cfg.Ready != nil {
		reg.Add(Route{Method: http.MethodGet, Path: "/readyz", Surface: Platform, Handler: h.readyz})
	}
	if h.cfg.Metrics != nil {
		reg.Add(Route{Method: http.MethodGet, Path: "/metrics", Surface: Platform, Handler: h.cfg.Metrics.ServeHTTP})
	}
}

// healthz says the process is up. It calls no dependency: a liveness probe that does turns that dependency's
// outage into a restart loop.
func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type readyJSON struct {
	Status     string   `json:"status"`
	Migrations string   `json:"migrations"`
	Shards     int      `json:"shards"`
	Problems   []string `json:"problems,omitempty"`
}

// readyz says whether the service may be sent traffic: only at migration head, with the configured shard count
// and the hot tier reachable (FR-042). A check that does not finish within its bound is not ready — a probe
// must answer, or it cannot gate a rollout.
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	timeout := h.cfg.ReadyTimeout
	if timeout <= 0 {
		timeout = defaultReadyTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	done := make(chan ports.ReadyReport, 1)
	go func() { done <- h.cfg.Ready.Check(ctx) }()
	var rep ports.ReadyReport
	select {
	case rep = <-done:
	case <-ctx.Done():
		rep = ports.ReadyReport{Migrations: "unknown", Problems: []string{"the readiness check did not finish within " + timeout.String()}}
	}
	out := readyJSON{Status: "ready", Migrations: rep.Migrations, Shards: rep.Shards, Problems: rep.Problems}
	status := http.StatusOK
	if !rep.Ready() {
		out.Status, status = "not_ready", http.StatusServiceUnavailable
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, out)
}
