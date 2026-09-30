package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func daily(name string, limit int64) domain.Limit {
	return domain.Limit{Name: name, Subject: "user", Unit: domain.CreditUnit, Max: limit, Window: domain.Daily, WarnAt: 0.8, DenyCode: domain.DenyLimitReached}
}

func TestAdmitMergesConfiguredAndCallerLimitsAndOnlyTightens(t *testing.T) {
	t.Parallel()
	chat := daily("chat_daily", 100)
	chat.Resource = "llm.chat"
	other := daily("storage_daily", 100)
	other.Resource = "storage"
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit, chat, other}})

	tighter := daily("chat_daily", 40) // tighter: wins
	looser := balanceLimit
	looser.Max = 5_000 // a bigger overdraft: ignored
	run := domain.Limit{Name: "run_cap", Subject: "job", Unit: domain.CreditUnit, Max: 500, Window: domain.Job, Dur: time.Hour, DenyCode: domain.DenyLimitReached}
	_, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{
		Resource: "llm.chat", Subjects: domain.Subjects{"user": "u1", "job": "j1"}, MaxCost: 10, Limits: []domain.Limit{tighter, looser, run}})
	if err != nil {
		t.Fatal(err)
	}
	got := r.bal.admitted[0]
	if len(got.Limits) != 3 || got.Limits[0].Name != "scope_balance" || got.Limits[0].Max != 0 ||
		got.Limits[1].Name != "chat_daily" || got.Limits[1].Max != 40 || got.Limits[2].Name != "run_cap" {
		t.Fatalf("a storage-only limit must not govern a chat call, the caller can only tighten, and a job budget is added: %+v", got.Limits)
	}
	if got.MaxCost != 10 || got.Resource != "llm.chat" {
		t.Fatalf("%+v", got)
	}
}

func TestAdmitRefusesACallerWindowLimitNothingCanCount(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	_, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1,
		Limits: []domain.Limit{{Name: "made_up", Unit: domain.CreditUnit, Max: 1, Window: domain.Daily, DenyCode: domain.DenyLimitReached}}})
	if !errors.Is(err, domain.ErrUncountableLimit) {
		t.Fatalf("a ceiling that could never bind must be refused, not silently ignored: %v", err)
	}
	if len(r.bal.admitted) != 0 {
		t.Fatal("the hot tier must not be asked")
	}
	// A malformed caller limit is refused too.
	_, err = r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1, Limits: []domain.Limit{{Name: "x", Window: domain.Rolling}}})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("%v", err)
	}
}

func TestAdmitRefusesAMissingSubjectBeforeTheHotTier(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit, daily("user_daily", 10)}})
	_, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1})
	if !errors.Is(err, domain.ErrMissingSubject) || len(r.bal.admitted) != 0 {
		t.Fatalf("invariant 17: %v (admitted %d)", err, len(r.bal.admitted))
	}
}

func TestAdmitSizesLimitsFromEntitlements(t *testing.T) {
	t.Parallel()
	sized := daily("user_daily", 100)
	sized.MaxEntitlement = "daily_user_credits"
	subj := domain.Subjects{"user": "u1"}
	ask := func(q quota, cfg metering.Config, noSource bool) (*rig, domain.AdmitRequest, error) {
		r := newRig(t, rigOpt{cfg: cfg, limits: []domain.Limit{balanceLimit, sized}, quotas: q, noQuotas: noSource})
		_, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{Subjects: subj, MaxCost: 1})
		var req domain.AdmitRequest
		if len(r.bal.admitted) > 0 {
			req = r.bal.admitted[0]
		}
		return r, req, err
	}
	names := func(r domain.AdmitRequest) []string {
		var n []string
		for _, l := range r.Limits {
			n = append(n, l.Name)
		}
		return n
	}

	if _, req, err := ask(quota{max: 250}, metering.Config{}, false); err != nil || req.Limits[1].Max != 250 {
		t.Fatalf("a plan, an override or the free tier sizes the ceiling: %v %+v", err, req.Limits)
	}
	if _, req, err := ask(quota{max: ports.Unlimited}, metering.Config{}, false); err != nil || len(req.Limits) != 1 {
		t.Fatalf("-1 means the entitlement grants no ceiling: %v %v", err, names(req))
	}
	if _, req, err := ask(quota{err: domain.ErrQuotaNotGranted}, metering.Config{}, false); err != nil || req.Limits[1].Max != 100 {
		t.Fatalf("nothing grants it: the row's own max is the fallback: %v %+v", err, req.Limits)
	}

	// Unresolvable: fail_closed refuses (and never asks the hot tier); it is NEVER treated as unlimited.
	boom := errors.New("entitlement store down")
	r, _, err := ask(quota{err: boom}, metering.Config{}, false)
	if !errors.Is(err, domain.ErrQuotaUnavailable) || !errors.Is(err, boom) || len(r.bal.admitted) != 0 {
		t.Fatalf("a quota that cannot be resolved must fail closed: %v", err)
	}
	if _, _, err = ask(quota{}, metering.Config{}, true); !errors.Is(err, domain.ErrQuotaUnavailable) {
		t.Fatalf("a limit that names an entitlement with no QuotaSource wired is a misconfiguration, not 'unlimited': %v", err)
	}
	// fail_open serves it, and says so.
	r, req, err := ask(quota{err: boom}, metering.Config{AdmitFail: metering.FailOpen}, false)
	if err != nil || len(req.Limits) != 1 || r.met.count(ports.MetricAdmitFailOpen) != 1 {
		t.Fatalf("fail_open drops the unresolved ceiling but counts it: %v %v %d", err, names(req), r.met.count(ports.MetricAdmitFailOpen))
	}
}

func TestAdmitCountsUncappedCallsAndCanRequireMaxCost(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	if _, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{}); err != nil {
		t.Fatal(err)
	}
	if r.met.count(ports.MetricAdmitUncapped) != 1 {
		t.Fatal("invariant 3: an omitted MaxCost must be counted, or the overshoot bound is silently unenforced")
	}
	if _, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 5}); err != nil || r.met.count(ports.MetricAdmitUncapped) != 1 {
		t.Fatal("a capped call is not counted")
	}
	strict := newRig(t, rigOpt{cfg: metering.Config{RequireMaxCost: true}, limits: []domain.Limit{balanceLimit}})
	if _, err := strict.m.Admit(context.Background(), ws, domain.AdmitRequest{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("RequireMaxCost makes it a precondition: %v", err)
	}
	if _, err := strict.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: -1}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("negative max_cost")
	}
}

func TestAdmitDuringAnOutageAppliesTheFailPolicy(t *testing.T) {
	t.Parallel()
	outages := []error{domain.ErrHotStoreUnavailable, domain.ErrShardFrozen, domain.ErrColdShard, domain.ErrBackpressure}
	for _, cause := range outages {
		closed := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
		closed.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) { return domain.Admission{}, cause }
		if _, err := closed.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1}); !errors.Is(err, domain.ErrHotStoreUnavailable) {
			t.Errorf("%v under fail_closed must refuse with ErrHotStoreUnavailable, got %v", cause, err)
		}

		open := newRig(t, rigOpt{cfg: metering.Config{AdmitFail: metering.FailOpen}, limits: []domain.Limit{balanceLimit}})
		open.bal.admit = closed.bal.admit
		adm, err := open.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1})
		if err != nil || !adm.Allowed || adm.Headroom <= 0 {
			t.Errorf("%v under fail_open serves (with headroom, not zero): %+v %v", cause, adm, err)
		}
		if open.met.count(ports.MetricAdmitFailOpen) != 1 {
			t.Errorf("%v: fail_open must be counted, it is served without a check", cause)
		}
	}
}

func TestAdmitRehydratesAColdScopeAndAsksAgain(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	r.books.state = ports.BookedState{
		Watermark: ports.Watermark{Exists: true, Gen: 1, AppliedSeq: 41},
		Booked:    map[domain.Pool]domain.Credits{"general": 500, "promo": 30},
		Suspense:  map[domain.Pool]domain.Credits{"general": -100},
	}
	calls := 0
	r.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
		calls++
		if calls == 1 {
			return domain.Admission{}, domain.ErrColdScope
		}
		return domain.Admission{Allowed: true, Headroom: 430}, nil
	}
	adm, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1})
	if err != nil || !adm.Allowed || calls != 2 {
		t.Fatalf("%+v %v (calls %d)", adm, err, calls)
	}
	st := r.bal.rehydOn[0]
	if st.AppliedSeq != 41 || st.ShardGen != 1 || st.Pools["general"] != 400 || st.Pools["promo"] != 30 {
		t.Fatalf("rehydrate must install booked + open suspense at applied_seq: %+v", st)
	}
	if r.met.count("metering_rehydrate_total{stale}") != 1 {
		t.Fatal("rehydrate is counted, by cause")
	}

	// Still cold after a rehydrate: another bump raced it. That is the hot tier being unavailable.
	r2 := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	r2.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
		return domain.Admission{}, domain.ErrColdScope
	}
	if _, err := r2.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1}); !errors.Is(err, domain.ErrHotStoreUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestAdmitFailsClosedWhenConfigurationCannotBeRead(t *testing.T) {
	t.Parallel()
	// Serving without the realm's ceilings is serving unbounded: even fail_open must not do it.
	r := newRig(t, rigOpt{cfg: metering.Config{AdmitFail: metering.FailOpen}})
	r.deps.Limits = brokenLimits{}
	m, _ := newMeter(t, r)
	if _, err := m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1}); err == nil {
		t.Fatal("an unreadable limit configuration must not admit")
	}
}

type brokenLimits struct{}

func (brokenLimits) Limits(context.Context, domain.Realm) ([]domain.Limit, error) {
	return nil, errors.New("postgres down")
}

func TestAdmitDenialsAreObservableAndNotificationsAreCoalesced(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	r.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
		l := balanceLimit
		return domain.Admission{Allowed: false, Exceeded: &l}, nil
	}
	for i := 0; i < 5; i++ {
		adm, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1})
		if err != nil || adm.Allowed {
			t.Fatal("a denial is a normal answer, not an error")
		}
	}
	if got := r.met.count(ports.MetricAdmitDenied); got != 5 {
		t.Fatalf("every denial is counted: %d", got)
	}
	if n := len(r.bus.subjects()); n != 1 {
		t.Fatalf("a block repeats on every request: coalesce to one per window, got %d", n)
	}
	if s := r.bus.subjects()[0]; s != "billing.blocked."+ws.Tag() {
		t.Fatalf("subject %q", s)
	}
	r.clock.Advance(2 * time.Minute)
	_, _ = r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1})
	if n := len(r.bus.subjects()); n != 2 {
		t.Fatalf("after the window it may publish again: %d", n)
	}
}

func TestAdmitWarnsOncePerWindowPerSubject(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit, daily("user_daily", 100)}})
	warn := daily("user_daily", 100)
	r.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
		return domain.Admission{Allowed: true, Warning: &warn, Headroom: 20}, nil
	}
	for _, u := range []string{"u1", "u1", "u1", "u2"} {
		if _, err := r.m.Admit(context.Background(), ws, domain.AdmitRequest{Subjects: domain.Subjects{"user": u}, MaxCost: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if subj := r.bus.subjects(); len(subj) != 2 || subj[0] != "billing.warn."+ws.Tag() {
		t.Fatalf("u1 warns once, u2 has their own: %v", subj)
	}
}

func TestAdmitObservesItsLatencyByOutcome(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	_, _ = r.m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1})
	_, _ = r.m.Admit(context.Background(), domain.Scope{Kind: "Bad Kind", ID: "x"}, domain.AdmitRequest{})
	if len(r.met.obs) != 2 || r.met.obs[0] != "allowed" || r.met.obs[1] != "invalid" {
		t.Fatalf("%v", r.met.obs)
	}
}

// captureBus records a notification as the bus sees it: subject, body, and the context it was handed.
type captureBus struct {
	ports.Bus
	mu  sync.Mutex
	got []capturedPublish
}

type capturedPublish struct {
	subject     string
	body        []byte
	ctxErr      error
	hasDeadline bool
	remaining   time.Duration
}

func (b *captureBus) Publish(ctx context.Context, subject string, body []byte) error {
	dl, ok := ctx.Deadline()
	c := capturedPublish{subject: subject, body: body, ctxErr: ctx.Err(), hasDeadline: ok}
	if ok {
		c.remaining = time.Until(dl)
	}
	b.mu.Lock()
	b.got = append(b.got, c)
	b.mu.Unlock()
	return nil
}

// A block names WHICH limit blocked and why — the alert, the dashboard and the host's upsell all key
// on it — and the notification is best effort: published on a live context with its own short
// deadline, so a slow bus can neither drop it up front nor stall the admission it describes.
func TestADenialNamesItsLimitAndIsPublishedOnALiveBoundedContext(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpt{limits: []domain.Limit{balanceLimit}})
	r.bal.admit = func(domain.Scope, domain.AdmitRequest) (domain.Admission, error) {
		l := balanceLimit
		return domain.Admission{Allowed: false, Exceeded: &l}, nil
	}
	bus := &captureBus{}
	r.deps.Bus = bus
	m, _ := newMeter(t, r)
	if _, err := m.Admit(context.Background(), ws, domain.AdmitRequest{MaxCost: 1}); err != nil {
		t.Fatal(err)
	}
	if got := r.met.count(ports.MetricAdmitDenied + "{" + balanceLimit.Name + "}"); got != 1 {
		t.Fatalf("the denial metric must carry the limit that denied: %v", r.met.counts)
	}
	if len(bus.got) != 1 {
		t.Fatalf("one notification, got %d", len(bus.got))
	}
	p := bus.got[0]
	var body map[string]any
	if err := json.Unmarshal(p.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["limit_name"] != balanceLimit.Name || body["deny_code"] != string(balanceLimit.DenyCode) {
		t.Fatalf("the notification must name the limit and its deny code: %v", body)
	}
	if p.ctxErr != nil {
		t.Fatalf("a notification is published on a live context, got %v", p.ctxErr)
	}
	if !p.hasDeadline || p.remaining > 100*time.Millisecond {
		t.Fatalf("a notification has its own deadline of at most 100ms: %v %v", p.hasDeadline, p.remaining)
	}
}
