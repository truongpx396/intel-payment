package metering_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering"
)

func valid() metering.Config {
	return metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}
}

func TestDefaultsAreAppliedToACopy(t *testing.T) {
	t.Parallel()
	c := valid()
	d := c.WithDefaults()
	if c.Shards != 0 || c.Realm != "" {
		t.Fatal("WithDefaults must not mutate the caller's value")
	}
	if d.Realm != metering.DefaultRealm || d.Shards != 256 || d.Settlement != metering.SettlementOutbox ||
		d.HotAckWait != metering.HotAckNone || d.LedgerGranularity != metering.GranularityEvent ||
		d.DrainBatch != 500 || d.HotIdemTTL != 24*time.Hour || d.UsageIdemWindow != 72*time.Hour ||
		d.IdemSafetyMargin != time.Hour || d.MaxClockSkew != 5*time.Minute ||
		d.AdmitFail != metering.FailClosed || d.NegativeBalance != metering.ClampToZero ||
		d.CreditExpiry != metering.ExpiryOff || d.MaxOperationAmount != 1<<50 || d.DefaultWarnAt != 0.8 ||
		d.SubjectPrefix != "billing" || d.AdmitTimeout != 250*time.Millisecond || d.RecordTimeout != 500*time.Millisecond ||
		d.ReconcileTolerance != 0 {
		t.Fatalf("defaults drifted from the contract: %+v", d)
	}
}

func TestValidateAcceptsTheMinimalConfig(t *testing.T) {
	t.Parallel()
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mut  func(*metering.Config)
		want string
	}{
		"no redis":  {func(c *metering.Config) { c.BalanceRedisURL = "" }, "BalanceRedisURL"},
		"no dsn":    {func(c *metering.Config) { c.LedgerDSN = "" }, "LedgerDSN"},
		"bad realm": {func(c *metering.Config) { c.Realm = "Prod A" }, "realm"},
		"zero shards is a default, negative is not": {func(c *metering.Config) { c.Shards = -1 }, "shards"},
		"too many shards":                    {func(c *metering.Config) { c.Shards = 16385 }, "shards"},
		"unknown settlement":                 {func(c *metering.Config) { c.Settlement = "yolo" }, "Settlement"},
		"unknown ack wait":                   {func(c *metering.Config) { c.HotAckWait = "fsync" }, "HotAckWait"},
		"unknown granularity":                {func(c *metering.Config) { c.LedgerGranularity = "weekly" }, "LedgerGranularity"},
		"unknown fail policy":                {func(c *metering.Config) { c.AdmitFail = "fail_sideways" }, "AdmitFail"},
		"unknown negative":                   {func(c *metering.Config) { c.NegativeBalance = "forgive" }, "NegativeBalance"},
		"unknown expiry":                     {func(c *metering.Config) { c.CreditExpiry = "lifo" }, "CreditExpiry"},
		"hot guard outlives the durable one": {func(c *metering.Config) { c.HotIdemTTL = 100 * time.Hour }, "HotIdemTTL"},
		"margin swallows the window":         {func(c *metering.Config) { c.IdemSafetyMargin = 72 * time.Hour }, "IdemSafetyMargin"},
		"negative tolerance":                 {func(c *metering.Config) { c.ReconcileTolerance = -1 }, "ReconcileTolerance"},
		"amount above 2^52":                  {func(c *metering.Config) { c.MaxOperationAmount = 1<<52 + 1 }, "MaxOperationAmount"},
		"negative amount":                    {func(c *metering.Config) { c.MaxOperationAmount = -1 }, "MaxOperationAmount"},
		"warn above one":                     {func(c *metering.Config) { c.DefaultWarnAt = 1.1 }, "DefaultWarnAt"},
		"negative batch":                     {func(c *metering.Config) { c.DrainBatch = -1 }, "DrainBatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tc.mut(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	err := metering.Config{Settlement: "x", AdmitFail: "y"}.Validate()
	for _, want := range []string{"BalanceRedisURL", "LedgerDSN", "Settlement", "AdmitFail"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("an operator fixing config should see all of it, not one at a time; missing %q in %v", want, err)
		}
	}
}
