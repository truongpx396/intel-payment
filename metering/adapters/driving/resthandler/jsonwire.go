package resthandler

import (
	"fmt"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// The JSON shapes of rest-api.md. They are the contract's names, not the domain's: snake_case, windows
// spelt out, timestamps ISO-8601 strings, credits and counts int64 (a JSON number that is not an integer
// is refused on the way in, never rounded).

type scopeJSON struct {
	Realm string `json:"realm,omitempty"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
}

func (s scopeJSON) domain() domain.Scope {
	return domain.Scope{Realm: domain.Realm(s.Realm), Kind: s.Kind, ID: s.ID}
}

type quantityJSON struct {
	Unit   string `json:"unit"`
	Amount int64  `json:"amount"`
}

type limitJSON struct {
	Name           string  `json:"name"`
	Subject        string  `json:"subject,omitempty"`
	Resource       string  `json:"resource,omitempty"`
	Unit           string  `json:"unit"`
	Max            int64   `json:"max"`
	Window         string  `json:"window"`
	DurSeconds     int64   `json:"dur_seconds,omitempty"`
	TZ             string  `json:"tz,omitempty"`
	WarnAt         float64 `json:"warn_at"`
	DenyCode       string  `json:"deny_code"`
	MaxEntitlement string  `json:"max_entitlement,omitempty"`
}

// limitFromJSON reads a limit a caller supplied. The window is required: a default would make an
// unfinished limit a balance, and a ceiling that silently means something else is the worst kind.
func limitFromJSON(l limitJSON, field string) (domain.Limit, error) {
	if l.Window == "" {
		return domain.Limit{}, fmt.Errorf("%w: %s.window is required", domain.ErrInvalid, field)
	}
	w, err := domain.ParseWindow(l.Window)
	if err != nil {
		return domain.Limit{}, fmt.Errorf("%w: %s.window %q is not one of balance, daily, hourly, rolling, job", domain.ErrInvalid, field, l.Window)
	}
	if l.DurSeconds < 0 || l.DurSeconds > int64(1<<62/int64(time.Second)) {
		return domain.Limit{}, fmt.Errorf("%w: %s.dur_seconds is out of range", domain.ErrInvalid, field)
	}
	return domain.Limit{
		Name: l.Name, Subject: l.Subject, Resource: l.Resource, Unit: domain.Unit(l.Unit), Max: l.Max, Window: w,
		Dur: time.Duration(l.DurSeconds) * time.Second, TZ: l.TZ, WarnAt: l.WarnAt, DenyCode: l.DenyCode, MaxEntitlement: l.MaxEntitlement,
	}, nil
}

func limitToJSON(l domain.Limit) limitJSON {
	return limitJSON{
		Name: l.Name, Subject: l.Subject, Resource: l.Resource, Unit: string(l.Unit), Max: l.Max, Window: l.Window.String(),
		DurSeconds: int64(l.Dur / time.Second), TZ: l.TZ, WarnAt: l.WarnAt, DenyCode: l.DenyCode, MaxEntitlement: l.MaxEntitlement,
	}
}

// parseTime reads an RFC 3339 timestamp, which carries its zone, and returns it in UTC. A timestamp with
// no zone is ambiguous, and an ambiguous occurred_at is a retry that cannot be told from a new event.
func parseTime(s, field string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be an ISO-8601 timestamp with a zone, like 2026-09-29T12:00:00Z", domain.ErrInvalid, field)
	}
	return t.UTC(), nil
}

// ------------------------------------------------------------------ requests --

type admitRequest struct {
	Scope    *scopeJSON        `json:"scope"`
	Resource string            `json:"resource"`
	Subjects map[string]string `json:"subjects"`
	Limits   []limitJSON       `json:"limits"`
	MaxCost  int64             `json:"max_cost"`
}

type recordRequest struct {
	IdemKey    string            `json:"idem_key"`
	Scope      *scopeJSON        `json:"scope"`
	Resource   string            `json:"resource"`
	RateKey    string            `json:"rate_key"`
	Quantities []quantityJSON    `json:"quantities"`
	Subjects   map[string]string `json:"subjects"`
	OccurredAt string            `json:"occurred_at"`
	Attributes map[string]string `json:"attributes"`
}

type grantRequest struct {
	IdemKey   string            `json:"idem_key"`
	Scope     *scopeJSON        `json:"scope"`
	Pool      string            `json:"pool"`
	Amount    int64             `json:"amount"`
	Reason    string            `json:"reason"`
	ExpiresAt string            `json:"expires_at"`
	SourceKey string            `json:"source_key"`
	Ref       map[string]string `json:"ref"`
}

type transferRequest struct {
	IdemKey        string            `json:"idem_key"`
	From           *scopeJSON        `json:"from"`
	To             *scopeJSON        `json:"to"`
	FromPool       string            `json:"from_pool"`
	ToPool         string            `json:"to_pool"`
	Amount         int64             `json:"amount"`
	Reason         string            `json:"reason"`
	MaxDestBalance int64             `json:"max_dest_balance"`
	Ref            map[string]string `json:"ref"`
}

func subjectsOf(m map[string]string) domain.Subjects {
	if len(m) == 0 {
		return nil
	}
	return domain.Subjects(m)
}

func mapOrNil(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// ----------------------------------------------------------------- responses --

type admissionJSON struct {
	Allowed  bool       `json:"allowed"`
	Exceeded *limitJSON `json:"exceeded,omitempty"`
	Warning  *limitJSON `json:"warning,omitempty"`
	Headroom int64      `json:"headroom"`
	Blocked  bool       `json:"blocked"`
}

func admissionToJSON(a domain.Admission) admissionJSON {
	out := admissionJSON{Allowed: a.Allowed, Headroom: int64(a.Headroom), Blocked: a.Blocked}
	if a.Exceeded != nil {
		l := limitToJSON(*a.Exceeded)
		out.Exceeded = &l
	}
	if a.Warning != nil {
		l := limitToJSON(*a.Warning)
		out.Warning = &l
	}
	return out
}

type receiptJSON struct {
	IdemKey         string `json:"idem_key"`
	Applied         bool   `json:"applied"`
	Deferred        bool   `json:"deferred"`
	Delta           int64  `json:"delta"`
	Balance         int64  `json:"balance"`
	Seq             int64  `json:"seq"`
	RateCardVersion string `json:"rate_card_version,omitempty"`
	Writeoff        int64  `json:"writeoff"`
}

func receiptToJSON(r domain.Receipt) receiptJSON {
	return receiptJSON{
		IdemKey: r.IdemKey, Applied: r.Applied, Deferred: r.Deferred, Delta: int64(r.Delta), Balance: int64(r.Balance),
		Seq: r.Seq, RateCardVersion: r.RateCardVersion, Writeoff: int64(r.Writeoff),
	}
}

type transferJSON struct {
	IdemKey     string `json:"idem_key"`
	Applied     bool   `json:"applied"`
	Amount      int64  `json:"amount"`
	FromBalance int64  `json:"from_balance"`
	Status      string `json:"status"`
}

type balanceJSON struct {
	Pools map[string]int64 `json:"pools"`
	Total int64            `json:"total"`
}
