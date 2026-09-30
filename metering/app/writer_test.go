package app_test

import (
	"strings"
	"testing"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/ports"
)

func TestNewWriterRefusesWhatItCannotRunWithout(t *testing.T) {
	t.Parallel()
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}
	clock := contracts.NewFakeClock(epoch)
	good := app.Deps{Balance: &fakeBalance{}, Books: &fakeBooks{}, Stream: nopStream{}, Bus: nopBus{}, Clock: clock}

	cases := map[string]struct {
		mutate func(*app.Deps, *metering.Config)
		want   string
	}{
		"no stream":       {func(d *app.Deps, _ *metering.Config) { d.Stream = nil }, "Stream"},
		"no books":        {func(d *app.Deps, _ *metering.Config) { d.Books = nil }, "Books"},
		"no clock":        {func(d *app.Deps, _ *metering.Config) { d.Clock = nil }, "Clock"},
		"no bus":          {func(d *app.Deps, _ *metering.Config) { d.Bus = nil }, "Bus"},
		"rollup, archive": {func(_ *app.Deps, c *metering.Config) { c.LedgerGranularity = metering.GranularityRollup }, "UsageArchive"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, cf := good, cfg
			c.mutate(&d, &cf)
			if _, err := app.NewWriter(cf, d); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error naming %s", err, c.want)
			}
		})
	}
	if _, err := app.NewWriter(cfg, good); err != nil {
		t.Fatalf("a complete wiring must build: %v", err)
	}
}

func TestNewExpirerNeedsItsStores(t *testing.T) {
	t.Parallel()
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}
	if _, err := app.NewExpirer(cfg, app.Deps{}); err == nil {
		t.Fatal("an expirer with nothing to expire against must not build")
	}
}

type nopStream struct{ ports.IntentStream }
type nopBus struct{ ports.Bus }
