//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	entpg "github.com/truongpx396/intel-payment/entitlement/adapters/driven/postgres"
	entapp "github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	entdomain "github.com/truongpx396/intel-payment/entitlement/domain"
	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/system"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// R104 (FR-017, D30), end to end: the ceiling a `limits` row names is sized by the scope's PLAN, through the real adapter, the
// real entitlement store and cache, the real Meter and the real hot path. A plan change resizes it on the next Admit; -1 applies
// no ceiling; nothing granted keeps the row's own max; an outage is the Meter's AdmitFailPolicy to decide.

// lateQuota is a QuotaSource the stack can be built with before the database it will read exists.
type lateQuota struct {
	mu sync.RWMutex
	q  ports.QuotaSource
}

func (l *lateQuota) set(q ports.QuotaSource) { l.mu.Lock(); l.q = q; l.mu.Unlock() }

func (l *lateQuota) Quota(ctx context.Context, s domain.Scope, key string) (int64, error) {
	l.mu.RLock()
	q := l.q
	l.mu.RUnlock()
	return q.Quota(ctx, s, key)
}

// fanBus delivers each publication to every subscription whose pattern is a prefix of the subject, in the publisher's goroutine.
type fanBus struct {
	mu   sync.Mutex
	subs []struct {
		prefix string
		h      ports.Handler
	}
}

func (b *fanBus) Publish(ctx context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	subs := append([]struct {
		prefix string
		h      ports.Handler
	}(nil), b.subs...)
	b.mu.Unlock()
	for _, s := range subs {
		if strings.HasPrefix(subject, s.prefix) {
			if err := s.h(ctx, ports.Message{Subject: subject, Payload: payload}); err != nil {
				return err
			}
		}
	}
	return nil
}

type closer func() error

func (c closer) Close() error { return c() }

func (b *fanBus) Subscribe(_ context.Context, pattern, _ string, h ports.Handler) (ports.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, struct {
		prefix string
		h      ports.Handler
	}{strings.TrimSuffix(pattern, ">"), h})
	return closer(func() error { return nil }), nil
}
func (b *fanBus) Pending(context.Context, string, string) (int64, time.Duration, error) {
	return 0, 0, nil
}
func (b *fanBus) Trim(context.Context, string, ports.RetentionPolicy) error { return nil }

func TestAPlanChangeResizesTheCeilingOnTheNextAdmitThroughTheRealHotPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	late := &lateQuota{}
	s := newStack(t, stackOpt{quotas: late})
	r := string(s.realm)

	// The entitlement layer over the stack's own database, with a cache that only an announcement (or an hour) can refresh.
	faults := contracts.NewBreakable(entpg.New(s.pool))
	ent, err := entapp.New(entapp.Deps{Store: faults, Clock: system.Clock{}, CacheTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	bus := &fanBus{}
	w, err := ent.Watch(ctx, bus, "billing", "entitlement-cache-integration")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	late.set(entapp.NewQuotaSource(ent))

	// Configuration: a daily ceiling sized by `daily_user_credits` (10 if nothing grants it), three plans, one subscriber.
	s.exec(`INSERT INTO entitlement_keys (realm, key, kind) VALUES ($1, 'daily_user_credits', 'quota')`, r)
	for code, size := range map[string]*int64{"free": ptr64(700), "pro": ptr64(2000), "bare": nil} {
		s.exec(`INSERT INTO plans (realm, code, name, kind, billing_interval, sort_order) VALUES ($1, $2, $2, 'subscription', 'month', $3)`, r, code, len(code))
		if size != nil {
			s.exec(`INSERT INTO plan_entitlements (plan_id, feature_key, value_kind, value_int)
			        SELECT id, 'daily_user_credits', 'quota', $3 FROM plans WHERE realm = $1 AND code = $2`, r, code, *size)
		}
	}
	s.exec(`INSERT INTO provider_accounts (id, provider, realms, mode, secret_ref, webhook_secret_ref) VALUES ('acct-' || $1, 'stripe', ARRAY[$1], 'test', 'env:K', 'env:W')`, r)
	s.exec(`INSERT INTO limits (realm, name, subject_kind, unit, max, max_entitlement, window_kind, deny_code, sort_order)
		VALUES ($1,'user_daily','user','credit',10,'daily_user_credits','daily','limit_reached',10)`, r)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if ls, _ := s.config.Limits(ctx, s.realm); len(ls) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the new limit never reached the cache")
		}
	}

	sc := s.scope()
	s.open(sc)
	if _, err := s.meter.Grant(ctx, domain.Grant{Scope: sc, Amount: 1_000_000, IdemKey: "pay", Reason: "purchase"}); err != nil {
		t.Fatal(err)
	}
	subscribe := func(plan string) {
		t.Helper()
		s.exec(`UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE realm = $1 AND scope_id = $2 AND status = 'active'`, r, sc.ID)
		s.exec(`INSERT INTO subscriptions (realm, scope_kind, scope_id, plan_id, provider_account, provider_subscription_id, status)
		        SELECT $1, $2, $3, id, 'acct-' || $1, gen_random_uuid()::text, 'active' FROM plans WHERE realm = $1 AND code = $4`, r, sc.Kind, sc.ID, plan)
		must(t, bus.Publish(ctx, "billing.entitlement."+sc.SubjectToken(), []byte(`{"reason":"subscription"}`))) // what the processor does
	}
	subscribe("free")

	u1 := domain.Subjects{"user": "u1"}
	gate := func() (domain.Admission, error) {
		return s.meter.Admit(ctx, sc, domain.AdmitRequest{Resource: "llm.chat", Subjects: u1, MaxCost: 1})
	}
	spend := func(key string) {
		t.Helper()
		e := s.event(sc, key)
		e.Subjects = u1
		if _, err := s.meter.Record(ctx, e); err != nil { // 660 each
			t.Fatal(err)
		}
	}

	// On the free plan the ceiling is 700: 660 spent leaves 40; 1320 is over.
	spend("evt-1")
	if adm, err := gate(); err != nil || !adm.Allowed || adm.Headroom != 40 {
		t.Fatalf("660 of the free plan's 700: %+v, %v", adm, err)
	}
	spend("evt-2")
	if adm, err := gate(); err != nil || adm.Allowed || adm.Exceeded == nil || adm.Exceeded.Max != 700 {
		t.Fatalf("1320 of the free plan's 700 must be refused at 700: %+v, %v", adm, err)
	}

	// The customer upgrades. The very next Admit enforces 2000.
	subscribe("pro")
	if adm, err := gate(); err != nil || !adm.Allowed || adm.Exceeded != nil || adm.Headroom != 680 {
		t.Fatalf("after the upgrade 1320 of 2000 leaves 680: %+v, %v", adm, err)
	}

	// A plan that grants nothing: the row's own max (10) is the ceiling, and 1320 is far over it.
	subscribe("bare")
	if adm, err := gate(); err != nil || adm.Allowed || adm.Exceeded == nil || adm.Exceeded.Max != 10 {
		t.Fatalf("a plan that grants nothing keeps the limit's own max: %+v, %v", adm, err)
	}

	// An override of -1 applies no ceiling at all — not a ceiling of -1, which compared as a number refuses everything.
	subscribe("pro")
	over, err := entapp.NewOverrides(entapp.OverrideDeps{Store: entpg.New(s.pool), Bus: bus, SubjectPrefix: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	g := entdomain.Grant{Scope: sc, Key: "daily_user_credits", Value: entdomain.UnlimitedQuota(), Source: entdomain.FromOverride, Reason: "negotiated at renewal"}
	if err := over.Put(ctx, g, "alice"); err != nil {
		t.Fatal(err)
	}
	spend("evt-3")
	if adm, err := gate(); err != nil || !adm.Allowed || adm.Exceeded != nil {
		t.Fatalf("an unlimited override leaves no ceiling to exceed at 1980: %+v, %v", adm, err)
	}

	// The store cannot answer: that is an error the Meter applies AdmitFailPolicy to — fail_closed by default — and never "unlimited".
	faults.BreakWith(errors.New("connection refused"))
	must(t, bus.Publish(ctx, "billing.entitlement."+sc.SubjectToken(), []byte(`{"reason":"override"}`)))
	if _, err := gate(); !errors.Is(err, domain.ErrQuotaUnavailable) {
		t.Fatalf("an entitlement outage under fail_closed: want ErrQuotaUnavailable, got %v", err)
	}
	faults.Heal()
	if adm, err := gate(); err != nil || !adm.Allowed {
		t.Fatalf("the outage is over: %+v, %v", adm, err)
	}
}

func ptr64(v int64) *int64 { return &v }

// The admin API's side of "refused when configured": a limit sized by a flag is a 400 at PutLimit, not a surprise at Admit.
func TestALimitSizedByAFlagIsRefusedWhenItIsConfiguredThroughTheAdminAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newStack(t, stackOpt{})
	r := string(s.realm)
	s.exec(`INSERT INTO entitlement_keys (realm, key, kind) VALUES ($1, 'sso', 'flag'), ($1, 'daily_user_credits', 'quota')`, r)
	admin, err := app.NewAdmin(s.cfg, s.deps.Pricers, pgadapter.NewConfigWriter(s.pool))
	if err != nil {
		t.Fatal(err)
	}
	ch := ports.ConfigChange{Actor: "alice", Reason: "size the daily ceiling by plan", Via: "test"}
	limit := func(key string) ports.LimitConfig {
		return ports.LimitConfig{Limit: domain.Limit{Name: "user_daily", Unit: domain.CreditUnit, Max: 10, Window: domain.Daily, DenyCode: domain.DenyLimitReached, MaxEntitlement: key}}
	}
	if _, err := admin.PutLimit(ctx, ch, s.realm, limit("sso")); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a limit sized by a flag: want ErrInvalid (a 400), got %v", err)
	}
	if _, err := admin.PutLimit(ctx, ch, s.realm, limit("daily_user_credits")); err != nil {
		t.Fatalf("a limit sized by a quota: %v", err)
	}
}
