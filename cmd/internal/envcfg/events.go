package envcfg

import (
	"fmt"
	"time"
)

// Events is what outbound delivery is told. Which endpoints exist and what they subscribe to are data behind the database (constitution
// VIII); this is only how the worker behaves.
type Events struct {
	// Retention is how long an event is kept, for the feed to backfill from. It is never shorter than the 72 hours a failed webhook is
	// retried for, or a host recovering from a full outage would find its events gone.
	Retention time.Duration
	// TickInterval is how often owed deliveries are sent: the longest a healthy endpoint waits for an event.
	TickInterval time.Duration
	// AllowPrivate lets a webhook go to localhost. It is for development only; in production the SSRF guard refuses it.
	AllowPrivate bool
}

const (
	minOutboundRetention = 72 * time.Hour
	maxEventsTick        = time.Minute
)

// Events reads the outbound-delivery settings. An unusable value is reported, never replaced by a default nobody chose.
func (r *Reader) Events() Events {
	e := Events{Retention: 30 * 24 * time.Hour, TickInterval: 5 * time.Second}
	if v := r.Str("PAYMENT_EVENT_RETENTION"); v != "" {
		d, err := parseRetention(v)
		switch {
		case err != nil:
			r.Fail("PAYMENT_EVENT_RETENTION", fmt.Sprintf("must be a number of days like 30d or a duration like 720h, got %q", v))
		case d < minOutboundRetention:
			r.Fail("PAYMENT_EVENT_RETENTION", fmt.Sprintf("must be at least 3d (72h), the time a failed webhook is retried for: a host recovering from a full outage would find its events gone, got %q", v))
		default:
			e.Retention = d
		}
	}
	before := len(r.errs)
	r.PositiveDuration("PAYMENT_EVENTS_TICK_INTERVAL", &e.TickInterval, "deliveries would be attempted in a tight loop")
	if len(r.errs) == before && e.TickInterval > maxEventsTick {
		r.Fail("PAYMENT_EVENTS_TICK_INTERVAL", fmt.Sprintf("must be at most %s: it is the longest a host waits to be told", maxEventsTick))
	}
	r.Bool("PAYMENT_EVENTS_ALLOW_PRIVATE_ENDPOINTS", &e.AllowPrivate)
	return e
}
