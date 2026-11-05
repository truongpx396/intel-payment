// Package app holds the use cases of outbound delivery: the recorder that turns what the engine announces into events, the dispatcher
// that sends them, and the feed that lets a host pull them.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/truongpx396/intel-payment/events"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// RecorderGroup is the recorder's consumer group. It is part of the deployment: a group that changed between releases would hand every
// announcement to the old group and to the new one, and a host would be told twice (the source key would absorb it, but not for free).
const RecorderGroup = "events-recorder"

// RecorderDeps are what the recorder needs.
type RecorderDeps struct {
	Store   events.Store
	Bus     ports.Bus
	Metrics ports.Metrics // default: no-op
	Prefix  string        // PAYMENT_SUBJECT_PREFIX
}

// Recorder turns the engine's announcements into events (outbound-events.md): <prefix>.balance.low.<tag> becomes credits.low,
// .warn. credits.warning, .blocked. credits.blocked, .payment. a payment.* event and .entitlement. entitlements.changed. It copies a
// named few fields and nothing else — an event carries scope ids and amounts, never card data, a provider secret or an email — and
// it trusts nothing it cannot cross-check: the subject, the realm and the scope must agree.
type Recorder struct {
	d   RecorderDeps
	met ports.Metrics
}

// NewRecorder validates the dependencies.
func NewRecorder(d RecorderDeps) (*Recorder, error) {
	var errs []error
	if d.Store == nil || d.Bus == nil {
		errs = append(errs, errors.New("an events Store and a Bus are required"))
	}
	if strings.TrimSpace(d.Prefix) == "" {
		errs = append(errs, errors.New("a subject prefix is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("events recorder: %w", err)
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &Recorder{d: d, met: met}, nil
}

type nopMetrics struct{}

func (nopMetrics) Count(string, int64, ports.Labels)     {}
func (nopMetrics) Gauge(string, int64, ports.Labels)     {}
func (nopMetrics) Observe(string, float64, ports.Labels) {}

// subjects are the announcements the recorder turns into events.
var subjects = []string{"balance.low", "warn", "blocked", "payment", "entitlement"}

type multiSub []ports.Subscription

func (m multiSub) Close() error {
	var errs []error
	for _, s := range m {
		errs = append(errs, s.Close())
	}
	return errors.Join(errs...)
}

// Subscribe joins every announcement subject under the recorder's group. Not being able to is the caller's failure to report: a worker
// that does not record is one whose hosts are never told anything.
func (r *Recorder) Subscribe(ctx context.Context) (ports.Subscription, error) {
	var subs multiSub
	for _, s := range subjects {
		sub, err := r.d.Bus.Subscribe(ctx, r.d.Prefix+"."+s+".>", RecorderGroup, r.Handle)
		if err != nil {
			_ = subs.Close()
			return nil, fmt.Errorf("join %s.%s.>: %w", r.d.Prefix, s, err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// Handle records one announcement. An announcement it cannot read is an error, so the bus delivers it again and then parks it: a
// malformed message is a defect somebody must see. One it can read and has no event for (a payment event of a kind the catalogue does
// not emit yet) is acknowledged: the catalogue grows without a version bump, and a producer ahead of it is not a fault.
func (r *Recorder) Handle(ctx context.Context, m ports.Message) error {
	ev, source, ok, err := r.translate(m)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	_, created, err := r.d.Store.Append(ctx, ev, source)
	if err != nil {
		return fmt.Errorf("record %s: %w", ev.Type, err)
	}
	if created {
		r.met.Count(events.MetricRecorded, 1, ports.Labels{"type": ev.Type})
	}
	return nil
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{events.ErrInvalid}, a...)...)
}

// checkRealm requires a realm that is one, so an event is never filed under a string a payload made up.
func checkRealm(realm string) error {
	if realm == "" {
		return invalid("the announcement names no realm")
	}
	if err := domain.Realm(realm).Validate(); err != nil {
		return invalid("%v", err)
	}
	return nil
}

func (r *Recorder) translate(m ports.Message) (ev events.Event, source string, ok bool, err error) {
	kind, token, found := r.kindOf(m.Subject)
	if !found {
		return ev, "", false, invalid("%q is not an announcement the recorder reads", m.Subject)
	}
	source = m.Subject + "|" + m.ID
	switch kind {
	case "balance.low":
		var p struct {
			Realm string `json:"realm"`
			Scope string `json:"scope"`
			Watch string `json:"watch"`
			Seq   int64  `json:"seq"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return ev, "", false, invalid("a low-balance announcement that is not JSON: %v", err)
		}
		sc, err := r.scopeOf(token, p.Realm, p.Scope)
		if err != nil {
			return ev, "", false, err
		}
		i := strings.LastIndex(p.Watch, ":")
		threshold, perr := strconv.ParseInt(p.Watch[i+1:], 10, 64)
		if i < 1 || perr != nil {
			return ev, "", false, invalid("a low-balance announcement with the watch %q, which is not <pool>:<threshold>", p.Watch)
		}
		return build(events.CreditsLow, sc, map[string]any{"pool": p.Watch[:i], "threshold": threshold, "seq": p.Seq}), source, true, nil
	case "warn":
		var p struct {
			Realm  string `json:"realm"`
			Scope  string `json:"scope"`
			Limit  string `json:"limit_name"`
			Sub    string `json:"subject"`
			Cap    int64  `json:"cap"`
			Window string `json:"window"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return ev, "", false, invalid("a warning that is not JSON: %v", err)
		}
		sc, err := r.scopeOf(token, p.Realm, p.Scope)
		if err != nil {
			return ev, "", false, err
		}
		data := map[string]any{"limit": p.Limit, "cap": p.Cap, "window": p.Window}
		if p.Sub != "" {
			data["subject"] = p.Sub
		}
		return build(events.CreditsWarning, sc, data), source, true, nil
	case "blocked":
		var p struct {
			Realm    string `json:"realm"`
			Scope    string `json:"scope"`
			Limit    string `json:"limit_name"`
			DenyCode string `json:"deny_code"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return ev, "", false, invalid("a refusal that is not JSON: %v", err)
		}
		sc, err := r.scopeOf(token, p.Realm, p.Scope)
		if err != nil {
			return ev, "", false, err
		}
		return build(events.CreditsBlocked, sc, map[string]any{"limit": p.Limit, "deny_code": p.DenyCode}), source, true, nil
	case "payment":
		return r.payment(m, token)
	default: // entitlement
		var p struct {
			Realm  string          `json:"realm"`
			Scope  events.ScopeRef `json:"scope"`
			Reason string          `json:"reason"`
		}
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			return ev, "", false, invalid("an entitlement announcement that is not JSON: %v", err)
		}
		sc, err := r.refOf(token, p.Realm, p.Scope)
		if err != nil {
			return ev, "", false, err
		}
		return build(events.EntitlementsChanged, sc, map[string]any{"reason": p.Reason}), source, true, nil
	}
}

func (r *Recorder) payment(m ports.Message, token string) (ev events.Event, src string, ok bool, err error) {
	var p struct {
		Realm      string          `json:"realm"`
		Scope      events.ScopeRef `json:"scope"`
		Event      string          `json:"event"`
		Payment    string          `json:"payment"`
		Amount     int64           `json:"amount"`
		Currency   string          `json:"currency"`
		ReceiptURL string          `json:"receipt_url"`
		Reason     string          `json:"reason"`
	}
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		return ev, "", false, invalid("a payment announcement that is not JSON: %v", err)
	}
	sc, err := r.refOf(token, p.Realm, p.Scope)
	if err != nil {
		return ev, "", false, err
	}
	typ, known := map[string]string{
		"succeeded": events.PaymentSucceeded, "failed": events.PaymentFailed, "refunded": events.PaymentRefunded,
		"disputed": events.PaymentDisputed, "charged_back": events.PaymentChargedBack,
	}[p.Event]
	if !known {
		return ev, "", false, nil // renewed, canceled: subscription events are designed, not emitted
	}
	data := map[string]any{"payment_id": p.Payment, "amount": p.Amount, "currency": p.Currency}
	if p.ReceiptURL != "" {
		data["receipt_url"] = p.ReceiptURL
	}
	if p.Reason != "" {
		data["failure_reason"] = p.Reason
	}
	// A processor that retries after a crash announces the same fact twice on two bus messages, and a host must not be told twice
	// with two ids: the fact is the key.
	sum := sha256.Sum256([]byte(strings.Join([]string{p.Realm, sc.kind, sc.id, p.Payment, p.Event, strconv.FormatInt(p.Amount, 10)}, "\x00")))
	return build(typ, sc, data), "payment|" + hex.EncodeToString(sum[:]), true, nil
}

// kindOf splits a subject into the announcement it is and the scope token it ends with.
func (r *Recorder) kindOf(subject string) (kind, token string, ok bool) {
	for _, k := range subjects {
		if t, found := strings.CutPrefix(subject, r.d.Prefix+"."+k+"."); found && t != "" {
			return k, t, true
		}
	}
	return "", "", false
}

type scoped struct{ realm, kind, id string }

// scopeOf reads a scope tag and requires the subject, the realm and the tag to agree.
func (r *Recorder) scopeOf(token, realm, tag string) (scoped, error) {
	if err := checkRealm(realm); err != nil {
		return scoped{}, err
	}
	sc, err := parseTag(tag)
	if err != nil {
		return scoped{}, invalid("%v", err)
	}
	if string(sc.Realm) != realm || sc.SubjectToken() != token {
		return scoped{}, invalid("the announcement's subject, realm and scope disagree")
	}
	return scoped{realm, sc.Kind, sc.ID}, nil
}

func (r *Recorder) refOf(token, realm string, ref events.ScopeRef) (scoped, error) {
	if err := checkRealm(realm); err != nil {
		return scoped{}, err
	}
	sc := domain.Scope{Realm: domain.Realm(realm), Kind: ref.Kind, ID: ref.ID}
	if err := sc.Validate(); err != nil {
		return scoped{}, invalid("%v", err)
	}
	if sc.SubjectToken() != token {
		return scoped{}, invalid("the announcement's subject and scope disagree")
	}
	return scoped{realm, ref.Kind, ref.ID}, nil
}

func build(typ string, sc scoped, data map[string]any) events.Event {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{}`) // a map of strings and numbers always marshals
	}
	return events.Event{Realm: sc.realm, Type: typ, ScopeKind: sc.kind, ScopeID: sc.id, Data: raw}
}

// parseTag reads a scope back from its Tag — <realm>/<kind>:<id>, as an announcement names one. The realm has no slash and the kind no
// colon, so the first slash and the first colon after it are the seams. It accepts only what Tag would write. (The payment layer reads
// the same tag; the two layers share a format, not a package.)
func parseTag(tag string) (domain.Scope, error) {
	realm, rest, ok := strings.Cut(tag, "/")
	if !ok {
		return domain.Scope{}, fmt.Errorf("%q is not a scope tag (<realm>/<kind>:<id>)", tag)
	}
	kind, id, ok := strings.Cut(rest, ":")
	if !ok {
		return domain.Scope{}, fmt.Errorf("%q is not a scope tag (<realm>/<kind>:<id>)", tag)
	}
	s := domain.Scope{Realm: domain.Realm(realm), Kind: kind, ID: id}
	if err := s.Validate(); err != nil {
		return domain.Scope{}, err
	}
	if s.Tag() != tag {
		return domain.Scope{}, fmt.Errorf("%q is not the tag of the scope it names", tag)
	}
	return s, nil
}
