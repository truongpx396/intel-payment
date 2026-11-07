package main

import (
	"errors"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// R110a: the `subdrift` tick sweeps every account that has a live subscription, or the one account its payload names. A payload it cannot
// read is refused — never read as "every account", which is a provider call for every subscription there is.

func TestADriftTickMayNameOneAccountAndNothingElse(t *testing.T) {
	t.Parallel()
	for payload, want := range map[string]string{"": "", `{}`: "", `{"account":null}`: "", `{"account":"acct"}`: "acct", `{"account":"acct-2_x"}`: "acct-2_x"} {
		got, err := parseDriftAccount([]byte(payload))
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", payload, got, err, want)
		}
	}
	for _, bad := range []string{`{"account":""}`, `{"account":5}`, `{"account":"Acct"}`, `{"account":"a b"}`, `{"accounts":["acct"]}`, `{"account":"acct","all":true}`, `not json`, `[1]`, `"acct"`} {
		if _, err := parseDriftAccount([]byte(bad)); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("%q: %v, want %v", bad, err, domain.ErrInvalid)
		}
	}
}

func TestADriftTickThatNamesJunkIsRefusedBeforeAnythingIsDialed(t *testing.T) {
	t.Parallel()
	for _, p := range []string{`{"account":""}`, `{"account":5}`, `{"account":"A B"}`, `{"accounts":["acct"]}`} {
		if code, out, errs := runTick(t, with(), "subdrift", p); code != 2 || out != "" || errs == "" {
			t.Errorf("%s: %d %q %q", p, code, out, errs)
		}
	}
}

// The claim of a drift tick is on the account it names, so two accounts are swept side by side and one account is swept one at a time; the
// tick that names none is the claim on all of them.
func TestTheClaimOfADriftTickIsTheAccountItNames(t *testing.T) {
	t.Parallel()
	for payload, want := range map[string]string{`{}`: "", `{"account":"acct"}`: "acct", `{"account":"acct-b"}`: "acct-b"} {
		got, err := driftTickKey([]byte(payload))
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", payload, got, err, want)
		}
	}
	if _, err := driftTickKey([]byte(`{"account":5}`)); err == nil {
		t.Error("a payload that cannot be read was given a claim")
	}
}
