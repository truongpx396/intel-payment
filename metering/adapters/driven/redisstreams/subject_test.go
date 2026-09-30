package redisstreams

import "testing"

func TestSubjectMatching(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pattern, subject string
		want             bool
	}{
		{"billing.warn.a/w:1", "billing.warn.a/w:1", true},
		{"billing.warn.a/w:1", "billing.warn.b/w:1", false},
		{"billing.warn.>", "billing.warn.a/w:1", true},
		{"billing.warn.>", "billing.warn.a/w:1.x.y", true},
		{"billing.warn.>", "billing.warn", false},
		{"billing.warn.>", "billing.blocked.a/w:1", false},
		{"billing.*.tick", "billing.reconcile.tick", true},
		{"billing.*.tick", "billing.idem.purge.tick", false},
		{"billing.>", "billing.idem.purge.tick", true},
		{"billing.*", "billing.a.b", false},
		{"billing.*.b", "billing.a", false},
		{">", "billing.a", true},
		{"billing.>.x", "billing.a.x", false}, // `>` is only a wildcard as the last token
	} {
		if got := matches(c.pattern, c.subject); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.pattern, c.subject, got, c.want)
		}
	}
}

func TestIDArithmetic(t *testing.T) {
	t.Parallel()
	a, err := parseID("1700000000000-5")
	if err != nil || a.String() != "1700000000000-5" || a.next().String() != "1700000000000-6" {
		t.Fatalf("%v %v", a, err)
	}
	if !a.less(a.next()) || a.less(a) {
		t.Fatal("ordering")
	}
	if (streamID{ms: 1, seq: ^uint64(0)}).next() != (streamID{ms: 2}) {
		t.Fatal("the sequence rolls over into the next millisecond")
	}
	for _, bad := range []string{"", "12", "x-1", "1-y"} {
		if _, err := parseID(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}
