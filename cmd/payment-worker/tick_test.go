package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// R110: `payment-worker tick` is the door an external scheduler publishes through. It refuses what it can see is wrong before it dials
// anything: a tick that does not exist, a per-shard tick that names no shard (guessing "all of them" is how one worker would be handed every
// shard's work), a shard the deployment does not have, a payload that is not JSON.

func runTick(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"tick"}, args...), getenv(env), &out, &errb)
	return code, out.String(), errb.String()
}

func TestATickThatDoesNotExistIsRefusedAndTheOnesThatDoAreListed(t *testing.T) {
	t.Parallel()
	code, out, errs := runTick(t, with(), "nightly")
	if code != 2 || out != "" || !strings.Contains(errs, `"nightly" is not a tick`) {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	for _, name := range []string{"reconcile", "audit", "transfer", "journal", "webhook", "outbound", "expiry", "dunning", "subdrift", "partitions", "idem.purge", "events.purge", "trim", "usage.rollup"} {
		if !strings.Contains(errs, name) {
			t.Errorf("the refusal does not list %s: %q", name, errs)
		}
	}
}

func TestAPerShardTickMustNameAShardTheDeploymentHas(t *testing.T) {
	t.Parallel()
	env := with("PAYMENT_SHARDS", "8")
	for _, args := range [][]string{
		{"reconcile"}, {"reconcile", `{}`}, {"audit", `{"shard":null}`}, {"reconcile", `{"shard":8}`}, {"audit", `{"shard":-1}`}, {"reconcile", `{"shard":"3"}`},
		{"reconcile", `{"shard":1,"kind":"full"`},
	} {
		if code, out, errs := runTick(t, env, args...); code != 2 || out != "" || errs == "" {
			t.Errorf("%v: %d %q %q", args, code, out, errs)
		}
	}
}

func TestAUsageRollupBackfillThatIsNotAWholeNumberOfDaysInRangeIsRefused(t *testing.T) {
	t.Parallel()
	for _, p := range []string{`{"days":0}`, `{"days":-5}`, `{"days":3661}`, `{"days":"30"}`, `{"days":1.5}`} {
		if code, out, errs := runTick(t, with(), "usage.rollup", p); code != 2 || out != "" || errs == "" {
			t.Errorf("%s: %d %q %q", p, code, out, errs)
		}
	}
}

func TestAPayloadThatIsNotJSONAndTooManyArgumentsAreRefused(t *testing.T) {
	t.Parallel()
	if code, _, errs := runTick(t, with(), "journal", "not json"); code != 2 || !strings.Contains(errs, "not JSON") {
		t.Fatalf("%d %q", code, errs)
	}
	if code, _, _ := runTick(t, with(), "journal", `{}`, "extra"); code != 2 {
		t.Fatalf("%d", code)
	}
	if code, _, errs := runTick(t, with()); code != 2 || !strings.Contains(errs, "usage: payment-worker tick") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestTheTickCommandStopsOnBadConfigurationBeforeItDialsAnything(t *testing.T) {
	t.Parallel()
	code, _, errs := runTick(t, with("PAYMENT_SHARDS", "many"), "journal")
	if code != 2 || !strings.Contains(errs, "PAYMENT_SHARDS") {
		t.Fatalf("%d %q", code, errs)
	}
	if strings.Contains(errs, "s3cret") {
		t.Fatalf("a credential reached the output: %q", errs)
	}
}

func TestAnythingElseAfterTheProgramNameIsStillAUsageError(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"reconcile"}, getenv(with()), &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("%d %q", code, errb.String())
	}
}
