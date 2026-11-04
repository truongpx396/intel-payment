package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// R104 (FR-017, D30): a metering ceiling is a plan decision, so metering asks the entitlement layer how large this scope's
// ceiling is. This is the entitlement half of that: what the adapter answers, in the words metering's Meter acts on. (That the
// Meter applies AdmitFailPolicy to an error is pinned in metering's own tests; that the whole thing binds through the real hot
// path is pinned in integration/.)

func quotaRig(t *testing.T, ttl time.Duration) (*rig, mports.QuotaSource) {
	t.Helper()
	r := newRig(t, ttl)
	r.mem.DeclareKey(t, "t1", domain.KeyDecl{Key: "daily_user_credits", Kind: domain.Quota})
	r.mem.DeclareKey(t, "t1", domain.KeyDecl{Key: "tier", Kind: domain.Enum, EnumOrder: []string{"a", "b"}})
	r.mem.PutPlan(t, "t1", "free", 1, domain.Entitlement{Key: "daily_user_credits", Value: domain.QuotaValue(700)}, domain.Entitlement{Key: "tier", Value: domain.EnumValue("a")})
	r.mem.PutPlan(t, "t1", "pro", 2, domain.Entitlement{Key: "daily_user_credits", Value: domain.QuotaValue(2000)})
	r.mem.PutPlan(t, "t1", "enterprise", 3, domain.Entitlement{Key: "daily_user_credits", Value: domain.UnlimitedQuota()})
	r.mem.Subscribe(t, scope, "free", domain.Active, nil)
	return r, app.NewQuotaSource(r.e)
}

func quota(t *testing.T, q mports.QuotaSource, key string) (int64, error) {
	t.Helper()
	return q.Quota(context.Background(), scope, key)
}

func TestTheQuotaSourceAnswersWhatThePlanSizesTheCeilingTo(t *testing.T) {
	t.Parallel()
	_, q := quotaRig(t, 0)
	if n, err := quota(t, q, "daily_user_credits"); err != nil || n != 700 {
		t.Fatalf("Quota = %d, %v; want the free plan's 700", n, err)
	}
}

// A plan change resizes the ceiling on the next call — through the cache and its announcement, not after a TTL.
func TestAPlanChangeResizesTheCeilingOnTheNextCall(t *testing.T) {
	t.Parallel()
	r, q := quotaRig(t, time.Hour)
	r.watch(t)
	if n, _ := quota(t, q, "daily_user_credits"); n != 700 {
		t.Fatalf("setup: %d", n)
	}
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil)
	r.announce(t, scope, "subscription")
	if n, err := quota(t, q, "daily_user_credits"); err != nil || n != 2000 {
		t.Fatalf("after the upgrade Quota = %d, %v; want 2000 on the next call", n, err)
	}
	// An override beats the plan, for this scope only.
	o, _ := app.NewOverrides(app.OverrideDeps{Store: r.mem, Bus: r.bus, SubjectPrefix: prefix})
	g := domain.Grant{Scope: scope, Key: "daily_user_credits", Value: domain.QuotaValue(50_000), Source: domain.FromOverride, Reason: "negotiated"}
	if err := o.Put(context.Background(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	if n, _ := quota(t, q, "daily_user_credits"); n != 50_000 {
		t.Fatalf("with an override Quota = %d; want 50000", n)
	}
	other := md.Scope{Realm: "t1", Kind: "org", ID: "someone-else"}
	r.mem.Subscribe(t, other, "free", domain.Active, nil)
	if n, _ := q.Quota(context.Background(), other, "daily_user_credits"); n != 700 {
		t.Fatalf("another scope Quota = %d; an override is one scope's", n)
	}
}

// -1 is a grant. It reaches the Meter as -1, which drops the ceiling; it is not "not granted" (which keeps the row's own max)
// and it is not a number to compare.
func TestUnlimitedIsReturnedAsMinusOneAndIsNotAnAbsence(t *testing.T) {
	t.Parallel()
	r, q := quotaRig(t, 0)
	r.mem.Subscribe(t, scope, "enterprise", domain.Active, nil)
	if mports.Unlimited != -1 {
		t.Fatalf("the Meter's constant for unlimited is %d; the books and the wire say -1", mports.Unlimited)
	}
	n, err := quota(t, q, "daily_user_credits")
	if err != nil || n != mports.Unlimited {
		t.Fatalf("Quota = %d, %v; want exactly -1 and no error", n, err)
	}
	if errors.Is(err, md.ErrQuotaNotGranted) {
		t.Fatal("unlimited was reported as not granted, which would keep the limit's own max")
	}
}

// Nothing grants it: the limit keeps the max its own row names. A key nobody declared is the same — and is not an error.
func TestAKeyNothingGrantsIsNotGrantedAndNotAnOutage(t *testing.T) {
	t.Parallel()
	r, q := quotaRig(t, 0)
	r.mem.PutPlan(t, "t1", "free", 1) // a free plan with no ceiling in it
	for _, key := range []string{"daily_user_credits", "never_declared"} {
		n, err := quota(t, q, key)
		if !errors.Is(err, md.ErrQuotaNotGranted) {
			t.Errorf("%s: Quota = %d, %v; want ErrQuotaNotGranted", key, n, err)
		}
		if errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("%s: an absence was reported as an outage", key)
		}
	}
}

// An error resolving a quota is not "no ceiling": it reaches the Meter as an error that is NOT ErrQuotaNotGranted, so the Meter
// applies AdmitFailPolicy. Whatever the adapter does, it must not turn an outage into an absence or into a size.
func TestAnErrorIsNeverTurnedIntoAnAbsenceOrASize(t *testing.T) {
	t.Parallel()
	r, q := quotaRig(t, 0)
	boom := errors.New("connection refused")
	r.faults.BreakWith(boom)
	n, err := quota(t, q, "daily_user_credits")
	if err == nil {
		t.Fatalf("Quota = %d with the store down; want an error", n)
	}
	if errors.Is(err, md.ErrQuotaNotGranted) {
		t.Fatal("an outage was reported as 'nothing grants it', which would give the scope the limit row's own max")
	}
	if !errors.Is(err, boom) || !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("the cause is lost: %v", err)
	}
	if n != 0 {
		t.Errorf("a size (%d) came back with the error", n)
	}
}

// A flag or an enum is not a size. The database refuses a limit that names one when it is configured; if one is there anyway
// (a key re-declared by a write that bypassed everything), it is an error at the call, never a number.
func TestAKeyThatIsNotAQuotaIsAnErrorNotASize(t *testing.T) {
	t.Parallel()
	_, q := quotaRig(t, 0)
	for _, key := range []string{"sso", "tier"} {
		n, err := quota(t, q, key)
		if !errors.Is(err, domain.ErrInvalid) || errors.Is(err, md.ErrQuotaNotGranted) || n != 0 {
			t.Errorf("%s: Quota = %d, %v; want ErrInvalid and no size", key, n, err)
		}
	}
}

// The adapter reads through the Entitler, so it shares the Entitler's cache and its announcements: a sizing on every Admit does
// not read the store on every Admit.
func TestTheQuotaSourceSharesTheEntitlersCache(t *testing.T) {
	t.Parallel()
	r, q := quotaRig(t, time.Hour)
	for range 5 {
		if _, err := quota(t, q, "daily_user_credits"); err != nil {
			t.Fatal(err)
		}
	}
	r.allowed(t, scope, "sso")
	if r.reads() != 1 {
		t.Fatalf("five sizings and a check read the store %d times; want 1", r.reads())
	}
}

// An adapter over no Entitler is no QuotaSource at all, so the Meter reports "no QuotaSource is wired" and applies its fail
// policy — and not a source that answers something.
func TestTheQuotaSourceOfNothingIsNothing(t *testing.T) {
	t.Parallel()
	if q := app.NewQuotaSource(nil); q != nil {
		t.Fatalf("a QuotaSource over no Entitler answered to %T", q)
	}
}
