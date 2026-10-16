package webhookhttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/truongpx396/intel-payment/billing/ports"
)

// Path is the route of the door. The REST handler's router binds the two names it carries.
const Path = "/webhooks/{provider}/{account}"

// Config is what the door needs.
type Config struct {
	// Ingress decides every delivery. Required.
	Ingress ports.WebhookIngress
	// MaxBody is the most the door reads of a body, in bytes (the ingress's own limit, so that the two agree). Required.
	MaxBody int
	// Logger receives the failures an operator must act on, and never any part of a delivery. Default slog.Default().
	Logger *slog.Logger
}

// New returns the handler of Path.
func New(cfg Config) (http.HandlerFunc, error) {
	var errs []error
	if cfg.Ingress == nil {
		errs = append(errs, errors.New("an ingress is required: without one the door would acknowledge deliveries nobody verified"))
	}
	if cfg.MaxBody <= 0 {
		errs = append(errs, errors.New("a positive body limit is required: the door reads what a stranger sends"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("webhookhttp: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	d := &door{cfg: cfg}
	return d.serve, nil
}

type door struct{ cfg Config }

func (d *door) serve(w http.ResponseWriter, r *http.Request) {
	provider, account := r.PathValue("provider"), r.PathValue("account")
	if provider == "" || account == "" {
		reply(w, http.StatusNotFound, "unknown_account")
		return
	}
	// One byte past the limit is enough to tell an over-long body from one that fits, and is all an attacker can make us hold.
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(d.cfg.MaxBody)+1))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig), len(body) > d.cfg.MaxBody:
		reply(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	case err != nil:
		// A body that stopped short is not the one the provider signed; verifying it would only report a forgery.
		reply(w, http.StatusBadRequest, "invalid_request")
		return
	}

	res := d.cfg.Ingress.Handle(r.Context(), provider, account, body, r.Header)
	if res.Status < 200 || res.Status > 599 || (res.Status >= 300 && res.Status < 400) {
		d.cfg.Logger.Error("webhook: the ingress answered with something that is not an HTTP status", "provider", provider, "account", account, "status", res.Status)
		reply(w, http.StatusInternalServerError, "internal")
		return
	}
	if res.Status >= 500 {
		d.cfg.Logger.Error("webhook: a delivery was not accepted and will be sent again", "provider", provider, "account", account, "status", res.Status, "code", res.Code)
	}
	reply(w, res.Status, res.Code)
}

// reply writes the answer: nothing for a success, the error envelope of the REST surface otherwise.
func reply(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if status < 300 {
		_, _ = io.WriteString(w, "{}\n")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": http.StatusText(status)}})
}
