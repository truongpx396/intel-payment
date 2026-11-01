package paymenthttp_test

import (
	"sort"
	"testing"
)

// Which privilege a route needs is the door's decision, and the registry enforces it before the handler runs. A read, a
// checkout and a portal are the host's everyday calls and need only `record`; what commits a customer to charges — a plan
// change, a cancellation, a standing instruction to top up — needs `grant`, so a credential that can only meter cannot
// spend a customer's money. Configuration — plans, the accounts they are sold through, the customers they are sold to — is the
// `admin` privilege's, reads included.
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
		"GET /v1/admin/customers admin",
		"GET /v1/admin/plans admin",
		"GET /v1/admin/plans/{code} admin",
		"GET /v1/admin/provider-accounts admin",
		"GET /v1/admin/provider-accounts/{id} admin",
		"PATCH /v1/admin/plans/{code} admin",
		"POST /v1/admin/plans admin",
		"PUT /v1/admin/customers admin",
		"PUT /v1/admin/plans/{code}/provider-prices/{account}/{currency} admin",
		"PUT /v1/admin/provider-accounts/{id} admin",
	}
	sort.Strings(want)
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
