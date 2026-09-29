package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

func TestRunUsage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no args":                        {nil, nil, "usage:"},
		"two args":                       {[]string{"up", "down"}, nil, "usage:"},
		"unknown command":                {[]string{"down"}, map[string]string{"PAYMENT_LEDGER_DSN": "x"}, "unknown command"},
		"missing dsn":                    {[]string{"up"}, nil, "PAYMENT_LEDGER_DSN is required"},
		"functions need the deploy role": {[]string{"functions"}, map[string]string{"PAYMENT_BALANCE_REDIS_URL": "redis://x"}, "PAYMENT_BALANCE_REDIS_DEPLOY_URL is required"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errb bytes.Buffer
			code := run(context.Background(), c.args, func(k string) string { return c.env[k] }, &out, &errb)
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("stderr %q lacks %q", errb.String(), c.want)
			}
		})
	}
}
