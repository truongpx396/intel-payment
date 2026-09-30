package natsjetstream

import (
	"strings"
	"testing"
)

func TestConsumerNamesAreValidAndDistinguishGroupFromPattern(t *testing.T) {
	t.Parallel()
	bad := ".*>/\\ \t\n"
	seen := map[string]string{}
	for _, c := range []struct{ group, pattern string }{
		{"workers", "billing.reconcile.tick"},
		{"workers", "billing.warn.>"},
		{"workers", "billing.*.a"},
		{"outbound", "billing.warn.>"},
		{"outbound", "billing.blocked.>"},
		{"a.b", "x"},
		{"a_b", "x"}, // sanitises to the same prefix as "a.b": the hash keeps them apart
		{strings.Repeat("g", 100), "billing.>"},
		{"grüppe", "billing.>"},
	} {
		n := ConsumerName(c.group, c.pattern)
		if strings.ContainsAny(n, bad) || n == "" || len(n) > 64 {
			t.Errorf("%q/%q → %q is not a valid consumer name", c.group, c.pattern, n)
		}
		if n != ConsumerName(c.group, c.pattern) {
			t.Errorf("%q/%q is not stable", c.group, c.pattern)
		}
		key := c.group + "|" + c.pattern
		if prev, dup := seen[n]; dup {
			t.Errorf("%q and %q collide on %q", prev, key, n)
		}
		seen[n] = key
	}
}

func TestStreamNamesAreValid(t *testing.T) {
	t.Parallel()
	for prefix, want := range map[string]string{"billing": "BILLING", "my.product": "MY_PRODUCT", "a b/c": "A_B_C", "ok-1_x": "OK-1_X"} {
		if got := streamName(prefix); got != want {
			t.Errorf("streamName(%q) = %q, want %q", prefix, got, want)
		}
	}
}

func TestIntentSubjectsLiveOutsideTheBusSubjectSpace(t *testing.T) {
	t.Parallel()
	o := IntentsOptions{Prefix: "billing"}
	o.defaults()
	if got := o.subject(7); got != "billing_intents.7" {
		t.Fatalf("%s", got)
	}
	if strings.HasPrefix(o.subject(7), "billing.") {
		t.Fatal("a subscriber to billing.> must never be handed a money intent")
	}
	if o.Stream != "BILLING_INTENTS" {
		t.Fatalf("%s", o.Stream)
	}
}
