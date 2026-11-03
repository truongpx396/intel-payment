package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/app"
	"github.com/truongpx396/intel-payment/entitlement/contracts"
	"github.com/truongpx396/intel-payment/entitlement/domain"
	"github.com/truongpx396/intel-payment/entitlement/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R102 (entitlement invariant 5): an override says who approved it and why, and a key can only be given a value it can hold.
// The refusals happen when the override is WRITTEN, not when it is read: a bad value that reaches the table is a bad value in
// every cache until someone finds it.

// spy counts the writes that reach the store, so a refusal can be shown to have written nothing.
type spy struct {
	ports.EntitlementStore
	puts atomic.Int32
}

func (s *spy) PutOverride(ctx context.Context, g domain.Grant, actor string) error {
	s.puts.Add(1)
	return s.EntitlementStore.PutOverride(ctx, g, actor)
}

func overridesOver(t *testing.T) (*app.Overrides, *spy, *contracts.MemStore) {
	t.Helper()
	mem := contracts.NewMemStore()
	for _, d := range []domain.KeyDecl{
		{Key: "sso", Kind: domain.Flag}, {Key: "seats", Kind: domain.Quota},
		{Key: "support_tier", Kind: domain.Enum, EnumOrder: []string{"bronze", "silver", "gold"}},
	} {
		mem.DeclareKey(t, "t1", d)
	}
	sp := &spy{EntitlementStore: mem}
	o, err := app.NewOverrides(app.OverrideDeps{Store: sp})
	if err != nil {
		t.Fatal(err)
	}
	return o, sp, mem
}

func grant(key string, v domain.Value, reason string) domain.Grant {
	return domain.Grant{Scope: scope, Key: domain.Key(key), Value: v, Source: domain.FromOverride, Reason: reason}
}

func TestAnOverrideThatDoesNotSayWhyIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	for name, reason := range map[string]string{"empty": "", "spaces": "   ", "a tab and a newline": "\t\n"} {
		o, sp, _ := overridesOver(t)
		err := o.Put(contracts.Ctx(), grant("sso", domain.FlagValue(true), reason), "alice")
		if !errors.Is(err, domain.ErrReasonRequired) || !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrReasonRequired (a 400), got %v", name, err)
		}
		if n := sp.puts.Load(); n != 0 {
			t.Errorf("%s: the refused override reached the store %d time(s)", name, n)
		}
	}
}

func TestAnOverrideThatDoesNotSayWhoIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	for name, actor := range map[string]string{"empty": "", "spaces": "  "} {
		o, sp, _ := overridesOver(t)
		if err := o.Put(contracts.Ctx(), grant("sso", domain.FlagValue(true), "pilot"), actor); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
		if n := sp.puts.Load(); n != 0 {
			t.Errorf("%s: reached the store %d time(s)", name, n)
		}
	}
}

// A key can only be given a value it can hold, and only an override can be written here.
func TestAnOverrideIsCheckedAgainstItsKeyWhenWritten(t *testing.T) {
	t.Parallel()
	subscription := grant("sso", domain.FlagValue(true), "pilot")
	subscription.Source = domain.FromSubscription
	noSource := grant("sso", domain.FlagValue(true), "pilot")
	noSource.Source = ""
	badScope := grant("sso", domain.FlagValue(true), "pilot")
	badScope.Scope.Kind = ""
	for name, g := range map[string]domain.Grant{
		"a key the realm never declared": grant("no_such_capability", domain.FlagValue(true), "pilot"),
		"a flag value for a quota key":   grant("seats", domain.FlagValue(true), "pilot"),
		"a quota for a flag key":         grant("sso", domain.QuotaValue(3), "pilot"),
		"an unranked enum member":        grant("support_tier", domain.EnumValue("platinum"), "pilot"),
		"a quota below unlimited":        grant("seats", domain.QuotaValue(-2), "pilot"),
		"an unset value":                 grant("seats", domain.Value{}, "pilot"),
		"a subscription-sourced grant":   subscription,
		"a grant with no source":         noSource,
		"a scope that is not valid":      badScope,
	} {
		o, sp, _ := overridesOver(t)
		if err := o.Put(contracts.Ctx(), g, "alice"); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
		if n := sp.puts.Load(); n != 0 {
			t.Errorf("%s: reached the store %d time(s)", name, n)
		}
	}
}

func TestAnOverrideThatIsExplainedAndHoldableIsWrittenAndTakesEffect(t *testing.T) {
	t.Parallel()
	o, sp, mem := overridesOver(t)
	for _, g := range []domain.Grant{
		grant("sso", domain.FlagValue(true), "2026-Q3 enterprise pilot, approved by sales"),
		grant("seats", domain.UnlimitedQuota(), "negotiated at renewal"),
		grant("support_tier", domain.EnumValue("gold"), "launch partner"),
	} {
		if err := o.Put(contracts.Ctx(), g, "alice"); err != nil {
			t.Fatalf("%s: %v", g.Key, err)
		}
	}
	if n := sp.puts.Load(); n != 3 {
		t.Fatalf("%d writes reached the store, want 3", n)
	}
	e, err := app.New(app.Deps{Store: mem, Clock: contracts.NewFakeClock(contracts.T0)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Allowed(contracts.Ctx(), scope, "sso"); err != nil || !ok {
		t.Errorf("the override is not in force: %v, %v", ok, err)
	}
	if limit, ok, _ := e.Quota(contracts.Ctx(), scope, "seats"); !ok || limit != -1 {
		t.Errorf("seats = %d, %v; want unlimited", limit, ok)
	}
	// A scope that leaves its realm blank is in the default realm; the key is declared in t1, so it is refused there.
	blank := grant("sso", domain.FlagValue(true), "pilot")
	blank.Scope.Realm = ""
	if err := o.Put(contracts.Ctx(), blank, "alice"); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an override in the default realm for a key only t1 declared: want ErrInvalid, got %v", err)
	}
}

// A store that cannot say which keys are declared cannot say whether the override is holdable: it is not written. The cause is
// kept for the operator.
func TestAnOverrideIsNotWrittenWhenTheKeysCannotBeRead(t *testing.T) {
	t.Parallel()
	o, sp, mem := overridesOver(t)
	faults := contracts.NewBreakable(mem)
	sp.EntitlementStore = faults
	boom := errors.New("connection refused")
	faults.BreakOnly(boom, "Keys")
	err := o.Put(contracts.Ctx(), grant("sso", domain.FlagValue(true), "pilot"), "alice")
	if !errors.Is(err, boom) || !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("want ErrUnavailable wrapping the cause, got %v", err)
	}
	if n := sp.puts.Load(); n != 0 {
		t.Errorf("reached the store %d time(s) with the keys unreadable", n)
	}
}

func TestNewOverridesNeedsAStore(t *testing.T) {
	t.Parallel()
	if _, err := app.NewOverrides(app.OverrideDeps{}); err == nil {
		t.Fatal("an Overrides with no store was built")
	}
	_ = md.DefaultRealm
}
