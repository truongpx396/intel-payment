package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R105: a page render asks about several capabilities at once, so Check answers a batch — every key it was given, in the order
// it was given, from ONE resolution (a consistent picture, one read) — and a key the realm never declared is denied inside the
// batch and is not an error. It is the same judgement Allowed, Quota, Choice and Consume make, with a reason and a way out.

func i64(v int64) *int64 { return &v }

func checkRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, time.Hour)
	r.mem.DeclareKey(t, "t1", domain.KeyDecl{Key: "tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "silver", "gold"}})
	r.mem.PutPlan(t, "t1", "free", 1, domain.Entitlement{Key: "tier", Value: domain.EnumValue("bronze")})
	r.mem.PutPlan(t, "t1", "team", 2, domain.Entitlement{Key: "seats", Value: domain.QuotaValue(25)})
	r.mem.PutPlan(t, "t1", "pro", 3, domain.Entitlement{Key: "sso", Value: domain.FlagValue(true)}, domain.Entitlement{Key: "seats", Value: domain.QuotaValue(100)},
		domain.Entitlement{Key: "tier", Value: domain.EnumValue("gold")})
	r.mem.PutPlan(t, "t1", "enterprise", 4, domain.Entitlement{Key: "seats", Value: domain.UnlimitedQuota()})
	r.mem.Subscribe(t, scope, "team", domain.Active, nil)
	return r
}

func check(t *testing.T, r *rig, reqs ...domain.CheckRequest) []domain.CheckResult {
	t.Helper()
	out, err := r.e.Check(context.Background(), scope, reqs)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return out
}

func TestACheckAnswersEveryKeyItWasGivenInTheOrderItWasGiven(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	reqs := []domain.CheckRequest{
		{Key: "tier"}, {Key: "no_such_capability"}, {Key: "sso"}, {Key: "seats", Current: i64(3)}, {Key: "sso"}, {Key: "seats"}, {Key: "export"},
	}
	got := check(t, r, reqs...)
	if len(got) != len(reqs) {
		t.Fatalf("%d answers for %d questions: %+v", len(got), len(reqs), got)
	}
	for i, q := range reqs {
		if got[i].Key != q.Key {
			t.Errorf("answer %d is for %q; the question was %q: answers are in request order, duplicates included", i, got[i].Key, q.Key)
		}
	}
	if got[2].Allowed != got[4].Allowed || got[2].Reason != got[4].Reason {
		t.Errorf("two questions about sso in one batch were answered differently: %+v and %+v", got[2], got[4])
	}
}

// An unknown key is denied INSIDE the batch: a page that asks about twelve capabilities and has one typo still renders eleven.
func TestAnUnknownKeyIsDeniedInsideTheBatchAndIsNotAnError(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	got := check(t, r, domain.CheckRequest{Key: "sso"}, domain.CheckRequest{Key: "ssoo"}, domain.CheckRequest{Key: "SSO"}, domain.CheckRequest{Key: "seats"})
	for _, i := range []int{1, 2} {
		d := got[i]
		if d.Allowed || d.Kind != "" || d.Reason == "" || d.UpgradePath != "" || d.Value.IsSet() {
			t.Errorf("%q: %+v; want denied, no kind, a reason, no way out and no value", d.Key, d)
		}
		if !strings.Contains(d.Reason, string(d.Key)) {
			t.Errorf("the reason does not say which key it is about: %q", d.Reason)
		}
	}
	if !got[3].Allowed {
		t.Errorf("a typo two entries earlier denied a key the plan includes: %+v", got[3])
	}
}

func TestACheckOfEachKindSaysWhatTheScopeHoldsAndWhyNotWhenItDoesNot(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	got := check(t, r,
		domain.CheckRequest{Key: "sso"}, domain.CheckRequest{Key: "seats"}, domain.CheckRequest{Key: "tier"}, domain.CheckRequest{Key: "export"})

	sso := got[0] // team does not include sso; pro does
	if sso.Allowed || sso.Kind != domain.Flag || sso.Reason == "" || sso.UpgradePath != "pro" {
		t.Errorf("sso on team: %+v; want denied, a flag, a reason, and pro as the way out", sso)
	}
	seats := got[1] // a quota asked about with no count: is it granted, and how large
	if !seats.Allowed || seats.Kind != domain.Quota || seats.Value != domain.QuotaValue(25) || seats.Limit == nil || *seats.Limit != 25 || seats.Current != nil || seats.Remaining != nil {
		t.Errorf("seats with no count: %+v; want granted at 25 and nothing measured", seats)
	}
	tier := got[2] // team holds nothing for tier. Free has one, but sorts BEFORE team: a way out is a plan further up, not down.
	if tier.Kind != domain.Enum || tier.Allowed || tier.Value.IsSet() || tier.UpgradePath != "pro" {
		t.Errorf("tier on team: %+v; want denied, an enum, no value, and pro (not the cheaper free) as the way out", tier)
	}
	export := got[3] // declared, but no plan has it
	if export.Allowed || export.Kind != domain.Flag || export.Reason == "" || export.UpgradePath != "" {
		t.Errorf("export: %+v; want denied with a reason and nowhere to go", export)
	}
}

// With a count the quota is measured, exactly as Consume measures it: delta defaults to one more.
func TestACheckWithACountMeasuresItExactlyAsConsumeDoes(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	ctx := context.Background()
	for _, c := range []struct{ current, delta int64 }{{0, 1}, {24, 1}, {25, 1}, {26, 0}, {20, 6}, {0, 25}, {0, 26}, {100, 1}} {
		got := check(t, r, domain.CheckRequest{Key: "seats", Current: i64(c.current), Delta: i64(c.delta)})[0]
		want, err := r.e.Consume(ctx, scope, "seats", c.current, c.delta)
		if err != nil {
			t.Fatal(err)
		}
		if got.Allowed != want.Allowed || got.Reason != want.Reason || got.UpgradePath != want.UpgradePath ||
			got.Limit == nil || *got.Limit != want.Limit || got.Current == nil || *got.Current != want.Current || got.Remaining == nil || *got.Remaining != want.Remaining {
			t.Errorf("current %d delta %d: Check %+v, Consume %+v", c.current, c.delta, got, want)
		}
	}
	// A delta left out is one more.
	one := check(t, r, domain.CheckRequest{Key: "seats", Current: i64(25)})[0]
	if one.Allowed || one.Reason == "" || one.UpgradePath != "pro" {
		t.Errorf("the 26th seat on a 25-seat plan: %+v; want refused with pro as the way out", one)
	}
	if ok := check(t, r, domain.CheckRequest{Key: "seats", Current: i64(24)})[0]; !ok.Allowed || ok.Remaining == nil || *ok.Remaining != 0 {
		t.Errorf("the 25th seat: %+v", ok)
	}
}

func TestACheckOfUnlimitedIsMinusOneAndNeverRefuses(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	r.mem.Subscribe(t, scope, "enterprise", domain.Active, nil)
	r.announce(t, scope, "subscription")
	got := check(t, r, domain.CheckRequest{Key: "seats"}, domain.CheckRequest{Key: "seats", Current: i64(1_000_000), Delta: i64(1)})
	for _, d := range got {
		if !d.Allowed || d.Value != domain.UnlimitedQuota() || d.Limit == nil || *d.Limit != -1 {
			t.Errorf("unlimited: %+v; want allowed with a limit of exactly -1", d)
		}
	}
	if d := got[1]; d.Remaining == nil || *d.Remaining != -1 {
		t.Errorf("unlimited leaves unlimited room, as -1: %+v", d)
	}
}

func TestAnOverrideHeldBackFlagNamesNoPlanThatWouldLiftIt(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil)
	g := domain.Grant{Scope: scope, Key: "sso", Value: domain.FlagValue(false), Source: domain.FromOverride, Reason: "suspended by finance"}
	if err := r.mem.PutOverride(context.Background(), g, "alice"); err != nil {
		t.Fatal(err)
	}
	d := check(t, r, domain.CheckRequest{Key: "sso"})[0]
	if d.Allowed || d.UpgradePath != "" || !strings.Contains(d.Reason, "account") {
		t.Errorf("sso switched off for the account: %+v; want denied, the account named, and no plan offered (no plan outranks an override)", d)
	}
}

// One resolution for the whole batch: a consistent picture, and one read however many keys are asked about.
func TestABatchIsOneResolution(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0) // no cache: every resolution is a read
	reqs := make([]domain.CheckRequest, 0, 30)
	for range 10 {
		reqs = append(reqs, domain.CheckRequest{Key: "sso"}, domain.CheckRequest{Key: "seats", Current: i64(3)}, domain.CheckRequest{Key: "nope"})
	}
	if _, err := r.e.Check(context.Background(), scope, reqs); err != nil {
		t.Fatal(err)
	}
	if r.reads() != 1 {
		t.Fatalf("a batch of %d read the store %d times; want 1", len(reqs), r.reads())
	}
}

// Fail closed: a batch is answered whole or not at all. A store that cannot answer does not leave some keys permitted.
func TestABatchIsRefusedWholeWhenTheStoreCannotAnswer(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	boom := errors.New("connection refused")
	r.faults.BreakWith(boom)
	r.announce(t, scope, "subscription")
	out, err := r.e.Check(context.Background(), scope, []domain.CheckRequest{{Key: "seats"}, {Key: "sso"}})
	if err == nil || !errors.Is(err, domain.ErrUnavailable) || !errors.Is(err, boom) {
		t.Fatalf("want ErrUnavailable keeping its cause, got %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("a refused batch came with answers: %+v", out)
	}
}

func TestABatchThatIsNotAQuestionIsRefusedWhole(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	many := make([]domain.CheckRequest, domain.MaxChecks+1)
	for i := range many {
		many[i] = domain.CheckRequest{Key: "sso"}
	}
	for name, reqs := range map[string][]domain.CheckRequest{
		"no checks at all":      nil,
		"too many":              many,
		"an empty key":          {{Key: "sso"}, {Key: ""}},
		"a key far too long":    {{Key: domain.Key(strings.Repeat("a", 300))}},
		"a negative count":      {{Key: "seats", Current: i64(-1)}},
		"a negative delta":      {{Key: "sso"}, {Key: "seats", Current: i64(1), Delta: i64(-1)}},
		"a delta with no count": {{Key: "seats", Delta: i64(1)}},
	} {
		out, err := r.e.Check(context.Background(), scope, reqs)
		if !errors.Is(err, domain.ErrInvalid) || len(out) != 0 {
			t.Errorf("%s: %+v, %v; want ErrInvalid and no answers", name, out, err)
		}
	}
	// The limit itself is allowed.
	if _, err := r.e.Check(context.Background(), scope, many[:domain.MaxChecks]); err != nil {
		t.Errorf("%d checks, the most there may be: %v", domain.MaxChecks, err)
	}
	// And a scope that is not valid is no scope.
	if _, err := r.e.Check(context.Background(), md.Scope{Realm: "t1", Kind: "", ID: "x"}, []domain.CheckRequest{{Key: "sso"}}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a scope with no kind: want ErrInvalid, got %v", err)
	}
}

func TestACheckCountsEachDecisionByOutcome(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	check(t, r, domain.CheckRequest{Key: "seats"}, domain.CheckRequest{Key: "sso"}, domain.CheckRequest{Key: "nope"})
	if a, d := r.met.get("entitlement_decisions_total|realm=t1|outcome=allowed"), r.met.get("entitlement_decisions_total|realm=t1|outcome=denied"); a != 1 || d != 2 {
		t.Errorf("1 allowed and 2 denied expected; got %d and %d", a, d)
	}
}

// A way out is a plan further up the catalogue than the one held. When the plan held is no longer sold there is no "further up" to
// measure from, and every plan on sale is a candidate.
func TestTheWayOutIsFurtherUpTheCatalogueThanThePlanHeld(t *testing.T) {
	t.Parallel()
	r := checkRig(t)
	r.mem.Subscribe(t, scope, "pro", domain.Active, nil)
	r.announce(t, scope, "subscription")
	// pro is the top paid plan that has tier; asking for a thing only free has leaves nothing further up.
	r.mem.PutPlan(t, "t1", "pro", 3, domain.Entitlement{Key: "sso", Value: domain.FlagValue(true)})
	r.announce(t, scope, "subscription")
	if d := check(t, r, domain.CheckRequest{Key: "tier"})[0]; d.Allowed || d.UpgradePath != "" {
		t.Errorf("tier on pro, which has none, where only the plan BELOW has it: %+v; want no way out", d)
	}
	r.mem.RetirePlan(t, "t1", "pro")
	r.announce(t, scope, "subscription")
	if d := check(t, r, domain.CheckRequest{Key: "tier"})[0]; d.UpgradePath != "free" {
		t.Errorf("tier for a subscriber of a plan no longer sold: %+v; want every plan on sale to be a candidate (free is the first)", d)
	}
}
