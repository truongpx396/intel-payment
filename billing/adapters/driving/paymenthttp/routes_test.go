package paymenthttp_test

import (
	"sort"
	"testing"
)

// Which privilege a route needs is the door's decision, and the registry enforces it before the handler runs. A read, a
// checkout and a portal are the host's everyday calls and need only `record`; what commits a customer to charges — a plan
// change, a cancellation, a standing instruction to top up — needs `grant`, so a credential that can only meter cannot
// spend a customer's money.
func TestRoutesAndTheirPrivileges(t *testing.T) {
	t.Parallel()
	want := []string{
		"GET /v1/auto-recharge record",
		"GET /v1/fulfilment record",
		"GET /v1/payments record",
		"GET /v1/plans record",
		"GET /v1/portal record",
		"GET /v1/subscription record",
		"POST /v1/checkout record",
		"POST /v1/subscription/cancel grant",
		"POST /v1/subscription/change grant",
		"PUT /v1/auto-recharge grant",
	}
	var got []string
	for _, r := range fullDoor(t).routes {
		got = append(got, r.Method+" "+r.Path+" "+r.Privilege.String())
		if r.Handler == nil {
			t.Errorf("%s %s has no handler", r.Method, r.Path)
		}
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("the door serves %d routes, want %d:\n got %v\nwant %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %q, want %q", i, got[i], want[i])
		}
	}
}
