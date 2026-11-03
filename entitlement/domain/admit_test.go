package domain_test

import (
	"errors"
	"math"
	"testing"

	"github.com/truongpx396/intel-payment/entitlement/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// R100: a quota measured against a count the host supplies. The host owns the count (invariant 4); this only decides
// whether `current + delta` fits. Arithmetic that could wrap is refused, never wrapped, and -1 is never compared.

func TestAdmitsPinsEveryBoundary(t *testing.T) {
	t.Parallel()
	top := int64(math.MaxInt64)
	cases := []struct {
		name           string
		limit          domain.Value
		current, delta int64
		allowed        bool
		limitWant      int64
		remaining      int64
	}{
		{"the last seat of 25", domain.QuotaValue(25), 24, 1, true, 25, 0},
		{"the 26th seat of 25", domain.QuotaValue(25), 25, 1, false, 25, 0},
		{"all 25 at once", domain.QuotaValue(25), 0, 25, true, 25, 0},
		{"26 at once", domain.QuotaValue(25), 0, 26, false, 25, 25},
		{"room to spare", domain.QuotaValue(25), 10, 5, true, 25, 10},
		{"a check of nothing, within", domain.QuotaValue(25), 10, 0, true, 25, 15},
		{"already over: the plan shrank under a count", domain.QuotaValue(25), 30, 0, false, 25, 0},
		{"a ceiling of zero admits nothing", domain.QuotaValue(0), 0, 1, false, 0, 0},
		{"a ceiling of zero admits a check of nothing", domain.QuotaValue(0), 0, 0, true, 0, 0},
		{"unlimited admits a million", domain.UnlimitedQuota(), 1_000_000, 1, true, -1, -1},
		{"unlimited admits at the int64 edge", domain.UnlimitedQuota(), top, top, true, -1, -1},
		{"the largest ceiling, full", domain.QuotaValue(top), top, 1, false, top, 0},
		{"the largest ceiling, one short", domain.QuotaValue(top), top - 1, 1, true, top, 0},
		{"a sum that would wrap is refused, not wrapped", domain.QuotaValue(top), 1, top, false, top, top - 1},
		{"two largest counts would wrap", domain.QuotaValue(100), top, top, false, 100, 0},
	}
	for _, c := range cases {
		d, err := c.limit.Admits(c.current, c.delta)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if d.Allowed != c.allowed {
			t.Errorf("%s: allowed = %v, want %v (%+v)", c.name, d.Allowed, c.allowed, d)
		}
		if d.Limit != c.limitWant || d.Current != c.current || d.Remaining != c.remaining {
			t.Errorf("%s: limit/current/remaining = %d/%d/%d, want %d/%d/%d", c.name, d.Limit, d.Current, d.Remaining, c.limitWant, c.current, c.remaining)
		}
	}
}

func TestAdmitsRefusesWhatIsNotACountOfAQuota(t *testing.T) {
	t.Parallel()
	bad := map[string]func() error{
		"a negative current": func() error { _, err := domain.QuotaValue(25).Admits(-1, 1); return err },
		"a negative delta":   func() error { _, err := domain.QuotaValue(25).Admits(1, -1); return err },
		"a flag":             func() error { _, err := domain.FlagValue(true).Admits(0, 1); return err },
		"an enum":            func() error { _, err := domain.EnumValue("gold").Admits(0, 1); return err },
		"an unset value":     func() error { _, err := (domain.Value{}).Admits(0, 1); return err },
		"unlimited, negative": func() error {
			_, err := domain.UnlimitedQuota().Admits(-5, 1)
			return err
		},
	}
	for name, f := range bad {
		if err := f(); !errors.Is(err, md.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

// An unset value must never read as "no ceiling". Refusing is the failure direction (invariant 1).
func TestUnsetNeverAdmitsAnything(t *testing.T) {
	t.Parallel()
	d, err := (domain.Value{}).Admits(0, 0)
	if err == nil && d.Allowed {
		t.Fatal("an unset value was read as permission")
	}
}
